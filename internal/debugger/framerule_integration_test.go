//go:build integration

package debugger_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/retrocpugeek/gdb-wui/internal/debugger"
	"github.com/retrocpugeek/gdb-wui/internal/ghidra"
	"github.com/retrocpugeek/gdb-wui/internal/srcfs"
	"github.com/retrocpugeek/gdb-wui/internal/testutil"
	"github.com/retrocpugeek/gdb-wui/internal/wire"
)

// The frame rule against a live inferior, with no Ghidra anywhere.
//
// TestCrossArchStackExpressionsReadTheRightMemory is the real thing and needs a
// Ghidra installation, so it never runs in CI. This one stands a compiler's own
// debug information in Ghidra's place, which needs nothing but binutils, and so
// it runs on every push.
//
// The substitution is sound because the two describe the same frame. gcc emits
// `DW_AT_frame_base: DW_OP_call_frame_cfa` for these functions and locates each
// local as `DW_OP_fbreg: <offset>` from it, and on every architecture whose
// call leaves the return address in a register the call frame address *is* the
// stack pointer at function entry — exactly what Ghidra's offsets are relative
// to. Checked rather than assumed: the frame base is read out of the binary
// below, and the offsets it yields are the same numbers Ghidra gives —
// AArch64's leafish has a, b, x and y at -20, -24, -8 and -4 in the sidecar and
// in the debug information alike.
//
// x86 is the exception and it cancels. `call` pushes the return address before
// the function is entered, so the call frame address is eight bytes above
// Ghidra's base: the DWARF offsets are eight larger than Ghidra's, and the
// depth this measures from the caller's stack pointer is eight smaller. The
// pair is self-consistent, so the arithmetic under test is the same
// arithmetic, and a constant that varExpr added of its own would still show
// up as a wrong address.
//
// What this proves: that varExpr's arithmetic, its choice of base register and
// its type translation produce an expression which gdb parses and which lands
// on the variable, across every architecture in the table — two families, both
// widths of each, and a big-endian one whose stack offsets go positive. What
// it cannot prove is
// that Ghidra's own numbers mean what this believes they mean. Only the test
// with a real decompiler behind it can say that.
func TestFrameRuleAgainstDebugInfo(t *testing.T) {
	for _, arch := range crossArches {
		t.Run(arch.name, func(t *testing.T) {
			testutil.RequireInstalledTools(t, arch.gcc, arch.qemu, "gdb-multiarch", "objdump")

			files := crossDemoProject(t, arch.gcc)
			layout := dwarfFrame(t, filepath.Join(files.Abs(), "crossdemo"))
			port := qemuStub(t, arch.qemu, files.Abs(), "crossdemo")
			h := startRealWithGDB(t, files, "gdb-multiarch")

			h.mustDo(wire.TypeExeLoad, wire.ExeLoadRequest{Path: "crossdemo"})
			h.mustDo(wire.TypeConsoleExec, wire.ConsoleExecRequest{
				Line: "target remote 127.0.0.1:" + strconv.Itoa(port),
			})
			h.mustDo(wire.TypeBpSetSource, wire.BreakpointRequest{
				Path: "crossdemo.c", Line: lineAccumulateReturn,
			})
			h.mustDo(wire.TypeBpSetSource, wire.BreakpointRequest{
				Path: "crossdemo.c", Line: lineBigframeReturn,
			})

			// accumulate first: main calls them in that order, in statements of
			// their own so that the order is the language's rather than the
			// compiler's choice of when to evaluate an argument.
			h.mustDo(wire.TypeExecContinue, wire.ExecRequest{})
			waitStop(t, h)
			checkFrameRule(t, h, arch.lang, layout["accumulate"],
				[]local{{"total", "int", 4}, {"n", "int", 4}})

			h.rec.reset()
			h.mustDo(wire.TypeExecContinue, wire.ExecRequest{})
			waitStop(t, h)
			// bigframe carries an array, which is the case that goes through
			// gdbCType: `char[4096]` is C gdb will parse, and most of Ghidra's
			// type vocabulary is not.
			checkFrameRule(t, h, arch.lang, layout["bigframe"],
				[]local{{"buf", "char[4096]", 4096}, {"total", "int", 4}, {"n", "int", 4}})
		})
	}

	// And the host's own architecture, at the optimisation level that broke
	// the rule it used to have. No emulator: this one runs.
	t.Run("x86-64-O2", func(t *testing.T) {
		testutil.RequireGDB(t, 10)
		testutil.RequireInstalledTools(t, "gcc", "objdump")

		files := optProject(t)
		layout := dwarfFrame(t, filepath.Join(files.Abs(), "opt"))
		h := startRealWithFiles(t, files)

		h.mustDo(wire.TypeExeLoad, wire.ExeLoadRequest{Path: "opt"})
		h.mustDo(wire.TypeBpSetSource, wire.BreakpointRequest{
			Path: "opt.c", Line: lineTallyLoop,
		})
		h.mustDo(wire.TypeExecRun, wire.ExecRequest{})
		waitStop(t, h)

		// gcc omits the frame pointer here, so $rbp holds whatever main left
		// in it. The rule this replaced computed addresses from that register
		// and landed 192 bytes away, in the caller's frame.
		checkFrameRule(t, h, "x86:LE:64:default", layout["tally"],
			[]local{{"buf", "char[64]", 64}})
	})
}

