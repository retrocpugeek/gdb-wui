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

// The decompiler's stack expressions on every architecture with a frame rule
// but the host's own, against a live program for each.
//
// The x86 rule can be checked against the host's own binaries; these cannot,
// and they are the case where being wrong is quiet. Ghidra's stack offsets are
// relative to the stack pointer at function entry, and an entry_sp that is four
// bytes out reads the neighbouring variable — a plausible number, from the
// wrong slot.
//
// Needs the cross toolchains, the emulators, a multi-architecture gdb and a
// Ghidra installation. CI has everything but Ghidra, so this skips there.

// The architectures come from crossArches, beside the test that debugs a
// program on each. One rule covers both, and the prologues gcc gives them
// share nothing: 32-bit ARM pushes and then subtracts a constant, AArch64
// writes `stp x29, x30, [sp, #-16]!` and then subtracts a register.

// Lines of crossDemo, which the test breaks on. Checked against the source below
// rather than trusted, because a line off by one moves the stop into the loop
// and quietly changes what the locals hold.
const (
	lineAccumulateReturn = 8
	lineBigframeReturn   = 17
)

const crossDemo = `#include <stdio.h>
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

// crossDemoProject writes crossDemo into a project and builds it for another
// architecture.
//
// Statically linked so qemu needs no sysroot, and built inside the project so
// gdb can find the source by the path the compiler recorded.
func crossDemoProject(t *testing.T, gcc string) *srcfs.FS {
	t.Helper()
	dir := t.TempDir()
	src := filepath.Join(dir, "crossdemo.c")
	if err := os.WriteFile(src, []byte(crossDemo), 0o644); err != nil {
		t.Fatal(err)
	}
	checkLine(t, crossDemo, lineAccumulateReturn, "return total;")
	checkLine(t, crossDemo, lineBigframeReturn, "return total;")

	out, err := exec.Command(gcc, "-g", "-O0", "-static",
		"-o", filepath.Join(dir, "crossdemo"), src).CombinedOutput()
	if err != nil {
		t.Fatalf("cross-compiling crossdemo.c: %v\n%s", err, out)
	}

	files, err := srcfs.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = files.Close() })
	return files
}

// crossDecompKit is a session on a program for another architecture: a real
// gdb-multiarch talking to a real qemu, with a real Ghidra behind the
// decompiled view.
type crossDecompKit struct {
	do  func(string, any) any
	try func(string, any) (any, *wire.Error)
}

func crossDecompHarness(t *testing.T, gcc, qemu string) crossDecompKit {
	t.Helper()
	// Ghidra first, and that order is the point: CI installs the cross
	// toolchains and RequireInstalledTools fails rather than skips when one of
	// them is missing there. CI has no Ghidra, so this test skips there for a
	// reason of its own, and asking for compilers it will never use would turn
	// that into a demand on the workflow.
	install, err := ghidra.Locate("")
	if err != nil {
		t.Skipf("no Ghidra installation: %v", err)
	}
	testutil.RequireInstalledTools(t, gcc, qemu, "gdb-multiarch")

	files := crossDemoProject(t, gcc)
	dir := files.Abs()

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

	do(wire.TypeExeLoad, wire.ExeLoadRequest{Path: "crossdemo"})
	port := qemuStub(t, qemu, dir, "crossdemo")
	do(wire.TypeConsoleExec, wire.ConsoleExecRequest{
		Line: fmt.Sprintf("target remote 127.0.0.1:%d", port),
	})
	waitReady(t, do)
	return crossDecompKit{do: do, try: try}
}

// TestCrossArchStackExpressionsReadTheRightMemory is the cross-architecture
// half of TestDecompStackExpressionsReadTheRightMemory, and it asks a stricter
// question. The program is built with debug information, so gdb knows where
// each local really is: every address the decompiler's expression computes is
// compared against the one gdb gets from DWARF for the variable of the same
// name.
//
// bigframe is in here for a specific reason. Its frame size comes back as 4124
// on ARM, 4132 on AArch64, 4152 on PowerPC and 4292 on PowerPC64, where the
// prologues move the stack pointer 4128, 4144, 4144 and 4240, because the size
// is derived from the variables Ghidra found rather than from the prologue. A
// rule built on frame.size lands every one of its locals in the wrong place,
// and on three of the four only just — which reads as a value.
func TestCrossArchStackExpressionsReadTheRightMemory(t *testing.T) {
	for _, arch := range crossArches {
		t.Run(arch.name, func(t *testing.T) {
			k := crossDecompHarness(t, arch.gcc, arch.qemu)
			do := k.do

			do(wire.TypeBpSetSource, wire.BreakpointRequest{
				Path: "crossdemo.c", Line: lineAccumulateReturn,
			})
			do(wire.TypeBpSetSource, wire.BreakpointRequest{
				Path: "crossdemo.c", Line: lineBigframeReturn,
			})
			do(wire.TypeExecContinue, wire.ExecRequest{})
			waitStopped(t, do, 60*time.Second)

			// accumulate(7) sums i*3 for i in 0..6 = 63, and one stack slot
			// holds it.
			fn := do(wire.TypeDecompFunction,
				wire.DecompFunctionRequest{Target: "accumulate"}).(wire.DecompFunction)
			if !checkStackVars(t, k, fn) {
				t.Error("accumulate has no readable stack variables at all; " +
					"the frame rule produced no expressions")
			}
			if !readsValue(t, k, fn, "63") {
				t.Error("no stack expression in accumulate read the " +
					"accumulator's value of 63")
			}

			do(wire.TypeExecContinue, wire.ExecRequest{})
			waitStopped(t, do, 60*time.Second)

			fn = do(wire.TypeDecompFunction,
				wire.DecompFunctionRequest{Target: "bigframe"}).(wire.DecompFunction)
			if !checkStackVars(t, k, fn) {
				t.Error("bigframe has no readable stack variables at all")
			}
		})
	}
}

// checkStackVars compares each stack expression's address with gdb's own,
// and reports whether there was anything to compare.
func checkStackVars(t *testing.T, k crossDecompKit, fn wire.DecompFunction) bool {
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
func readsValue(t *testing.T, k crossDecompKit, fn wire.DecompFunction, want string) bool {
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
//
// Both bases, because gdb chooses. A pointer prints as hex with a 0x, but a
// register whose type is an integer prints in decimal — `$sp` is `(void *)
// 0x407ff620` on ARM and `1082128192` on PowerPC — and reading the second as
// hex yields an address that is wrong and plausible.
func addrIn(value string) (uint64, bool) {
	fields := strings.Fields(value)
	for i := len(fields) - 1; i >= 0; i-- {
		f := fields[i]
		if hex, ok := strings.CutPrefix(f, "0x"); ok {
			if n, err := strconv.ParseUint(hex, 16, 64); err == nil {
				return n, true
			}
			continue
		}
		if n, err := strconv.ParseUint(f, 10, 64); err == nil {
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
