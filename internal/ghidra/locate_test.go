package ghidra

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestCheckProjectPath pins Ghidra's rule, which is stricter than it looks and
// cost a confusing failure on first real use: the default cache location was
// <project>/.gdb-wui/ghidra, and every test until then had passed -decomp-dir
// with a dot-free path.
//
// Measured against Ghidra 12.1.2: both .../x/.gdbwui/ghidra and
// .../x/.hidden/sub/ghidra are refused, while the same tree without dots
// imports. So the rule is any element, not just the last.
func TestCheckProjectPath(t *testing.T) {
	ok := []string{
		"/tmp/gdb-wui-decomp",
		"/home/user/project/gdb-wui-decomp/abc123",
		"relative/path/here",
	}
	for _, p := range ok {
		if err := CheckProjectPath(p); err != nil {
			t.Errorf("CheckProjectPath(%q) = %v, want nil", p, err)
		}
	}

	bad := []string{
		"/home/user/project/.gdb-wui/ghidra", // the default that failed
		"/home/user/.cache/gdb-wui",          // $XDG_CACHE_HOME, hence unusable
		"/home/user/.local/state/gdb-wui",    // $XDG_STATE_HOME, likewise
		"/home/user/.hidden/sub/clean",       // a dot element anywhere counts
	}
	for _, p := range bad {
		err := CheckProjectPath(p)
		if err == nil {
			t.Errorf("CheckProjectPath(%q) = nil; Ghidra refuses this", p)
			continue
		}
		// The message has to name the offending element: Ghidra's own names
		// neither the path nor the reason.
		if !strings.Contains(err.Error(), p) {
			t.Errorf("error for %q does not name the path: %v", p, err)
		}
	}

	// A relative path is resolved before checking, so a dot element in the
	// working directory is caught too.
	abs, _ := filepath.Abs("x")
	if err := CheckProjectPath("x"); err != nil && !strings.Contains(err.Error(), abs) {
		t.Errorf("relative path was not resolved: %v", err)
	}
}
