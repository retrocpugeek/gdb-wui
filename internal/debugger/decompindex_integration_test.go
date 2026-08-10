//go:build integration

package debugger_test

import (
	"strings"
	"testing"
	"time"

	"github.com/retrocpugeek/gdb-wui/internal/wire"
)

// Finding things in a binary that has no names.
//
// The subject is the pane and the go-to box on a stripped executable, where
// gdb's symbol table is empty and Ghidra's names are the only ones in
// existence. Every test here is about a name being usable — listed, resolved,
// broken on — rather than merely visible in decompiled text.

// decompSymbols lists what the pane would show for a filter.
func decompSymbols(t *testing.T, do func(string, any) any, filter string) wire.SymbolsList {
	t.Helper()
	return do(wire.TypeSymbolsList,
		wire.SymbolsListRequest{Filter: filter, Limit: 5000}).(wire.SymbolsList)
}

func firstFrom(syms []wire.Symbol, from, kind string) *wire.Symbol {
	for i := range syms {
		if syms[i].From == from && (kind == "" || syms[i].Kind == kind) {
			return &syms[i]
		}
	}
	return nil
}

func symNamed(syms []wire.Symbol, name string) *wire.Symbol {
	for i := range syms {
		if syms[i].Name == name {
			return &syms[i]
		}
	}
	return nil
}

// aRelocatedFunction gets the fixture to a stop with the program mapped, and
// returns one decompiler function name with the address it has now.
//
// Running first is the point. A PIE is linked at 0x101149 and loaded somewhere
// around 0x555555555149, so a resolver that read the address out of FUN_00101149
// looks perfectly correct until the program starts — and then sends every jump
// and every breakpoint to an unmapped address.
func aRelocatedFunction(t *testing.T, do func(string, any) any) wire.Symbol {
	t.Helper()
	before := decompSymbols(t, do, "FUN_")
	fn := firstFrom(before.Symbols, wire.SymbolFromDecompiler, wire.SymbolFunction)
	if fn == nil {
		t.Fatalf("no decompiler function among %d entries", len(before.Symbols))
	}
	linkTime := fn.Address

	// Stopping at the entry point lands in the dynamic loader, which Ghidra was
	// never given and cannot name. That is fine and is not what is being
	// measured: the executable is mapped by then, which is all the bias needs.
	do(wire.TypeExecRun, wire.ExecRequest{StopAtEntry: true})
	waitStopped(t, do, 30*time.Second)

	after := decompSymbols(t, do, fn.Name)
	moved := symNamed(after.Symbols, fn.Name)
	if moved == nil {
		t.Fatalf("%s disappeared from the index once the program started", fn.Name)
	}
	if moved.Address == linkTime {
		t.Fatalf("%s is still at its link-time %s after the program started; "+
			"nothing here would catch an address read out of the name",
			fn.Name, linkTime)
	}
	return *moved
}

// TestDecompilerFunctionsAreListed is the pane on a stripped binary, which
// until now was empty for every program it was most needed on.
func TestDecompilerFunctionsAreListed(t *testing.T) {
	k := decompHarness(t)
	do := k.do
	do(wire.TypeExeLoad, wire.ExeLoadRequest{Path: "stripped"})
	waitReady(t, do)

	// The premise, checked rather than assumed: the program's own functions are
	// gone. Its *imports* are not — a dynamically linked binary keeps .dynsym
	// however stripped it is, and gdb reports printf from it — so this asks
	// about the two functions the fixture defines, not about the table's size.
	all := decompSymbols(t, do, "")
	for _, sym := range all.Symbols {
		if sym.From != wire.SymbolFromBinary {
			continue
		}
		if sym.Name == "tally" || sym.Name == "main" {
			t.Fatalf("the stripped binary still reports %q; it is not stripped", sym.Name)
		}
	}

	out := decompSymbols(t, do, "FUN_")
	fn := firstFrom(out.Symbols, wire.SymbolFromDecompiler, wire.SymbolFunction)
	if fn == nil {
		t.Fatalf("no decompiler function among %d entries", len(out.Symbols))
	}
	if !strings.HasPrefix(fn.Name, "FUN_") {
		t.Errorf("name = %q, want a FUN_ label", fn.Name)
	}
	if fn.Address == "" {
		t.Error("no address, so there is nothing for a jump or a breakpoint to use")
	}
	if fn.Debug {
		t.Error("a decompiler name is claiming debug info, which would send a jump to a source file that does not exist")
	}
}

// TestDecompilerGlobalsAreListedAsVariables covers the other population.
//
// A global is the most readable thing in a stripped function — a fixed address,
// live at every pc, needing no frame — and DAT_00104010 is the only handle
// anyone has on it. Listing functions alone would leave the readable half out.
func TestDecompilerGlobalsAreListedAsVariables(t *testing.T) {
	k := decompHarness(t)
	do := k.do
	do(wire.TypeExeLoad, wire.ExeLoadRequest{Path: "stripped"})
	waitReady(t, do)

	out := decompSymbols(t, do, "DAT_")
	g := firstFrom(out.Symbols, wire.SymbolFromDecompiler, wire.SymbolVariable)
	if g == nil {
		var got []string
		for _, s := range out.Symbols {
			got = append(got, s.Name)
		}
		t.Fatalf("no decompiler global among %v", got)
	}
	if g.Kind != wire.SymbolVariable {
		t.Errorf("kind = %q, want variable — a global is data and disassembling it is meaningless", g.Kind)
	}
	if g.Address == "" {
		t.Error("no address, so the memory viewer has nothing to open")
	}
}

