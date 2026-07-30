package debugger

import (
	"context"
	"fmt"
	"strings"

	"github.com/retrocpugeek/gdb-wui/internal/mi"
	"github.com/retrocpugeek/gdb-wui/internal/wire"
)

// onRecord handles one MI record inside the actor.
func (s *Session) onRecord(rec mi.Record) {
	if s.cfg.MILog && rec.Raw != "" {
		s.cfg.Events.Broadcast(wire.EventMI, wire.MILogEntry{Direction: "in", Text: rec.Raw})
	}

	switch rec.Type {
	case mi.RecExec:
		s.onExec(rec)
	case mi.RecNotify:
		s.onNotify(rec)
	case mi.RecConsole, mi.RecLog, mi.RecTarget:
		s.cfg.Events.Broadcast(wire.EventConsole, map[string]string{
			"text":   rec.Text,
			"stream": streamName(rec.Type),
		})
	case mi.RecGarbage:
		// The inferior's stdout, interleaved into gdb's. M5 gives the debuggee
		// its own pty and this stops happening; until then, showing it in the
		// console beats discarding the program's output.
		if rec.Err != nil {
			s.logf("unparseable MI line: %q (%v)", rec.Text, rec.Err)
		}
		if rec.Text != "" {
			s.cfg.Events.Broadcast(wire.EventConsole, map[string]string{
				"text":   rec.Text + "\n",
				"stream": "inferior",
			})
		}
	case mi.RecResult:
		// A result with no pending command: something the user typed at the
		// console produced it. M5 resyncs after console commands; here it is
		// only worth a log line.
		s.logf("unsolicited result: %s", rec.Raw)
	}
	s.publish()
}

func streamName(t mi.Type) string {
	switch t {
	case mi.RecConsole:
		return "console"
	case mi.RecLog:
		return "log"
	case mi.RecTarget:
		return "target"
	}
	return "unknown"
}

func (s *Session) onExec(rec mi.Record) {
	switch rec.Class {
	case "running":
		thread := 0
		if id := rec.Results.Str("thread-id"); id != "all" {
			thread = atoiSafe(id)
		}
		s.setRunning(thread)
	case mi.ClassStopped:
		s.onStopped(rec)
	}
}

// setRunning moves to the running state and tells the browsers.
func (s *Session) setRunning(thread int) {
	if s.st.runState == wire.RunStateRunning {
		return
	}
	s.st.runState = wire.RunStateRunning
	// The stack and locals describe a stop that is now over. Clearing them
	// means a panel cannot render stale values while the inferior runs.
	s.st.frames = nil
	s.st.locals = nil
	s.publish()
	s.cfg.Events.Broadcast(wire.EventRunning, wire.Running{
		ThreadID: thread,
		RunState: s.st.runState,
	})
}

// setExited moves to the exited state.
//
// It is called from two places because gdb reports an exit twice, in this
// order: =thread-group-exited carries the exit code, and the *stopped that
// follows carries the reason but not the code. Merging rather than overwriting
// means the UI sees one event with everything, instead of a codeless exit
// followed by a redundant second one.
func (s *Session) setExited(code *int, signal string) {
	s.st.inferiorPID = 0
	already := s.st.runState == wire.RunStateExited
	if code == nil {
		code = s.st.exitCode
	}
	if signal == "" {
		signal = s.st.exitSignal
	}
	learnedSomething := (code != nil && s.st.exitCode == nil) ||
		(signal != "" && s.st.exitSignal == "")
	s.st.exitCode, s.st.exitSignal = code, signal

	if already && !learnedSomething {
		return
	}
	s.st.runState = wire.RunStateExited
	s.st.threads = nil
	s.st.frames = nil
	s.st.locals = nil
	s.st.selFrame = 0
	s.publish()
	s.cfg.Events.Broadcast(wire.EventExited, wire.Exited{
		ExitCode: code,
		Signal:   signal,
		RunState: s.st.runState,
	})
}

