//go:build integration

package debugger_test

import (
	"context"
	"fmt"
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
	"github.com/retrocpugeek/gdb-wui/internal/testutil"
	"github.com/retrocpugeek/gdb-wui/internal/wire"
)

// The decompiler's stack expressions on ARM, against a live ARM program.
//
// The x86 rule can be checked against the host's own binaries; this one cannot,
// and it is the case where being wrong is quiet. Ghidra's stack offsets are
// relative to the stack pointer at function entry, and an entry_sp that is four
// bytes out reads the neighbouring variable — a plausible number, from the
// wrong slot.
//
// Needs the cross toolchain, the emulator, a multi-architecture gdb and a
// Ghidra installation. CI has the first three, so this skips there.

// Lines of armDemo, which the test breaks on. Checked against the source below
// rather than trusted, because a line off by one moves the stop into the loop
// and quietly changes what the locals hold.
const (
	lineArmAccumulateReturn = 8
	lineArmBigframeReturn   = 17
)

const armDemo = `#include <stdio.h>
#include <string.h>

int accumulate(int n)
{
	int total = 0;
	for (int i = 0; i < n; i++) total += i * 3;
	return total;
}

int bigframe(int n)
{
	char buf[4096];
	int total = 0;
	memset(buf, 'a', sizeof buf);
	for (int i = 0; i < n; i++) total += buf[i] - 'a' + i * 3;
	return total;
}

int main(void)
{
	int a = accumulate(7);
	int b = bigframe(7);
	printf("%d %d\n", a, b);
	return 0;
}
`

// armDecompKit is a session on an ARM program: a real gdb-multiarch talking to
// a real qemu, with a real Ghidra behind the decompiled view.
type armDecompKit struct {
	do  func(string, any) any
	try func(string, any) (any, *wire.Error)
}

