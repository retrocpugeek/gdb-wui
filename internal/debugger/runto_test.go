package debugger_test

import (
	"strings"
	"testing"

	"github.com/retrocpugeek/gdb-wui/internal/gdbfake"
	"github.com/retrocpugeek/gdb-wui/internal/wire"
)

// exec.runTo is two gdb commands that have to happen together: a temporary
// breakpoint, then a resume. These tests are about the pair — that the second
// one follows, that it is the right one for the state the program is in, and
// that a failure between them does not leave the first behind.

const runToBreak = `
> -break-insert -t "PROJECT/main.c:12"
< ^done,bkpt={number="2",type="breakpoint",disp="del",enb="y",addr="0x1155",func="main",file="main.c",fullname="PROJECT/main.c",line="12"}
`

func TestRunToBreaksThenContinues(t *testing.T) {
	h := start(t, loadTranscript+stopTranscript+runToBreak+`
> -exec-continue --thread 1
< ^running
< *running,thread-id="all"
`)
	h.load()
	h.mustDo(wire.TypeExecRun, wire.ExecRequest{})
	h.rec.wait(t, wire.EventStopped)

	h.mustDo(wire.TypeExecRunTo, wire.ExecRunToRequest{Path: "main.c", Line: 12})

	if got := h.sess.Snapshot().RunState; got != wire.RunStateRunning {
		t.Errorf("runState = %q after a run-to, want running", got)
	}
	// The breakpoint is ours while it lasts, which is what keeps it visible:
	// the mirror hides temporary breakpoints gdb invented for itself.
	var found bool
	for _, bp := range h.sess.Snapshot().Breakpoints {
		if bp.Number == 2 && bp.Temporary {
			found = true
		}
	}
	if !found {
		t.Errorf("the temporary breakpoint is not in the mirror: %+v",
			h.sess.Snapshot().Breakpoints)
	}
}

// Not made pending, unlike a breakpoint someone is placing. A run-to whose
// location never resolves would otherwise run the program to completion
// instead of saying it could not find the place.
func TestRunToDoesNotMakeAPendingBreakpoint(t *testing.T) {
	h := start(t, loadTranscript+stopTranscript+runToBreak+`
> -exec-continue --thread 1
< ^running
`)
	h.load()
	h.mustDo(wire.TypeExecRun, wire.ExecRequest{})
	h.rec.wait(t, wire.EventStopped)
	h.mustDo(wire.TypeExecRunTo, wire.ExecRunToRequest{Path: "main.c", Line: 12})

	for _, cmd := range h.fake.Received() {
		if strings.HasPrefix(cmd, "-break-insert") && strings.Contains(cmd, "-f") {
			t.Errorf("run-to inserted a pending breakpoint: %s", cmd)
		}
	}
}

// With nothing running, run-to starts the program. That is what makes it a way
// to begin a session at a line rather than at main.
func TestRunToRunsWhenNothingIsRunning(t *testing.T) {
	h := start(t, loadTranscript+`
> -break-insert -t "*0x401136"
< ^done,bkpt={number="2",type="breakpoint",disp="del",enb="y",addr="0x401136"}
>? -inferior-tty-set*
< ^done
> -exec-run
< ^running
`)
	h.load()
	h.mustDo(wire.TypeExecRunTo, wire.ExecRunToRequest{Location: "0x401136"})

	if got := h.sess.Snapshot().RunState; got != wire.RunStateRunning {
		t.Errorf("runState = %q, want running", got)
	}
}

// A resume gdb refused must not leave the breakpoint armed. The user asked to
// run somewhere and did not get there; a breakpoint they never placed, left
// behind at a line they were only reading, is the wrong thing to find later.
func TestRunToRemovesItsBreakpointWhenTheResumeFails(t *testing.T) {
	h := start(t, loadTranscript+stopTranscript+runToBreak+`
> -exec-continue --thread 1
< ^error,msg="Cannot execute this command while the target is running."
> -break-delete 2
< ^done
`)
	h.load()
	h.mustDo(wire.TypeExecRun, wire.ExecRequest{})
	h.rec.wait(t, wire.EventStopped)

	if _, werr := h.do(wire.TypeExecRunTo, wire.ExecRunToRequest{Path: "main.c", Line: 12}); werr == nil {
		t.Fatal("a refused resume was reported as success")
	}
	for _, bp := range h.sess.Snapshot().Breakpoints {
		if bp.Number == 2 {
			t.Errorf("the run-to breakpoint survived a failed resume: %+v", bp)
		}
	}
}

// One place, named one way. Both at once is a client bug worth reporting as
// one, because guessing which the user meant would sometimes run to the wrong
// place and say nothing.
func TestRunToNeedsExactlyOnePlace(t *testing.T) {
	h := start(t, loadTranscript, gdbfake.WithDefaultDone())
	h.load()

	for _, req := range []wire.ExecRunToRequest{
		{},
		{Path: "main.c", Line: 12, Location: "0x401136"},
		{Path: "main.c"},
	} {
		if _, werr := h.do(wire.TypeExecRunTo, req); werr == nil ||
			werr.Code != wire.CodeBadRequest {
			t.Errorf("%+v: got %v, want bad_request", req, werr)
		}
	}
}