// Line of optSource that the -O2 test stops on, inside the loop so that buf is
// certainly live rather than merely declared.
const lineTallyLoop = 11

// optSource is a program with stack that survives -O2: arrays the compiler
// cannot hold in registers, and a call in the middle so the frame is live
// across it. noinline because the point is to have a frame at all.
const optSource = `#include <stdio.h>
#include <string.h>

__attribute__((noinline)) int tally(int n)
{
	char buf[64];
	int total = 0;
	memset(buf, 'a', sizeof buf);
	buf[sizeof buf - 1] = 0;
	for (int i = 0; i < n && i < (int)sizeof buf; i++)
		total += buf[i] - 'a' + i * 3;
	printf("%.4s\n", buf);
	return total;
}

int main(void)
{
	printf("%d\n", tally(7));
	return 0;
}
`

// optProject builds optSource for the host at -O2.
func optProject(t *testing.T) *srcfs.FS {
	t.Helper()
	dir := t.TempDir()
	src := filepath.Join(dir, "opt.c")
	if err := os.WriteFile(src, []byte(optSource), 0o644); err != nil {
		t.Fatal(err)
	}
	checkLine(t, optSource, lineTallyLoop, "total += buf[i]")

	out, err := exec.Command("gcc", "-g", "-O2", "-o", filepath.Join(dir, "opt"), src).
		CombinedOutput()
	if err != nil {
		t.Fatalf("compiling opt.c: %v\n%s", err, out)
	}
	files, err := srcfs.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = files.Close() })
	return files
}

// local is one variable to check, with the type a decompiler would report for
// it.
type local struct {
	name string
	typ  string
	size int
}

// checkFrameRule builds each variable's expression the way the server does and
// asks gdb where it lands.
func checkFrameRule(t *testing.T, h *harness, lang string,
	offsets map[string]int, locals []local,
) {
	t.Helper()

	// The depth the prologue left behind, which is what Ghidra reports as
	// frame.spDepth: the stack pointer here, against the caller's. `bl` pushes
	// nothing on either architecture, so the caller's stack pointer *is* this
	// frame's entry stack pointer, and gdb calls it "Previous frame's sp".
	here := evalAddr(t, h, "$sp", 0)
	entry := evalAddr(t, h, "$sp", 1)
	depth := int(int64(here) - int64(entry))
	if depth >= 0 {
		t.Fatalf("the stack did not grow down: $sp %#x, caller's $sp %#x", here, entry)
	}
	t.Logf("  frame at %#x, $sp %#x, spDepth %d", entry, here, depth)

	for _, l := range locals {
		off, ok := offsets[l.name]
		if !ok {
			t.Errorf("the debug information locates no %s in this frame", l.name)
			continue
		}
		expr := debugger.VarExpr(
			ghidra.Var{
				Name: l.name, Type: l.typ, Size: l.size,
				Storage: ghidra.Storage{Kind: ghidra.StorageStack, Offset: off},
			},
			ghidra.Frame{SPDepth: &depth}, lang)
		if expr == "" {
			t.Errorf("%s: no expression for a stack variable on %s", l.name, lang)
			continue
		}
		want := evalAddr(t, h, "&"+l.name, 0)
		got := evalAddr(t, h, "&("+expr+")", 0)
		if got != want {
			t.Errorf("%s: %s is at %#x, the debug information says %#x (out by %d)",
				l.name, expr, got, want, int64(got)-int64(want))
			continue
		}
		t.Logf("  %-8s fbreg %-6d %-34s %#x", l.name, off, expr, got)
	}
}

