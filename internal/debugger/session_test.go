package debugger_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/retrocpugeek/gdb-wui/internal/debugger"
	"github.com/retrocpugeek/gdb-wui/internal/gdbfake"
	"github.com/retrocpugeek/gdb-wui/internal/mi"
	"github.com/retrocpugeek/gdb-wui/internal/srcfs"
	"github.com/retrocpugeek/gdb-wui/internal/testutil"
	"github.com/retrocpugeek/gdb-wui/internal/wire"
)

// recorder collects broadcast events.
type recorder struct {
	mu     sync.Mutex
	events []recorded
	ch     chan recorded
	// probe is set by tests that need to observe state at the instant an event
	// is announced. Atomic because it is written from the test goroutine and
	// read from the actor's.
	probe atomic.Pointer[func(event string)]
}

type recorded struct {
	name    string
	payload any
}

func newRecorder() *recorder { return &recorder{ch: make(chan recorded, 512)} }

// probe runs inside Broadcast, on the actor goroutine, at the exact instant an
// event is announced. That is the only place from which the snapshot-vs-event
// ordering can be observed without a race: afterwards the actor has moved on
// and published anyway, which is why asserting from the test goroutine
// reproduces the bug about one run in fifty rather than every time.
func (r *recorder) setProbe(f func(event string)) {
	r.probe.Store(&f)
}

func (r *recorder) Broadcast(event string, payload any) {
	if f := r.probe.Load(); f != nil {
		(*f)(event)
	}
	r.mu.Lock()
	r.events = append(r.events, recorded{event, payload})
	r.mu.Unlock()
	select {
	case r.ch <- recorded{event, payload}:
	default:
	}
}

func (r *recorder) all() []recorded {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]recorded(nil), r.events...)
}

// reset forgets everything recorded so far.
//
// Setup broadcasts too — loading a program reconciles breakpoints and announces
// an empty list — so a test asserting on "the next breakpointsChanged" has to
// say where "next" starts, or it matches the setup's event and passes or fails
// for the wrong reason.
func (r *recorder) reset() {
	r.mu.Lock()
	r.events = nil
	r.mu.Unlock()
	for {
		select {
		case <-r.ch:
		default:
			return
		}
	}
}

func (r *recorder) count(name string) int {
	var n int
	for _, e := range r.all() {
		if e.name == name {
			n++
		}
	}
	return n
}

// wait blocks for the next event with the given name.
func (r *recorder) wait(t *testing.T, name string) any {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case e := <-r.ch:
			if e.name == name {
				return e.payload
			}
		case <-deadline:
			var seen []string
			for _, e := range r.all() {
				seen = append(seen, e.name)
			}
			t.Fatalf("timed out waiting for %q; saw %v", name, seen)
		}
	}
}

// testLogf is t.Logf that falls silent once the test ends: gdb-side goroutines
// outlive the test body, and logging after tRunner marks the test done is a
// race inside the testing package.
func testLogf(t *testing.T) func(string, ...any) {
	var mu sync.Mutex
	finished := false
	t.Cleanup(func() {
		mu.Lock()
		defer mu.Unlock()
		finished = true
	})
	return func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		if finished {
			return
		}
		t.Logf(format, args...)
	}
}

// project builds a tiny project with a fake ELF so exe.load can be exercised
// without a compiler.
func project(t *testing.T) *srcfs.FS {
	t.Helper()
	dir := t.TempDir()
	write := func(name string, content []byte) {
		if err := os.WriteFile(filepath.Join(dir, name), content, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("main.c", []byte("int main(void){return 0;}\n"))
	write("prog", append([]byte{0x7f, 'E', 'L', 'F'}, make([]byte, 64)...))
	write("notelf", []byte("#!/bin/sh\necho hi\n"))

	f, err := srcfs.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	return f
}

// harness wires a session to a scripted fake gdb.
type harness struct {
	t     *testing.T
	sess  *debugger.Session
	fake  *gdbfake.Fake
	rec   *recorder
	files *srcfs.FS
}

// start wires a session to a scripted fake gdb.
//
// The transcript may use PROJECT wherever the project's absolute path appears;
// it is substituted here. Commands carry absolute paths because gdb is a
// separate process, and the temp directory is not known until the fixture is
// built.
func start(t *testing.T, transcript string, opts ...gdbfake.Option) *harness {
	t.Helper()
	rec := newRecorder()
	files := project(t)
	transcript = strings.ReplaceAll(transcript, "PROJECT", files.Abs())

	fake, err := gdbfake.StartTranscript(transcript, append(opts, gdbfake.WithDefaultDone())...)
	if err != nil {
		t.Fatalf("transcript: %v", err)
	}

	sess, err := debugger.New(t.Context(), debugger.Config{
		MI: mi.Options{
			// A non-nil empty handshake: transcripts describe only the
			// dialogue under test.
			Handshake: []string{},
			Stdin:     fake.ClientStdin,
			Stdout:    fake.ClientStdout,
			Logf:      testLogf(t),
		},
		Files:          files,
		Events:         rec,
		Logf:           testLogf(t),
		Version:        "test",
		CommandTimeout: 3 * time.Second,
	})
	if err != nil {
		t.Fatalf("debugger.New: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = sess.Close(ctx)
		fake.Close()
	})
	return &harness{t: t, sess: sess, fake: fake, rec: rec, files: files}
}

// do issues a request through the session.
func (h *harness) do(typ string, payload any) (any, *wire.Error) {
	h.t.Helper()
	var raw json.RawMessage
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			h.t.Fatal(err)
		}
		raw = b
	}
	ctx, cancel := context.WithTimeout(h.t.Context(), 5*time.Second)
	defer cancel()
	return h.sess.Handle(ctx, wire.Request{ID: 1, Type: typ, Payload: raw})
}

