package main

import "testing"

// TestGDBCommandsCollectInOrder pins the one thing the flag has to get right.
//
// Order is the whole content of the setting: `set architecture` before `target
// remote` works and the other way round misparses the stub's first reply. A
// value that replaced rather than appended would silently run only the last.
func TestGDBCommandsCollectInOrder(t *testing.T) {
	var c gdbCommands
	for _, line := range []string{"set architecture arm", "target remote :9999"} {
		if err := c.Set(line); err != nil {
			t.Fatalf("Set(%q): %v", line, err)
		}
	}
	// Surrounding space is a shell artefact, not part of the command.
	if err := c.Set("  info registers  "); err != nil {
		t.Fatal(err)
	}
	want := []string{"set architecture arm", "target remote :9999", "info registers"}
	if len(c) != len(want) {
		t.Fatalf("collected %q, want %q", []string(c), want)
	}
	for i := range want {
		if c[i] != want[i] {
			t.Errorf("[%d] = %q, want %q", i, c[i], want[i])
		}
	}
	got, ok := c.Get().([]string)
	if !ok || len(got) != len(want) {
		t.Errorf("Get() = %#v; config.Save reads this to decide it is a list", c.Get())
	}
}

// TestGDBCommandsRefuseWhatGDBWouldRefuse. Both of these reach gdb through
// console.exec, which takes one command at a time — so a newline would be
// rejected a whole startup later, by which point the message names neither the
// flag nor which of the commands was the problem.
func TestGDBCommandsRefuseWhatGDBWouldRefuse(t *testing.T) {
	for _, bad := range []string{"", "   ", "set architecture arm\ntarget remote :9999"} {
		var c gdbCommands
		if err := c.Set(bad); err == nil {
			t.Errorf("Set(%q) was accepted", bad)
		}
		if len(c) != 0 {
			t.Errorf("Set(%q) collected %q anyway", bad, []string(c))
		}
	}
}

// TestGDBCommandsStartEmpty keeps -help honest and keeps config.Save from
// writing the flag into every file: Save skips a flag whose String matches the
// default it was registered with, which for this one is the empty list.
func TestGDBCommandsStartEmpty(t *testing.T) {
	var c gdbCommands
	if c.String() != "" {
		t.Errorf("an unset -gdb-command reads as %q, want empty", c.String())
	}
}
