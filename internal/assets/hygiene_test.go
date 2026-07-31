package assets_test

import (
	"crypto/sha256"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/retrocpugeek/gdb-wui/internal/assets"
	"github.com/retrocpugeek/gdb-wui/internal/wire"
)

// This file holds the checks that stand in for a build step. A zero-build
// frontend has no compiler, so its failure mode is a blank page with a green
// CI; each test here converts one such silent failure into a red one.

func embedded(t *testing.T) fs.FS {
	t.Helper()
	a, err := assets.Embedded()
	if err != nil {
		t.Fatalf("Embedded: %v", err)
	}
	return a.FS()
}

// TestJavaScriptParses is the single highest-value frontend check: without a
// bundler, one typo in a module yields a blank page and no test failure
// anywhere. node --check costs about a second.
func TestJavaScriptParses(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed; skipping the syntax check")
	}
	root := repoRoot(t)
	var checked int
	err = filepath.WalkDir(filepath.Join(root, "internal", "assets", "web"),
		func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			if !strings.HasSuffix(path, ".js") && !strings.HasSuffix(path, ".mjs") {
				return nil
			}
			// --input-type=module: these are ES modules, and node's default
			// script mode rejects `import` with a syntax error that would make
			// every file look broken.
			cmd := exec.Command(node, "--input-type=module", "--check")
			src, readErr := os.ReadFile(path)
			if readErr != nil {
				return readErr
			}
			cmd.Stdin = strings.NewReader(string(src))
			if out, runErr := cmd.CombinedOutput(); runErr != nil {
				rel, _ := filepath.Rel(root, path)
				t.Errorf("%s does not parse:\n%s", rel, out)
			}
			checked++
			return nil
		})
	if err != nil {
		t.Fatalf("walking the web tree: %v", err)
	}
	if checked == 0 {
		t.Error("no JavaScript was checked; the walk found nothing")
	}
}

