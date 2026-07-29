package srcfs

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// project builds a small tree to browse:
//
//	main.c
//	README.md
//	src/util.c
//	src/deep/util.c
//	.git/config             (skipped)
//	node_modules/x/i.js     (skipped)
//	binary.o                (contains NUL)
//	link-in    -> src/util.c        (allowed: stays inside)
//	link-out   -> /etc/passwd       (refused: escapes)
//	link-dir   -> /etc              (refused: escapes)
func project(t *testing.T) *FS {
	t.Helper()
	dir := t.TempDir()

	write := func(rel, content string) {
		full := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("main.c", "int main(void) { return 0; }\n")
	write("README.md", "# project\n")
	write("src/util.c", "// src/util.c\n")
	write("src/deep/util.c", "// src/deep/util.c\n")
	write(".git/config", "[core]\n")
	write("node_modules/x/i.js", "module.exports = 1;\n")
	write("binary.o", "ELF\x00\x01\x02binary")

	link := func(target, name string) {
		if err := os.Symlink(target, filepath.Join(dir, name)); err != nil {
			t.Fatal(err)
		}
	}
	link("src/util.c", "link-in")
	link("/etc/passwd", "link-out")
	link("/etc", "link-dir")

	fsys, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = fsys.Close() })
	return fsys
}

// TestTraversalIsRefused is the core containment test. Each of these is a real
// bypass of a strings.HasPrefix check.
func TestTraversalIsRefused(t *testing.T) {
	f := project(t)
	for _, p := range []string{
		"../etc/passwd",
		"../../etc/passwd",
		"src/../../etc/passwd",
		"/etc/passwd",
		"src/./../../etc/passwd",
		"..",
		"src/..",
		"a/../../b",
		// Percent-decoding happens in net/http before we see the path, so the
		// decoded form is what must be refused.
		"..%2fetc%2fpasswd",
		"....//etc/passwd",
	} {
		if _, err := f.ReadFile(p); err == nil {
			t.Errorf("ReadFile(%q) succeeded; it must be refused", p)
		}
		if _, err := f.Tree(p); err == nil {
			t.Errorf("Tree(%q) succeeded; it must be refused", p)
		}
	}
}

// TestSymlinkEscapeIsRefused is the case a prefix check cannot catch: the path
// is entirely inside the root and contains no "..", but the filesystem takes it
// out.
func TestSymlinkEscapeIsRefused(t *testing.T) {
	f := project(t)

	if _, err := f.ReadFile("link-out"); err == nil {
		t.Error("reading a symlink to /etc/passwd succeeded")
	} else if !errors.Is(err, ErrDenied) && !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want denied or not-found", err)
	}
	if _, err := f.Tree("link-dir"); err == nil {
		t.Error("listing a symlink to /etc succeeded")
	}
	if _, err := f.ReadFile("link-dir/passwd"); err == nil {
		t.Error("reading through a symlink to /etc succeeded")
	}
}

// TestSymlinkInsideRootIsAllowed: containment must not be so blunt that it
// breaks ordinary projects, which use in-tree symlinks routinely.
func TestSymlinkInsideRootIsAllowed(t *testing.T) {
	f := project(t)
	got, err := f.ReadFile("link-in")
	if err != nil {
		t.Fatalf("reading an in-root symlink failed: %v", err)
	}
	if !strings.Contains(string(got.Content), "src/util.c") {
		t.Errorf("content = %q", got.Content)
	}
}

func TestTreeRoot(t *testing.T) {
	f := project(t)
	for _, p := range []string{"", ".", "/"} {
		got, err := f.Tree(p)
		if err != nil {
			t.Fatalf("Tree(%q): %v", p, err)
		}
		if got.Path != "" {
			t.Errorf("Tree(%q).Path = %q, want empty", p, got.Path)
		}
		names := entryNames(got)
		if contains(names, ".git") {
			t.Error(".git was listed; it should be skipped")
		}
		if contains(names, "node_modules") {
			t.Error("node_modules was listed; it should be skipped")
		}
		if !contains(names, "main.c") || !contains(names, "src") {
			t.Errorf("entries = %v, want main.c and src", names)
		}
	}
}

// TestTreeOrdersDirectoriesFirst pins the ordering so a second client renders
// the same tree.
func TestTreeOrdersDirectoriesFirst(t *testing.T) {
	f := project(t)
	got, err := f.Tree("")
	if err != nil {
		t.Fatal(err)
	}
	seenFile := false
	for _, e := range got.Entries {
		if !e.Dir {
			seenFile = true
			continue
		}
		if seenFile {
			t.Errorf("directory %q appears after a file; want directories first", e.Name)
		}
	}
}

func TestTreeEntryMetadata(t *testing.T) {
	f := project(t)
	got, err := f.Tree("")
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]Entry{}
	for _, e := range got.Entries {
		byName[e.Name] = e
	}

	if e := byName["src"]; !e.Dir || e.Path != "src" {
		t.Errorf("src = %+v, want a directory at path src", e)
	}
	if e := byName["main.c"]; e.Dir || e.Size == 0 || e.Path != "main.c" {
		t.Errorf("main.c = %+v, want a sized file", e)
	}
	// A symlink is reported as a link rather than hidden or silently followed.
	if e := byName["link-in"]; !e.Symlink {
		t.Errorf("link-in = %+v, want Symlink true", e)
	}
	if e := byName["link-out"]; !e.Symlink {
		t.Errorf("link-out = %+v, want Symlink true", e)
	}
}

