//go:build integration

package debugger_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/retrocpugeek/gdb-wui/internal/debugger"
	"github.com/retrocpugeek/gdb-wui/internal/mi"
	"github.com/retrocpugeek/gdb-wui/internal/srcfs"
	"github.com/retrocpugeek/gdb-wui/internal/testutil"
	"github.com/retrocpugeek/gdb-wui/internal/wire"
)

// Line numbers in testdata/fixtures/hello.c that these tests depend on. Named
// rather than inlined so an edit to the fixture fails in one obvious place.
const (
	lineAddSum   = 5  // int sum = a + b;
	lineMainInit = 12 // int total = 0;
	lineMainLoop = 14 // for (i = 0; i < 3; i++)
)

// realProject copies a fixture into a fresh project directory and compiles it
// there, so gdb reports paths that genuinely live inside the project root —
// which is the case source resolution has to handle.
func realProject(t *testing.T, fixture string) *srcfs.FS {
	t.Helper()
	testutil.RequireTools(t, "gcc")

	dir := t.TempDir()
	src := filepath.Join(testutil.RepoRoot(t), "testdata", "fixtures", fixture+".c")
	body, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, fixture+".c")
	if err := os.WriteFile(dst, body, 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("gcc", "-g", "-O0", "-o", filepath.Join(dir, fixture), dst)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("compiling %s: %v\n%s", fixture, err, out)
	}

	f, err := srcfs.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	return f
}

// startReal brings up a session against a real gdb.
func startReal(t *testing.T, fixture string) *harness {
	t.Helper()
	testutil.RequireGDB(t, 10)

	files := realProject(t, fixture)
	rec := newRecorder()

	sess, err := debugger.New(t.Context(), debugger.Config{
		MI: mi.Options{
			Dir:      files.Abs(),
			Logf:     testLogf(t),
			ExtraEnv: []string{"HOME=" + t.TempDir()},
		},
		Files:          files,
		Events:         rec,
		Logf:           testLogf(t),
		Version:        "test",
		CommandTimeout: 20 * time.Second,
	})
	if err != nil {
		t.Fatalf("debugger.New: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := sess.Close(ctx); err != nil {
			t.Errorf("close: %v", err)
		}
	})
	return &harness{t: t, sess: sess, rec: rec, files: files}
}

// TestVerticalSliceAgainstRealGDB is the M3 acceptance criterion as a test:
// load, break, run, hit, step, continue to exit.
func TestVerticalSliceAgainstRealGDB(t *testing.T) {
	h := startReal(t, "hello")

	h.mustDo(wire.TypeExeLoad, wire.ExeLoadRequest{Path: "hello"})

	bp := h.mustDo(wire.TypeBpSetSource, wire.BreakpointRequest{
		Path: "hello.c", Line: lineMainInit,
	}).(wire.Breakpoint)
	if bp.Line != lineMainInit {
		t.Errorf("breakpoint landed on line %d, want %d", bp.Line, lineMainInit)
	}
	if bp.Path != "hello.c" {
		t.Errorf("breakpoint path = %q, want the root-relative hello.c", bp.Path)
	}
	if bp.Pending {
		t.Error("the breakpoint is pending against a loaded program with symbols")
	}

	h.mustDo(wire.TypeExecRun, wire.ExecRequest{})
	stopped := h.rec.wait(t, wire.EventStopped).(wire.Stopped)

	if stopped.Reason != "breakpoint-hit" {
		t.Fatalf("reason = %q, want breakpoint-hit", stopped.Reason)
	}
	if stopped.BreakpointNumber != bp.Number {
		t.Errorf("hit breakpoint %d, want %d", stopped.BreakpointNumber, bp.Number)
	}
	if len(stopped.Frames) == 0 {
		t.Fatal("the stop carried no frames")
	}
	frame := stopped.Frames[0]
	if frame.Func != "main" {
		t.Errorf("stopped in %q, want main", frame.Func)
	}
	if !frame.Source.Available || frame.Source.Path != "hello.c" {
		t.Errorf("source = %+v, want hello.c resolved inside the project", frame.Source)
	}
	if frame.Source.Line != lineMainInit {
		t.Errorf("line = %d, want %d", frame.Source.Line, lineMainInit)
	}
	// The fat event must carry the rest too, or the UI needs extra round-trips.
	if len(stopped.Threads) == 0 {
		t.Error("no threads in the stop event")
	}
	if len(stopped.Locals) == 0 {
		t.Error("no locals in the stop event")
	}

	// Step over one line.
	firstSeq := stopped.StopSeq
	h.mustDo(wire.TypeExecNext, wire.ExecRequest{StopSeq: firstSeq})
	stepped := h.rec.wait(t, wire.EventStopped).(wire.Stopped)
	if stepped.StopSeq <= firstSeq {
		t.Errorf("stopSeq did not advance: %d then %d", firstSeq, stepped.StopSeq)
	}
	if stepped.Reason != "end-stepping-range" {
		t.Errorf("reason = %q, want end-stepping-range", stepped.Reason)
	}
	if got := stepped.Frames[0].Source.Line; got != lineMainLoop {
		t.Errorf("after next, line = %d, want %d", got, lineMainLoop)
	}

	// A request naming the previous stop must now be refused.
	if _, werr := h.do(wire.TypeExecNext, wire.ExecRequest{StopSeq: firstSeq}); werr == nil {
		t.Error("a step naming a superseded stop was accepted")
	} else if werr.Code != wire.CodeBusy {
		t.Errorf("stale step: code = %q, want busy", werr.Code)
	}

	// Run to completion.
	h.mustDo(wire.TypeExecContinue, wire.ExecRequest{StopSeq: stepped.StopSeq})
	exited := h.rec.wait(t, wire.EventExited).(wire.Exited)
	if exited.ExitCode == nil || *exited.ExitCode != 0 {
		t.Errorf("exitCode = %v, want 0", exited.ExitCode)
	}
	if snap := h.sess.Snapshot(); snap.RunState != wire.RunStateExited {
		t.Errorf("runState = %q after exit", snap.RunState)
	}
}