// TestColourLiteralsOnlyInTokens enforces the theming rule. A colour that
// escapes tokens.css is invisible to the light theme and to xterm, which builds
// its palette from these same custom properties at boot.
func TestColourLiteralsOnlyInTokens(t *testing.T) {
	fsys := embedded(t)
	colour := regexp.MustCompile(`#[0-9a-fA-F]{3,8}\b|\brgba?\(|\bhsla?\(`)

	err := fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".css") {
			return err
		}
		// tokens.css is the one file allowed colours; vendored code is exempt
		// because it is byte-identical third-party source and rewriting it
		// would break the hash manifest that keeps it honest.
		if p == "css/tokens.css" || strings.HasPrefix(p, "vendor/") {
			return nil
		}
		body, err := fs.ReadFile(fsys, p)
		if err != nil {
			return err
		}
		for i, line := range strings.Split(string(body), "\n") {
			if colour.MatchString(line) {
				t.Errorf("%s:%d has a colour literal; it belongs in css/tokens.css\n  %s",
					p, i+1, strings.TrimSpace(line))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestIndexReferencesResolve catches the other classic zero-build failure: a
// renamed file leaves a dangling <script src> that 404s at runtime.
func TestIndexReferencesResolve(t *testing.T) {
	fsys := embedded(t)
	index, err := fs.ReadFile(fsys, "index.html")
	if err != nil {
		t.Fatal(err)
	}
	refs := regexp.MustCompile(`(?:src|href)="(/[^"]+)"`).FindAllStringSubmatch(string(index), -1)
	if len(refs) == 0 {
		t.Fatal("index.html references no assets; the regex or the file is wrong")
	}
	for _, m := range refs {
		ref := strings.TrimPrefix(m[1], "/")
		if _, err := fs.Stat(fsys, ref); err != nil {
			t.Errorf("index.html references %q, which is not in the embedded tree", m[1])
		}
	}
}

// TestModuleImportsResolve walks the import graph the browser will walk.
func TestModuleImportsResolve(t *testing.T) {
	fsys := embedded(t)
	importRe := regexp.MustCompile(`(?m)^\s*import\s+[^"']*["']([^"']+)["']`)

	err := fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".js") {
			return err
		}
		if strings.HasPrefix(p, "vendor/") {
			return nil // checked by TestVendoredModulesAreBrowserLoadable
		}
		body, err := fs.ReadFile(fsys, p)
		if err != nil {
			return err
		}
		for _, m := range importRe.FindAllStringSubmatch(string(body), -1) {
			spec := m[1]
			if !strings.HasPrefix(spec, ".") && !strings.HasPrefix(spec, "/") {
				t.Errorf("%s imports %q; bare specifiers do not resolve in a browser "+
					"without an import map", p, spec)
				continue
			}
			var target string
			if strings.HasPrefix(spec, "/") {
				target = strings.TrimPrefix(spec, "/")
			} else {
				target = pathJoin(dirOf(p), spec)
			}
			if _, err := fs.Stat(fsys, target); err != nil {
				t.Errorf("%s imports %q, which resolves to %q and does not exist",
					p, spec, target)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestImportDirection keeps the layering honest: core is infrastructure and
// must not reach up into the panels that use it.
func TestImportDirection(t *testing.T) {
	fsys := embedded(t)
	err := fs.WalkDir(fsys, "js/core", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		body, err := fs.ReadFile(fsys, p)
		if err != nil {
			return err
		}
		if strings.Contains(string(body), "panels/") {
			t.Errorf("%s imports from panels/; core must not depend on panels", p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestProtocolDocumented is the docs-honesty test. Every request type, event
// and error code the code knows about must be written down, because the
// frontend is not type-checked against the server and the document is the only
// thing standing in for that.
func TestProtocolDocumented(t *testing.T) {
	doc, err := os.ReadFile(filepath.Join(repoRoot(t), "docs", "protocol.md"))
	if err != nil {
		t.Fatalf("reading docs/protocol.md: %v", err)
	}
	text := string(doc)

	for _, typ := range wire.RequestTypes {
		if !strings.Contains(text, typ) {
			t.Errorf("request type %q is not documented in docs/protocol.md", typ)
		}
	}
	for _, ev := range wire.EventNames {
		if !strings.Contains(text, "`"+ev+"`") {
			t.Errorf("event %q is not documented in docs/protocol.md", ev)
		}
	}
	for _, code := range wire.ErrorCodes {
		if !strings.Contains(text, "`"+code+"`") {
			t.Errorf("error code %q is not documented in docs/protocol.md", code)
		}
	}

	// The other direction, which is how the Reserved list quietly went stale:
	// six names sat under "Requesting one today returns `unsupported`" long
	// after they had been implemented. Documenting a working feature as absent
	// is as misleading as leaving one undocumented.
	if _, after, found := strings.Cut(text, "### Reserved"); found {
		reserved, _, _ := strings.Cut(after, "\n## ")
		for _, typ := range wire.RequestTypes {
			if strings.Contains(reserved, "`"+typ+"`") {
				t.Errorf("request type %q is implemented but still listed under "+
					"Reserved in docs/protocol.md", typ)
			}
		}
	}
}

func dirOf(p string) string {
	i := strings.LastIndex(p, "/")
	if i < 0 {
		return "."
	}
	return p[:i]
}

// pathJoin resolves a relative import against a directory, slash-only.
func pathJoin(dir, rel string) string {
	parts := strings.Split(dir, "/")
	if dir == "." {
		parts = nil
	}
	for _, seg := range strings.Split(rel, "/") {
		switch seg {
		case ".", "":
		case "..":
			if len(parts) > 0 {
				parts = parts[:len(parts)-1]
			}
		default:
			parts = append(parts, seg)
		}
	}
	return strings.Join(parts, "/")
}

func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.mod above the test directory")
		}
		dir = parent
	}
}

// TestVendoredHashes is the supply-chain check for a repository with no
// lockfile.
//
// Every vendored file's sha256 is recorded in VENDOR.md, and this recomputes
// them. A file edited in place — by a well-meaning fix, a patch, or something
// worse — fails here rather than shipping inside the binary. It is about forty
// lines and it is the entire story.
func TestVendoredHashes(t *testing.T) {
	fsys := embedded(t)

	manifest, err := fs.ReadFile(fsys, "vendor/VENDOR.md")
	if err != nil {
		t.Fatalf("reading VENDOR.md: %v", err)
	}
	recorded := map[string]string{}
	// Table rows look like: | `xterm-6.0.0/xterm.mjs` | … | `<sha256>` |
	row := regexp.MustCompile("\\|\\s*`([^`]+)`\\s*\\|.*\\|\\s*`([0-9a-f]{64})`\\s*\\|")
	for _, m := range row.FindAllStringSubmatch(string(manifest), -1) {
		recorded[m[1]] = m[2]
	}
	if len(recorded) == 0 {
		t.Fatal("VENDOR.md lists no hashes; the manifest or this regex is wrong")
	}

	var checked int
	err = fs.WalkDir(fsys, "vendor", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel := strings.TrimPrefix(p, "vendor/")
		if rel == "VENDOR.md" {
			return nil
		}
		body, err := fs.ReadFile(fsys, p)
		if err != nil {
			return err
		}
		sum := fmt.Sprintf("%x", sha256.Sum256(body))

		want, ok := recorded[rel]
		if !ok {
			t.Errorf("%s is vendored but not listed in VENDOR.md (sha256 %s)", rel, sum)
			return nil
		}
		if sum != want {
			t.Errorf("%s has changed:\n  have %s\n  want %s\n"+
				"A vendored file must be byte-identical to what the registry published. "+
				"If this was deliberate, update VENDOR.md.", rel, sum, want)
		}
		delete(recorded, rel)
		checked++
		return nil
	})
	if err != nil {
		t.Fatalf("walking vendor: %v", err)
	}
	for rel := range recorded {
		t.Errorf("VENDOR.md lists %s, which is not vendored", rel)
	}
	if checked == 0 {
		t.Error("no vendored files were checked")
	}
}

// TestVendoredModulesAreBrowserLoadable guards the trap the plan calls out: an
// ESM build that turns out to be a CommonJS wrapper loads fine in Node and not
// at all in a browser, and the symptom is a blank page.
func TestVendoredModulesAreBrowserLoadable(t *testing.T) {
	fsys := embedded(t)
	bare := regexp.MustCompile(`(?m)(?:^|[;\s])(?:import|export)\s*(?:[^'"\n]*\sfrom\s*)?["']([^."'/][^"']*)["']`)

	err := fs.WalkDir(fsys, "vendor", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".mjs") {
			return err
		}
		body, err := fs.ReadFile(fsys, p)
		if err != nil {
			return err
		}
		for _, m := range bare.FindAllStringSubmatch(string(body), -1) {
			t.Errorf("%s imports the bare specifier %q; a browser cannot resolve that "+
				"without an import map, and the page would be blank", p, m[1])
		}
		if !strings.Contains(string(body), "export") {
			t.Errorf("%s has no export; is it really an ES module?", p)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking vendor: %v", err)
	}
}