// mustDo fails the test if the request errors.
func (h *harness) mustDo(typ string, payload any) any {
	h.t.Helper()
	out, werr := h.do(typ, payload)
	if werr != nil {
		h.t.Fatalf("%s: %s: %s", typ, werr.Code, werr.Message)
	}
	return out
}

// loadProgram runs the exe.load dialogue the other tests need first.
const loadTranscript = `
> -file-exec-and-symbols*
< ^done
> -environment-cd*
< ^done
> -break-list
< ^done,BreakpointTable={nr_rows="0",nr_cols="6",body=[]}
`

func (h *harness) load() {
	h.t.Helper()
	h.mustDo(wire.TypeExeLoad, wire.ExeLoadRequest{Path: "prog"})
}

func TestExeLoadValidatesELF(t *testing.T) {
	h := start(t, ``)
	_, werr := h.do(wire.TypeExeLoad, wire.ExeLoadRequest{Path: "notelf"})
	if werr == nil {
		t.Fatal("a shell script was accepted as a program")
	}
	if werr.Code != wire.CodeBadRequest {
		t.Errorf("code = %q, want bad_request", werr.Code)
	}
	if !strings.Contains(werr.Message, "not an ELF") {
		t.Errorf("message = %q; it should say what is wrong", werr.Message)
	}
}

func TestExeLoadRejectsTraversal(t *testing.T) {
	h := start(t, ``)
	_, werr := h.do(wire.TypeExeLoad, wire.ExeLoadRequest{Path: "../../bin/ls"})
	if werr == nil {
		t.Fatal("a path outside the project was accepted")
	}
	if werr.Code != wire.CodePathDenied && werr.Code != wire.CodeNotFound {
		t.Errorf("code = %q, want path_denied or not_found", werr.Code)
	}
}

func TestExeLoadSucceeds(t *testing.T) {
	h := start(t, loadTranscript)
	h.load()

	payload := h.rec.wait(t, wire.EventExeLoaded)
	loaded, ok := payload.(wire.ExeLoaded)
	if !ok {
		t.Fatalf("payload is %T", payload)
	}
	if loaded.Path != "prog" {
		t.Errorf("path = %q", loaded.Path)
	}
	if got := h.sess.Snapshot().ExePath; got != "prog" {
		t.Errorf("snapshot exePath = %q", got)
	}
}

// TestRunStateGate is the contract that replaces gdb's cryptic errors. It must
// refuse without sending anything, which is what makes it faster than gdb's own
// rejection as well as clearer.
func TestRunStateGate(t *testing.T) {
	h := start(t, loadTranscript+`
# the inferior terminal is allocated before the first run only
>? -inferior-tty-set*
< ^done
> -exec-run
< ^running
< *running,thread-id="all"
`)
	h.load()
	h.mustDo(wire.TypeExecRun, wire.ExecRequest{})
	h.rec.wait(t, wire.EventRunning)

	for _, typ := range []string{
		wire.TypeExecContinue, wire.TypeExecStep, wire.TypeExecNext,
		wire.TypeExecFinish, wire.TypeStackList, wire.TypeFrameSelect,
	} {
		_, werr := h.do(typ, wire.ExecRequest{})
		if werr == nil {
			t.Errorf("%s was accepted while the inferior is running", typ)
			continue
		}
		if werr.Code != wire.CodeBusy {
			t.Errorf("%s: code = %q, want busy", typ, werr.Code)
		}
	}

	// Pause and kill must remain available: they are the only way out.
	for _, typ := range []string{wire.TypeExecPause, wire.TypeBpList} {
		if _, werr := h.do(typ, wire.ExecRequest{}); werr != nil && werr.Code == wire.CodeBusy {
			t.Errorf("%s was gated as busy; it must work while running", typ)
		}
	}
}

func TestNotReadyBeforeRun(t *testing.T) {
	h := start(t, loadTranscript)
	h.load()
	for _, typ := range []string{wire.TypeExecContinue, wire.TypeExecStep, wire.TypeStackList} {
		_, werr := h.do(typ, wire.ExecRequest{})
		if werr == nil || werr.Code != wire.CodeNotReady {
			t.Errorf("%s: got %v, want not_ready", typ, werr)
		}
	}
}

// stopTranscript drives a full run-to-breakpoint, including everything the fat
// stopped event fetches.
const stopTranscript = `
# the inferior terminal is allocated before the first run only
>? -inferior-tty-set*
< ^done
> -exec-run
< ^running
< *running,thread-id="all"
< *stopped,reason="breakpoint-hit",disp="keep",bkptno="1",frame={addr="0x1149",func="main",args=[],file="main.c",fullname="PROJECT/main.c",line="1"},thread-id="1",stopped-threads="all"
> -thread-info
< ^done,threads=[{id="1",target-id="process 1",name="prog",state="stopped",core="3"}],current-thread-id="1"
> -stack-list-frames --thread 1 0 63
< ^done,stack=[frame={level="0",addr="0x1149",func="main",file="main.c",fullname="PROJECT/main.c",line="1"}]
> -stack-list-arguments --thread 1 --simple-values 0 63
< ^done,stack-args=[frame={level="0",args=[{name="argc",type="int",value="1"}]}]
> -stack-list-variables --thread 1 --frame 0 --simple-values
< ^done,variables=[{name="argc",type="int",value="1"},{name="cfg",type="struct config"}]
`

