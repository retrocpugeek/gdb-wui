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

// Naming the frames gdb cannot name.
//
// The subject is a stripped binary's call stack, which gdb renders as a column
// of "?? ()". Every test here is about whether the decompiler's answer is
// *placed correctly* — a name attached to the wrong address is worse than no
// name, because "??" at least says nothing.

func namesOf(t *testing.T, do func(string, any) any, addrs ...string) wire.DecompNames {
	t.Helper()
	return do(wire.TypeDecompNames, wire.DecompNamesRequest{Addresses: addrs}).(wire.DecompNames)
}

// dataNamesOf is the same question asked by a pane that shows data rather than
// code — the watch list, whose rows on a stripped binary are an address and a
// cast and nothing else.
func dataNamesOf(t *testing.T, do func(string, any) any, addrs ...string) wire.DecompNames {
	t.Helper()
	return do(wire.TypeDecompNames,
		wire.DecompNamesRequest{Addresses: addrs, Data: true}).(wire.DecompNames)
}

// TestNameAFrameAddress is the whole feature in one assertion: an address gdb
// calls "??" gets a name.
func TestNameAFrameAddress(t *testing.T) {
	k := decompHarness(t)
	do := k.do
	do(wire.TypeExeLoad, wire.ExeLoadRequest{Path: "nodebug"})
	waitReady(t, do)

	// Break by symbol — the fixture keeps its symbol table — then read the
	// stack, which is what a client would be trying to render.
	do(wire.TypeBpSetAddress, wire.BreakpointAddressRequest{Location: "accumulate"})
	do(wire.TypeExecRun, wire.ExecRequest{})
	waitStopped(t, do, 30*time.Second)

	stack := do(wire.TypeStackList, wire.StackListRequest{}).(wire.StackList)
	if len(stack.Frames) < 2 {
		t.Fatalf("stack has %d frames, want accumulate and main", len(stack.Frames))
	}

	addrs := make([]string, 0, len(stack.Frames))
	for _, f := range stack.Frames {
		addrs = append(addrs, f.Address)
	}
	out := namesOf(t, do, addrs...)
	if out.State != wire.DecompReady {
		t.Fatalf("state = %q, want ready", out.State)
	}
	if len(out.Names) == 0 {
		t.Fatal("no frame was named")
	}

	byAddr := map[string]wire.DecompName{}
	for _, n := range out.Names {
		byAddr[n.Addr] = n
	}
	first, ok := byAddr[stack.Frames[0].Address]
	if !ok {
		t.Fatalf("frame 0 (%s) was not named", stack.Frames[0].Address)
	}
	if !strings.Contains(first.Name, "accumulate") {
		t.Errorf("frame 0 named %q, want something containing accumulate", first.Name)
	}
	if first.Signature == "" {
		t.Error("no recovered prototype; the name alone does not say what it takes")
	}
	if first.Entry == "" {
		t.Error("no entry address, so a client cannot show the offset into the function")
	}
	// Which of the two populations answered. A client renders a place in code
	// and a piece of data differently — "+0x1c" means something for one and
	// nothing for the other — and the name alone cannot say which this is,
	// since either may have been renamed to anything.
	if first.Kind != wire.SymbolFunction {
		t.Errorf("kind = %q, want %q", first.Kind, wire.SymbolFunction)
	}
}

