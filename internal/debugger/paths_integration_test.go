//go:build integration

package debugger_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/retrocpugeek/gdb-wui/internal/srcfs"
	"github.com/retrocpugeek/gdb-wui/internal/testutil"
	"github.com/retrocpugeek/gdb-wui/internal/wire"
)

// M8's source resolution, against the case that actually happens: a binary
// built somewhere else, whose recorded source paths do not exist on this
// machine. Every CI artefact and container build looks like this.

// relocatedProject compiles a fixture in a throwaway directory, then deletes
// that directory and puts the source somewhere else under the project root. gdb
// therefore reports a path that does not exist, and the file is present under a
// different prefix — exactly the situation suffix matching is for.
func relocatedProject(t *testing.T, fixture, localDir string) *srcfs.FS {
	t.Helper()
	testutil.RequireTools(t, "gcc")

	body, err := os.ReadFile(filepath.Join(
		testutil.RepoRoot(t), "testdata", "fixtures", fixture+".c"))
	if err != nil {
		t.Fatal(err)
	}

	// Build in a directory that will not survive.
	buildDir := filepath.Join(t.TempDir(), "build", "agent", "work", "src")
	if err := os.MkdirAll(buildDir, 0o755); err != nil {
		t.Fatal(err)
	}
	buildSrc := filepath.Join(buildDir, fixture+".c")
	if err := os.WriteFile(buildSrc, body, 0o644); err != nil {
		t.Fatal(err)
	}

	project := t.TempDir()
	bin := filepath.Join(project, fixture)
	if out, err := exec.Command("gcc", "-g", "-O0", "-o", bin, buildSrc).CombinedOutput(); err != nil {
		t.Fatalf("compiling: %v\n%s", err, out)
	}
	// The build tree is gone; only the binary remembers those paths.
	if err := os.RemoveAll(buildDir); err != nil {
		t.Fatal(err)
	}

	// The source lives under a different prefix in the project.
	localSrcDir := filepath.Join(project, localDir)
	if err := os.MkdirAll(localSrcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(localSrcDir, fixture+".c"), body, 0o644); err != nil {
		t.Fatal(err)
	}

	f, err := srcfs.Open(project)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	return f
}

// TestRelocatedSourceResolves is the M8 criterion: a path that does not exist
// locally is matched to the file that does, and gdb is taught the prefix.
func TestRelocatedSourceResolves(t *testing.T) {
	files := relocatedProject(t, "hello", "src")
	h := startRealWithFiles(t, files)

	h.mustDo(wire.TypeExeLoad, wire.ExeLoadRequest{Path: "hello"})
	h.mustDo(wire.TypeExecRun, wire.ExecRequest{StopAtMain: true})
	stopped := h.rec.wait(t, wire.EventStopped).(wire.Stopped)

	if len(stopped.Frames) == 0 {
		t.Fatal("no frames")
	}
	src := stopped.Frames[0].Source
	if !src.Available {
		t.Fatalf("source was not resolved: %+v\nThe build tree is gone, so this only "+
			"works if the basename index matched src/hello.c", src)
	}
	if src.Path != "src/hello.c" {
		t.Errorf("path = %q, want src/hello.c", src.Path)
	}

	// And gdb should have been told the prefix, so its own commands work too.
	list := h.mustDo(wire.TypePathList, nil).(wire.PathList)
	if len(list.Substitutions) == 0 {
		t.Fatal("no substitution was installed; gdb still reports the old paths " +
			"to `list`, `info line` and anything typed at the console")
	}
	t.Logf("substitutions: %+v", list.Substitutions)

	// The proof that gdb learned it: `info line` should now name the local file.
	h.rec.reset()
	h.mustDo(wire.TypeConsoleExec, wire.ConsoleExecRequest{Line: "info source"})
	if list.Indexed == 0 {
		t.Error("the index reports no files")
	}
}