// TestGoToADecompilerName is the feature: a name read off the screen, typed
// into the box, resolving to where the code actually is.
//
// The expected address comes from decomp.names rather than from the name
// itself, deliberately. The digits in FUN_00101149 are Ghidra's *link-time*
// address, and a resolver that read them out of the string would look correct
// on this test right up until the program was started and relocated.
func TestGoToADecompilerName(t *testing.T) {
	k := decompHarness(t)
	do := k.do
	do(wire.TypeExeLoad, wire.ExeLoadRequest{Path: "stripped"})
	waitReady(t, do)

	fn := aRelocatedFunction(t, k.do)

	// A second opinion before the assertion, from a path that shares no code
	// with the index: Ghidra's own containment lookup, asked which function
	// covers that address. If the two disagree the index is off by a bias and
	// the go-to below would be checking one wrong number against another.
	named := namesOf(t, do, fn.Address)
	if len(named.Names) == 0 {
		t.Fatalf("the decompiler puts no function at %s, where the index says %s is",
			fn.Address, fn.Name)
	}
	if named.Names[0].Name != fn.Name {
		t.Fatalf("%s is at %s according to the index, and %s according to decomp.names",
			fn.Name, fn.Address, named.Names[0].Name)
	}

	loc := do(wire.TypeGotoLocate,
		wire.GotoLocateRequest{Target: fn.Name}).(wire.GotoLocation)
	if loc.Address != fn.Address {
		t.Errorf("go to %s = %s, want %s — the name resolved somewhere else",
			fn.Name, loc.Address, fn.Address)
	}
	if loc.Func != fn.Name {
		t.Errorf("func = %q, want %q; the view has nothing to label the destination with",
			loc.Func, fn.Name)
	}
}

// TestBreakOnADecompilerName is the second half of the same gesture, and the
// one that used to fail quietly.
//
// gdb answers an unresolvable location under -f with a *pending* breakpoint
// rather than an error. Pending is right for a shared library that has not
// loaded; here nothing will ever define FUN_00101149, so the breakpoint sat
// there looking set and never fired.
func TestBreakOnADecompilerName(t *testing.T) {
	k := decompHarness(t)
	do := k.do
	do(wire.TypeExeLoad, wire.ExeLoadRequest{Path: "stripped"})
	waitReady(t, do)

	fn := aRelocatedFunction(t, k.do)

	bp := do(wire.TypeBpSetAddress,
		wire.BreakpointAddressRequest{Location: fn.Name}).(wire.Breakpoint)
	if bp.Pending {
		t.Fatalf("breakpoint on %s is pending; nothing will ever define that name, "+
			"so it would sit there looking set and never fire", fn.Name)
	}
	// Numerically, not textually: gdb zero-pads a breakpoint address to the
	// pointer width and the index does not, so 0x0000555555555020 and
	// 0x555555555020 are the same place written two ways.
	if !sameAddressStr(bp.Address, fn.Address) {
		t.Errorf("breakpoint at %s, want %s", bp.Address, fn.Address)
	}
}

// TestABinarySymbolWinsOverTheDecompiler. Both know accumulate; only one of
// them knows it as a fact. Listing it twice would be the pane inventing a
// second function, and resolving it through Ghidra would give up gdb's prologue
// skip for nothing.
func TestABinarySymbolWinsOverTheDecompiler(t *testing.T) {
	k := decompHarness(t)
	do := k.do
	do(wire.TypeExeLoad, wire.ExeLoadRequest{Path: "nodebug"})
	waitReady(t, do)

	out := decompSymbols(t, do, "accumulate")
	seen := 0
	for _, sym := range out.Symbols {
		if sym.Name != "accumulate" {
			continue
		}
		seen++
		if sym.From != wire.SymbolFromBinary {
			t.Errorf("accumulate is listed as %q; the binary's own symbol should win",
				sym.From)
		}
	}
	if seen != 1 {
		t.Errorf("accumulate appears %d times, want once", seen)
	}
}

// TestAnUnknownNameIsStillRefused. The index answers for names it has; a
// resolver that started guessing would turn a typo into a jump to somewhere
// plausible and wrong.
func TestAnUnknownNameIsStillRefused(t *testing.T) {
	k := decompHarness(t)
	do := k.do
	do(wire.TypeExeLoad, wire.ExeLoadRequest{Path: "stripped"})
	waitReady(t, do)

	if _, werr := k.try(wire.TypeGotoLocate,
		wire.GotoLocateRequest{Target: "FUN_deadbeef"}); werr == nil {
		t.Error("a name the decompiler does not have was resolved anyway")
	}
}