func TestFatStoppedEvent(t *testing.T) {
	h := start(t, loadTranscript+stopTranscript)
	h.load()
	h.mustDo(wire.TypeExecRun, wire.ExecRequest{})

	payload := h.rec.wait(t, wire.EventStopped)
	stopped, ok := payload.(wire.Stopped)
	if !ok {
		t.Fatalf("payload is %T", payload)
	}
	if stopped.Reason != "breakpoint-hit" {
		t.Errorf("reason = %q", stopped.Reason)
	}
	if stopped.BreakpointNumber != 1 {
		t.Errorf("bkptno = %d", stopped.BreakpointNumber)
	}
	if stopped.StopSeq != 1 {
		t.Errorf("stopSeq = %d, want 1", stopped.StopSeq)
	}
	// One event has to carry everything.
	if len(stopped.Threads) != 1 {
		t.Errorf("threads = %d, want 1", len(stopped.Threads))
	}
	if len(stopped.Frames) != 1 {
		t.Errorf("frames = %d, want 1", len(stopped.Frames))
	}
	if len(stopped.Locals) != 2 {
		t.Fatalf("locals = %d, want 2", len(stopped.Locals))
	}
	// Absence of "value" with --simple-values is the expandable signal.
	byName := map[string]wire.Variable{}
	for _, v := range stopped.Locals {
		byName[v.Name] = v
	}
	if byName["argc"].Expandable {
		t.Error("argc has a value, so it must not be marked expandable")
	}
	if !byName["cfg"].Expandable {
		t.Error("cfg has no value with --simple-values, so it must be expandable")
	}
}

// TestStopSeqRejectsStaleRequests is the double-click-step case: the second
// click carries the stop sequence from before the first step landed.
func TestStopSeqRejectsStaleRequests(t *testing.T) {
	h := start(t, loadTranscript+stopTranscript)
	h.load()
	h.mustDo(wire.TypeExecRun, wire.ExecRequest{})
	h.rec.wait(t, wire.EventStopped)

	if got := h.sess.Snapshot().StopSeq; got != 1 {
		t.Fatalf("stopSeq = %d, want 1", got)
	}
	_, werr := h.do(wire.TypeExecStep, wire.ExecRequest{StopSeq: 99})
	if werr == nil {
		t.Fatal("a request naming a stop that never happened was accepted")
	}
	if werr.Code != wire.CodeBusy {
		t.Errorf("code = %q, want busy", werr.Code)
	}

	// Zero means "I do not care", which is what a toolbar button sends.
	if _, werr := h.do(wire.TypeStackList, wire.StackListRequest{}); werr != nil {
		t.Errorf("an unguarded request was rejected: %v", werr)
	}
}

func TestSourceResolutionInsideProject(t *testing.T) {
	h := start(t, loadTranscript+stopTranscript)
	h.load()
	h.mustDo(wire.TypeExecRun, wire.ExecRequest{})
	stopped := h.rec.wait(t, wire.EventStopped).(wire.Stopped)

	if len(stopped.Frames) == 0 {
		t.Fatal("no frames")
	}
	src := stopped.Frames[0].Source
	if !src.Available {
		t.Fatalf("source not available for a file inside the project: %+v", src)
	}
	if src.Path != "main.c" {
		t.Errorf("path = %q, want the root-relative main.c", src.Path)
	}
	if src.Line != 1 {
		t.Errorf("line = %d", src.Line)
	}
}

// TestSourceOutsideProjectDegrades is the libc frame: gdb reports a path that
// does not exist here, and the UI must be told so rather than shown a blank
// pane.
func TestSourceOutsideProjectDegrades(t *testing.T) {
	h := start(t, loadTranscript+`
# the inferior terminal is allocated before the first run only
>? -inferior-tty-set*
< ^done
> -exec-run
< ^running
< *stopped,reason="signal-received",signal-name="SIGINT",frame={addr="0x7ffff7aa05ae",func="__internal_syscall_cancel",file="./nptl/cancellation.c",fullname="./nptl/./nptl/cancellation.c",line="44"},thread-id="1"
> -thread-info
< ^done,threads=[{id="1",target-id="p",state="stopped"}],current-thread-id="1"
> -stack-list-frames --thread 1 0 63
< ^done,stack=[frame={level="0",addr="0x7ffff7aa05ae",func="__internal_syscall_cancel",file="./nptl/cancellation.c",fullname="./nptl/./nptl/cancellation.c",line="44"}]
> -stack-list-arguments --thread 1 --simple-values 0 63
< ^done,stack-args=[frame={level="0",args=[]}]
> -stack-list-variables --thread 1 --frame 0 --simple-values
< ^done,variables=[]
`)
	h.load()
	h.mustDo(wire.TypeExecRun, wire.ExecRequest{})
	stopped := h.rec.wait(t, wire.EventStopped).(wire.Stopped)

	src := stopped.Frames[0].Source
	if src.Available {
		t.Error("a libc path was reported as available")
	}
	if src.GDBPath == "" {
		t.Error("GDBPath is empty; the UI needs it to offer a locate-source action")
	}
	if stopped.Signal != "SIGINT" {
		t.Errorf("signal = %q", stopped.Signal)
	}
}

