//go:build integration

package debugger_test

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/retrocpugeek/gdb-wui/internal/debugger"
	"github.com/retrocpugeek/gdb-wui/internal/ghidra"
	"github.com/retrocpugeek/gdb-wui/internal/mi"
	"github.com/retrocpugeek/gdb-wui/internal/srcfs"
	"github.com/retrocpugeek/gdb-wui/internal/wire"
)

// Decompilation against both real programs at once: a real gdb and a real
// Ghidra. Slow, and the only place the two coordinate systems actually meet.

type decompKit struct {
	do  func(string, any) any
	try func(string, any) (any, *wire.Error)
	// dir is where the fixtures were built, so a test can reach the binaries
	// or put a Ghidra project of its own beside them.
	dir string
}

func decompHarness(t *testing.T) decompKit {
	t.Helper()
	return decompHarnessWith(t, nil)
}

// decompHarnessWith is decompHarness with the decompiler configured
// differently — the read-only case, where the project belongs to the user and
// gdb-wui may not write to it.
func decompHarnessWith(t *testing.T, adjust func(*debugger.DecompConfig)) decompKit {
	t.Helper()
	if _, err := exec.LookPath("gdb"); err != nil {
		t.Skip("gdb is not installed")
	}
	if _, err := exec.LookPath("gcc"); err != nil {
		t.Skip("gcc is not installed")
	}
	install, err := ghidra.Locate("")
	if err != nil {
		t.Skipf("no Ghidra installation: %v", err)
	}

	dir := t.TempDir()
	src := filepath.Join(dir, "demo.c")
	const body = `
#include <stdio.h>
int accumulate(int n) {
	int total = 0;
	for (int i = 0; i < n; i++) total += i * 3;
	return total;
}
int main(void) { printf("%d\n", accumulate(7)); return 0; }
`
	if err := os.WriteFile(src, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	// -no-pie is not passed, because these tests are about the relocation.
	out, err := exec.Command("gcc", "-g", "-O0", "-o", filepath.Join(dir, "demo"), src).
		CombinedOutput()
	if err != nil {
		t.Fatalf("gcc: %v\n%s", err, out)
	}
	// A second build with no debug info, not stripped, so that its function
	// symbols survive. That is what firmware looks like, and it is the
	// only build where gdb's prologue skip lands in the gap between the
	// function's entry and the first address any decompiled line claims.
	out, err = exec.Command("gcc", "-O0", "-o", filepath.Join(dir, "nodebug"), src).
		CombinedOutput()
	if err != nil {
		t.Fatalf("gcc (nodebug): %v\n%s", err, out)
	}

	files, err := srcfs.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	logf := func(f string, a ...any) { t.Logf(f, a...) }
	decompCfg := debugger.DecompConfig{
		Install:   install,
		CacheRoot: filepath.Join(dir, "cache"),
	}
	if adjust != nil {
		adjust(&decompCfg)
	}
	sess, err := debugger.New(t.Context(), debugger.Config{
		MI:             mi.Options{Dir: dir, Logf: logf},
		Files:          files,
		Events:         newRecorder(),
		Logf:           logf,
		Version:        "test",
		CommandTimeout: 30 * time.Second,
		Decomp:         decompCfg,
	})
	if err != nil {
		t.Fatalf("debugger.New: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = sess.Close(ctx)
	})

	try := func(typ string, payload any) (any, *wire.Error) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		return sess.Handle(ctx, wire.Request{ID: 1, Type: typ, Payload: marshal(t, payload)})
	}
	do := func(typ string, payload any) any {
		t.Helper()
		out, werr := try(typ, payload)
		if werr != nil {
			t.Fatalf("%s: %s: %s", typ, werr.Code, werr.Message)
		}
		return out
	}
	return decompKit{do: do, try: try, dir: dir}
}

// waitReady polls decomp.status until the decompiler stops starting. Import
// and analysis is seconds for a hello-world and minutes for firmware, so this
// is a real wait rather than a formality.
func waitReady(t *testing.T, do func(string, any) any) wire.DecompStatus {
	t.Helper()
	deadline := time.Now().Add(5 * time.Minute)
	for {
		st := do(wire.TypeDecompStatus, nil).(wire.DecompStatus)
		if st.State != wire.DecompStarting {
			if st.State != wire.DecompReady {
				t.Fatalf("decompiler did not become ready: %s %s", st.State, st.Error)
			}
			return st
		}
		if time.Now().After(deadline) {
			t.Fatal("decompiler never became ready")
		}
		time.Sleep(2 * time.Second)
	}
}

