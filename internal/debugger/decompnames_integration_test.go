//go:build integration

package debugger_test

import (
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
