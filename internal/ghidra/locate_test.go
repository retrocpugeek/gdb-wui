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

// TestCheckVersion pins the boundary finding 28 cost a day to: 12.0.4 has no
// DecompileOptions.setCommentIndent, so the scripts do not compile against it
// and Ghidra reports that as a ClassNotFoundException naming no version.
func TestCheckVersion(t *testing.T) {
	tooOld := []string{"12.0.4", "12.1.1", "12.1", "12.0", "11.3.2", "9.2"}
	for _, v := range tooOld {
		err := CheckVersion(&Install{Dir: "/opt/ghidra", Version: v})
		if err == nil {
			t.Errorf("CheckVersion(%q) = nil, want an error", v)
			continue
		}
		// The message has to carry both numbers, or it cannot be acted on.
		for _, want := range []string{v, MinVersion, "/opt/ghidra"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("CheckVersion(%q) = %q, missing %q", v, err, want)
			}
		}
	}

	ok := []string{MinVersion, "12.1.2", "12.1.3", "12.1.10", "12.2", "13.0", "12.1.2.1"}
	for _, v := range ok {
		if err := CheckVersion(&Install{Dir: "/opt/ghidra", Version: v}); err != nil {
			t.Errorf("CheckVersion(%q) = %v, want nil", v, err)
		}
	}

	// Unknown or unparseable is allowed through: it costs the check, not the
	// feature, and a release numbered some new way should still run.
	unknown := []string{"", "12.1.2-DEV", "DEV", "12.x"}
	for _, v := range unknown {
		if err := CheckVersion(&Install{Dir: "/opt/ghidra", Version: v}); err != nil {
			t.Errorf("CheckVersion(%q) = %v, want nil for an unreadable version", v, err)
		}
	}

	if err := CheckVersion(nil); err != nil {
		t.Errorf("CheckVersion(nil) = %v, want nil", err)
	}
}

// TestCompareVersions covers the ordering CheckVersion rests on, including the
// case that made it necessary: a component missing counts as zero, so 12.1 is
// older than 12.1.2 rather than equal to it.
func TestCompareVersions(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"12.1.2", "12.1.2", 0},
		{"12.1", "12.1.2", -1},
		{"12.1.2", "12.1", 1},
		{"12.0.4", "12.1.2", -1},
		{"12.1.10", "12.1.2", 1}, // numeric, not lexical
		{"9.2", "12.1.2", -1},    // likewise
		{"13", "12.1.2", 1},
		{"12.1.2.0", "12.1.2", 0},
	}
	for _, c := range cases {
		got, ok := compareVersions(c.a, c.b)
		if !ok {
			t.Errorf("compareVersions(%q, %q) not ok", c.a, c.b)
			continue
		}
		if got != c.want {
			t.Errorf("compareVersions(%q, %q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}

	for _, v := range []string{"12.1.2-DEV", "", "12.x", "-1.0"} {
		if _, ok := compareVersions(v, MinVersion); ok {
			t.Errorf("compareVersions(%q, %q) ok = true, want false", v, MinVersion)
		}
	}
}