// evalAddr evaluates an expression in one frame and returns the address it
// names.
func evalAddr(t *testing.T, h *harness, expr string, frame int) uint64 {
	t.Helper()
	out := h.mustDo(wire.TypeEvalExpr,
		wire.EvalExprRequest{Expr: expr, Frame: frame}).(wire.EvalExpr)
	addr, ok := addrIn(out.Value)
	if !ok {
		t.Fatalf("%s evaluated to %q, which names no address", expr, out.Value)
	}
	return addr
}

// waitStop waits for the stop that follows an exec request, which is an
// acknowledgement rather than a completion.
func waitStop(t *testing.T, h *harness) {
	t.Helper()
	waitFor(t, h, func(snap wire.Hello) bool {
		return snap.RunState == wire.RunStateStopped && len(snap.Frames) > 0
	}, "the program to stop")
}

var (
	dwarfSubprogram = regexp.MustCompile(`DW_TAG_(subprogram|variable|formal_parameter)`)
	dwarfName       = regexp.MustCompile(`DW_AT_name\s*:.*?([A-Za-z_][A-Za-z0-9_]*)\s*$`)
	dwarfFbreg      = regexp.MustCompile(`DW_OP_fbreg:\s*(-?\d+)`)
	dwarfFrameBase  = regexp.MustCompile(`DW_AT_frame_base\s*:.*\((DW_OP_\w+)\)`)
)

// dwarfFrame reads each function's stack layout out of the binary: variable
// name to offset from the frame base, per function.
//
// objdump rather than a DWARF library, and the host's rather than the cross
// toolchain's: reading the debug information is architecture-independent, and
// a test that needs no extra package installed is a test that runs.
//
// It insists the frame base is the call frame address. gcc uses that for these
// targets and this test is built on it — a function whose locals were located
// from some other base would be measuring something else, and quietly.
func dwarfFrame(t *testing.T, exe string) map[string]map[string]int {
	t.Helper()
	out, err := exec.Command("objdump", "--dwarf=info", exe).Output()
	if err != nil {
		t.Fatalf("objdump --dwarf=info: %v", err)
	}

	frames := map[string]map[string]int{}
	var fn, name string
	for _, line := range strings.Split(string(out), "\n") {
		if tag := dwarfSubprogram.FindStringSubmatch(line); tag != nil {
			if tag[1] == "subprogram" {
				fn = ""
			}
			name = ""
			continue
		}
		if m := dwarfName.FindStringSubmatch(line); m != nil {
			if fn == "" {
				fn = m[1]
				continue
			}
			name = m[1]
			continue
		}
		if m := dwarfFrameBase.FindStringSubmatch(line); m != nil && fn != "" {
			if m[1] != "DW_OP_call_frame_cfa" {
				t.Fatalf("%s locates its locals from %s, not from the call frame address",
					fn, m[1])
			}
			continue
		}
		if m := dwarfFbreg.FindStringSubmatch(line); m != nil && fn != "" && name != "" {
			off, err := strconv.Atoi(m[1])
			if err != nil {
				t.Fatal(err)
			}
			if frames[fn] == nil {
				frames[fn] = map[string]int{}
			}
			frames[fn][name] = off
			name = ""
		}
	}
	if len(frames) == 0 {
		t.Fatalf("no frame layouts in %s; objdump printed %d bytes", exe, len(out))
	}
	return frames
}