// TestNameADataAddress is the watch list's question: this address is all I have,
// what is here?
//
// Ghidra's function manager — which answers every other test in this file —
// says "no function" for a global, correctly and unhelpfully. The name index
// answers instead, and the address goes in and comes back through the load
// bias, which is what makes this worth a running program rather than a static
// lookup: a PIE's globals move exactly as much as its code does.
func TestNameADataAddress(t *testing.T) {
	k := decompHarness(t)
	do := k.do
	do(wire.TypeExeLoad, wire.ExeLoadRequest{Path: "stripped"})
	waitReady(t, do)

	// Relocated first, then read: the addresses in the pane are runtime ones
	// from here on, and a lookup that quietly compared them against Ghidra's
	// link-time coordinates would find nothing.
	do(wire.TypeExecRun, wire.ExecRequest{StopAtEntry: true})
	waitStopped(t, do, 30*time.Second)

	out := decompSymbols(t, do, "DAT_")
	g := firstFrom(out.Symbols, wire.SymbolFromDecompiler, wire.SymbolVariable)
	if g == nil {
		t.Fatal("the decompiler lists no global to ask about")
	}

	named := dataNamesOf(t, do, g.Address)
	if len(named.Names) != 1 {
		t.Fatalf("got %d names for %s, where the pane says %s is",
			len(named.Names), g.Address, g.Name)
	}
	if named.Names[0].Name != g.Name {
		t.Errorf("%s is named %q, want %q", g.Address, named.Names[0].Name, g.Name)
	}
	if named.Names[0].Kind != wire.SymbolVariable {
		t.Errorf("kind = %q, want %q — a client that read this as code would "+
			"offer to disassemble a global", named.Names[0].Kind, wire.SymbolVariable)
	}
}

// TestDataNamesAreOptional. The call stack asks this question on every stop and
// has no use for the labels: a frame address is in a function or in code the
// decompiler was never given. Answering them costs the whole name index, so the
// flag has to be what decides it rather than the address.
func TestDataNamesAreOptional(t *testing.T) {
	k := decompHarness(t)
	do := k.do
	do(wire.TypeExeLoad, wire.ExeLoadRequest{Path: "stripped"})
	waitReady(t, do)

	out := decompSymbols(t, do, "DAT_")
	g := firstFrom(out.Symbols, wire.SymbolFromDecompiler, wire.SymbolVariable)
	if g == nil {
		t.Fatal("the decompiler lists no global to ask about")
	}

	if got := namesOf(t, do, g.Address); len(got.Names) != 0 {
		t.Errorf("%s was named %q without asking for data", g.Address, got.Names[0].Name)
	}
	if got := dataNamesOf(t, do, g.Address); len(got.Names) != 1 {
		t.Errorf("the same address asked with data got %d names, want 1", len(got.Names))
	}
}

// TestNamesRefusesANearMiss. The index holds where each label starts and not
// how far it runs, so an address a few bytes in is not something it can name.
// Answering with the preceding label would turn "this is DAT_001a08de" into
// "this is somewhere at or after DAT_001a08de", which is a weaker claim than
// the one a watch row would then be making.
func TestNamesRefusesANearMiss(t *testing.T) {
	k := decompHarness(t)
	do := k.do
	do(wire.TypeExeLoad, wire.ExeLoadRequest{Path: "stripped"})
	waitReady(t, do)

	out := decompSymbols(t, do, "DAT_")
	g := firstFrom(out.Symbols, wire.SymbolFromDecompiler, wire.SymbolVariable)
	if g == nil {
		t.Fatal("the decompiler lists no global to ask about")
	}
	at, err := strconv.ParseUint(strings.TrimPrefix(g.Address, "0x"), 16, 64)
	if err != nil {
		t.Fatalf("unparseable address %q: %v", g.Address, err)
	}

	inside := fmt.Sprintf("0x%x", at+1)
	got := dataNamesOf(t, do, inside)
	for _, n := range got.Names {
		t.Errorf("%s was named %q; the index knows where %s starts and not how "+
			"far it runs", inside, n.Name, g.Name)
	}
}

