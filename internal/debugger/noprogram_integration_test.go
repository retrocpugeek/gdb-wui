//go:build integration

package debugger_test

import (
	"strings"
	"testing"

	"github.com/retrocpugeek/gdb-wui/internal/wire"
)

// The session with no program in gdb.
//
// Attached to an emulator or a stub running an image gdb will not take — a raw
// kernel Image has no ELF header, and gdb answers "not in executable format" —
// there is no file and there never will be. Everything still has to work from
// addresses: they are the only vocabulary such a target has, and the
// decompiler supplies them through -ghidra-binary.
//
// gdb itself is content with this. `-break-insert -f *0x40000000` with no file
// and no target resolves immediately rather than going pending, and disable,
// delete and list all behave. What refused it was gdb-wui's own gate.

// TestABreakpointAtAnAddressNeedsNoProgram is the regression test. Setting one
// was refused outright with "no program is loaded", which left an emulated
// kernel with no way to break anywhere at all.
func TestABreakpointAtAnAddressNeedsNoProgram(t *testing.T) {
	h := startReal(t, "hello")
	// Deliberately no h.load(): this is the whole point.

	const at = "0x40000000"
	bp := h.mustDo(wire.TypeBpSetAddress,
		wire.BreakpointAddressRequest{Location: at}).(wire.Breakpoint)
	if bp.Number == 0 {
		t.Fatal("breakpoint has no number")
	}
	// Resolved, not pending. A pending breakpoint would look set and never
	// fire, which is the failure this has to be distinguished from.
	if bp.Pending {
		t.Errorf("breakpoint at %s is pending; gdb needs no symbols for an address", at)
	}
	if !strings.EqualFold(bp.Address, at) {
		t.Errorf("breakpoint is at %q, want %s", bp.Address, at)
	}

	list := h.mustDo(wire.TypeBpList, struct{}{}).(wire.BreakpointList)
	if len(list.Breakpoints) != 1 {
		t.Fatalf("bp.list reports %d breakpoints, want 1", len(list.Breakpoints))
	}

	// Whatever may create one has to be able to undo it. Both of these were
	// refused by the same gate, so a breakpoint set through some other route
	// could not have been cleared either.
	h.mustDo(wire.TypeBpSetEnabled, wire.BreakpointIDRequest{Number: bp.Number, Enabled: false})
	h.mustDo(wire.TypeBpDelete, wire.BreakpointIDRequest{Number: bp.Number})

	list = h.mustDo(wire.TypeBpList, struct{}{}).(wire.BreakpointList)
	if len(list.Breakpoints) != 0 {
		t.Errorf("bp.list still reports %d breakpoints after deleting it",
			len(list.Breakpoints))
	}
}

// TestANameStillNeedsAProgram is the other half, and the reason the check
// moved onto the location rather than being dropped.
//
// gdb would accept a name here — the pending insert takes anything — and leave
// a breakpoint that looks set and can never fire, because nothing will ever
// define the symbol. Saying so is more useful than that.
func TestANameStillNeedsAProgram(t *testing.T) {
	h := startReal(t, "hello")

	_, werr := h.do(wire.TypeBpSetAddress, wire.BreakpointAddressRequest{Location: "main"})
	if werr == nil {
		t.Fatal("a breakpoint on a name was accepted with no symbol table")
	}
	if !strings.Contains(werr.Message, "main") {
		t.Errorf("the refusal does not name what could not be found: %s", werr.Message)
	}

	// And a source line, which needs a line table for the same reason.
	_, werr = h.do(wire.TypeBpSetSource, wire.BreakpointRequest{Path: "hello.c", Line: 1})
	if werr == nil {
		t.Fatal("a source breakpoint was accepted with no program")
	}

	// Nothing was left behind by either refusal.
	list := h.mustDo(wire.TypeBpList, struct{}{}).(wire.BreakpointList)
	if len(list.Breakpoints) != 0 {
		t.Errorf("a refused breakpoint was inserted anyway: %+v", list.Breakpoints)
	}
}

// TestBreakingByNameWorksAfterAttaching is the case that decides how the check
// above is written.
//
// Attaching gives gdb a symbol table it read from the process, and gdb-wui
// records no path for it — exePath is set by exe.load and by nothing else. So
// "is there a program" and "does gdb know this name" are different questions,
// and only the second one is the one a breakpoint by name depends on. Asking
// the first would refuse `break main` on a process gdb can see every symbol of.
func TestBreakingByNameWorksAfterAttaching(t *testing.T) {
	files, pid := startTracee(t)
	h := startRealWithFiles(t, files)
	attachTo(t, h, pid)

	if got := h.sess.Snapshot().ExePath; got != "" {
		t.Skipf("attaching now records an exe path (%q), so this tests nothing", got)
	}
	bp := h.mustDo(wire.TypeBpSetAddress,
		wire.BreakpointAddressRequest{Location: "main"}).(wire.Breakpoint)
	if bp.Pending {
		t.Errorf("breakpoint on main is pending, though gdb has the symbols")
	}
	if bp.Address == "" {
		t.Errorf("breakpoint on main resolved to no address: %+v", bp)
	}
}
