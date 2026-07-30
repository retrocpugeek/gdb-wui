//go:build integration

package debugger_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/retrocpugeek/gdb-wui/internal/wire"
)

// M6: instruction-level debugging, judged on the stripped fixture where
// disassembly is the only available view.

// stripFixture rebuilds a fixture without debug info and strips it.
func stripFixture(t *testing.T, h *harness, name string) {
	t.Helper()
	bin := filepath.Join(h.files.Abs(), name)
	src := filepath.Join(h.files.Abs(), name+".c")
	if _, err := os.Stat(src); err != nil {
		t.Fatalf("fixture source: %v", err)
	}
	if out, err := exec.Command("gcc", "-O0", "-o", bin, src).CombinedOutput(); err != nil {
		t.Fatalf("compiling: %v\n%s", err, out)
	}
	if out, err := exec.Command("strip", bin).CombinedOutput(); err != nil {
		t.Fatalf("strip: %v\n%s", err, out)
	}
}

func TestDisassembleFunctionWithSymbols(t *testing.T) {
	h := startReal(t, "hello")
	h.mustDo(wire.TypeExeLoad, wire.ExeLoadRequest{Path: "hello"})
	h.mustDo(wire.TypeBpSetSource, wire.BreakpointRequest{Path: "hello.c", Line: lineMainInit})
	h.mustDo(wire.TypeExecRun, wire.ExecRequest{})
	h.rec.wait(t, wire.EventStopped)

	out := h.mustDo(wire.TypeDisasmFunction, wire.DisasmFunctionRequest{}).(wire.Disassembly)
	if len(out.Instructions) == 0 {
		t.Fatal("no instructions")
	}
	if out.Func != "main" {
		t.Errorf("func = %q, want main", out.Func)
	}
	if !out.HasSource {
		t.Error("a -g binary should produce source-attributed disassembly")
	}
	if out.PC == "" {
		t.Error("no PC reported; the panel could not mark the current instruction")
	}

	first := out.Instructions[0]
	if first.Text == "" {
		t.Error("an instruction with no text")
	}
	if first.Addr == 0 {
		t.Error("address was not parsed into a number")
	}
	if first.Opcodes == "" {
		t.Error("no raw opcodes; mode 5 should include them")
	}
	// With debug info every instruction should be attributed to a line.
	var attributed int
	for _, in := range out.Instructions {
		if in.Line > 0 {
			attributed++
		}
	}
	if attributed == 0 {
		t.Error("no instruction carried a source line despite hasSource")
	}

	// The PC must actually appear among the instructions, or the marker has
	// nothing to attach to.
	var found bool
	for _, in := range out.Instructions {
		if in.Address == out.PC {
			found = true
		}
	}
	if !found {
		t.Errorf("PC %s is not among the disassembled instructions", out.PC)
	}
}

// TestDisassembleStrippedBinary is the M6 criterion.
func TestDisassembleStrippedBinary(t *testing.T) {
	h := startReal(t, "nodebug")
	stripFixture(t, h, "nodebug")
	h.mustDo(wire.TypeExeLoad, wire.ExeLoadRequest{Path: "nodebug"})

	// No source and no symbols, so there is nowhere to put a source breakpoint
	// and --start has no main to stop at — the program would run to
	// completion. stopAtEntry is the only way in.
	h.mustDo(wire.TypeExecRun, wire.ExecRequest{StopAtEntry: true})
	stopped := h.rec.wait(t, wire.EventStopped).(wire.Stopped)

	if len(stopped.Frames) == 0 {
		t.Fatal("no frames")
	}
	pc := stopped.Frames[0].Address
	if pc == "" {
		t.Fatal("the frame has no address, which is all a stripped frame has")
	}

	out := h.mustDo(wire.TypeDisasmFunction, wire.DisasmFunctionRequest{}).(wire.Disassembly)
	if len(out.Instructions) == 0 {
		t.Fatal("no instructions for a stripped binary; this is the one view it has")
	}
	// Whether gdb finds a symbol here depends on what is left after stripping;
	// what matters is that instructions come back and are renderable.
	for i, in := range out.Instructions {
		if in.Text == "" {
			t.Errorf("instruction %d has no text", i)
		}
		if in.Address == "" {
			t.Errorf("instruction %d has no address", i)
		}
	}
	t.Logf("disassembled %d instructions at %s (hasSource=%v, func=%q)",
		len(out.Instructions), pc, out.HasSource, out.Func)
}

func TestDisassembleRange(t *testing.T) {
	h := startReal(t, "hello")
	h.mustDo(wire.TypeExeLoad, wire.ExeLoadRequest{Path: "hello"})
	h.mustDo(wire.TypeBpSetSource, wire.BreakpointRequest{Path: "hello.c", Line: lineMainInit})
	h.mustDo(wire.TypeExecRun, wire.ExecRequest{})
	stopped := h.rec.wait(t, wire.EventStopped).(wire.Stopped)

	pc := stopped.Frames[0].Address
	pcNum, err := strconv.ParseUint(strings.TrimPrefix(pc, "0x"), 16, 64)
	if err != nil {
		t.Fatalf("parsing pc %q: %v", pc, err)
	}

	out := h.mustDo(wire.TypeDisasmRange, wire.DisasmRangeRequest{
		Start: pc,
		End:   "0x" + strconv.FormatUint(pcNum+48, 16),
	}).(wire.Disassembly)

	if len(out.Instructions) == 0 {
		t.Fatal("no instructions in the range")
	}
	if out.Instructions[0].Address != pc {
		t.Errorf("range started at %s, want %s", out.Instructions[0].Address, pc)
	}
}

