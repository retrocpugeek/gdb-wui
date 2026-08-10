package debugger

import "testing"

func TestPlausibleDecompName(t *testing.T) {
	yes := []string{"FUN_0010e2dc", "DAT_001a08de", "run_applet_and_exit", "_start", "a1"}
	no := []string{"", "$pc", "&head", "0x401136", "demo.c", "a b", "1st", "main()"}
	for _, s := range yes {
		if !plausibleDecompName(s) {
			t.Errorf("plausibleDecompName(%q) = false, want true", s)
		}
	}
	for _, s := range no {
		if plausibleDecompName(s) {
			t.Errorf("plausibleDecompName(%q) = true; it is an expression for gdb, not a label", s)
		}
	}
}