// TestDecompBiasFollowsRelocation is the regression test for a bug that shipped
// briefly and was silent.
//
// The bias was cached as a number after the first lookup. That is correct until
// the program starts: a position-independent executable relocates on run, and
// from then on every address the decompiled pane reported was a link-time one
// dressed as a runtime address. Caught by watching a decompiled entry stay at
// 0x11e9 while gdb had the same function at 0x5555555551e9.
//
// Only the *choice* of symbol is cacheable; its address must be asked for
// again each time.
func TestDecompBiasFollowsRelocation(t *testing.T) {
	k := decompHarness(t)
	do := k.do
	do(wire.TypeExeLoad, wire.ExeLoadRequest{Path: "demo"})
	waitReady(t, do)

	before := do(wire.TypeDecompFunction,
		wire.DecompFunctionRequest{Target: "accumulate"}).(wire.DecompFunction)
	if before.BiasFrom == "" {
		t.Fatal("no bias symbol was established; the rest of this test is meaningless")
	}

	// Run to a stop so the loader has relocated the image.
	do(wire.TypeBpSetSource, wire.BreakpointRequest{Path: "demo.c", Line: 5})
	do(wire.TypeExecRun, wire.ExecRequest{})
	waitStopped(t, do, 30*time.Second)

	after := do(wire.TypeDecompFunction,
		wire.DecompFunctionRequest{Target: "accumulate"}).(wire.DecompFunction)

	if after.Entry == before.Entry {
		t.Errorf("entry did not move across relocation: %s both before and after.\n"+
			"Either the bias is being cached as a number, or this binary is not a PIE.",
			before.Entry)
	}
	if after.Bias == before.Bias {
		t.Errorf("bias unchanged (%#x) across relocation", before.Bias)
	}

	// The running address must be the one gdb reports for the same function.
	gdbAddr := do(wire.TypeEvalExpr,
		wire.EvalExprRequest{Expr: "&accumulate"}).(wire.EvalExpr)
	if !sameAddress(after.Entry, gdbAddr.Addr) {
		t.Errorf("decompiled entry %s does not match gdb's %#x for the same function",
			after.Entry, gdbAddr.Addr)
	}
}

// TestDecompLineMapMatchesTheProgramCounter is the property the pane exists
// for: stop somewhere, and the line it highlights must be the line whose
// address set contains the pc.
func TestDecompLineMapMatchesTheProgramCounter(t *testing.T) {
	k := decompHarness(t)
	do := k.do
	do(wire.TypeExeLoad, wire.ExeLoadRequest{Path: "demo"})
	waitReady(t, do)

	do(wire.TypeBpSetSource, wire.BreakpointRequest{Path: "demo.c", Line: 5})
	do(wire.TypeExecRun, wire.ExecRequest{})
	waitStopped(t, do, 30*time.Second)

	// No target: follow the selected frame, which is what the pane does.
	fn := do(wire.TypeDecompFunction, wire.DecompFunctionRequest{}).(wire.DecompFunction)
	if fn.PCLine == 0 {
		t.Fatalf("no line for the program counter in %s", fn.Name)
	}
	if fn.Name != "accumulate" {
		t.Errorf("stopped in %q, expected accumulate", fn.Name)
	}

	// The reported line must really claim the pc.
	pc := currentPCOf(t, do)
	var found bool
	for _, l := range fn.Lines {
		if l.N != fn.PCLine {
			continue
		}
		for _, a := range l.Addrs {
			if sameAddressStr(a, pc) {
				found = true
			}
		}
	}
	if !found {
		t.Errorf("pcLine %d does not claim the pc %s", fn.PCLine, pc)
	}

	// And every mapped address must be inside the function body. A map that
	// points outside the body would look authoritative and be wrong.
	lo, hi := mustAddr(t, fn.BodyStart), mustAddr(t, fn.BodyEnd)
	for _, l := range fn.Lines {
		for _, a := range l.Addrs {
			v := mustAddr(t, a)
			if v < lo || v > hi {
				t.Errorf("line %d claims %s, outside the body %s..%s",
					l.N, a, fn.BodyStart, fn.BodyEnd)
			}
		}
	}
}