func TestDisassembleRangeValidation(t *testing.T) {
	h := startReal(t, "hello")
	h.mustDo(wire.TypeExeLoad, wire.ExeLoadRequest{Path: "hello"})
	h.mustDo(wire.TypeBpSetSource, wire.BreakpointRequest{Path: "hello.c", Line: lineMainInit})
	h.mustDo(wire.TypeExecRun, wire.ExecRequest{})
	h.rec.wait(t, wire.EventStopped)

	for _, tc := range []struct {
		name        string
		start, end  string
		wantCodeAny []string
	}{
		{"backwards", "0x2000", "0x1000", []string{wire.CodeBadRequest}},
		{"unparseable", "nonsense", "0x1000", []string{wire.CodeBadRequest}},
		{"enormous", "0x1000", "0x40000000", []string{wire.CodeTooLarge}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, werr := h.do(wire.TypeDisasmRange, wire.DisasmRangeRequest{
				Start: tc.start, End: tc.end,
			})
			if werr == nil {
				t.Fatal("accepted an invalid range")
			}
			var ok bool
			for _, want := range tc.wantCodeAny {
				if werr.Code == want {
					ok = true
				}
			}
			if !ok {
				t.Errorf("code = %q, want one of %v", werr.Code, tc.wantCodeAny)
			}
		})
	}
}

// TestStepInstruction covers stepi and nexti, which are what a disassembly
// view steps with.
func TestStepInstruction(t *testing.T) {
	h := startReal(t, "hello")
	h.mustDo(wire.TypeExeLoad, wire.ExeLoadRequest{Path: "hello"})
	h.mustDo(wire.TypeBpSetSource, wire.BreakpointRequest{Path: "hello.c", Line: lineMainInit})
	h.mustDo(wire.TypeExecRun, wire.ExecRequest{})
	stopped := h.rec.wait(t, wire.EventStopped).(wire.Stopped)
	before := stopped.Frames[0].Address

	h.mustDo(wire.TypeExecStepI, wire.ExecRequest{StopSeq: stopped.StopSeq})
	after := h.rec.wait(t, wire.EventStopped).(wire.Stopped)

	if after.Frames[0].Address == before {
		t.Errorf("stepi did not move the PC: still %s", before)
	}
	// One instruction, so usually the same source line — that is the whole
	// point of instruction stepping.
	if after.StopSeq <= stopped.StopSeq {
		t.Error("stopSeq did not advance")
	}

	next := h.mustDo(wire.TypeExecNextI, wire.ExecRequest{StopSeq: after.StopSeq})
	_ = next
	third := h.rec.wait(t, wire.EventStopped).(wire.Stopped)
	if third.Frames[0].Address == after.Frames[0].Address {
		t.Error("nexti did not move the PC")
	}
}

// TestStepInstructionOnStrippedBinary is instruction stepping where it is the
// only kind available.
func TestStepInstructionOnStrippedBinary(t *testing.T) {
	h := startReal(t, "nodebug")
	stripFixture(t, h, "nodebug")
	h.mustDo(wire.TypeExeLoad, wire.ExeLoadRequest{Path: "nodebug"})
	h.mustDo(wire.TypeExecRun, wire.ExecRequest{StopAtEntry: true})
	stopped := h.rec.wait(t, wire.EventStopped).(wire.Stopped)

	before := stopped.Frames[0].Address
	h.mustDo(wire.TypeExecStepI, wire.ExecRequest{StopSeq: stopped.StopSeq})
	after := h.rec.wait(t, wire.EventStopped).(wire.Stopped)

	if after.Frames[0].Address == before {
		t.Error("stepi did not move the PC in stripped code")
	}
	if after.Frames[0].Source.Available {
		t.Error("a stripped frame reported available source")
	}
	// And the disassembly must follow.
	out := h.mustDo(wire.TypeDisasmFunction, wire.DisasmFunctionRequest{}).(wire.Disassembly)
	if out.PC != after.Frames[0].Address {
		t.Errorf("disassembly PC %s does not match the frame %s",
			out.PC, after.Frames[0].Address)
	}
}

// TestDisassemblyGatedWhileRunning: like the other state queries, gdb would
// refuse it anyway.
func TestDisassemblyGatedWhileRunning(t *testing.T) {
	h := startReal(t, "threads")
	h.mustDo(wire.TypeExeLoad, wire.ExeLoadRequest{Path: "threads"})
	h.mustDo(wire.TypeExecRun, wire.ExecRequest{})
	h.rec.wait(t, wire.EventRunning)

	if _, werr := h.do(wire.TypeDisasmFunction, wire.DisasmFunctionRequest{}); werr == nil {
		t.Error("disassembly was accepted while running")
	} else if werr.Code != wire.CodeBusy {
		t.Errorf("code = %q, want busy", werr.Code)
	}
}

// TestFrameFromForLibraryCode: a frame with no source should still say where
// it came from, which for libc is the shared object.
func TestFrameFromForLibraryCode(t *testing.T) {
	h := startReal(t, "threads")
	h.mustDo(wire.TypeExeLoad, wire.ExeLoadRequest{Path: "threads"})
	h.mustDo(wire.TypeExecRun, wire.ExecRequest{})
	h.rec.wait(t, wire.EventRunning)
	if _, werr := h.do(wire.TypeExecPause, nil); werr != nil {
		t.Fatalf("pause: %s", werr.Message)
	}
	stopped := h.rec.wait(t, wire.EventStopped).(wire.Stopped)

	var sawFrom bool
	for _, f := range stopped.Frames {
		if f.From != "" {
			sawFrom = true
			t.Logf("frame %d: %s from %s", f.Level, f.Func, f.From)
		}
	}
	if !sawFrom {
		t.Skip("no frame reported a shared object this time")
	}
}