func TestTreeNested(t *testing.T) {
	f := project(t)
	got, err := f.Tree("src")
	if err != nil {
		t.Fatalf("Tree(src): %v", err)
	}
	if got.Path != "src" {
		t.Errorf("Path = %q, want src", got.Path)
	}
	names := entryNames(got)
	if !contains(names, "util.c") || !contains(names, "deep") {
		t.Errorf("entries = %v", names)
	}
	for _, e := range got.Entries {
		if !strings.HasPrefix(e.Path, "src/") {
			t.Errorf("entry path %q is not rooted at src/", e.Path)
		}
	}
}

func TestTreeTruncates(t *testing.T) {
	dir := t.TempDir()
	for i := range MaxEntries + 50 {
		name := filepath.Join(dir, "f"+pad(i))
		if err := os.WriteFile(name, nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	f, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	got, err := f.Tree("")
	if err != nil {
		t.Fatal(err)
	}
	if !got.Truncated {
		t.Error("Truncated = false; a listing that drops entries must say so")
	}
	if len(got.Entries) != MaxEntries {
		t.Errorf("got %d entries, want the cap of %d", len(got.Entries), MaxEntries)
	}
}

func TestReadFile(t *testing.T) {
	f := project(t)
	got, err := f.ReadFile("src/deep/util.c")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !strings.Contains(string(got.Content), "src/deep/util.c") {
		t.Errorf("content = %q", got.Content)
	}
	if got.ETag == "" {
		t.Error("ETag is empty")
	}
	if got.Size != int64(len(got.Content)) {
		t.Errorf("Size = %d, len = %d", got.Size, len(got.Content))
	}
}

func TestReadFileRejectsBinary(t *testing.T) {
	f := project(t)
	if _, err := f.ReadFile("binary.o"); !errors.Is(err, ErrBinary) {
		t.Errorf("err = %v, want ErrBinary", err)
	}
}

func TestReadFileRejectsDirectory(t *testing.T) {
	f := project(t)
	if _, err := f.ReadFile("src"); !errors.Is(err, ErrIsDir) {
		t.Errorf("err = %v, want ErrIsDir", err)
	}
}

func TestReadFileRejectsTooLarge(t *testing.T) {
	dir := t.TempDir()
	big := make([]byte, MaxFileSize+1)
	for i := range big {
		big[i] = 'a'
	}
	if err := os.WriteFile(filepath.Join(dir, "big.txt"), big, 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	if _, err := f.ReadFile("big.txt"); !errors.Is(err, ErrTooLarge) {
		t.Errorf("err = %v, want ErrTooLarge", err)
	}
}

func TestMissingFile(t *testing.T) {
	f := project(t)
	if _, err := f.ReadFile("nope.c"); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

// TestETagChangesWithContent: a stale ETag serves stale source next to a live
// breakpoint marker, which is worse than no caching at all.
func TestETagChangesWithContent(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "a.c")
	if err := os.WriteFile(p, []byte("one"), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	first, err := f.ReadFile("a.c")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("two, which is longer"), 0o644); err != nil {
		t.Fatal(err)
	}
	second, err := f.ReadFile("a.c")
	if err != nil {
		t.Fatal(err)
	}
	if first.ETag == second.ETag {
		t.Errorf("ETag unchanged after a rewrite: %s", first.ETag)
	}
}

func TestOpenRejectsMissingRoot(t *testing.T) {
	if _, err := Open(filepath.Join(t.TempDir(), "nope")); err == nil {
		t.Error("Open of a missing directory succeeded")
	}
}

func entryNames(l Listing) []string {
	out := make([]string, 0, len(l.Entries))
	for _, e := range l.Entries {
		out = append(out, e.Name)
	}
	return out
}

func contains(hay []string, needle string) bool {
	for _, s := range hay {
		if s == needle {
			return true
		}
	}
	return false
}

func pad(i int) string {
	s := "0000" + itoa(i)
	return s[len(s)-5:]
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}

// TestClassify covers the signal the file tree uses to tell a program from a
// file. Getting it wrong means clicking a source file tries to load it into
// gdb, or a program silently opens as text.
func TestClassify(t *testing.T) {
	dir := t.TempDir()
	write := func(name string, body []byte, mode os.FileMode) {
		if err := os.WriteFile(filepath.Join(dir, name), body, mode); err != nil {
			t.Fatal(err)
		}
	}
	elf := append([]byte{0x7f, 'E', 'L', 'F'}, make([]byte, 32)...)
	write("prog", elf, 0o755)
	// An ELF file without the execute bit: a .o or a stripped artefact. Not a
	// debuggee you can run, so it is not offered as one.
	write("prog.o", elf, 0o644)
	write("script.sh", []byte("#!/bin/sh\necho hi\n"), 0o755)
	write("main.c", []byte("int main(void){return 0;}\n"), 0o644)
	write("noext", []byte("just text\n"), 0o644)
	write("tiny", []byte("#!"), 0o755)

	f, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	got, err := f.Tree("")
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]string{}
	for _, e := range got.Entries {
		kinds[e.Name] = e.Kind
	}

	for name, want := range map[string]string{
		"prog":      KindELF,
		"prog.o":    KindPlain,
		"script.sh": KindExec,
		"main.c":    KindPlain,
		"noext":     KindPlain,
		"tiny":      KindExec,
	} {
		if kinds[name] != want {
			t.Errorf("%s: kind = %q, want %q", name, kinds[name], want)
		}
	}
}