// TestDecompStackExpressionsReadTheRightMemory closes the loop that matters
// most: an expression derived from Ghidra's frame base, evaluated by gdb,
// against a value the test knows. A wrong frame rule produces a plausible
// number from the wrong slot, which a user cannot detect.
func TestDecompStackExpressionsReadTheRightMemory(t *testing.T) {
	k := decompHarness(t)
	do := k.do
	do(wire.TypeExeLoad, wire.ExeLoadRequest{Path: "demo"})
	waitReady(t, do)

	do(wire.TypeBpSetSource, wire.BreakpointRequest{Path: "demo.c", Line: 6})
	do(wire.TypeExecRun, wire.ExecRequest{})
	waitStopped(t, do, 30*time.Second)

	fn := do(wire.TypeDecompFunction,
		wire.DecompFunctionRequest{Target: "accumulate"}).(wire.DecompFunction)

	// accumulate(7) sums i*3 for i in 0..6 = 63. Whatever Ghidra called the
	// accumulator, one stack slot must hold that.
	var checked, matched int
	for _, v := range fn.Vars {
		if v.Storage != wire.DecompStorageStack || v.Expr == "" {
			continue
		}
		checked++
		res, werr := k.try(wire.TypeEvalExpr, wire.EvalExprRequest{Expr: v.Expr})
		if werr != nil {
			t.Errorf("%s: expression %q did not evaluate: %s", v.Name, v.Expr, werr.Message)
			continue
		}
		out := res.(wire.EvalExpr)
		if strings.TrimSpace(out.Value) == "63" {
			matched++
		}
		t.Logf("  %-12s %-32s = %s", v.Name, v.Expr, out.Value)
	}
	if checked == 0 {
		t.Skip("no stack variables with expressions in this build")
	}
	if matched == 0 {
		t.Error("no stack expression read the accumulator's value of 63; " +
			"the frame-base rule is producing readable but wrong addresses")
	}
}

// --- helpers ---------------------------------------------------------------