func armDecompHarness(t *testing.T) armDecompKit {
	t.Helper()
	testutil.RequireInstalledTools(t, "arm-linux-gnueabihf-gcc", "qemu-arm", "gdb-multiarch")
	install, err := ghidra.Locate("")
	if err != nil {
		t.Skipf("no Ghidra installation: %v", err)
	}

	dir := t.TempDir()
	src := filepath.Join(dir, "armdemo.c")
	if err := os.WriteFile(src, []byte(armDemo), 0o644); err != nil {
		t.Fatal(err)
	}
	checkLine(t, armDemo, lineArmAccumulateReturn, "return total;")
	checkLine(t, armDemo, lineArmBigframeReturn, "return total;")

	// Statically linked so qemu needs no sysroot, and built inside the project
	// so gdb can find the source by the path the compiler recorded.
	out, err := exec.Command("arm-linux-gnueabihf-gcc", "-g", "-O0", "-static",
		"-o", filepath.Join(dir, "armdemo"), src).CombinedOutput()
	if err != nil {
		t.Fatalf("cross-compiling armdemo.c: %v\n%s", err, out)
	}

	files, err := srcfs.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = files.Close() })

	logf := func(f string, a ...any) { t.Logf(f, a...) }
	sess, err := debugger.New(t.Context(), debugger.Config{
		MI: mi.Options{
			Path:     "gdb-multiarch",
			Dir:      dir,
			Logf:     logf,
			ExtraEnv: []string{"HOME=" + t.TempDir()},
		},
		Files:          files,
		Events:         newRecorder(),
		Logf:           logf,
		Version:        "test",
		CommandTimeout: 30 * time.Second,
		Decomp: debugger.DecompConfig{
			Install:   install,
			CacheRoot: filepath.Join(dir, "cache"),
		},
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

	do(wire.TypeExeLoad, wire.ExeLoadRequest{Path: "armdemo"})
	port := qemuStub(t, dir, "armdemo")
	do(wire.TypeConsoleExec, wire.ConsoleExecRequest{
		Line: fmt.Sprintf("target remote 127.0.0.1:%d", port),
	})
	waitReady(t, do)
	return armDecompKit{do: do, try: try}
}

// TestArmStackExpressionsReadTheRightMemory is the ARM half of
// TestDecompStackExpressionsReadTheRightMemory, and it asks a stricter
// question. The program is built with debug information, so gdb knows where
// each local really is: every address the decompiler's expression computes is
// compared against the one gdb gets from DWARF for the variable of the same
// name.
//
// bigframe is in here for a specific reason. Ghidra reports its frame size as
// 4124 where the prologue moves the stack pointer 4128, because the size is
// derived from the variables Ghidra found rather than from the prologue. A
// rule built on frame.size lands every one of its locals four bytes low.
func TestArmStackExpressionsReadTheRightMemory(t *testing.T) {
	k := armDecompHarness(t)
	do := k.do

	do(wire.TypeBpSetSource, wire.BreakpointRequest{
		Path: "armdemo.c", Line: lineArmAccumulateReturn,
	})
	do(wire.TypeBpSetSource, wire.BreakpointRequest{
		Path: "armdemo.c", Line: lineArmBigframeReturn,
	})
	do(wire.TypeExecContinue, wire.ExecRequest{})
	waitStopped(t, do, 60*time.Second)

	// accumulate(7) sums i*3 for i in 0..6 = 63, and one stack slot holds it.
	fn := do(wire.TypeDecompFunction,
		wire.DecompFunctionRequest{Target: "accumulate"}).(wire.DecompFunction)
	if !checkArmStackVars(t, k, fn) {
		t.Error("accumulate has no readable stack variables at all; " +
			"the ARM frame rule produced no expressions")
	}
	if !readsValue(t, k, fn, "63") {
		t.Error("no stack expression in accumulate read the accumulator's " +
			"value of 63")
	}

	do(wire.TypeExecContinue, wire.ExecRequest{})
	waitStopped(t, do, 60*time.Second)

	fn = do(wire.TypeDecompFunction,
		wire.DecompFunctionRequest{Target: "bigframe"}).(wire.DecompFunction)
	if !checkArmStackVars(t, k, fn) {
		t.Error("bigframe has no readable stack variables at all")
	}
}

// checkArmStackVars compares each stack expression's address with gdb's own,
// and reports whether there was anything to compare.
func checkArmStackVars(t *testing.T, k armDecompKit, fn wire.DecompFunction) bool {
	t.Helper()
	var compared int
	for _, v := range fn.Vars {
		if v.Storage != wire.DecompStorageStack || v.Expr == "" {
			continue
		}
		// gdb knows this variable by name only when the decompiler took the
		// name from the debug information; Ghidra's own inventions — n_local
		// beside gdb's n — have nothing to compare against.
		truth, werr := k.try(wire.TypeEvalExpr, wire.EvalExprRequest{Expr: "&" + v.Name})
		if werr != nil {
			continue
		}
		want, ok := addrIn(truth.(wire.EvalExpr).Value)
		if !ok {
			continue
		}
		res, werr := k.try(wire.TypeEvalExpr,
			wire.EvalExprRequest{Expr: "&(" + v.Expr + ")"})
		if werr != nil {
			t.Errorf("%s: %s did not evaluate: %s", v.Name, v.Expr, werr.Message)
			continue
		}
		got, ok := addrIn(res.(wire.EvalExpr).Value)
		if !ok {
			t.Errorf("%s: &(%s) gave %q, which names no address",
				v.Name, v.Expr, res.(wire.EvalExpr).Value)
			continue
		}
		compared++
		if got != want {
			t.Errorf("%s in %s: %s is at %#x, gdb says %#x (out by %d)",
				v.Name, fn.Name, v.Expr, got, want, int64(got)-int64(want))
			continue
		}
		t.Logf("  %-10s %-34s %#x, agreeing with gdb", v.Name, v.Expr, got)
	}
	return compared > 0
}

// readsValue says whether any of the function's stack expressions evaluates to
// the value given.
func readsValue(t *testing.T, k armDecompKit, fn wire.DecompFunction, want string) bool {
	t.Helper()
	for _, v := range fn.Vars {
		if v.Storage != wire.DecompStorageStack || v.Expr == "" {
			continue
		}
		res, werr := k.try(wire.TypeEvalExpr, wire.EvalExprRequest{Expr: v.Expr})
		if werr != nil {
			continue
		}
		if strings.TrimSpace(res.(wire.EvalExpr).Value) == want {
			return true
		}
	}
	return false
}

// addrIn pulls the address out of what gdb prints for a pointer: `&total` is
// answered as `(int *) 0x407ff5e8`, and the type is in the way.
func addrIn(value string) (uint64, bool) {
	fields := strings.Fields(value)
	for i := len(fields) - 1; i >= 0; i-- {
		if n, err := strconv.ParseUint(strings.TrimPrefix(fields[i], "0x"), 16, 64); err == nil {
			return n, true
		}
	}
	return 0, false
}

// checkLine fails when the source line a breakpoint constant names is not the
// line it was written for.
func checkLine(t *testing.T, source string, n int, want string) {
	t.Helper()
	lines := strings.Split(source, "\n")
	if n < 1 || n > len(lines) {
		t.Fatalf("line %d is outside the fixture", n)
	}
	if !strings.Contains(lines[n-1], want) {
		t.Fatalf("line %d is %q, which does not contain %q", n, lines[n-1], want)
	}
}