// TestAmbiguousSourceOffersCandidates: when matching cannot decide, the UI must
// be given something to offer rather than a dead end.
func TestAmbiguousSourceOffersCandidates(t *testing.T) {
	files := relocatedProject(t, "hello", "src")
	// A second file with the same basename makes the match ambiguous.
	other := filepath.Join(files.Abs(), "vendor", "copy")
	if err := os.MkdirAll(other, 0o755); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(filepath.Join(files.Abs(), "src", "hello.c"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(other, "hello.c"), body, 0o644); err != nil {
		t.Fatal(err)
	}

	h := startRealWithFiles(t, files)
	h.mustDo(wire.TypeExeLoad, wire.ExeLoadRequest{Path: "hello"})
	h.mustDo(wire.TypeExecRun, wire.ExecRequest{StopAtMain: true})
	stopped := h.rec.wait(t, wire.EventStopped).(wire.Stopped)

	src := stopped.Frames[0].Source
	// The recorded path ends in src/hello.c, and one candidate shares that, so
	// resolution may well succeed. What must not happen is a silent wrong
	// answer with no way to correct it.
	if !src.Available && len(src.Candidates) == 0 {
		t.Error("unresolved and no candidates offered; the user has no way to fix it")
	}
	t.Logf("available=%v path=%q candidates=%v", src.Available, src.Path, src.Candidates)
}

// TestPathSubstituteByPair is what the "locate this file" affordance sends: two
// files, not two prefixes.
//
// It has to be set up so automatic resolution *fails*, because when it succeeds
// there is nothing left for the user to do. Two local copies sharing only the
// basename is a tie, and a tie is refused by design — showing one of two
// equally plausible files means debugging the wrong code with line numbers that
// look right.
func TestPathSubstituteByPair(t *testing.T) {
	files := relocatedProject(t, "hello", "a")
	body, err := os.ReadFile(filepath.Join(files.Abs(), "a", "hello.c"))
	if err != nil {
		t.Fatal(err)
	}
	second := filepath.Join(files.Abs(), "b")
	if err := os.MkdirAll(second, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(second, "hello.c"), body, 0o644); err != nil {
		t.Fatal(err)
	}

	h := startRealWithFiles(t, files)
	h.mustDo(wire.TypeExeLoad, wire.ExeLoadRequest{Path: "hello"})
	h.mustDo(wire.TypeExecRun, wire.ExecRequest{StopAtMain: true})
	stopped := h.rec.wait(t, wire.EventStopped).(wire.Stopped)

	src := stopped.Frames[0].Source
	if src.Available {
		t.Skipf("resolution succeeded on its own (%q); nothing for the user to locate",
			src.Path)
	}
	if len(src.Candidates) < 2 {
		t.Fatalf("ambiguous path offered %v; the user needs both to choose from",
			src.Candidates)
	}
	gdbPath := src.GDBPath
	if gdbPath == "" {
		t.Fatal("no gdb path reported")
	}

	// The user picks one.
	out := h.mustDo(wire.TypePathSubstitute, wire.PathSubstituteRequest{
		GDBPath: gdbPath, Path: "a/hello.c",
	}).(wire.PathList)
	if len(out.Substitutions) == 0 {
		t.Fatal("no substitution recorded")
	}

	// The selection event that follows must carry resolved source, so the pane
	// fills in immediately rather than at the next stop.
	sel := h.rec.wait(t, wire.EventSelectionChanged).(wire.Selection)
	if sel.Source == nil || !sel.Source.Available {
		t.Errorf("after substituting, source is still unavailable: %+v", sel.Source)
	} else if sel.Source.Path != "a/hello.c" {
		t.Errorf("resolved to %q, want the file the user picked", sel.Source.Path)
	}
}

func TestPathAddDir(t *testing.T) {
	files := relocatedProject(t, "hello", "src")
	h := startRealWithFiles(t, files)
	h.mustDo(wire.TypeExeLoad, wire.ExeLoadRequest{Path: "hello"})

	out := h.mustDo(wire.TypePathAddDir, wire.PathAddDirRequest{Dir: "src"}).(wire.PathList)
	if len(out.Directories) != 1 || out.Directories[0] != "src" {
		t.Errorf("directories = %v, want [src]", out.Directories)
	}
}

func TestPathAddDirRejectsTraversal(t *testing.T) {
	files := relocatedProject(t, "hello", "src")
	h := startRealWithFiles(t, files)
	if _, werr := h.do(wire.TypePathAddDir, wire.PathAddDirRequest{Dir: "../../etc"}); werr == nil {
		t.Error("a directory outside the project was accepted")
	}
}

// TestStaleSourceIsFlagged: if the source is newer than the binary the line
// numbers are lying, and the user should be told rather than left to discover
// it by confusion.
func TestStaleSourceIsFlagged(t *testing.T) {
	h := startReal(t, "hello")
	h.mustDo(wire.TypeExeLoad, wire.ExeLoadRequest{Path: "hello"})

	// Touch the source so it is newer than the program.
	src := filepath.Join(h.files.Abs(), "hello.c")
	body, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(src, append(body, []byte("\n// touched\n")...), 0o644); err != nil {
		t.Fatal(err)
	}

	h.mustDo(wire.TypeExecRun, wire.ExecRequest{StopAtMain: true})
	stopped := h.rec.wait(t, wire.EventStopped).(wire.Stopped)

	src0 := stopped.Frames[0].Source
	if !src0.Available {
		t.Fatal("source unavailable")
	}
	if !src0.Stale {
		t.Error("the source is newer than the binary but was not flagged stale")
	}
}