// onStopped builds the fat stop event.
//
// Everything the UI needs to repaint is gathered here and sent as one message.
// The alternative — let each panel ask — costs four or five round-trips per
// single-step, and stepping is the thing users do most.
func (s *Session) onStopped(rec mi.Record) {
	reason := rec.Results.Str("reason")
	s.st.lastStopReason = reason
	s.st.stopSeq++

	if strings.HasPrefix(reason, "exited") {
		var code *int
		if raw, ok := rec.Results.StrOK("exit-code"); ok {
			// gdb reports the exit code in octal, as "01" or "0".
			n := parseExitCode(raw)
			code = &n
		} else if reason == "exited-normally" {
			zero := 0
			code = &zero
		}
		s.setExited(code, rec.Results.Str("signal-name"))
		return
	}

	s.st.runState = wire.RunStateStopped
	if id := rec.Results.Str("thread-id"); id != "" {
		s.st.selThread = atoiSafe(id)
	}
	s.st.selFrame = 0

	// A bounded context: this runs on the actor, and a gdb that never answers
	// would otherwise wedge the whole session with no way back.
	ctx, cancel := context.WithTimeout(context.Background(), s.cfg.CommandTimeout)
	defer cancel()

	out := wire.Stopped{
		StopSeq:       s.st.stopSeq,
		Reason:        reason,
		Signal:        rec.Results.Str("signal-name"),
		SignalMeaning: rec.Results.Str("signal-meaning"),
		ThreadID:      s.st.selThread,
		RunState:      s.st.runState,
	}
	if n, ok := rec.Results.Int("bkptno"); ok {
		out.BreakpointNumber = n
	}
	if rv, ok := rec.Results.Get("return-value"); ok {
		out.ReturnValue = rv.Results().Str("value")
		if out.ReturnValue == "" && rv.Kind == mi.KindConst {
			out.ReturnValue = rv.Str
		}
	}

	if threads, werr := s.fetchThreads(ctx); werr == nil {
		s.st.threads = threads
		out.Threads = threads
	} else {
		s.logf("-thread-info after stop: %s", werr.Message)
	}

	if frames, werr := s.fetchFrames(ctx, s.st.selThread, 0, maxFrames); werr == nil {
		s.st.frames = frames
		out.Frames = frames
	} else {
		s.logf("-stack-list-frames after stop: %s", werr.Message)
	}

	if locals, werr := s.fetchLocals(ctx, s.st.selThread, 0); werr == nil {
		s.st.locals = locals
		out.Locals = locals
	} else {
		s.logf("-stack-list-variables after stop: %s", werr.Message)
	}

	// One -var-update for every live varobj. gdb returns only what actually
	// changed, which is both cheap and exactly the change-highlighting signal
	// the variables panel wants.
	s.refreshVarobjs(ctx)
	// A watch whose varobj went away — first stop after a re-run — is
	// recreated here, so the panel is populated by the time the event lands.
	s.recreateWatches(ctx)
	if watches := s.watchList(); len(watches.Watches) > 0 {
		s.cfg.Events.Broadcast(wire.EventWatchesChanged, watches)
	}

	s.publish()
	s.cfg.Events.Broadcast(wire.EventStopped, out)
}

// parseExitCode reads gdb's octal exit code.
func parseExitCode(raw string) int {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0
	}
	var n int
	if _, err := fmt.Sscanf(raw, "%o", &n); err != nil {
		return atoiSafe(raw)
	}
	return n
}

func (s *Session) onNotify(rec mi.Record) {
	switch rec.Class {
	case "breakpoint-created", "breakpoint-modified":
		bkpt, ok := rec.Results.Tuple("bkpt")
		if !ok {
			return
		}
		bp := s.parseBreakpoint(bkpt)
		if bp.Number == 0 {
			return
		}
		// A temporary breakpoint we did not create is gdb's own — the one
		// -exec-run --start injects. Showing it would put an undeletable
		// marker in the gutter.
		if bp.Temporary && !s.st.ours[bp.Number] {
			return
		}
		s.st.breakpoints[bp.Number] = bp
		s.broadcastBreakpoints()

	case "breakpoint-deleted":
		if n, ok := rec.Results.Int("id"); ok {
			delete(s.st.breakpoints, n)
			delete(s.st.ours, n)
			s.broadcastBreakpoints()
		}

	case "thread-group-started":
		// The pid inferior.signal needs. gdb reports it here and nowhere else
		// that is convenient.
		if pid, ok := rec.Results.Int("pid"); ok {
			s.st.inferiorPID = pid
		}

	case "thread-group-exited":
		if s.st.runState == wire.RunStateNoProgram {
			return
		}
		// This is where the exit code actually lives; the *stopped that
		// follows has the reason but no code.
		var code *int
		if raw, ok := rec.Results.StrOK("exit-code"); ok {
			n := parseExitCode(raw)
			code = &n
		}
		s.setExited(code, "")

	case "thread-created", "thread-exited":
		// Only meaningful while stopped: mid-run gdb refuses -thread-info's
		// details, and the stop that follows re-reads everything anyway.
		if s.st.runState != wire.RunStateStopped {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), s.cfg.CommandTimeout)
		defer cancel()
		if threads, werr := s.fetchThreads(ctx); werr == nil {
			s.st.threads = threads
			s.cfg.Events.Broadcast(wire.EventThreadsChanged, wire.ThreadsList{
				StopSeq: s.st.stopSeq, Threads: threads, Selected: s.st.selThread,
			})
		}

	case "library-loaded", "library-unloaded":
		// Suppressed: one per shared object, dozens per run, and nothing in
		// the UI consumes them.
	}
}

