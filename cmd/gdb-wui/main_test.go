package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/retrocpugeek/gdb-wui/internal/ghidra"
)

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

// fakeInstall writes just enough of a Ghidra tree for Locate to accept it.
func fakeInstall(t *testing.T, version string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "support"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "support", "analyzeHeadless"),
		[]byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "Ghidra"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "Ghidra", "application.properties"),
		[]byte("application.name=Ghidra\napplication.version="+version+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// TestDecompConfigRefusesAnOldGhidra covers finding 28 end to end at the flag
// level: too old has to be refused here, in the millisecond it takes to read a
// properties file, rather than four minutes later as a ClassNotFoundException
// out of Ghidra's script compiler.
//
// The reason has to survive into the config, because logf is a no-op without
// -v: the pane is the only place a default run says anything at all.
func TestDecompConfigRefusesAnOldGhidra(t *testing.T) {
	old := fakeInstall(t, "12.0.4")
	cfg := decompConfig(options{ghidraDir: old}, t.TempDir(), func(string, ...any) {})
	if cfg.Install != nil {
		t.Error("Install is set for a Ghidra too old to compile the scripts")
	}
	for _, want := range []string{"12.0.4", ghidra.MinVersion, old} {
		if !strings.Contains(cfg.Unavailable, want) {
			t.Errorf("Unavailable = %q, missing %q", cfg.Unavailable, want)
		}
	}
}

// TestDecompConfigAcceptsTheMinimum: the boundary release itself, and one past
// it, both have to work — an off-by-one here disables the feature for everyone.
func TestDecompConfigAcceptsTheMinimum(t *testing.T) {
	for _, v := range []string{ghidra.MinVersion, "12.1.3", "13.0"} {
		dir := fakeInstall(t, v)
		cfg := decompConfig(options{ghidraDir: dir}, t.TempDir(), func(string, ...any) {})
		if cfg.Install == nil {
			t.Errorf("Ghidra %s was refused: %s", v, cfg.Unavailable)
			continue
		}
		if cfg.Unavailable != "" {
			t.Errorf("Ghidra %s accepted but Unavailable = %q", v, cfg.Unavailable)
		}
	}
}
