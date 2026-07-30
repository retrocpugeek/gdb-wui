package debugger

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"strings"
	"syscall"
	"time"

	"github.com/retrocpugeek/gdb-wui/internal/ptyio"
	"github.com/retrocpugeek/gdb-wui/internal/wire"
)

// The gdb console, the inferior's terminal, and threads.

// consoleExec runs a command as if the user had typed it at gdb's prompt.
//
// This is the escape hatch that keeps the semantic command set honest: anything
// the UI does not model, gdb can still do. The cost is that the command may
// change state behind the server's back — `b main.c:12`, `next`, `thread 2`
// are all ordinary things to type — so every console command is followed by a
// resync. Skipping it would leave the breakpoint mirror and the selection
// quietly wrong, which is worse than not offering a console at all.
func (s *Session) consoleExec(r *request) (any, *wire.Error) {
	req, werr := decode[wire.ConsoleExecRequest](r.req.Payload)
	if werr != nil {
		return nil, werr
	}
	line := strings.TrimSpace(req.Line)
	if line == "" {
		return nil, wire.NewError(wire.CodeBadRequest, "line is required")
	}
	if strings.ContainsAny(line, "\r\n") {
		return nil, wire.NewError(wire.CodeBadRequest, "one command at a time")
	}

	// The ~ and & records the command produces are streamed by the ordinary
	// event path, so the browser sees output as it arrives rather than in one
	// lump at the end.
	if _, werr := s.send(r.ctx, "-interpreter-exec console "+quote(line)); werr != nil {
		// A gdb error here is the user's command being wrong, which is normal
		// at a console. Surface it as console output and report success, so the
		// UI does not raise a dialog over a typo.
		s.cfg.Events.Broadcast(wire.EventConsole, map[string]string{
			"text":   werr.Message + "\n",
			"stream": "log",
		})
	}

	resynced := s.resyncAfterConsole(r.ctx)
	return wire.ConsoleExecResult{
		Resynced: resynced,
		RunState: s.st.runState,
		StopSeq:  s.st.stopSeq,
	}, nil
}

// resyncAfterConsole re-reads the state a typed command may have changed.
//
// Deliberately narrow: breakpoints, the thread list and the selection. Anything
// derived from a *stop* does not need re-reading, because a console command
// that resumes the program produces a real stop event and the ordinary path
// handles it.
func (s *Session) resyncAfterConsole(ctx context.Context) []string {
	if s.st.runState == wire.RunStateRunning {
		// Nothing can be read while running, and the stop that follows will
		// refresh everything anyway.
		return nil
	}
	var done []string

	s.reconcileBreakpoints(ctx)
	done = append(done, "breakpoints")

	if s.st.runState == wire.RunStateStopped {
		if threads, werr := s.fetchThreads(ctx); werr == nil {
			s.st.threads = threads
			done = append(done, "threads")
			s.cfg.Events.Broadcast(wire.EventThreadsChanged, wire.ThreadsList{
				StopSeq: s.st.stopSeq, Threads: threads, Selected: s.st.selThread,
			})
		}
		// The user may have typed "thread 2" or "frame 1"; ask gdb what it
		// thinks is selected rather than assuming it is still ours.
		if rec, werr := s.send(ctx, "-thread-info"); werr == nil {
			if id, ok := rec.Results.Int("current-thread-id"); ok && id != s.st.selThread {
				s.st.selThread = id
				s.st.selFrame = 0
				done = append(done, "selection")
				s.broadcastSelection(ctx)
			}
		}
	}
	return done
}

func (s *Session) broadcastSelection(ctx context.Context) {
	sel := wire.Selection{
		ThreadID: s.st.selThread,
		Frame:    s.st.selFrame,
		StopSeq:  s.st.stopSeq,
	}
	if frames, werr := s.fetchFrames(ctx, s.st.selThread, 0, maxFrames); werr == nil {
		s.st.frames = frames
		sel.Frames = frames
		if len(frames) > 0 {
			src := frames[0].Source
			sel.Source = &src
		}
	}
	if locals, werr := s.fetchLocals(ctx, s.st.selThread, s.st.selFrame); werr == nil {
		s.st.locals = locals
		sel.Locals = locals
	}
	s.cfg.Events.Broadcast(wire.EventSelectionChanged, sel)
}

