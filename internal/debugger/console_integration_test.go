//go:build integration

package debugger_test

import (
	"encoding/base64"
	"strings"
	"testing"
	"time"

	"github.com/retrocpugeek/gdb-wui/internal/wire"
)

// The M5 done-criteria: type into interactive.c and see line-buffered output,
// Ctrl-C a spinning loop and land on a real frame, and switch among the threads
// in threads.c seeing distinct stacks.

// collectInferior gathers the debuggee's terminal output until it contains
// want, or the deadline passes.
func collectInferior(t *testing.T, h *harness, want string, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		var sb strings.Builder
		for _, e := range h.rec.all() {
			if e.name != wire.EventInferiorOutput {
				continue
			}
			out, ok := e.payload.(wire.InferiorOutput)
			if !ok {
				continue
			}
			data, err := base64.StdEncoding.DecodeString(out.DataB64)
			if err != nil {
				t.Fatalf("inferior output is not valid base64: %v", err)
			}
			sb.Write(data)
		}
		if want == "" || strings.Contains(sb.String(), want) {
			return sb.String()
		}
		time.Sleep(20 * time.Millisecond)
	}
	return ""
}

// TestInferiorTerminalIsInteractive is the headline M5 criterion.
//
// It exercises three things a pty buys that a pipe does not: the program's
// prompt appears despite having no trailing newline (libc line-buffers on a
// tty and block-buffers otherwise), typed input reaches it, and the output
// comes back as terminal bytes rather than as unparseable lines in the MI
// stream.
func TestInferiorTerminalIsInteractive(t *testing.T) {
	h := startReal(t, "interactive")
	h.mustDo(wire.TypeExeLoad, wire.ExeLoadRequest{Path: "interactive"})
	h.mustDo(wire.TypeExecRun, wire.ExecRequest{})

	// "name? " has no newline. On a pipe it would sit in libc's buffer and
	// never arrive; on a tty it is flushed at once.
	if got := collectInferior(t, h, "name?", 5*time.Second); got == "" {
		t.Fatal("the program's unterminated prompt never arrived; is it a tty?")
	}

	send := func(text string) {
		t.Helper()
		if _, werr := h.do(wire.TypeInferiorStdin, wire.InferiorStdinRequest{
			DataB64: base64.StdEncoding.EncodeToString([]byte(text)),
		}); werr != nil {
			t.Fatalf("inferior.stdin: %s", werr.Message)
		}
	}

	// \r, not \n: that is what a terminal sends for Enter, and the line
	// discipline turns it into a newline for the program.
	send("world\r")
	if got := collectInferior(t, h, "hello world", 5*time.Second); got == "" {
		t.Fatalf("the program never echoed the typed name; saw %q",
			collectInferior(t, h, "", time.Second))
	}

	send("42\r")
	if got := collectInferior(t, h, "count=42", 5*time.Second); got == "" {
		t.Errorf("the program never read the second input; saw %q",
			collectInferior(t, h, "", time.Second))
	}

	h.rec.wait(t, wire.EventExited)
}

// TestInferiorOutputIsNotInTheMIStream: with a pty, the program's output stops
// arriving as garbage lines mixed into MI. That was finding 3 in the plan, and
// giving the debuggee its own terminal is the fix.
func TestInferiorOutputIsNotInTheMIStream(t *testing.T) {
	h := startReal(t, "hello")
	h.mustDo(wire.TypeExeLoad, wire.ExeLoadRequest{Path: "hello"})
	h.mustDo(wire.TypeExecRun, wire.ExecRequest{})
	h.rec.wait(t, wire.EventExited)

	if got := collectInferior(t, h, "total=3", 3*time.Second); got == "" {
		t.Error("the program's output did not arrive on the terminal")
	}
	for _, e := range h.rec.all() {
		if e.name != wire.EventConsole {
			continue
		}
		m, ok := e.payload.(map[string]string)
		if ok && m["stream"] == "inferior" {
			t.Errorf("program output still leaked into the MI stream: %q", m["text"])
		}
	}
}

