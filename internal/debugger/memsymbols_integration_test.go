//go:build integration

package debugger_test

import (
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/retrocpugeek/gdb-wui/internal/wire"
)

// The memory viewer's symbol column, on a program gdb cannot name.
//
// gdb answers this column by decorating an address — `(void*)0x4041a0` prints
// as `0x4041a0 <counter>` — so on a stripped binary it is blank down its whole
// length, which is the program you most need it for. The decompiler's labels
// fill it, and the tests here are about the *extent* of a label rather than
// about a name appearing: naming an address that a label does not actually
// cover would be a confident lie in the one column a reader uses to place
// themselves.

func memSymbolsAt(t *testing.T, do func(string, any) any, addrs ...string) wire.MemSymbols {
	t.Helper()
	return do(wire.TypeMemSymbols, wire.MemSymbolsRequest{Addresses: addrs}).(wire.MemSymbols)
}

// dataOfType finds a decompiler label whose type matches, or — with an empty
// want — one with no type at all.
//
// The distinction is the whole subject: Ghidra knows how far a *typed* label
// runs and nothing at all about an untyped one, and the column has to say
// correspondingly more or less. Note that "typed" and "wider than one byte" are
// different things: an analysed single byte is defined data of type undefined1,
// which is why the tests below name the type they want rather than testing for
// the presence of one.
func dataOfType(syms []wire.Symbol, want string) *wire.Symbol {
	for i := range syms {
		s := &syms[i]
		if s.From != wire.SymbolFromDecompiler || s.Kind != wire.SymbolVariable {
			continue
		}
		if s.Type == want {
			return s
		}
	}
	return nil
}

// oneByteTypes are the shapes that occupy a single byte, so an address one past
// such a label is outside it however the label was named.
var oneByteTypes = map[string]bool{
	"": true, "undefined1": true, "byte": true, "char": true, "bool": true,
}

func untypedData(syms []wire.Symbol) *wire.Symbol {
	for i := range syms {
		s := &syms[i]
		if s.From != wire.SymbolFromDecompiler || s.Kind != wire.SymbolVariable {
			continue
		}
		if oneByteTypes[s.Type] {
			return s
		}
	}
	return nil
}

func plus(t *testing.T, addr string, n uint64) string {
	t.Helper()
	v, err := strconv.ParseUint(strings.TrimPrefix(addr, "0x"), 16, 64)
	if err != nil {
		t.Fatalf("unparseable address %q: %v", addr, err)
	}
	return fmt.Sprintf("0x%x", v+n)
}

// stoppedStripped gets the fixture to a stop with the program mapped, so the
// addresses in play are runtime ones and the bias is doing real work.
func stoppedStripped(t *testing.T, do func(string, any) any) {
	t.Helper()
	do(wire.TypeExeLoad, wire.ExeLoadRequest{Path: "stripped"})
	waitReady(t, do)
	do(wire.TypeExecRun, wire.ExecRequest{StopAtEntry: true})
	waitStopped(t, do, 30*time.Second)
}

// TestTheMemoryColumnNamesADecompilerLabel is the feature: the column stops
// being blank on the program it was most needed for.
func TestTheMemoryColumnNamesADecompilerLabel(t *testing.T) {
	k := decompHarness(t)
	do := k.do
	stoppedStripped(t, do)

	out := decompSymbols(t, do, "DAT_")
	g := firstFrom(out.Symbols, wire.SymbolFromDecompiler, wire.SymbolVariable)
	if g == nil {
		t.Fatal("the decompiler lists no global to look for")
	}

	syms := memSymbolsAt(t, do, g.Address)
	if len(syms.Symbols) != 1 {
		t.Fatalf("got %d names for %s, where the pane says %s is",
			len(syms.Symbols), g.Address, g.Name)
	}
	got := syms.Symbols[0]
	if got.Name != g.Name {
		t.Errorf("%s is named %q, want %q", g.Address, got.Name, g.Name)
	}
	// Marked, or the column would be presenting Ghidra's guess as something the
	// binary says. Every other pane that shows a recovered name says so.
	if got.From != wire.SymbolFromDecompiler {
		t.Errorf("from = %q, want %q", got.From, wire.SymbolFromDecompiler)
	}
}

// TestTheMemoryColumnDoesNotGuessPastAnUntypedLabel.
//
// An untyped label is one undefined byte as far as Ghidra is concerned, whatever
// follows it — applet_names in a stripped busybox is a 1954-byte table and
// Ghidra calls it one byte until somebody types it. So the honest answer for
// the byte after it is nothing. Bounding it by the *next* label instead would
// name every byte of the padding and alignment in between, and a column reading
// "DAT_00104010+2048" over a run of zeroes is worse than a blank one: it reads
// like knowledge.
func TestTheMemoryColumnDoesNotGuessPastAnUntypedLabel(t *testing.T) {
	k := decompHarness(t)
	do := k.do
	stoppedStripped(t, do)

	out := decompSymbols(t, do, "")
	g := untypedData(out.Symbols)
	if g == nil {
		t.Skip("every label in this program is wider than a byte; nothing to check the rule on")
	}

	// The premise: the label itself is named. Without this the test could pass
	// because nothing is named at all.
	if syms := memSymbolsAt(t, do, g.Address); len(syms.Symbols) != 1 {
		t.Fatalf("%s (%s) is not named at all, so there is nothing to overrun",
			g.Name, g.Address)
	}

	after := plus(t, g.Address, 1)
	syms := memSymbolsAt(t, do, after)
	for _, s := range syms.Symbols {
		if s.From == wire.SymbolFromDecompiler {
			t.Errorf("%s was named %q; %s is untyped, so Ghidra does not know it "+
				"reaches that far", after, s.Name, g.Name)
		}
	}
}

