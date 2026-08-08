// Package testutil holds the gates and fixture plumbing shared by the
// integration tests.
package testutil

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// RequireTools skips the test unless every named tool is on PATH.
//
// Skip, not fail: a contributor without gdb installed should get a clear skip
// from `go test ./...`, not a red build they cannot act on. CI installs the
// tools explicitly, so nothing is silently unverified there.
func RequireTools(t *testing.T, tools ...string) {
	t.Helper()
	for _, tool := range tools {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s is not installed; skipping", tool)
		}
	}
}

// RequireGDB skips unless gdb is present and at least version min (major).
// gdb 10 is the floor: it is the first release whose MI3 carries the features
// the debugger layer relies on.
func RequireGDB(t *testing.T, minMajor int) {
	t.Helper()
	RequireTools(t, "gdb")
	out, err := exec.Command("gdb", "--version").Output()
	if err != nil {
		t.Skipf("gdb --version failed: %v", err)
	}
	major := gdbMajor(string(out))
	if major == 0 {
		t.Skipf("could not parse gdb version from %q", firstLine(string(out)))
	}
	if major < minMajor {
		t.Skipf("gdb %d is older than the required %d", major, minMajor)
	}
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// gdbMajor pulls the major version out of a "GNU gdb (Ubuntu 17.1-2ubuntu1) 17.1"
// banner. The distribution suffix in parentheses is skipped, because it
// contains digits that are not the version.
func gdbMajor(banner string) int {
	line := firstLine(banner)
	if i := strings.LastIndexByte(line, ')'); i >= 0 {
		line = line[i+1:]
	}
	fields := strings.Fields(line)
	if len(fields) == 0 {
		return 0
	}
	v := fields[len(fields)-1]
	if i := strings.IndexByte(v, '.'); i >= 0 {
		v = v[:i]
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0
	}
	return n
}

// RepoRoot returns the module root, found by walking up from the test's
// directory to the go.mod. Tests run in their package directory, so no test can
// hardcode a relative path to testdata without breaking when it moves.
func RepoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.mod found above the test directory")
		}
		dir = parent
	}
}

// Fixture compiles testdata/fixtures/<name>.c and returns the binary's path.
// The binary lands in the test's temp dir, so it is removed automatically and
// concurrent tests never share one.
func Fixture(t *testing.T, name string, cflags ...string) string {
	t.Helper()
	RequireTools(t, "gcc")

	src := filepath.Join(RepoRoot(t), "testdata", "fixtures", name+".c")
	if _, err := os.Stat(src); err != nil {
		t.Fatalf("fixture source: %v", err)
	}
	bin := filepath.Join(t.TempDir(), name)

	args := append([]string{"-o", bin}, cflags...)
	args = append(args, src)
	cmd := exec.Command("gcc", args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("compiling %s: %v\n%s", name, err, out)
	}
	return bin
}

// DebugFixture compiles a fixture the way a user would build a debuggable
// program.
func DebugFixture(t *testing.T, name string, extra ...string) string {
	t.Helper()
	return Fixture(t, name, append([]string{"-g", "-O0"}, extra...)...)
}