func marshal(t *testing.T, payload any) json.RawMessage {
	t.Helper()
	if payload == nil {
		return nil
	}
	b, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func mustAddr(t *testing.T, s string) uint64 {
	t.Helper()
	n, err := strconv.ParseUint(strings.TrimPrefix(s, "0x"), 16, 64)
	if err != nil {
		t.Fatalf("unparseable address %q: %v", s, err)
	}
	return n
}

func sameAddress(s string, n uint64) bool {
	v, err := strconv.ParseUint(strings.TrimPrefix(s, "0x"), 16, 64)
	return err == nil && v == n
}

func sameAddressStr(a, b string) bool {
	x, err1 := strconv.ParseUint(strings.TrimPrefix(a, "0x"), 16, 64)
	y, err2 := strconv.ParseUint(strings.TrimPrefix(b, "0x"), 16, 64)
	return err1 == nil && err2 == nil && x == y
}

// waitStopped polls until the inferior is stopped. Exec requests are
// acknowledgements, not completions — the stop arrives as an event afterwards
// — so a test that reads state straight after exec.run reads stale state.
func waitStopped(t *testing.T, do func(string, any) any, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		h := do(wire.TypeSessionHello, nil).(wire.Hello)
		switch h.RunState {
		case wire.RunStateStopped:
			return
		case wire.RunStateExited:
			t.Fatal("the program exited before reaching the breakpoint")
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("the program never stopped")
}

// currentPCOf returns frame 0's address.
func currentPCOf(t *testing.T, do func(string, any) any) string {
	t.Helper()
	h := do(wire.TypeSessionHello, nil).(wire.Hello)
	if len(h.Frames) == 0 {
		t.Fatal("no frames")
	}
	return h.Frames[0].Address
}

// TestStepLineWalksOutOfTheLine is the reason exec.stepLine exists.
//
// gdb's `next` needs a line table; without one its step range is the whole
// function, so a step over runs to the function's exit. Measured on a
// symbols-but-no-DWARF build: `break main` then `next` lands inside libc,
// having returned out of main.
//
// The assertion has to be that the walk crossed *several* instructions. An
// earlier version only checked the pc had left the line, which a single
// instruction step satisfies whenever the line claims one address — verified by
// deleting the walk entirely and watching the test stay green. So this picks a
// line claiming more than one address and requires the pc to end up past all
// of them.
func TestStepLineWalksOutOfTheLine(t *testing.T) {
	k := decompHarness(t)
	do := k.do
	do(wire.TypeExeLoad, wire.ExeLoadRequest{Path: "demo"})
	waitReady(t, do)

	// Stop inside the function FIRST. Addresses only mean anything once the
	// program is running: this is a PIE, so a function fetched beforehand
	// carries link-time addresses and a breakpoint set from one never hits.
	do(wire.TypeBpSetAddress, wire.BreakpointAddressRequest{Location: "accumulate"})
	do(wire.TypeExecRun, wire.ExecRequest{})
	waitStopped(t, do, 30*time.Second)

	// Walk forward until the pc is on a line covering several instructions.
	// A single-address line cannot tell a walk from one instruction step.
	var fn wire.DecompFunction
	var addrs []string
	var lo, hi uint64
	for range 24 {
		fn = do(wire.TypeDecompFunction, wire.DecompFunctionRequest{}).(wire.DecompFunction)
		if fn.PCLine == 0 || fn.PCLineApprox {
			do(wire.TypeExecStepLine, wire.ExecStepLineRequest{Over: true})
			waitStopped(t, do, 30*time.Second)
			continue
		}
		var here []string
		for _, l := range fn.Lines {
			if l.N == fn.PCLine {
				here = l.Addrs
			}
		}
		pc := mustAddr(t, currentPCOf(t, do))
		a, b := pc, pc
		for _, x := range here {
			v := mustAddr(t, x)
			if v < a {
				a = v
			}
			if v > b {
				b = v
			}
		}
		// Forward-only, compact, and solely owned. The last is the subtle one:
		// a loop header's addresses wrap around the body, so other lines'
		// instructions sit inside its span and the walk leaves it almost
		// immediately — correctly. Only a line that owns every address in its
		// span can support "the pc must end up past the whole line".
		owned := len(here) >= 2 && pc == a && b > a && b-a < 64
		if owned {
			for _, other := range fn.Lines {
				if other.N == fn.PCLine {
					continue
				}
				for _, x := range other.Addrs {
					if v := mustAddr(t, x); v > a && v < b {
						owned = false
					}
				}
			}
		}
		if owned {
			addrs, lo, hi = here, a, b
			break
		}
		do(wire.TypeExecStepLine, wire.ExecStepLineRequest{Lines: fn.Lines, BodyStart: fn.BodyStart, BodyEnd: fn.BodyEnd, Over: true})
		waitStopped(t, do, 30*time.Second)
	}
	if addrs == nil {
		t.Skip("never landed at the start of a multi-address line; " +
			"nothing here can distinguish a walk from one step")
	}
	t.Logf("stepping out of a line spanning %#x..%#x (%d addresses)", lo, hi, len(addrs))

	do(wire.TypeExecStepLine, wire.ExecStepLineRequest{Lines: fn.Lines, BodyStart: fn.BodyStart, BodyEnd: fn.BodyEnd, Over: true})
	waitStopped(t, do, 30*time.Second)
	after := mustAddr(t, currentPCOf(t, do))

	if after <= hi {
		t.Errorf("stepped to %#x, still inside the line %#x..%#x — "+
			"the walk stopped after one instruction instead of crossing the line",
			after, lo, hi)
	}
	// And it must not have left the function, which is what plain next does.
	bodyLo, bodyHi := mustAddr(t, fn.BodyStart), mustAddr(t, fn.BodyEnd)
	if after < bodyLo || after > bodyHi {
		t.Errorf("stepped clean out of the function: %#x is outside %s..%s "+
			"(this is the bug exec.stepLine exists to avoid)",
			after, fn.BodyStart, fn.BodyEnd)
	}
}

// TestStepLineWithNoRangeIsOneInstruction. Empty addresses must degrade to a
// single instruction step rather than running away.
func TestStepLineWithNoRangeIsOneInstruction(t *testing.T) {
	k := decompHarness(t)
	do := k.do
	do(wire.TypeExeLoad, wire.ExeLoadRequest{Path: "demo"})
	do(wire.TypeBpSetSource, wire.BreakpointRequest{Path: "demo.c", Line: 5})
	do(wire.TypeExecRun, wire.ExecRequest{})
	waitStopped(t, do, 30*time.Second)

	before := mustAddr(t, currentPCOf(t, do))
	do(wire.TypeExecStepLine, wire.ExecStepLineRequest{Over: true})
	waitStopped(t, do, 30*time.Second)
	after := mustAddr(t, currentPCOf(t, do))

	if after == before {
		t.Error("the pc did not move")
	}
	// One instruction. x86-64 tops out at 15 bytes; anything further means the
	// walk kept going with no range to stop it.
	if after < before || after-before > 16 {
		t.Errorf("moved from %#x to %#x — that is not one instruction", before, after)
	}
}

// TestStepLineFromAFunctionBreakpoint is the case that shipped broken.
//
// Breaking on a function leaves the pc in the prologue, which gdb skips and
// which belongs to no decompiled line — so the line resolves only
// approximately. The client refused to step by line from there, fell back to
// gdb's own `next`, and with no line table that runs to the function's exit:
// observed in a browser as a step from `main` landing in
// __libc_start_call_main.
//
// It is not an edge case. It is where anyone starts stepping.
func TestStepLineFromAFunctionBreakpoint(t *testing.T) {
	k := decompHarness(t)
	do := k.do
	// The no-debug build: with a line table gdb's prologue skip lands on a real
	// line and there is no gap to test.
	do(wire.TypeExeLoad, wire.ExeLoadRequest{Path: "nodebug"})
	waitReady(t, do)

	do(wire.TypeBpSetAddress, wire.BreakpointAddressRequest{Location: "accumulate"})
	do(wire.TypeExecRun, wire.ExecRequest{})
	waitStopped(t, do, 30*time.Second)

	fn := do(wire.TypeDecompFunction, wire.DecompFunctionRequest{}).(wire.DecompFunction)
	if !fn.PCLineApprox {
		t.Skipf("the breakpoint landed exactly on line %d; this build has no "+
			"prologue gap to test", fn.PCLine)
	}
	before := mustAddr(t, currentPCOf(t, do))
	// The lowest address any line claims: where the walk must end up, because
	// the prologue is everything below it.
	var firstMapped uint64
	for _, l := range fn.Lines {
		for _, a := range l.Addrs {
			if v := mustAddr(t, a); firstMapped == 0 || v < firstMapped {
				firstMapped = v
			}
		}
	}
	t.Logf("prologue: pc %#x, approximately line %d, first mapped address %#x (%d bytes on)",
		before, fn.PCLine, firstMapped, firstMapped-before)

	do(wire.TypeExecStepLine, wire.ExecStepLineRequest{
		Lines: fn.Lines, BodyStart: fn.BodyStart, BodyEnd: fn.BodyEnd, Over: true})
	waitStopped(t, do, 30*time.Second)

	after := do(wire.TypeDecompFunction, wire.DecompFunctionRequest{}).(wire.DecompFunction)
	if after.Name != fn.Name {
		t.Fatalf("stepped out of %s into %s — this is the bug: a step from a "+
			"function breakpoint ran to the function's exit", fn.Name, after.Name)
	}
	if after.PCLine == 0 {
		t.Fatal("no line for the pc after stepping")
	}
	if after.PCLineApprox {
		t.Errorf("still between lines after stepping (approximately line %d); "+
			"the walk should end on a line exactly", after.PCLine)
	}
	// The walk must land on the first address a line claims — not before it and
	// not past it.
	//
	// One limitation: on x86-64 the prologue gap is one or two
	// instructions, because Ghidra maps the stack-protector load that sits
	// immediately after gdb's skip point. So this cannot distinguish a walk
	// from a single instruction step; reverting the walk entirely leaves it
	// green. It does catch the opposite error — treating an approximate start
	// as a real line, which skips the line the step should have landed on —
	// and that is the mistake the code actually made. The walk itself is
	// verified in a browser on a no-debug build, and the gap is 72 bytes on the
	// MIPS firmware where it was reported.
	got := mustAddr(t, currentPCOf(t, do))
	if firstMapped != 0 && got != firstMapped {
		t.Errorf("stepped to %#x; the first address any line claims is %#x. "+
			"A walk from the prologue must cross it, not advance one instruction",
			got, firstMapped)
	}
}