// TestStepIntoAndFinish covers the other half of the exec group.
func TestStepIntoAndFinish(t *testing.T) {
	h := startReal(t, "hello")
	h.mustDo(wire.TypeExeLoad, wire.ExeLoadRequest{Path: "hello"})
	h.mustDo(wire.TypeBpSetSource, wire.BreakpointRequest{Path: "hello.c", Line: lineAddSum})
	h.mustDo(wire.TypeExecRun, wire.ExecRequest{})

	stopped := h.rec.wait(t, wire.EventStopped).(wire.Stopped)
	if stopped.Frames[0].Func != "add" {
		t.Fatalf("stopped in %q, want add", stopped.Frames[0].Func)
	}
	// A nested stop means a real stack, which is what the frame list needs to
	// prove it kept every repeated "frame" key.
	if len(stopped.Frames) < 2 {
		t.Fatalf("stack has %d frames, want at least add and main", len(stopped.Frames))
	}
	if stopped.Frames[1].Func != "main" {
		t.Errorf("frame 1 is %q, want main", stopped.Frames[1].Func)
	}
	// Arguments come with the frame.
	if len(stopped.Frames[0].Args) != 2 {
		t.Errorf("add has %d args, want 2: %+v", len(stopped.Frames[0].Args), stopped.Frames[0].Args)
	}

	h.mustDo(wire.TypeExecFinish, wire.ExecRequest{StopSeq: stopped.StopSeq})
	finished := h.rec.wait(t, wire.EventStopped).(wire.Stopped)
	if finished.Reason != "function-finished" {
		t.Errorf("reason = %q, want function-finished", finished.Reason)
	}
	if finished.Frames[0].Func != "main" {
		t.Errorf("finished into %q, want main", finished.Frames[0].Func)
	}
	if finished.ReturnValue == "" {
		t.Error("no return value reported for a finished function")
	}
}

// TestFrameSelectFetchesLocals: clicking a frame in the stack panel must swap
// the locals with that frame's.
func TestFrameSelectFetchesLocals(t *testing.T) {
	h := startReal(t, "hello")
	h.mustDo(wire.TypeExeLoad, wire.ExeLoadRequest{Path: "hello"})
	h.mustDo(wire.TypeBpSetSource, wire.BreakpointRequest{Path: "hello.c", Line: lineAddSum})
	h.mustDo(wire.TypeExecRun, wire.ExecRequest{})
	stopped := h.rec.wait(t, wire.EventStopped).(wire.Stopped)

	sel := h.mustDo(wire.TypeFrameSelect, wire.FrameSelectRequest{
		Frame: 1, StopSeq: stopped.StopSeq,
	}).(wire.Selection)
	if sel.Frame != 1 {
		t.Errorf("frame = %d", sel.Frame)
	}
	// main has i and total; add has sum. Seeing main's locals proves the frame
	// argument reached gdb.
	names := map[string]bool{}
	for _, v := range sel.Locals {
		names[v.Name] = true
	}
	if !names["total"] {
		t.Errorf("frame 1 locals = %+v, want main's (total, i)", sel.Locals)
	}
}