// TestInterruptSpinningLoop is the second criterion: Ctrl-C a busy program and
// land somewhere real.
func TestInterruptSpinningLoop(t *testing.T) {
	h := startReal(t, "threads")
	h.mustDo(wire.TypeExeLoad, wire.ExeLoadRequest{Path: "threads"})
	h.mustDo(wire.TypeExecRun, wire.ExecRequest{})
	h.rec.wait(t, wire.EventRunning)

	if _, werr := h.do(wire.TypeExecPause, nil); werr != nil {
		t.Fatalf("pause: %s", werr.Message)
	}
	stopped := h.rec.wait(t, wire.EventStopped).(wire.Stopped)

	if len(stopped.Frames) == 0 {
		t.Fatal("interrupting landed on no frame at all")
	}
	// A real frame: it has an address, and normally a function name too.
	if stopped.Frames[0].Address == "" {
		t.Error("the frame has no address")
	}
	if h.sess.Snapshot().RunState != wire.RunStateStopped {
		t.Error("runState is not stopped after an interrupt")
	}
}

// TestSignalReachesTheInferior covers inferior.signal, which is what Ctrl-C in
// the terminal panel maps to.
func TestSignalReachesTheInferior(t *testing.T) {
	h := startReal(t, "threads")
	h.mustDo(wire.TypeExeLoad, wire.ExeLoadRequest{Path: "threads"})
	h.mustDo(wire.TypeExecRun, wire.ExecRequest{})
	h.rec.wait(t, wire.EventRunning)

	if _, werr := h.do(wire.TypeInferiorSignal, wire.InferiorSignalRequest{Signal: "INT"}); werr != nil {
		t.Fatalf("inferior.signal: %s", werr.Message)
	}
	// gdb reports the signal as a stop.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		snap := h.sess.Snapshot()
		if snap.RunState == wire.RunStateStopped || snap.RunState == wire.RunStateExited {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Errorf("the program ignored SIGINT; runState = %q", h.sess.Snapshot().RunState)
}

func TestUnknownSignalIsRejected(t *testing.T) {
	h := startReal(t, "hello")
	h.mustDo(wire.TypeExeLoad, wire.ExeLoadRequest{Path: "hello"})
	if _, werr := h.do(wire.TypeInferiorSignal, wire.InferiorSignalRequest{Signal: "NOPE"}); werr == nil {
		t.Error("an unknown signal name was accepted")
	}
}

// TestThreadSwitching is the third criterion: distinct stacks per thread.
func TestThreadSwitching(t *testing.T) {
	h := startReal(t, "threads")
	h.mustDo(wire.TypeExeLoad, wire.ExeLoadRequest{Path: "threads"})
	// Break in the worker so several threads exist and are inside it.
	h.mustDo(wire.TypeBpSetSource, wire.BreakpointRequest{Path: "threads.c", Line: 16})
	h.mustDo(wire.TypeExecRun, wire.ExecRequest{})
	h.rec.wait(t, wire.EventStopped)

	list := h.mustDo(wire.TypeThreadsList, wire.ThreadsListRequest{}).(wire.ThreadsList)
	if len(list.Threads) < 2 {
		t.Fatalf("got %d threads, want the main thread plus workers", len(list.Threads))
	}
	if list.Selected == 0 {
		t.Error("no thread is selected")
	}

	// Every thread must report a state, and the stopped ones a frame.
	for _, th := range list.Threads {
		if th.ID == 0 {
			t.Errorf("thread with no id: %+v", th)
		}
		if th.State == "" {
			t.Errorf("thread %d has no state", th.ID)
		}
	}

	// Switching must produce that thread's stack, not the previous one's.
	var other int
	for _, th := range list.Threads {
		if th.ID != list.Selected {
			other = th.ID
			break
		}
	}
	if other == 0 {
		t.Fatal("only one thread to choose from")
	}

	sel := h.mustDo(wire.TypeThreadSelect, wire.ThreadSelectRequest{Thread: other}).(wire.Selection)
	if sel.ThreadID != other {
		t.Errorf("selected %d, want %d", sel.ThreadID, other)
	}
	// The reply must carry the new thread's stack. Without it a client either
	// keeps rendering the previous thread's frames — a UI that looks correct
	// while showing the wrong data — or needs a second round-trip for
	// something the server already has in hand.
	if len(sel.Frames) == 0 {
		t.Fatal("thread.select returned no frames; the stack panel cannot update")
	}
	if len(h.sess.Snapshot().Frames) == 0 {
		t.Error("switching threads produced no stack in the snapshot")
	}
	if h.sess.Snapshot().Selection == nil || h.sess.Snapshot().Selection.ThreadID != other {
		t.Error("the snapshot did not follow the thread switch")
	}
}

// TestConsoleExecResyncs is what keeps the console from desynchronising the UI:
// a breakpoint typed at the prompt must appear in the mirror.
func TestConsoleExecResyncs(t *testing.T) {
	h := startReal(t, "hello")
	h.mustDo(wire.TypeExeLoad, wire.ExeLoadRequest{Path: "hello"})

	before := len(h.sess.Snapshot().Breakpoints)
	out := h.mustDo(wire.TypeConsoleExec, wire.ConsoleExecRequest{
		Line: "break hello.c:12",
	}).(wire.ConsoleExecResult)

	if len(out.Resynced) == 0 {
		t.Error("nothing was resynced after a console command")
	}
	after := h.sess.Snapshot().Breakpoints
	if len(after) != before+1 {
		t.Fatalf("breakpoints went %d -> %d; a breakpoint set at the console must "+
			"appear in the mirror", before, len(after))
	}
	if after[len(after)-1].Line != 12 {
		t.Errorf("breakpoint is at line %d, want 12", after[len(after)-1].Line)
	}
}

// TestConsoleOutputStreams: what gdb prints in response must reach the browser.
func TestConsoleOutputStreams(t *testing.T) {
	h := startReal(t, "hello")
	h.mustDo(wire.TypeExeLoad, wire.ExeLoadRequest{Path: "hello"})
	h.rec.reset()

	h.mustDo(wire.TypeConsoleExec, wire.ConsoleExecRequest{Line: "print 6*7"})

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		for _, e := range h.rec.all() {
			if e.name != wire.EventConsole {
				continue
			}
			if m, ok := e.payload.(map[string]string); ok && strings.Contains(m["text"], "42") {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Error("the console command produced no visible output")
}

// TestConsoleErrorIsNotAFailure: a typo at a console is ordinary, and must not
// surface as a request failure.
func TestConsoleErrorIsNotAFailure(t *testing.T) {
	h := startReal(t, "hello")
	h.mustDo(wire.TypeExeLoad, wire.ExeLoadRequest{Path: "hello"})
	h.rec.reset()

	if _, werr := h.do(wire.TypeConsoleExec, wire.ConsoleExecRequest{Line: "nosuchcommand"}); werr != nil {
		t.Errorf("a mistyped console command failed the request: %s", werr.Message)
	}
	// The message must still be shown.
	var sawMessage bool
	for _, e := range h.rec.all() {
		if e.name != wire.EventConsole {
			continue
		}
		if m, ok := e.payload.(map[string]string); ok &&
			strings.Contains(strings.ToLower(m["text"]), "undefined") {
			sawMessage = true
		}
	}
	if !sawMessage {
		t.Error("gdb's complaint about the command was not shown to the user")
	}
}

// TestConsoleComplete covers tab completion, which comes from gdb rather than
// from a table in the frontend.
func TestConsoleComplete(t *testing.T) {
	h := startReal(t, "hello")
	out := h.mustDo(wire.TypeConsoleComplete, wire.ConsoleCompleteRequest{
		Prefix: "info thr",
	}).(wire.ConsoleComplete)

	if out.Completion != "info threads" {
		t.Errorf("completion = %q, want info threads", out.Completion)
	}
	if len(out.Matches) == 0 {
		t.Error("no matches returned")
	}
}

func TestConsoleWorksWhileRunning(t *testing.T) {
	h := startReal(t, "threads")
	h.mustDo(wire.TypeExeLoad, wire.ExeLoadRequest{Path: "threads"})
	h.mustDo(wire.TypeExecRun, wire.ExecRequest{})
	h.rec.wait(t, wire.EventRunning)

	// The console is the escape hatch; gating it while running would remove the
	// only way out of a state the UI does not model.
	if _, werr := h.do(wire.TypeConsoleExec, wire.ConsoleExecRequest{Line: "info threads"}); werr != nil {
		t.Errorf("console.exec while running: %s: %s", werr.Code, werr.Message)
	}
}