// consoleComplete asks gdb to complete a prefix.
//
// gdb does the work, so the frontend carries no command table and cannot drift
// from the debugger it is driving — including commands added by a user's Python
// extensions, which no built-in table could know about.
func (s *Session) consoleComplete(r *request) (any, *wire.Error) {
	req, werr := decode[wire.ConsoleCompleteRequest](r.req.Payload)
	if werr != nil {
		return nil, werr
	}
	if strings.TrimSpace(req.Prefix) == "" {
		return wire.ConsoleComplete{}, nil
	}
	rec, werr := s.send(r.ctx, "-complete "+quote(req.Prefix))
	if werr != nil {
		// Completion failing is not worth an error; the user is mid-typing.
		return wire.ConsoleComplete{}, nil
	}
	out := wire.ConsoleComplete{Completion: rec.Results.Str("completion")}
	if matches, ok := rec.Results.List("matches"); ok {
		for _, m := range matches {
			out.Matches = append(out.Matches, m.Str)
		}
	}
	if reached, ok := rec.Results.Bool("max_completions_reached"); ok {
		out.Truncated = reached
	}
	return out, nil
}

// --- the inferior's terminal ------------------------------------------------

// ensureTerminal allocates the pty and tells gdb about it.
//
// Called before the program starts, because -inferior-tty-set only affects the
// next run. The terminal outlives individual runs: it is allocated once and
// reused, which is why ptyio holds a slave descriptor open.
func (s *Session) ensureTerminal(ctx context.Context) {
	if s.term != nil {
		return
	}
	term, err := ptyio.Open()
	if err != nil {
		// Not fatal. Without a pty the program still runs; its output arrives
		// interleaved into the MI stream and shows up in the console, which is
		// how M3 behaved. Degrading beats refusing to run.
		s.logf("allocating an inferior terminal: %v", err)
		return
	}
	if _, werr := s.send(ctx, "-inferior-tty-set "+quote(term.Name())); werr != nil {
		s.logf("-inferior-tty-set: %s", werr.Message)
		_ = term.Close()
		return
	}
	s.term = term
	s.termPtr.Store(&term)
	go s.pumpTerminal(term)
}

// pumpTerminal forwards the program's output to the browser.
func (s *Session) pumpTerminal(term *ptyio.Terminal) {
	buf := make([]byte, 8192)
	for {
		n, err := term.Read(buf)
		if n > 0 {
			// Base64: these are arbitrary bytes from an arbitrary program,
			// including invalid UTF-8 and control sequences, and JSON strings
			// cannot carry them intact.
			s.cfg.Events.Broadcast(wire.EventInferiorOutput, wire.InferiorOutput{
				DataB64: base64.StdEncoding.EncodeToString(buf[:n]),
			})
		}
		if err != nil {
			if err != io.EOF {
				s.logf("inferior terminal: %v", err)
			}
			return
		}
	}
}

// inferiorStdin writes to the program's terminal.
//
// It bypasses the actor for the same reason exec.pause does: the loop is often
// blocked in a gdb round-trip, and a keystroke that waits for it is a keystroke
// the user experiences as a hang. Writing to the pty touches no session state,
// so there is nothing to serialise.
func (s *Session) inferiorStdin(ctx context.Context, req wire.Request) (any, *wire.Error) {
	payload, werr := decode[wire.InferiorStdinRequest](req.Payload)
	if werr != nil {
		return nil, werr
	}
	data, err := base64.StdEncoding.DecodeString(payload.DataB64)
	if err != nil {
		return nil, wire.NewError(wire.CodeBadRequest, "dataB64 is not valid base64")
	}
	term := s.terminal()
	if term == nil {
		return nil, wire.NewError(wire.CodeNotReady, "the program has no terminal")
	}
	if _, err := term.Write(data); err != nil {
		return nil, wire.NewError(wire.CodeGDBDead, "the program's terminal is closed")
	}
	return map[string]any{"written": len(data)}, nil
}