// TestStrippedBinaryFrameSurvives is finding 5 at the domain layer: no file, no
// line, func="??" — addr is the only identity, and nothing may crash.
func TestStrippedBinaryFrameSurvives(t *testing.T) {
	h := start(t, loadTranscript+`
# the inferior terminal is allocated before the first run only
>? -inferior-tty-set*
< ^done
> -exec-run
< ^running
< *stopped,reason="end-stepping-range",frame={addr="0x555555555129",func="??"},thread-id="1"
> -thread-info
< ^done,threads=[{id="1",target-id="p",state="stopped"}],current-thread-id="1"
> -stack-list-frames --thread 1 0 63
< ^done,stack=[frame={level="0",addr="0x555555555129",func="??"},frame={level="1",addr="0x7ffff7829d90"}]
> -stack-list-arguments --thread 1 --simple-values 0 63
< ^error,msg="No symbol table info available."
> -stack-list-variables --thread 1 --frame 0 --simple-values
< ^error,msg="No symbol table info available."
`)
	h.load()
	h.mustDo(wire.TypeExecRun, wire.ExecRequest{})
	stopped := h.rec.wait(t, wire.EventStopped).(wire.Stopped)

	if len(stopped.Frames) != 2 {
		t.Fatalf("frames = %d, want 2", len(stopped.Frames))
	}
	if stopped.Frames[0].Address == "" {
		t.Error("address is empty; it is the only guaranteed frame identity")
	}
	if stopped.Frames[0].Source.Available {
		t.Error("a frame with no file was reported as having source")
	}
	// A locals command that errors must not lose the whole stop event.
	if stopped.Reason != "end-stepping-range" {
		t.Errorf("reason = %q", stopped.Reason)
	}
}

func TestExitedClearsState(t *testing.T) {
	h := start(t, loadTranscript+`
# the inferior terminal is allocated before the first run only
>? -inferior-tty-set*
< ^done
> -exec-run
< ^running
< *stopped,reason="exited-normally"
`)
	h.load()
	h.mustDo(wire.TypeExecRun, wire.ExecRequest{})

	payload := h.rec.wait(t, wire.EventExited)
	exited, ok := payload.(wire.Exited)
	if !ok {
		t.Fatalf("payload is %T", payload)
	}
	if exited.RunState != wire.RunStateExited {
		t.Errorf("runState = %q", exited.RunState)
	}
	if exited.ExitCode == nil || *exited.ExitCode != 0 {
		t.Errorf("exitCode = %v, want 0", exited.ExitCode)
	}
	snap := h.sess.Snapshot()
	if len(snap.Frames) != 0 || len(snap.Locals) != 0 {
		t.Error("frames or locals survived the program exiting")
	}
}

func TestExitCodeIsOctal(t *testing.T) {
	h := start(t, loadTranscript+`
# the inferior terminal is allocated before the first run only
>? -inferior-tty-set*
< ^done
> -exec-run
< ^running
< *stopped,reason="exited",exit-code="012"
`)
	h.load()
	h.mustDo(wire.TypeExecRun, wire.ExecRequest{})
	exited := h.rec.wait(t, wire.EventExited).(wire.Exited)
	if exited.ExitCode == nil || *exited.ExitCode != 10 {
		t.Errorf("exitCode = %v, want 10 (gdb reports it in octal)", exited.ExitCode)
	}
}

func TestBreakpointMirror(t *testing.T) {
	h := start(t, loadTranscript+`
> -break-insert -f "PROJECT/main.c:1"
< ^done,bkpt={number="1",type="breakpoint",disp="keep",enabled="y",addr="0x1149",func="main",file="main.c",fullname="PROJECT/main.c",line="1",times="0",original-location="main.c:1"}
`)
	h.load()
	h.rec.reset()
	out := h.mustDo(wire.TypeBpSetSource, wire.BreakpointRequest{Path: "main.c", Line: 1})
	bp, ok := out.(wire.Breakpoint)
	if !ok {
		t.Fatalf("payload is %T", out)
	}
	if bp.Number != 1 || bp.Line != 1 {
		t.Errorf("breakpoint = %+v", bp)
	}
	if bp.Path != "main.c" {
		t.Errorf("path = %q, want the root-relative main.c", bp.Path)
	}
	if bp.Pending {
		t.Error("a resolved breakpoint is marked pending")
	}

	list := h.rec.wait(t, wire.EventBreakpointsChanged).(wire.BreakpointList)
	if len(list.Breakpoints) != 1 {
		t.Errorf("mirror has %d breakpoints, want 1", len(list.Breakpoints))
	}
	if got := h.sess.Snapshot().Breakpoints; len(got) != 1 {
		t.Errorf("snapshot has %d breakpoints, want 1", len(got))
	}
}

