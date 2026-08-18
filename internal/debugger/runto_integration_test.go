//go:build integration

package debugger_test

import (
	"testing"
	"time"

	"github.com/retrocpugeek/gdb-wui/internal/wire"
)

// Running to a place, against a real gdb.
//
// The unit tests next door prove the command pair; these prove the promise the
// menu item makes — that the program stops where it was told and nothing is
// left behind afterwards.

func TestRunToStopsThereAndCleansUp(t *testing.T) {
	h := startReal(t, "hello")
	h.mustDo(wire.TypeExeLoad, wire.ExeLoadRequest{Path: "hello"})

	// From a program that has not started: run-to has to run it.
	h.mustDo(wire.TypeExecRunTo, wire.ExecRunToRequest{
		Path: "hello.c", Line: lineMainLoop,
	})
	stopped := h.rec.wait(t, wire.EventStopped).(wire.Stopped)
	if len(stopped.Frames) == 0 {
		t.Fatalf("no frames at the stop: %+v", stopped)
	}
	if got := stopped.Frames[0].Source.Line; got != lineMainLoop {
		t.Errorf("stopped on line %d, want %d", got, lineMainLoop)
	}

	// gdb deletes a temporary breakpoint when it is hit, and =breakpoint-deleted
	// takes it out of the mirror. A run-to that left one behind would put a
	// marker in the gutter of a line the user was only reading.
	waitForNoBreakpoints(t, h, "the first run-to")

	// And from a stop: this one is a continue rather than a run.
	h.mustDo(wire.TypeExecRunTo, wire.ExecRunToRequest{
		Path: "hello.c", Line: lineAddSum, StopSeq: stopped.StopSeq,
	})
	next := h.rec.wait(t, wire.EventStopped).(wire.Stopped)
	if len(next.Frames) == 0 {
		t.Fatalf("no frames at the second stop: %+v", next)
	}
	if got := next.Frames[0].Func; got != "add" {
		t.Errorf("stopped in %q, want add", got)
	}
	if got := next.Frames[0].Source.Line; got != lineAddSum {
		t.Errorf("stopped on line %d, want %d", got, lineAddSum)
	}
	waitForNoBreakpoints(t, h, "the second run-to")
}

// waitForNoBreakpoints waits for the mirror to empty.
//
// Polled rather than read once: the deletion is a separate =breakpoint-deleted
// notification, and gdb sends it after the *stopped that reports the hit. CI
// caught the difference — locally the deletion had always landed by the time
// the stop was handled, and on a loaded runner it had not.
func waitForNoBreakpoints(t *testing.T, h *harness, what string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		bps := h.sess.Snapshot().Breakpoints
		if len(bps) == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Errorf("breakpoints left after %s: %+v", what, bps)
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// A place gdb cannot resolve is an error, not a run to completion. This is the
// difference from an ordinary breakpoint, which is made pending on purpose.
func TestRunToRefusesAPlaceItCannotFind(t *testing.T) {
	h := startReal(t, "hello")
	h.mustDo(wire.TypeExeLoad, wire.ExeLoadRequest{Path: "hello"})

	if _, werr := h.do(wire.TypeExecRunTo, wire.ExecRunToRequest{
		Location: "no_such_function_anywhere",
	}); werr == nil {
		t.Fatal("run-to accepted a location gdb cannot resolve")
	}
	if got := h.sess.Snapshot().RunState; got != wire.RunStateNoProgram {
		t.Errorf("runState = %q; the program was started anyway", got)
	}
	if bps := h.sess.Snapshot().Breakpoints; len(bps) != 0 {
		t.Errorf("a breakpoint was left behind: %+v", bps)
	}
}