// inferiorSignal sends a signal to the debuggee.
func (s *Session) inferiorSignal(r *request) (any, *wire.Error) {
	req, werr := decode[wire.InferiorSignalRequest](r.req.Payload)
	if werr != nil {
		return nil, werr
	}
	sig, ok := signals[strings.ToUpper(strings.TrimPrefix(req.Signal, "SIG"))]
	if !ok {
		return nil, wire.NewError(wire.CodeBadRequest, "unknown signal "+req.Signal)
	}
	if s.st.inferiorPID == 0 {
		return nil, wire.NewError(wire.CodeNotReady, "the program is not running")
	}
	// The process group, so a signal reaches children too — and because that is
	// what pressing Ctrl-C in a terminal does.
	if err := syscall.Kill(-s.st.inferiorPID, sig); err != nil {
		// Falling back to the process itself covers a program that never got
		// its own group.
		if err := syscall.Kill(s.st.inferiorPID, sig); err != nil {
			return nil, wire.NewError(wire.CodeInternal, err.Error())
		}
	}
	return map[string]any{"sent": req.Signal}, nil
}

var signals = map[string]syscall.Signal{
	"INT":  syscall.SIGINT,
	"TERM": syscall.SIGTERM,
	"KILL": syscall.SIGKILL,
	"QUIT": syscall.SIGQUIT,
	"HUP":  syscall.SIGHUP,
	"USR1": syscall.SIGUSR1,
	"USR2": syscall.SIGUSR2,
	"STOP": syscall.SIGSTOP,
	"CONT": syscall.SIGCONT,
}

// inferiorResize sets the terminal size. Like stdin, it does not touch session
// state and so does not queue behind the actor.
func (s *Session) inferiorResize(ctx context.Context, req wire.Request) (any, *wire.Error) {
	payload, werr := decode[wire.InferiorResizeRequest](req.Payload)
	if werr != nil {
		return nil, werr
	}
	term := s.terminal()
	if term == nil {
		return map[string]any{"resized": false}, nil
	}
	if err := term.Resize(uint16(payload.Rows), uint16(payload.Cols)); err != nil {
		return nil, wire.NewError(wire.CodeInternal, err.Error())
	}
	return map[string]any{"resized": true}, nil
}

// --- threads ----------------------------------------------------------------

func (s *Session) threadsList(r *request) (any, *wire.Error) {
	req, werr := decode[wire.ThreadsListRequest](r.req.Payload)
	if werr != nil {
		return nil, werr
	}
	if werr := s.checkStopSeq(req.StopSeq); werr != nil {
		return nil, werr
	}
	threads, werr := s.fetchThreads(r.ctx)
	if werr != nil {
		return nil, werr
	}
	s.st.threads = threads
	return wire.ThreadsList{
		StopSeq: s.st.stopSeq, Threads: threads, Selected: s.st.selThread,
	}, nil
}

func (s *Session) threadSelect(r *request) (any, *wire.Error) {
	req, werr := decode[wire.ThreadSelectRequest](r.req.Payload)
	if werr != nil {
		return nil, werr
	}
	if werr := s.checkStopSeq(req.StopSeq); werr != nil {
		return nil, werr
	}
	if req.Thread <= 0 {
		return nil, wire.NewError(wire.CodeBadRequest, "thread must be positive")
	}

	// Tell gdb too. Programmatic commands always pass --thread, so this is not
	// needed for them; it is for console commands the user types next, which do
	// use gdb's own selection.
	if _, werr := s.send(r.ctx, fmt.Sprintf("-thread-select %d", req.Thread)); werr != nil {
		return nil, werr
	}
	s.st.selThread = req.Thread
	s.st.selFrame = 0

	sel := wire.Selection{ThreadID: req.Thread, Frame: 0, StopSeq: s.st.stopSeq}
	if frames, werr := s.fetchFrames(r.ctx, req.Thread, 0, maxFrames); werr == nil {
		s.st.frames = frames
		sel.Frames = frames
		if len(frames) > 0 {
			src := frames[0].Source
			sel.Source = &src
		}
	}
	if locals, werr := s.fetchLocals(r.ctx, req.Thread, 0); werr == nil {
		s.st.locals = locals
		sel.Locals = locals
	}
	s.cfg.Events.Broadcast(wire.EventSelectionChanged, sel)
	return sel, nil
}

// terminal returns the pty, if one exists. Safe to call off the actor.
func (s *Session) terminal() *ptyio.Terminal {
	if p := s.termPtr.Load(); p != nil {
		return *p
	}
	return nil
}

// closeTerminal releases the pty at shutdown.
func (s *Session) closeTerminal() {
	if term := s.terminal(); term != nil {
		// A brief moment for any last output to be pumped before the reader
		// sees EOF; without it the program's final line can be lost.
		time.Sleep(20 * time.Millisecond)
		_ = term.Close()
	}
}