// TestRunStateGateAgainstRealGDB proves the gate is not merely a mirror of our
// own bookkeeping: the same commands really are refused by gdb.
func TestRunStateGateAgainstRealGDB(t *testing.T) {
	h := startReal(t, "threads")
	h.mustDo(wire.TypeExeLoad, wire.ExeLoadRequest{Path: "threads"})
	h.mustDo(wire.TypeExecRun, wire.ExecRequest{})
	h.rec.wait(t, wire.EventRunning)

	if _, werr := h.do(wire.TypeStackList, wire.StackListRequest{}); werr == nil {
		t.Error("stack.list was accepted while the inferior is running")
	} else if werr.Code != wire.CodeBusy {
		t.Errorf("code = %q, want busy", werr.Code)
	}

	// Pause must work, and must produce a stop.
	if _, werr := h.do(wire.TypeExecPause, nil); werr != nil {
		t.Fatalf("pause: %s", werr.Message)
	}
	stopped := h.rec.wait(t, wire.EventStopped).(wire.Stopped)
	if len(stopped.Frames) == 0 {
		t.Error("pausing landed on no frame at all")
	}
	if h.sess.Snapshot().RunState != wire.RunStateStopped {
		t.Error("runState is not stopped after a pause")
	}
}

// TestKillUsesConsoleCommand pins the M1 finding: -exec-kill does not exist, so
// exec.kill must go through the console and must still work.
func TestKillUsesConsoleCommand(t *testing.T) {
	h := startReal(t, "threads")
	h.mustDo(wire.TypeExeLoad, wire.ExeLoadRequest{Path: "threads"})
	h.mustDo(wire.TypeExecRun, wire.ExecRequest{})
	h.rec.wait(t, wire.EventRunning)

	if _, werr := h.do(wire.TypeExecKill, nil); werr != nil {
		t.Fatalf("kill: %s: %s", werr.Code, werr.Message)
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if h.sess.Snapshot().RunState == wire.RunStateExited {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Errorf("runState = %q after kill, want exited", h.sess.Snapshot().RunState)
}

// TestStrippedBinaryAgainstRealGDB is finding 5 end to end: no symbols, so
// breakpoints by line fail and frames have no source — and none of it may
// break the session.
func TestStrippedBinaryAgainstRealGDB(t *testing.T) {
	h := startReal(t, "nodebug")
	// Rebuild without -g and strip it.
	bin := filepath.Join(h.files.Abs(), "nodebug")
	src := filepath.Join(h.files.Abs(), "nodebug.c")
	if out, err := exec.Command("gcc", "-O0", "-o", bin, src).CombinedOutput(); err != nil {
		t.Fatalf("compiling: %v\n%s", err, out)
	}
	if out, err := exec.Command("strip", bin).CombinedOutput(); err != nil {
		t.Fatalf("strip: %v\n%s", err, out)
	}

	h.mustDo(wire.TypeExeLoad, wire.ExeLoadRequest{Path: "nodebug"})

	if _, werr := h.do(wire.TypeBpSetSource, wire.BreakpointRequest{
		Path: "nodebug.c", Line: 10,
	}); werr == nil {
		t.Log("gdb accepted a source breakpoint on a stripped binary (pending)")
	}

	// Running to completion must still work and still report an exit.
	h.mustDo(wire.TypeExecRun, wire.ExecRequest{})
	exited := h.rec.wait(t, wire.EventExited).(wire.Exited)
	if exited.RunState != wire.RunStateExited {
		t.Errorf("runState = %q", exited.RunState)
	}
}

// TestReloadRestoresState is the M3 done-criterion about the browser reload,
// expressed at the layer that actually has to make it true: a fresh snapshot,
// taken as a new connection would, must describe the whole stopped session.
func TestReloadRestoresState(t *testing.T) {
	h := startReal(t, "hello")
	h.mustDo(wire.TypeExeLoad, wire.ExeLoadRequest{Path: "hello"})
	h.mustDo(wire.TypeBpSetSource, wire.BreakpointRequest{Path: "hello.c", Line: lineMainInit})
	h.mustDo(wire.TypeExecRun, wire.ExecRequest{})
	h.rec.wait(t, wire.EventStopped)

	snap := h.sess.Snapshot()
	if snap.RunState != wire.RunStateStopped {
		t.Fatalf("runState = %q", snap.RunState)
	}
	if snap.ExePath != "hello" {
		t.Errorf("exePath = %q", snap.ExePath)
	}
	if len(snap.Breakpoints) != 1 {
		t.Errorf("breakpoints = %d, want 1", len(snap.Breakpoints))
	}
	if len(snap.Frames) == 0 || snap.Frames[0].Func != "main" {
		t.Errorf("frames = %+v", snap.Frames)
	}
	if len(snap.Locals) == 0 {
		t.Error("no locals; a reloaded page would show an empty variables panel")
	}
	if snap.Selection == nil {
		t.Fatal("no selection")
	}
	if snap.Selection.StopSeq != snap.StopSeq {
		t.Errorf("selection stopSeq %d != snapshot stopSeq %d",
			snap.Selection.StopSeq, snap.StopSeq)
	}
	if snap.GDBVersion == "" {
		t.Log("gdbVersion is empty (set by cmd, not by the session)")
	}
}

// TestSwitchProgramWhileStopped is the ordinary "debug something else" flow:
// load a second program without restarting the server.
//
// It is subtler than it looks. -file-exec-and-symbols discards the live
// inferior and gdb announces that with =thread-group-exited — which arrives
// *after* exe.load has already reset the state, so a naive handler leaves the
// UI reading "exited" when it should read "no program".
func TestSwitchProgramWhileStopped(t *testing.T) {
	h := startReal(t, "hello")

	// Build a second program in the same project.
	second := filepath.Join(h.files.Abs(), "structs")
	src := filepath.Join(testutil.RepoRoot(t), "testdata", "fixtures", "structs.c")
	body, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	dstSrc := filepath.Join(h.files.Abs(), "structs.c")
	if err := os.WriteFile(dstSrc, body, 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("gcc", "-g", "-O0", "-o", second, dstSrc).CombinedOutput(); err != nil {
		t.Fatalf("compiling structs: %v\n%s", err, out)
	}

	// Get the first program to a stop.
	h.mustDo(wire.TypeExeLoad, wire.ExeLoadRequest{Path: "hello"})
	h.mustDo(wire.TypeBpSetSource, wire.BreakpointRequest{Path: "hello.c", Line: lineMainInit})
	h.mustDo(wire.TypeExecRun, wire.ExecRequest{})
	h.rec.wait(t, wire.EventStopped)

	// Switch. This must be allowed while stopped.
	h.mustDo(wire.TypeExeLoad, wire.ExeLoadRequest{Path: "structs"})

	// Let any trailing notifications land.
	time.Sleep(500 * time.Millisecond)

	snap := h.sess.Snapshot()
	if snap.ExePath != "structs" {
		t.Errorf("exePath = %q, want structs", snap.ExePath)
	}
	if snap.RunState != wire.RunStateNoProgram {
		t.Errorf("runState = %q, want noProgram: swapping the file is not the "+
			"program exiting, and the UI should not claim it is", snap.RunState)
	}
	if len(snap.Frames) != 0 || len(snap.Locals) != 0 {
		t.Error("the previous program's stack or locals survived the switch")
	}

	// Breakpoints survive the switch, because gdb keeps them and re-resolves
	// against the new symbols. One from the old program cannot resolve, so it
	// comes back pending rather than silently vanishing — which is the honest
	// answer: the user set it, and it is still set.
	bps := snap.Breakpoints
	if len(bps) != 1 {
		t.Fatalf("breakpoints after the switch = %d, want the one from before: %+v",
			len(bps), bps)
	}
	t.Logf("carried-over breakpoint: %+v", bps[0])

	// And the new program must be runnable.
	h.mustDo(wire.TypeExecRun, wire.ExecRequest{})
	h.rec.wait(t, wire.EventExited)
}