// fetchThreads reads the thread list.
func (s *Session) fetchThreads(ctx context.Context) ([]wire.Thread, *wire.Error) {
	rec, werr := s.send(ctx, "-thread-info")
	if werr != nil {
		return nil, werr
	}
	list, ok := rec.Results.List("threads")
	if !ok {
		return nil, nil
	}
	out := make([]wire.Thread, 0, len(list))
	for _, t := range list {
		th := wire.Thread{
			ID:       atoiSafe(t.Results().Str("id")),
			TargetID: t.Results().Str("target-id"),
			Name:     t.Results().Str("name"),
			State:    t.Results().Str("state"),
			Core:     t.Results().Str("core"),
		}
		if f, ok := t.Tuple("frame"); ok {
			frame := s.parseFrame(f)
			th.Frame = &frame
		}
		out = append(out, th)
	}
	// gdb lists threads newest-first; ascending is what a UI wants.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].ID < out[j-1].ID; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out, nil
}

func (s *Session) fetchFrames(ctx context.Context, thread, low, high int) ([]wire.Frame, *wire.Error) {
	cmd := fmt.Sprintf("-stack-list-frames --thread %d %d %d", thread, low, high)
	rec, werr := s.send(ctx, cmd)
	if werr != nil {
		return nil, werr
	}
	stack, ok := rec.Results.Get("stack")
	if !ok {
		return nil, nil
	}
	// All("frame") rather than a map lookup: MI repeats the key, once per
	// frame, and only an ordered representation keeps them all.
	raw := stack.All("frame")
	out := make([]wire.Frame, 0, len(raw))
	for _, f := range raw {
		out = append(out, s.parseFrame(f.Items))
	}
	s.attachArgs(ctx, thread, low, high, out)
	return out, nil
}

// attachArgs fills in each frame's arguments.
//
// -stack-list-frames does not return them — verified against gdb 17.1 — so a
// stack panel that wants "main(argc=1, argv=0x…)" needs a second command. It is
// one extra round-trip per stop, still inside the single fat event, and a
// failure degrades to frames without arguments rather than losing the stop.
func (s *Session) attachArgs(ctx context.Context, thread, low, high int, frames []wire.Frame) {
	if len(frames) == 0 {
		return
	}
	cmd := fmt.Sprintf("-stack-list-arguments --thread %d --simple-values %d %d", thread, low, high)
	rec, werr := s.send(ctx, cmd)
	if werr != nil {
		s.logf("-stack-list-arguments: %s", werr.Message)
		return
	}
	stackArgs, ok := rec.Results.Get("stack-args")
	if !ok {
		return
	}
	byLevel := map[int][]wire.Arg{}
	for _, f := range stackArgs.All("frame") {
		level, _ := f.Int("level")
		args, ok := f.List("args")
		if !ok {
			continue
		}
		list := make([]wire.Arg, 0, len(args))
		for _, a := range args {
			list = append(list, wire.Arg{
				Name:  a.Results().Str("name"),
				Value: a.Results().Str("value"),
			})
		}
		byLevel[level] = list
	}
	for i := range frames {
		if args, ok := byLevel[frames[i].Level]; ok {
			frames[i].Args = args
		}
	}
}

func (s *Session) fetchLocals(ctx context.Context, thread, frame int) ([]wire.Variable, *wire.Error) {
	cmd := fmt.Sprintf("-stack-list-variables --thread %d --frame %d --simple-values", thread, frame)
	rec, werr := s.send(ctx, cmd)
	if werr != nil {
		return nil, werr
	}
	list, ok := rec.Results.List("variables")
	if !ok {
		return nil, nil
	}
	out := make([]wire.Variable, 0, len(list))
	for _, v := range list {
		value, hasValue := v.StrOK("value")
		out = append(out, wire.Variable{
			Name:  v.Results().Str("name"),
			Type:  v.Results().Str("type"),
			Value: value,
			// With --simple-values gdb omits "value" precisely for aggregates,
			// so its absence is the expandable signal. This is also the
			// defence against a 100k-element array: nothing was fetched.
			Expandable: !hasValue,
		})
	}
	return out, nil
}

func (s *Session) parseFrame(t mi.Results) wire.Frame {
	level, _ := t.Int("level")
	f := wire.Frame{
		Level:   level,
		Address: t.Str("addr"),
		Func:    t.Str("func"),
		From:    t.Str("from"),
	}
	if args, ok := t.List("args"); ok {
		for _, a := range args {
			f.Args = append(f.Args, wire.Arg{
				Name:  a.Results().Str("name"),
				Value: a.Results().Str("value"),
			})
		}
	}
	line, _ := t.Int("line")
	f.Source = s.resolveSourceFull(t.Str("fullname"), t.Str("file"), line)
	return f
}