// TestPendingBreakpointResolves is finding 6: the address arrives later, in a
// notification, so the mirror has to be event-driven.
func TestPendingBreakpointResolves(t *testing.T) {
	h := start(t, loadTranscript+`
> -break-insert -f "PROJECT/main.c:1"
< ^done,bkpt={number="2",type="breakpoint",disp="keep",enabled="y",addr="<PENDING>",pending="main.c:1",times="0",original-location="main.c:1"}
< =breakpoint-modified,bkpt={number="2",type="breakpoint",disp="keep",enabled="y",addr="0x1149",func="main",file="main.c",fullname="PROJECT/main.c",line="1",times="0",original-location="main.c:1"}
`)
	h.load()
	bp := h.mustDo(wire.TypeBpSetSource, wire.BreakpointRequest{Path: "main.c", Line: 1}).(wire.Breakpoint)
	if !bp.Pending {
		t.Error("a <PENDING> breakpoint was not marked pending")
	}
	if bp.Address != "" {
		t.Errorf("address = %q; <PENDING> is not an address", bp.Address)
	}

	// The notification must update the mirror.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		list := h.sess.Snapshot().Breakpoints
		if len(list) == 1 && !list[0].Pending && list[0].Address != "" {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Errorf("the pending breakpoint never resolved: %+v", h.sess.Snapshot().Breakpoints)
}

// TestTemporaryBreakpointWeDidNotCreateIsHidden is finding 11: -exec-run
// --start injects one, and the user cannot delete a marker they did not make.
func TestTemporaryBreakpointWeDidNotCreateIsHidden(t *testing.T) {
	h := start(t, loadTranscript+`
> -break-list
< ^done,BreakpointTable={nr_rows="2",nr_cols="6",body=[bkpt={number="1",type="breakpoint",disp="keep",enabled="y",addr="0x1149",func="main",fullname="PROJECT/main.c",line="1",times="0"},bkpt={number="2",type="breakpoint",disp="del",enabled="y",addr="0x1155",func="main",fullname="PROJECT/main.c",line="2",times="0",original-location="-qualified main"}]}
`)
	h.load()
	out := h.mustDo(wire.TypeBpList, nil).(wire.BreakpointList)
	if len(out.Breakpoints) != 1 {
		t.Fatalf("mirror has %d breakpoints, want 1: %+v", len(out.Breakpoints), out.Breakpoints)
	}
	if out.Breakpoints[0].Number != 1 {
		t.Errorf("kept breakpoint %d, want 1", out.Breakpoints[0].Number)
	}
}

func TestBreakpointDelete(t *testing.T) {
	h := start(t, loadTranscript+`
> -break-insert -f "PROJECT/main.c:1"
< ^done,bkpt={number="1",type="breakpoint",disp="keep",enabled="y",addr="0x1149",fullname="PROJECT/main.c",line="1",times="0"}
> -break-delete 1
< ^done
`)
	h.load()
	h.mustDo(wire.TypeBpSetSource, wire.BreakpointRequest{Path: "main.c", Line: 1})
	out := h.mustDo(wire.TypeBpDelete, wire.BreakpointIDRequest{Number: 1}).(wire.BreakpointList)
	if len(out.Breakpoints) != 0 {
		t.Errorf("mirror still has %d breakpoints", len(out.Breakpoints))
	}
}

func TestBreakpointRejectsTraversal(t *testing.T) {
	h := start(t, loadTranscript)
	h.load()
	_, werr := h.do(wire.TypeBpSetSource, wire.BreakpointRequest{Path: "../../etc/passwd", Line: 1})
	if werr == nil {
		t.Fatal("a breakpoint outside the project was accepted")
	}
	if werr.Code != wire.CodePathDenied && werr.Code != wire.CodeNotFound {
		t.Errorf("code = %q", werr.Code)
	}
}

// TestSnapshotRestoresStoppedState is the M3 acceptance criterion in miniature:
// everything a reloading browser needs is in the snapshot.
func TestSnapshotRestoresStoppedState(t *testing.T) {
	h := start(t, loadTranscript+`
> -break-insert -f "PROJECT/main.c:1"
< ^done,bkpt={number="1",type="breakpoint",disp="keep",enabled="y",addr="0x1149",func="main",fullname="PROJECT/main.c",line="1",times="0"}
`+stopTranscript)
	h.load()
	h.mustDo(wire.TypeBpSetSource, wire.BreakpointRequest{Path: "main.c", Line: 1})
	h.mustDo(wire.TypeExecRun, wire.ExecRequest{})
	h.rec.wait(t, wire.EventStopped)

	snap := h.sess.Snapshot()
	if snap.RunState != wire.RunStateStopped {
		t.Errorf("runState = %q", snap.RunState)
	}
	if snap.ExePath != "prog" {
		t.Errorf("exePath = %q", snap.ExePath)
	}
	if len(snap.Breakpoints) != 1 {
		t.Errorf("breakpoints = %d", len(snap.Breakpoints))
	}
	if len(snap.Frames) == 0 {
		t.Error("no frames in the snapshot; a reloading browser could not show the stack")
	}
	if len(snap.Locals) == 0 {
		t.Error("no locals in the snapshot")
	}
	if snap.Selection == nil || snap.Selection.StopSeq != snap.StopSeq {
		t.Errorf("selection = %+v, stopSeq = %d", snap.Selection, snap.StopSeq)
	}
	if snap.LastStopReason != "breakpoint-hit" {
		t.Errorf("lastStopReason = %q", snap.LastStopReason)
	}
}

// TestNoDeclaredTypeIsUnsupported is the debugger half of the docs check: every
// type in wire.RequestTypes must be routed somewhere. Whether it succeeds
// depends on state; coming back "unsupported" never does.
func TestNoDeclaredTypeIsUnsupported(t *testing.T) {
	h := start(t, ``)
	for _, typ := range wire.RequestTypes {
		_, werr := h.do(typ, map[string]any{})
		if werr != nil && werr.Code == wire.CodeUnsupported {
			t.Errorf("%s came back unsupported; it is declared in wire.RequestTypes", typ)
		}
	}
}

func TestGDBDeathIsBroadcast(t *testing.T) {
	h := start(t, `
! eof
`)
	payload := h.rec.wait(t, wire.EventGDBDead)
	dead, ok := payload.(wire.GDBDead)
	if !ok {
		t.Fatalf("payload is %T", payload)
	}
	if dead.Reason == "" {
		t.Error("no reason given")
	}
	// Every subsequent request must fail cleanly rather than hang.
	if _, werr := h.do(wire.TypeBpList, nil); werr == nil {
		t.Error("a request succeeded after gdb died")
	} else if werr.Code != wire.CodeGDBDead {
		t.Errorf("code = %q, want gdb_dead", werr.Code)
	}
}

// TestInferiorOutputBecomesConsole covers finding 3 at this layer: a garbage
// line is the program talking, and it must reach the UI.
func TestInferiorOutputBecomesConsole(t *testing.T) {
	// A leading send with no expect fires as soon as the fake starts, which is
	// exactly how the real thing arrives: unsolicited, between other records.
	h := start(t, `
< total=3 argc=1
`)
	_ = h

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		for _, e := range h.rec.all() {
			if e.name != wire.EventConsole {
				continue
			}
			if m, ok := e.payload.(map[string]string); ok && strings.Contains(m["text"], "total=3") {
				if m["stream"] != "inferior" {
					t.Errorf("stream = %q, want inferior", m["stream"])
				}
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Error("the inferior's output never reached the console")
}

// TestVarobjLRUEviction covers the leak defence.
//
// Every -var-create is a permanent allocation inside gdb until a matching
// -var-delete. A UI that expands a struct a few hundred times over an afternoon
// will make a few hundred, and without a cap they all live until gdb exits —
// a leak that shows up as gdb slowing to a crawl hours later, with nothing in
// the UI to suggest why. This is the test that the cap is real and that
// eviction actually deletes rather than merely forgetting.
func TestVarobjLRUEviction(t *testing.T) {
	h := start(t, loadTranscript+stopTranscript)
	h.load()
	h.mustDo(wire.TypeExecRun, wire.ExecRequest{})
	h.rec.wait(t, wire.EventStopped)

	// Comfortably past the cap, so eviction has to have happened repeatedly.
	const created = 600
	for i := range created {
		path := fmt.Sprintf("local:v%d", i)
		if _, werr := h.do(wire.TypeVarsExpand, wire.VarsExpandRequest{
			Path: path, Expr: fmt.Sprintf("v%d", i),
		}); werr != nil {
			t.Fatalf("expand %s: %s", path, werr.Message)
		}
	}

	if n := h.sess.VarobjCount(); n > 512 {
		t.Errorf("%d varobjs live after %d creations; the 512 cap did not hold", n, created)
	}

	// Eviction must issue real -var-delete commands, not just drop references.
	var deletes int
	for _, cmd := range h.fake.Received() {
		if strings.HasPrefix(cmd, "-var-delete ") {
			deletes++
		}
	}
	if deletes == 0 {
		t.Error("no -var-delete was ever sent; the registry forgot objects without " +
			"deleting them, which is the leak this cap exists to prevent")
	}
	if deletes < created-512 {
		t.Errorf("%d deletes for %d creations over a 512 cap; want at least %d",
			deletes, created, created-512)
	}
}

// TestVarobjsClearedOnRerun is the plan's named criterion, at the unit level so
// it runs without gdb.
func TestVarobjsClearedOnRerun(t *testing.T) {
	h := start(t, loadTranscript+stopTranscript)
	h.load()
	h.mustDo(wire.TypeExecRun, wire.ExecRequest{})
	h.rec.wait(t, wire.EventStopped)

	h.mustDo(wire.TypeVarsExpand, wire.VarsExpandRequest{Path: "local:cfg", Expr: "cfg"})
	if h.sess.VarobjCount() == 0 {
		t.Fatal("expanding created no varobjs; the test proves nothing")
	}

	h.rec.reset()
	h.mustDo(wire.TypeExecRun, wire.ExecRequest{})

	if n := h.sess.VarobjCount(); n != 0 {
		t.Errorf("%d varobjs survived a re-run, want 0: every one refers to a frame "+
			"in a process that no longer exists", n)
	}
	h.rec.wait(t, wire.EventVarsInvalidated)
}

// TestRemoteTargetIsDetachedNotKilled guards something destructive.
//
// gdb kills a `target remote` connection both on an explicit `kill` and on a
// plain quit — verified against gdb 17.1 and a qemu stub. So a debugger UI that
// tears down the way it would for a program it started will terminate somebody
// else's emulator, gdbserver or hardware session when the browser tab closes.
// Detaching first is the only way to leave it running.
func TestRemoteTargetIsDetachedNotKilled(t *testing.T) {
	h := start(t, `
> -interpreter-exec console "target remote 127.0.0.1:9999"
< ^done
> -break-list
< ^done,BreakpointTable={nr_rows="0",nr_cols="6",body=[]}
> -thread-info
< ^done,threads=[],current-thread-id="1"
`)
	h.mustDo(wire.TypeConsoleExec, wire.ConsoleExecRequest{
		Line: "target remote 127.0.0.1:9999",
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := h.sess.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}

	var sawDetach, sawKill bool
	for _, cmd := range h.fake.Received() {
		switch {
		case cmd == "-target-detach":
			sawDetach = true
		case strings.Contains(cmd, `console "kill"`):
			sawKill = true
		}
	}
	if !sawDetach {
		t.Error("no -target-detach on shutdown; the remote target would be killed")
	}
	if sawKill {
		t.Error("shutdown sent kill to a remote target — that terminates a process " +
			"this server did not start")
	}
}

// TestLocalTargetIsStillKilled is the other half: for a program gdb started,
// killing is right, and the remote handling must not have disabled it.
func TestLocalTargetIsStillKilled(t *testing.T) {
	h := start(t, loadTranscript)
	h.load()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := h.sess.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}

	var sawKill, sawDetach bool
	for _, cmd := range h.fake.Received() {
		switch {
		case strings.Contains(cmd, `console "kill"`):
			sawKill = true
		case cmd == "-target-detach":
			sawDetach = true
		}
	}
	if !sawKill {
		t.Error("a locally started program was not killed on shutdown; it would outlive gdb")
	}
	if sawDetach {
		t.Error("a local inferior was detached from; it should be killed")
	}
}

// TestDetachClearsRemoteFlag: after the user disconnects by hand, shutdown
// should go back to the ordinary path.
func TestDetachClearsRemoteFlag(t *testing.T) {
	h := start(t, ``, gdbfake.WithDefaultDone())
	h.mustDo(wire.TypeConsoleExec, wire.ConsoleExecRequest{Line: "target remote :9999"})
	h.mustDo(wire.TypeConsoleExec, wire.ConsoleExecRequest{Line: "disconnect"})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = h.sess.Close(ctx)

	for _, cmd := range h.fake.Received() {
		if cmd == "-target-detach" {
			t.Error("detached after the user had already disconnected")
		}
	}
}

// TestFrameSelectDoesNotReselectTheThread is about noise, and about a user
// hunting a problem that is not there.
//
// -thread-select is never needed for this server's own commands, which always
// pass --thread. Issuing it anyway makes gdb probe the target with a T packet;
// a minimal remote stub that does not implement T answers empty and logs
// "command not supported", which reads as though the frame click failed.
func TestFrameSelectDoesNotReselectTheThread(t *testing.T) {
	h := start(t, loadTranscript+stopTranscript+`
> -stack-select-frame 1
< ^done
> -stack-list-variables --thread 1 --frame 1 --simple-values
< ^done,variables=[]
`)
	h.load()
	h.mustDo(wire.TypeExecRun, wire.ExecRequest{})
	stopped := h.rec.wait(t, wire.EventStopped).(wire.Stopped)

	h.mustDo(wire.TypeFrameSelect, wire.FrameSelectRequest{
		Frame: 1, StopSeq: stopped.StopSeq,
	})

	for _, cmd := range h.fake.Received() {
		if strings.HasPrefix(cmd, "-thread-select") {
			t.Errorf("selecting a frame re-selected the already-current thread (%q); "+
				"that is a wasted round-trip, and a T packet a minimal stub logs as "+
				"unsupported", cmd)
		}
	}
}

// TestThreadSelectStillHappensWhenItChanges is the other half: the command is
// skipped only when it would do nothing.
func TestThreadSelectStillHappensWhenItChanges(t *testing.T) {
	h := start(t, loadTranscript+stopTranscript+`
> -thread-select 2
< ^done
> -stack-list-frames --thread 2 0 63
< ^done,stack=[frame={level="0",addr="0x2000",func="worker"}]
> -stack-list-arguments --thread 2 --simple-values 0 63
< ^done,stack-args=[frame={level="0",args=[]}]
> -stack-list-variables --thread 2 --frame 0 --simple-values
< ^done,variables=[]
`)
	h.load()
	h.mustDo(wire.TypeExecRun, wire.ExecRequest{})
	h.rec.wait(t, wire.EventStopped)

	h.mustDo(wire.TypeThreadSelect, wire.ThreadSelectRequest{Thread: 2})

	var selected bool
	for _, cmd := range h.fake.Received() {
		if cmd == "-thread-select 2" {
			selected = true
		}
	}
	if !selected {
		t.Error("switching to a different thread did not tell gdb; a console command " +
			"typed next would act on the wrong thread")
	}
}

// The connect button and the connection indicator both hang off the server's
// idea of whether a remote target exists, so that idea has to be reported and
// has to be right.
func TestRemoteStateIsReported(t *testing.T) {
	h := start(t, ``, gdbfake.WithDefaultDone())

	if snap := h.sess.Snapshot(); snap.Remote != nil {
		t.Fatalf("Remote = %+v before connecting, want nil", snap.Remote)
	}

	h.mustDo(wire.TypeConsoleExec, wire.ConsoleExecRequest{
		Line: "target remote 127.0.0.1:9999",
	})
	h.rec.wait(t, wire.EventRemoteChanged)

	snap := h.sess.Snapshot()
	if snap.Remote == nil || !snap.Remote.Connected {
		t.Fatalf("Remote = %+v after connecting, want connected", snap.Remote)
	}
	if snap.Remote.Address != "127.0.0.1:9999" {
		t.Errorf("Address = %q, want 127.0.0.1:9999", snap.Remote.Address)
	}

	h.mustDo(wire.TypeConsoleExec, wire.ConsoleExecRequest{Line: "disconnect"})
	h.rec.wait(t, wire.EventRemoteChanged)
	if snap := h.sess.Snapshot(); snap.Remote != nil {
		t.Errorf("Remote = %+v after disconnecting, want nil", snap.Remote)
	}
}

// A connection gdb refused must not be reported as one that succeeded. The
// indicator would be a lie, and shutdown would try to detach from something
// that was never attached — against a *local* inferior that means not killing
// a process that should be killed.
func TestRefusedRemoteIsNotConnected(t *testing.T) {
	h := start(t, `
> -interpreter-exec console "target remote 127.0.0.1:9"
< ^error,msg="could not connect: Connection refused."
> -break-list
< ^done,BreakpointTable={nr_rows="0",nr_cols="6",body=[]}
`)
	h.mustDo(wire.TypeConsoleExec, wire.ConsoleExecRequest{
		Line: "target remote 127.0.0.1:9",
	})

	if snap := h.sess.Snapshot(); snap.Remote != nil {
		t.Errorf("Remote = %+v after a refused connection, want nil", snap.Remote)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = h.sess.Close(ctx)
	for _, cmd := range h.fake.Received() {
		if cmd == "-target-detach" {
			t.Error("detached from a target that was never connected")
		}
	}
}

// `attach` hands this server a process it did not start, and teardown must let
// it go rather than kill it. Closing a browser tab is not a reason for somebody
// else's program to die, and the process was running before the session began.
func TestAttachIsATargetWeDidNotStart(t *testing.T) {
	h := start(t, ``, gdbfake.WithDefaultDone())

	h.mustDo(wire.TypeConsoleExec, wire.ConsoleExecRequest{Line: "attach 4242"})
	h.rec.wait(t, wire.EventRemoteChanged)

	snap := h.sess.Snapshot()
	if snap.Remote == nil || !snap.Remote.Connected {
		t.Fatalf("Remote = %+v after attaching, want connected", snap.Remote)
	}
	if snap.Remote.Kind != wire.TargetAttach {
		t.Errorf("Kind = %q, want %q", snap.Remote.Kind, wire.TargetAttach)
	}
	if snap.Remote.PID != 4242 {
		t.Errorf("PID = %d, want 4242", snap.Remote.PID)
	}
	if snap.Remote.Address != "" {
		t.Errorf("Address = %q, want empty: an attached process has no address",
			snap.Remote.Address)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = h.sess.Close(ctx)

	var detached bool
	for _, cmd := range h.fake.Received() {
		if cmd == "-target-detach" {
			detached = true
		}
		if strings.Contains(cmd, `console "kill"`) {
			t.Error("killed a process gdb-wui attached to rather than detaching")
		}
	}
	if !detached {
		t.Error("shut down without detaching from the attached process")
	}
}

// An attach gdb refused leaves no inferior at all, and ptrace_scope permitting
// only descendants makes that the common case rather than an unlikely one.
// Reporting it as attached would stop teardown killing a program gdb-wui did
// start, which is the opposite of what this is for.
func TestRefusedAttachIsNotConnected(t *testing.T) {
	h := start(t, `
> -interpreter-exec console "attach 4242"
< ^error,msg="ptrace: Operation not permitted."
> -break-list
< ^done,BreakpointTable={nr_rows="0",nr_cols="6",body=[]}
`)
	h.mustDo(wire.TypeConsoleExec, wire.ConsoleExecRequest{Line: "attach 4242"})

	if snap := h.sess.Snapshot(); snap.Remote != nil {
		t.Errorf("Remote = %+v after a refused attach, want nil", snap.Remote)
	}
}

// Detaching gives the process back, and the indicator has to say so — a pill
// still reading "attached pid 4242" would offer a detach that errors.
func TestDetachClearsTheTarget(t *testing.T) {
	h := start(t, ``, gdbfake.WithDefaultDone())

	h.mustDo(wire.TypeConsoleExec, wire.ConsoleExecRequest{Line: "attach 4242"})
	h.rec.wait(t, wire.EventRemoteChanged)
	h.mustDo(wire.TypeConsoleExec, wire.ConsoleExecRequest{Line: "detach"})

	if snap := h.sess.Snapshot(); snap.Remote != nil {
		t.Errorf("Remote = %+v after detaching, want nil", snap.Remote)
	}
}

// Connecting twice must not emit a second remoteChanged: the indicator is
// already right, and a UI that repaints on every console command would flicker.
func TestRemoteChangedOnlyOnChange(t *testing.T) {
	h := start(t, ``, gdbfake.WithDefaultDone())
	h.mustDo(wire.TypeConsoleExec, wire.ConsoleExecRequest{Line: "target remote :9999"})
	h.rec.wait(t, wire.EventRemoteChanged)
	h.mustDo(wire.TypeConsoleExec, wire.ConsoleExecRequest{Line: "target remote :9999"})
	h.mustDo(wire.TypeConsoleExec, wire.ConsoleExecRequest{Line: "print 1"})

	if n := h.rec.count(wire.EventRemoteChanged); n != 1 {
		t.Errorf("remoteChanged fired %d times, want 1", n)
	}
}

// Every broadcast must go through emit, which refreshes the snapshot first.
//
// A handler that broadcasts directly announces a change the snapshot does not
// yet carry, because serve() publishes only after dispatch returns. The
// symptom is a client acting on an event and reading state from before it —
// which is how this was found: CI failed with `snapshot exePath = ""`
// immediately after the exeLoaded event said a program had loaded.
//
// Checked by reading the source because the alternative is a race that
// reproduces once in fifty runs.
func TestAllBroadcastsGoThroughEmit(t *testing.T) {
	dir := filepath.Join(testutil.RepoRoot(t), "internal", "debugger")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(src), "\n") {
			if !strings.Contains(line, "cfg.Events.Broadcast(") {
				continue
			}
			// The bodies of emit and emitOffActor are the two legitimate
			// calls; everything else goes through one of them.
			if name == "session.go" && strings.Contains(line, "s.cfg.Events.Broadcast(event, payload)") {
				continue
			}
			t.Errorf("%s:%d broadcasts directly; use s.emit (actor goroutine, "+
				"refreshes the snapshot first) or s.emitOffActor (other "+
				"goroutines, payload must not be snapshot state):\n\t%s",
				name, i+1, strings.TrimSpace(line))
		}
	}
}

// The snapshot must already describe the change an event announces.
//
// Sampled from inside the broadcast, which is the only place the ordering is
// observable without a race — see recorder.setProbe. Asserting after the event
// has been received instead is what CI was doing when it failed with
// `snapshot exePath = ""`, and that reproduces roughly one run in fifty.
func TestSnapshotIsCurrentWhenEventFires(t *testing.T) {
	h := start(t, loadTranscript)

	var atEvent string
	var seen bool
	h.rec.setProbe(func(event string) {
		if event == wire.EventExeLoaded {
			atEvent = h.sess.Snapshot().ExePath
			seen = true
		}
	})

	h.load()
	h.rec.wait(t, wire.EventExeLoaded)
	if !seen {
		t.Fatal("exeLoaded never fired")
	}
	if atEvent != "prog" {
		t.Errorf("snapshot exePath = %q at the instant exeLoaded was broadcast, "+
			"want prog: the event announced a change the snapshot did not carry",
			atEvent)
	}
}