// TestTheMemoryColumnFollowsGhidrasLength is the other half: where Ghidra *does*
// know the extent, the column uses it and reads like gdb's own <name+off>.
//
// A pointer, because its width is a property of the architecture rather than of
// this fixture, and every dynamically linked program has a table of them.
func TestTheMemoryColumnFollowsGhidrasLength(t *testing.T) {
	k := decompHarness(t)
	do := k.do
	stoppedStripped(t, do)

	g := dataOfType(decompSymbols(t, do, "").Symbols, "pointer")
	if g == nil {
		t.Skip("no pointer-typed label in this program; there is no known extent to follow")
	}

	inside := plus(t, g.Address, 1)
	syms := memSymbolsAt(t, do, inside)
	if len(syms.Symbols) != 1 {
		t.Fatalf("%s is one byte into %s (%s, a pointer) and was not named",
			inside, g.Name, g.Address)
	}
	want := g.Name + "+1"
	if syms.Symbols[0].Name != want {
		t.Errorf("name = %q, want %q — the offset is what makes the column "+
			"say where in the object you are", syms.Symbols[0].Name, want)
	}
}

// TestTheMemoryColumnStopsWhereTheTypeStops pins the far edge, which is the
// half that can be wrong without looking wrong.
//
// The extent is made rather than looked for: the test types a label itself,
// which is also the gesture a reader makes when they work out what a global is.
// That it takes effect at once is part of what is checked — an edit drops the
// index and the next question rebuilds it.
func TestTheMemoryColumnStopsWhereTheTypeStops(t *testing.T) {
	k := decompHarness(t)
	do := k.do
	stoppedStripped(t, do)

	fn := firstFrom(decompSymbols(t, do, "FUN_").Symbols,
		wire.SymbolFromDecompiler, wire.SymbolFunction)
	if fn == nil {
		t.Skip("no decompiler function to decompile the edit from")
	}

	// Candidates, because a label at the end of its memory block has no room:
	// Ghidra answers "Insufficent memory at address 00102000 (length: 16
	// bytes)" and means it. The first one that takes is as good as any.
	const width = 16
	var typed *wire.Symbol
	for _, s := range decompSymbols(t, do, "DAT_").Symbols {
		if s.From != wire.SymbolFromDecompiler || !oneByteTypes[s.Type] {
			continue
		}
		if _, werr := k.try(wire.TypeDecompRetype, wire.DecompEditRequest{
			Kind:     wire.DecompEditGlobal,
			Function: fn.Address,
			Name:     s.Name,
			Address:  s.Address,
			Value:    fmt.Sprintf("char[%d]", width),
		}); werr == nil {
			typed = &s
			break
		}
	}
	if typed == nil {
		t.Skip("no label had room for a char[16]; nothing to bound")
	}

	// The name is read back rather than assumed to survive the edit. A label
	// Ghidra generated is regenerated from the data it now describes: typing
	// DAT_00104000 as char[16] renames it to s__00104000, because a char array
	// is a string as far as the label generator is concerned.
	head := memSymbolsAt(t, do, typed.Address)
	if len(head.Symbols) != 1 {
		t.Fatalf("%s is typed char[%d] and its own address came back as %+v",
			typed.Address, width, head.Symbols)
	}
	name := head.Symbols[0].Name

	// Inside, so the test is known to be measuring an edge that exists.
	last := plus(t, typed.Address, width-1)
	syms := memSymbolsAt(t, do, last)
	want := fmt.Sprintf("%s+%d", name, width-1)
	if len(syms.Symbols) != 1 || syms.Symbols[0].Name != want {
		t.Fatalf("the last byte of %s (%s, char[%d]) came back as %+v, want %q",
			name, typed.Address, width, syms.Symbols, want)
	}

	// And one past it. That address is either nothing or whatever label starts
	// there; what it must not be is this one.
	past := plus(t, typed.Address, width)
	for _, s := range memSymbolsAt(t, do, past).Symbols {
		if strings.HasPrefix(s.Name, name+"+") {
			t.Errorf("%s was named %q; char[%d] ends before it", past, s.Name, width)
		}
	}
}

// TestTheMemoryColumnLeavesGdbsAnswersAlone. The fallback is a fallback: a
// program with symbols must go on getting gdb's names, which are the ones that
// stay right across relocation and across every shared library's own load bias.
func TestTheMemoryColumnLeavesGdbsAnswersAlone(t *testing.T) {
	k := decompHarness(t)
	do := k.do
	do(wire.TypeExeLoad, wire.ExeLoadRequest{Path: "nodebug"})
	waitReady(t, do)
	// Stopped, because mem.symbols asks gdb to evaluate a cast and gdb needs an
	// inferior for that — the column is only ever drawn against a stopped
	// program anyway.
	do(wire.TypeExecRun, wire.ExecRequest{StopAtEntry: true})
	waitStopped(t, do, 30*time.Second)

	out := decompSymbols(t, do, "accumulate")
	fn := symNamed(out.Symbols, "accumulate")
	if fn == nil || fn.Address == "" {
		t.Skip("the fixture's symbol table does not carry accumulate with an address")
	}

	syms := memSymbolsAt(t, do, fn.Address)
	if len(syms.Symbols) != 1 {
		t.Fatalf("gdb names %s and the column showed %d names for it",
			fn.Name, len(syms.Symbols))
	}
	if syms.Symbols[0].From != wire.SymbolFromBinary {
		t.Errorf("from = %q, want the binary's own — gdb answered this one",
			syms.Symbols[0].From)
	}
	if !strings.Contains(syms.Symbols[0].Name, "accumulate") {
		t.Errorf("name = %q, want something naming accumulate", syms.Symbols[0].Name)
	}
}