// TestNamesAreRuntimeAddresses. The reply has to be in gdb's coordinates, not
// Ghidra's: this is a PIE, so the two differ by the load bias, and an entry
// left in link-time coordinates would put every offset wildly wrong.
func TestNamesAreRuntimeAddresses(t *testing.T) {
	k := decompHarness(t)
	do := k.do
	do(wire.TypeExeLoad, wire.ExeLoadRequest{Path: "nodebug"})
	waitReady(t, do)
	do(wire.TypeBpSetAddress, wire.BreakpointAddressRequest{Location: "accumulate"})
	do(wire.TypeExecRun, wire.ExecRequest{})
	waitStopped(t, do, 30*time.Second)

	stack := do(wire.TypeStackList, wire.StackListRequest{}).(wire.StackList)
	pc := stack.Frames[0].Address

	out := namesOf(t, do, pc)
	if len(out.Names) != 1 {
		t.Fatalf("got %d names for one address", len(out.Names))
	}
	got := out.Names[0]

	// The same function through the decompiler, whose entry is already known
	// to be biased correctly — that is what TestDecompBiasFollowsRelocation
	// pins. Agreeing with it is the check.
	fn := do(wire.TypeDecompFunction,
		wire.DecompFunctionRequest{Target: pc}).(wire.DecompFunction)
	if fn.BiasFrom == "" {
		t.Skip("no bias could be established; there is nothing to compare against")
	}
	if got.Entry != fn.Entry {
		t.Errorf("entry = %s, want %s — the decompiled view's answer for the "+
			"same address", got.Entry, fn.Entry)
	}
	if got.Name != fn.Name {
		t.Errorf("name = %q, want %q", got.Name, fn.Name)
	}
	// And the offset is the distance into the function, which is the thing a
	// stack row shows beside the name.
	if got.Offset < 0 {
		t.Errorf("offset = %d, want the distance from the entry", got.Offset)
	}
}

// TestNamesOmitsWhatItDoesNotHave.
//
// A stack runs out through libc and the dynamic loader, and the decompiler has
// neither. Those frames must come back unnamed rather than attributed to
// whatever function happens to sit at the same offset inside the program.
func TestNamesOmitsWhatItDoesNotHave(t *testing.T) {
	k := decompHarness(t)
	do := k.do
	do(wire.TypeExeLoad, wire.ExeLoadRequest{Path: "nodebug"})
	waitReady(t, do)
	do(wire.TypeBpSetAddress, wire.BreakpointAddressRequest{Location: "accumulate"})
	do(wire.TypeExecRun, wire.ExecRequest{})
	waitStopped(t, do, 30*time.Second)

	for _, addr := range []string{
		"0x10",           // the unmapped first page
		"0x7ffff7c00000", // where libc lands, far outside the program
		"not-an-address", // a client bug, not a crash
	} {
		out := namesOf(t, do, addr)
		for _, n := range out.Names {
			t.Errorf("%s was named %q; nothing in the program is there", addr, n.Name)
		}
	}
}

// TestNamesWithNoDecompiler. Every stop in a stripped binary would ask this
// question, so answering it must be quiet and cheap when there is nothing to
// answer with.
func TestNamesWithNoDecompiler(t *testing.T) {
	h := startReal(t, "hello")
	h.mustDo(wire.TypeExeLoad, wire.ExeLoadRequest{Path: "hello"})

	out, werr := h.do(wire.TypeDecompNames, wire.DecompNamesRequest{
		Addresses: []string{"0x401136"},
	})
	if werr != nil {
		t.Fatalf("asking with no decompiler configured is an error: %s", werr.Message)
	}
	got := out.(wire.DecompNames)
	if len(got.Names) != 0 {
		t.Errorf("names = %+v, want none", got.Names)
	}
	if got.State != wire.DecompOff {
		t.Errorf("state = %q, want off, so a client can say why it got nothing", got.State)
	}
}

// TestNamesWithNoAddresses is the shape a client sends when every frame is
// already named. It must not reach Ghidra at all.
func TestNamesWithNoAddresses(t *testing.T) {
	h := startReal(t, "hello")
	h.mustDo(wire.TypeExeLoad, wire.ExeLoadRequest{Path: "hello"})
	out := h.mustDo(wire.TypeDecompNames, wire.DecompNamesRequest{}).(wire.DecompNames)
	if len(out.Names) != 0 {
		t.Errorf("names = %+v, want none", out.Names)
	}
}
