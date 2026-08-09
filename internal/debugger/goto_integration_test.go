//go:build integration

package debugger_test

import (
	"strconv"
	"strings"
	"testing"

	"github.com/retrocpugeek/gdb-wui/internal/wire"
)

// Locating a place by name, against a real gdb.
//
// The question running through this file is whether the four centre views can
// all be pointed at the same place from one answer: the source view needs a
// file and a line, the disassembly an address, and neither is derivable from
// the other without gdb.

// structsInspectLine is the line -data-disassemble attributes inspect's first
// instruction to: the opening brace, not the declaration, because that is where
// the prologue is.
const structsInspectLine = 19

func locate(t *testing.T, h *harness, target string) wire.GotoLocation {
	t.Helper()
	return h.mustDo(wire.TypeGotoLocate, wire.GotoLocateRequest{Target: target}).(wire.GotoLocation)
}

// TestLocateAFunctionByName is the common case: the name of a function, typed
// into the box.
func TestLocateAFunctionByName(t *testing.T) {
	h := stopInStructs(t)

	got := locate(t, h, "inspect")
	if got.Addr == 0 {
		t.Fatal("no address for inspect")
	}
	if got.Func != "inspect" {
		t.Errorf("func = %q, want inspect", got.Func)
	}
	if got.Source == nil {
		t.Fatal("no source position; the fixture is built with -g")
	}
	if got.Source.Path != "structs.c" {
		t.Errorf("path = %q, want structs.c resolved inside the project", got.Source.Path)
	}
	if got.Source.Line != structsInspectLine {
		t.Errorf("line = %d, want %d", got.Source.Line, structsInspectLine)
	}
	if !got.Source.Available {
		t.Error("the source is in the project and must be fetchable")
	}
	if !strings.HasPrefix(got.Address, "0x") {
		t.Errorf("address = %q, want hex", got.Address)
	}
}

// TestLocateAnAddress is the other direction: a hex address must produce the
// source line, which is the only thing that lets the source view follow a jump
// typed as a number.
func TestLocateAnAddress(t *testing.T) {
	h := stopInStructs(t)

	byName := locate(t, h, "inspect")
	byAddr := locate(t, h, byName.Address)

	if byAddr.Addr != byName.Addr {
		t.Errorf("address round trip: %#x then %#x", byName.Addr, byAddr.Addr)
	}
	if byAddr.Func != "inspect" {
		t.Errorf("func = %q, want inspect", byAddr.Func)
	}
	if byAddr.Source == nil || byAddr.Source.Line != byName.Source.Line {
		t.Errorf("source = %+v, want the same line the name gave", byAddr.Source)
	}
}

// TestLocateAFileAndLine. Typing structs.c:42 is the most direct thing a reader
// can do, and it is the one case gdb has no MI command for: -data-disassemble
// answers it only because the grouped output says which line each instruction
// belongs to.
func TestLocateAFileAndLine(t *testing.T) {
	h := stopInStructs(t)

	got := locate(t, h, "structs.c:"+strconv.Itoa(structsBreakLine))
	if got.Source == nil {
		t.Fatal("no source position for a file:line target")
	}
	if got.Source.Line != structsBreakLine {
		t.Errorf("line = %d, want %d", got.Source.Line, structsBreakLine)
	}
	if got.Source.Path != "structs.c" {
		t.Errorf("path = %q, want structs.c", got.Source.Path)
	}
	if got.Addr == 0 {
		t.Fatal("no address; the views that are not the source view need one")
	}
	if got.Func != "main" {
		t.Errorf("func = %q, want main", got.Func)
	}

	// And the address really is that line's, not the enclosing function's:
	// -data-disassemble -f/-l starts at the function entry, so an
	// implementation that trusted where gdb began would land on main+0.
	entry := locate(t, h, "main")
	if got.Addr == entry.Addr {
		t.Errorf("structs.c:%d resolved to main's entry (%#x) rather than to the line",
			structsBreakLine, got.Addr)
	}
	if got.Addr < entry.Addr {
		t.Errorf("line %d is at %#x, before main's entry at %#x",
			structsBreakLine, got.Addr, entry.Addr)
	}
}

// TestLocateALineWithNoCode. A declaration or a blank line is a real place in
// the file and has no address. Handing back a neighbouring line's address would
// put the disassembly somewhere the user did not ask for and give no sign of it.
func TestLocateALineWithNoCode(t *testing.T) {
	h := stopInStructs(t)

	// Line 1 is #include <stdio.h>.
	got := locate(t, h, "structs.c:1")
	if got.Source == nil {
		t.Fatal("a line with no code is still a place in the file")
	}
	if got.Source.Line != 1 {
		t.Errorf("line = %d, want 1", got.Source.Line)
	}
	if got.Addr != 0 {
		t.Errorf("addr = %#x, want none — line 1 generates no code", got.Addr)
	}
}

// TestLocateRejectsWhatItCannotFind. Each of these is something a user will
// type, and each must come back as a refusal rather than as a wrong place.
func TestLocateRejectsWhatItCannotFind(t *testing.T) {
	h := stopInStructs(t)

	for _, target := range []string{
		"no_such_symbol_at_all",
		"nosuch.c:12",
		"structs.c:99999",
		"",
		"   ",
	} {
		if out, werr := h.do(wire.TypeGotoLocate, wire.GotoLocateRequest{Target: target}); werr == nil {
			t.Errorf("%q was located: %+v", target, out)
		}
	}
}

// TestLocateTakesAnExpression: the box accepts what the memory viewer's does,
// because a reader who has learnt one has learnt the other.
func TestLocateTakesAnExpression(t *testing.T) {
	h := stopInStructs(t)

	got := locate(t, h, "&cfg")
	if got.Addr == 0 {
		t.Fatal("&cfg did not resolve")
	}
	// It is a stack address, so there is no function and no source line, and
	// that is the honest answer rather than a failure.
	if got.Source != nil {
		t.Errorf("source = %+v, want none for a stack address", got.Source)
	}
}

// TestLocateAStrippedSymbol. Without debug info there is an address and no
// source line at all — the case the disassembly exists for.
func TestLocateAStrippedSymbol(t *testing.T) {
	h := startReal(t, "hello")
	addFixtureNoDebug(t, h, "minsym")
	h.mustDo(wire.TypeExeLoad, wire.ExeLoadRequest{Path: "minsym"})
	h.mustDo(wire.TypeBpSetAddress, wire.BreakpointAddressRequest{Location: "main"})
	h.mustDo(wire.TypeExecRun, wire.ExecRequest{})
	h.rec.wait(t, wire.EventStopped)

	got := locate(t, h, "main")
	if got.Addr == 0 {
		t.Fatal("main did not resolve in a binary with no debug info")
	}
	if got.Func != "main" {
		t.Errorf("func = %q, want main", got.Func)
	}
	if got.Source != nil && got.Source.Available {
		t.Errorf("source = %+v, want none — the fixture is built without -g", got.Source)
	}
}

// TestLocateWithoutRunning. Opening the source at a function before starting
// the program is a reasonable first thing to do, and needs no process.
func TestLocateWithoutRunning(t *testing.T) {
	h := startReal(t, "structs")
	h.mustDo(wire.TypeExeLoad, wire.ExeLoadRequest{Path: "structs"})

	got := locate(t, h, "inspect")
	if got.Source == nil || got.Source.Line != structsInspectLine {
		t.Errorf("source = %+v, want structs.c:%d with nothing running",
			got.Source, structsInspectLine)
	}
}
