//go:build integration

package debugger_test

import (
	"testing"
	"time"

	"github.com/retrocpugeek/gdb-wui/internal/wire"
)

// Breakpoints on a decompiler name placed before the program starts.
//
// The harness's stripped program is position-independent, so the decompiler's
// addresses before `run` are link-time ones and not where it runs. A breakpoint
// placed on one then went in at the link-time address, failed to insert when
// the program started, and left it running past the place. Ghidra's `entry`
// is the name used: gdb has no symbol for it, and every run passes through it.

// entryAt is where the decompiler's entry is once the program is loaded.
func entryAt(t *testing.T, do func(string, any) any) string {
	t.Helper()
	e := symNamed(decompSymbols(t, do, "entry").Symbols, "entry")
	if e == nil || e.From != wire.SymbolFromDecompiler {
		t.Fatalf("no decompiler name entry: %+v", e)
	}
	return e.Address
}

func breakpointsNow(t *testing.T, do func(string, any) any) []wire.Breakpoint {
	t.Helper()
	return do(wire.TypeBpList, nil).(wire.BreakpointList).Breakpoints
}

func TestBreakOnADecompilerNameBeforeRunning(t *testing.T) {
	k := decompHarness(t)
	do := k.do
	do(wire.TypeExeLoad, wire.ExeLoadRequest{Path: "stripped"})
	waitReady(t, do)

	bp := do(wire.TypeBpSetAddress, wire.BreakpointAddressRequest{Location: "entry"}).(wire.Breakpoint)
	if !bp.Pending {
		t.Fatalf("a breakpoint on entry before the program starts is at %s; "+
			"that is a link-time address, not where the program will run", bp.Address)
	}

	do(wire.TypeExecRun, wire.ExecRequest{})
	waitStopped(t, do, 60*time.Second)
	want := entryAt(t, do)
	if pc := currentPCOf(t, do); !sameAddressStr(pc, want) {
		t.Fatalf("stopped at %s, want the decompiler's entry at %s", pc, want)
	}
	bps := breakpointsNow(t, do)
	if len(bps) != 1 || bps[0].Pending || !sameAddressStr(bps[0].Address, want) {
		t.Errorf("breakpoints after the start: %+v; want one, at %s", bps, want)
	}
}

// Run→entry wanted the stop at the first instruction: it gets it, with the
// breakpoint placed for the rest of the run.
func TestRunToEntryPlacesADeferredBreakpoint(t *testing.T) {
	k := decompHarness(t)
	do := k.do
	do(wire.TypeExeLoad, wire.ExeLoadRequest{Path: "stripped"})
	waitReady(t, do)
	do(wire.TypeBpSetAddress, wire.BreakpointAddressRequest{Location: "entry"})

	do(wire.TypeExecRun, wire.ExecRequest{StopAtEntry: true})
	waitStopped(t, do, 60*time.Second)
	want := entryAt(t, do)
	if pc := currentPCOf(t, do); sameAddressStr(pc, want) {
		t.Fatalf("Run→entry went on to entry (%s) instead of stopping at the first instruction", pc)
	}
	bps := breakpointsNow(t, do)
	if len(bps) != 1 || bps[0].Pending || !sameAddressStr(bps[0].Address, want) {
		t.Fatalf("breakpoints at the first instruction: %+v; want one, at %s", bps, want)
	}
	do(wire.TypeExecContinue, wire.ExecRequest{})
	waitStopped(t, do, 60*time.Second)
	if pc := currentPCOf(t, do); !sameAddressStr(pc, want) {
		t.Errorf("continued to %s, want entry at %s", pc, want)
	}
}

// Run-to a decompiler name from a program not yet started.
func TestRunToADecompilerNameBeforeRunning(t *testing.T) {
	k := decompHarness(t)
	do := k.do
	do(wire.TypeExeLoad, wire.ExeLoadRequest{Path: "stripped"})
	waitReady(t, do)

	do(wire.TypeExecRunTo, wire.ExecRunToRequest{Location: "entry"})
	waitStopped(t, do, 60*time.Second)
	want := entryAt(t, do)
	if pc := currentPCOf(t, do); !sameAddressStr(pc, want) {
		t.Fatalf("ran to %s, want entry at %s", pc, want)
	}
	if bps := breakpointsNow(t, do); len(bps) != 0 {
		t.Errorf("the run-to's temporary breakpoint is still there: %+v", bps)
	}
}
