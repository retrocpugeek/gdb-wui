package debugger

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/retrocpugeek/gdb-wui/internal/mi"
	"github.com/retrocpugeek/gdb-wui/internal/srcfs"
	"github.com/retrocpugeek/gdb-wui/internal/wire"
)

// elfMagic is checked before a program reaches gdb.
//
// gdb's own error for a non-executable is unhelpful ("not in executable
// format") and arrives after a round-trip; four bytes here turn "I clicked the
// wrong file" into a sentence the user can act on.
var elfMagic = []byte{0x7f, 'E', 'L', 'F'}

func decode[T any](raw json.RawMessage) (T, *wire.Error) {
	var out T
	if len(raw) == 0 {
		return out, nil
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return out, wire.NewError(wire.CodeBadRequest, "malformed payload: "+err.Error())
	}
	return out, nil
}

func (s *Session) exeLoad(r *request) (any, *wire.Error) {
	req, werr := decode[wire.ExeLoadRequest](r.req.Payload)
	if werr != nil {
		return nil, werr
	}
	if req.Path == "" {
		return nil, wire.NewError(wire.CodeBadRequest, "path is required")
	}
	if s.files == nil {
		return nil, wire.NewError(wire.CodeInternal, "no project is configured")
	}

	head, err := s.files.Head(req.Path, 4)
	if err != nil {
		return nil, fsError(err)
	}
	if !bytes.Equal(head, elfMagic) {
		return nil, wire.NewError(wire.CodeBadRequest,
			fmt.Sprintf("%s is not an ELF executable", req.Path))
	}
	abs, err := s.files.AbsPath(req.Path)
	if err != nil {
		return nil, fsError(err)
	}

	if _, werr := s.send(r.ctx, "-file-exec-and-symbols "+quote(abs)); werr != nil {
		return nil, werr
	}
	if _, werr := s.send(r.ctx, "-environment-cd "+quote(s.files.Abs())); werr != nil {
		return nil, werr
	}
	if len(req.Args) > 0 {
		args := make([]string, 0, len(req.Args))
		for _, a := range req.Args {
			args = append(args, quote(a))
		}
		if _, werr := s.send(r.ctx, "-exec-arguments "+strings.Join(args, " ")); werr != nil {
			return nil, werr
		}
	}

	// Loading a program invalidates everything that described the previous one.
	s.deleteAllVarobjs(r.ctx)
	s.st.registerNames = nil
	s.invalidateSymbols()
	s.st.exePath = req.Path
	s.st.runState = wire.RunStateNoProgram
	s.st.threads = nil
	s.st.frames = nil
	s.st.locals = nil
	s.st.lastStopReason = ""
	s.st.selThread, s.st.selFrame = 1, 0

	// gdb keeps breakpoints across a new file, and re-resolves them. Re-read
	// rather than assume: pending ones may have just acquired addresses.
	s.reconcileBreakpoints(r.ctx)

	out := wire.ExeLoaded{Path: req.Path, RunState: s.st.runState}
	s.emit(wire.EventExeLoaded, out)
	return out, nil
}

func (s *Session) execRun(r *request) (any, *wire.Error) {
	req, werr := decode[wire.ExecRequest](r.req.Payload)
	if werr != nil {
		return nil, werr
	}
	// -inferior-tty-set only affects the next run, so the terminal has to exist
	// before this one starts.
	s.ensureTerminal(r.ctx)

	// Every varobj refers to a frame in a process that is about to be replaced.
	// Keeping them would mean serving values from a program that no longer
	// exists; a test asserts the registry is empty after this.
	s.deleteAllVarobjs(r.ctx)

	cmd := "-exec-run"
	switch {
	case req.StopAtEntry:
		// starti has no MI form, and --start is not a substitute: it stops at
		// main, which a stripped binary has no symbol for, so the program would
		// run to completion instead. This is the only way to get an
		// instruction-level session started on stripped code.
		cmd = `-interpreter-exec console "starti"`
	case req.StopAtMain:
		// --start injects a temporary breakpoint at main, which shows up in
		// -break-list with disp="del". The mirror filters it; see
		// reconcileBreakpoints.
		cmd += " --start"
	}
	if _, werr := s.send(r.ctx, cmd); werr != nil {
		return nil, werr
	}
	// gdb answers ^running and the *running record sets the state; setting it
	// here too avoids a window where a second request slips past the gate.
	s.setRunning(0)
	return wire.ExecAck{RunState: s.st.runState, StopSeq: s.st.stopSeq}, nil
}

func (s *Session) execResume(r *request, base string) (any, *wire.Error) {
	req, werr := decode[wire.ExecRequest](r.req.Payload)
	if werr != nil {
		return nil, werr
	}
	if werr := s.checkStopSeq(req.StopSeq); werr != nil {
		return nil, werr
	}
	thread := s.thread(req.Thread)
	// --thread explicitly, never gdb's notion of the selected thread: the user
	// may have typed "thread 2" in the console since we last looked.
	if _, werr := s.send(r.ctx, fmt.Sprintf("%s --thread %d", base, thread)); werr != nil {
		return nil, werr
	}
	s.setRunning(thread)
	return wire.ExecAck{RunState: s.st.runState, StopSeq: s.st.stopSeq}, nil
}

func (s *Session) execFinish(r *request) (any, *wire.Error) {
	req, werr := decode[wire.ExecRequest](r.req.Payload)
	if werr != nil {
		return nil, werr
	}
	if werr := s.checkStopSeq(req.StopSeq); werr != nil {
		return nil, werr
	}
	thread := s.thread(req.Thread)
	frame := req.Frame
	if frame == 0 {
		frame = s.st.selFrame
	}
	if len(s.st.frames) <= 1 {
		return nil, wire.NewError(wire.CodeBadRequest,
			"the outermost frame cannot be finished")
	}
	cmd := fmt.Sprintf("-exec-finish --thread %d --frame %d", thread, frame)
	if _, werr := s.send(r.ctx, cmd); werr != nil {
		return nil, werr
	}
	s.setRunning(thread)
	return wire.ExecAck{RunState: s.st.runState, StopSeq: s.st.stopSeq}, nil
}

// execKill stops the inferior.
//
// There is no -exec-kill: gdb 17.1 answers `^error,msg="Undefined MI command:
// exec-kill",code="undefined-command"`. The console command is the only way,
// which is why this is a semantic request type rather than a passthrough.
func (s *Session) execKill(r *request) (any, *wire.Error) {
	if s.st.runState == wire.RunStateNoProgram {
		return wire.ExecAck{RunState: s.st.runState, StopSeq: s.st.stopSeq}, nil
	}
	if s.st.runState == wire.RunStateRunning {
		// gdb will not kill a running inferior from the console while it is
		// running; interrupt first and let the stop land.
		if _, err := s.client.SendUnlocked(r.ctx, "-exec-interrupt"); err != nil {
			s.logf("interrupt before kill: %v", err)
		}
	}
	if _, werr := s.send(r.ctx, `-interpreter-exec console "kill"`); werr != nil {
		// "The program is not being run." means we already got what we wanted.
		if !strings.Contains(werr.Message, "not being run") {
			return nil, werr
		}
	}
	s.setExited(nil, "")
	return wire.ExecAck{RunState: s.st.runState, StopSeq: s.st.stopSeq}, nil
}

func (s *Session) bpSetSource(r *request) (any, *wire.Error) {
	req, werr := decode[wire.BreakpointRequest](r.req.Payload)
	if werr != nil {
		return nil, werr
	}
	if req.Path == "" || req.Line <= 0 {
		return nil, wire.NewError(wire.CodeBadRequest, "path and a positive line are required")
	}
	if s.files == nil {
		return nil, wire.NewError(wire.CodeInternal, "no project is configured")
	}
	abs, err := s.files.AbsPath(req.Path)
	if err != nil {
		return nil, fsError(err)
	}

	// -f makes the breakpoint pending rather than an error when the location
	// cannot be resolved yet. The address arrives later in a
	// =breakpoint-modified, which is why the mirror is event-driven.
	cmd := "-break-insert -f"
	if req.Temporary {
		cmd += " -t"
	}
	if req.Condition != "" {
		cmd += " -c " + quote(req.Condition)
	}
	cmd += " " + quote(fmt.Sprintf("%s:%d", abs, req.Line))

	rec, werr := s.send(r.ctx, cmd)
	if werr != nil {
		return nil, werr
	}
	bkpt, ok := rec.Results.Tuple("bkpt")
	if !ok {
		return nil, wire.NewError(wire.CodeInternal, "gdb accepted the breakpoint but reported none")
	}
	bp := s.parseBreakpoint(bkpt)
	s.st.ours[bp.Number] = true
	s.st.breakpoints[bp.Number] = bp
	s.broadcastBreakpoints()
	return bp, nil
}

func (s *Session) bpDelete(r *request) (any, *wire.Error) {
	req, werr := decode[wire.BreakpointIDRequest](r.req.Payload)
	if werr != nil {
		return nil, werr
	}
	if req.Number <= 0 {
		return nil, wire.NewError(wire.CodeBadRequest, "number is required")
	}
	if _, werr := s.send(r.ctx, fmt.Sprintf("-break-delete %d", req.Number)); werr != nil {
		return nil, werr
	}
	delete(s.st.breakpoints, req.Number)
	delete(s.st.ours, req.Number)
	s.broadcastBreakpoints()
	return s.breakpointListPayload(), nil
}

func (s *Session) bpSetEnabled(r *request) (any, *wire.Error) {
	req, werr := decode[wire.BreakpointIDRequest](r.req.Payload)
	if werr != nil {
		return nil, werr
	}
	if req.Number <= 0 {
		return nil, wire.NewError(wire.CodeBadRequest, "number is required")
	}
	cmd := "-break-disable"
	if req.Enabled {
		cmd = "-break-enable"
	}
	if _, werr := s.send(r.ctx, fmt.Sprintf("%s %d", cmd, req.Number)); werr != nil {
		return nil, werr
	}
	if bp, ok := s.st.breakpoints[req.Number]; ok {
		bp.Enabled = req.Enabled
		s.st.breakpoints[req.Number] = bp
	}
	s.broadcastBreakpoints()
	return s.breakpointListPayload(), nil
}

func (s *Session) bpList(r *request) (any, *wire.Error) {
	s.reconcileBreakpoints(r.ctx)
	return s.breakpointListPayload(), nil
}

func (s *Session) stackList(r *request) (any, *wire.Error) {
	req, werr := decode[wire.StackListRequest](r.req.Payload)
	if werr != nil {
		return nil, werr
	}
	if werr := s.checkStopSeq(req.StopSeq); werr != nil {
		return nil, werr
	}
	thread := s.thread(req.Thread)
	high := req.High
	if high <= 0 {
		high = maxFrames
	}
	frames, werr := s.fetchFrames(r.ctx, thread, req.Low, high)
	if werr != nil {
		return nil, werr
	}
	if thread == s.st.selThread {
		s.st.frames = frames
	}
	return wire.StackList{StopSeq: s.st.stopSeq, ThreadID: thread, Frames: frames}, nil
}

func (s *Session) frameSelect(r *request) (any, *wire.Error) {
	req, werr := decode[wire.FrameSelectRequest](r.req.Payload)
	if werr != nil {
		return nil, werr
	}
	if werr := s.checkStopSeq(req.StopSeq); werr != nil {
		return nil, werr
	}
	thread := s.thread(req.Thread)
	if req.Frame < 0 {
		return nil, wire.NewError(wire.CodeBadRequest, "frame must not be negative")
	}

	// Programmatic commands always pass --thread/--frame, so this selection is
	// not needed for them. It is issued for the benefit of console commands the
	// user types, which do use gdb's own notion of selection — but only when it
	// would actually change something. Re-selecting the current thread makes gdb
	// send a T packet to the target for nothing, and on a remote stub that is
	// either wasted traffic or, for a minimal stub without T, a puzzling
	// "command not supported" in the user's emulator log.
	if werr := s.selectThreadIfNeeded(r.ctx, thread); werr != nil {
		return nil, werr
	}
	if _, werr := s.send(r.ctx, fmt.Sprintf("-stack-select-frame %d", req.Frame)); werr != nil {
		return nil, werr
	}
	s.st.selThread, s.st.selFrame = thread, req.Frame

	locals, werr := s.fetchLocals(r.ctx, thread, req.Frame)
	if werr != nil {
		return nil, werr
	}
	s.st.locals = locals

	sel := wire.Selection{
		ThreadID: thread,
		Frame:    req.Frame,
		StopSeq:  s.st.stopSeq,
		Frames:   s.st.frames,
		Locals:   locals,
	}
	if f, ok := s.frameAt(req.Frame); ok {
		src := f.Source
		sel.Source = &src
	}
	s.emit(wire.EventSelectionChanged, sel)
	return sel, nil
}

// maxFrames bounds a stack listing. Deep recursion produces stacks in the
// thousands, and nobody reads frame 900.
const maxFrames = 63

func (s *Session) frameAt(level int) (wire.Frame, bool) {
	for _, f := range s.st.frames {
		if f.Level == level {
			return f, true
		}
	}
	return wire.Frame{}, false
}

func (s *Session) thread(requested int) int {
	if requested > 0 {
		return requested
	}
	if s.st.selThread > 0 {
		return s.st.selThread
	}
	return 1
}

// quote renders a string as an MI-safe quoted argument.
func quote(s string) string {
	var sb strings.Builder
	sb.WriteByte('"')
	for i := 0; i < len(s); i++ {
		switch c := s[i]; c {
		case '"', '\\':
			sb.WriteByte('\\')
			sb.WriteByte(c)
		case '\n':
			sb.WriteString(`\n`)
		default:
			sb.WriteByte(c)
		}
	}
	sb.WriteByte('"')
	return sb.String()
}

func fsError(err error) *wire.Error {
	switch {
	case errors.Is(err, srcfs.ErrNotFound):
		return wire.NewError(wire.CodeNotFound, "no such file")
	case errors.Is(err, srcfs.ErrDenied):
		return wire.NewError(wire.CodePathDenied, "path is outside the project root")
	case errors.Is(err, srcfs.ErrIsDir):
		return wire.NewError(wire.CodeBadRequest, "path is a directory")
	default:
		return wire.NewError(wire.CodeInternal, err.Error())
	}
}

// parseBreakpoint turns one MI bkpt tuple into the wire form.
func (s *Session) parseBreakpoint(t mi.Results) wire.Breakpoint {
	num, _ := t.Int("number")
	line, _ := t.Int("line")
	hits, _ := t.Int("times")
	enabled, ok := t.Bool("enabled")
	if !ok {
		enabled = true
	}
	addr := t.Str("addr")
	bp := wire.Breakpoint{
		Number:    num,
		Type:      t.Str("type"),
		Enabled:   enabled,
		Address:   addr,
		Func:      t.Str("func"),
		Line:      line,
		Condition: t.Str("cond"),
		HitCount:  hits,
		Temporary: t.Str("disp") == "del",
		Original:  t.Str("original-location"),
		// gdb writes the literal string "<PENDING>" as the address until it
		// resolves the location.
		Pending: addr == "<PENDING>" || t.Has("pending"),
	}
	if bp.Pending {
		bp.Address = ""
	}
	if full := t.Str("fullname"); full != "" {
		if rel, ok := s.files.RelPath(full); ok && s.files != nil {
			bp.Path = rel
		} else {
			bp.GDBPath = full
		}
	} else if file := t.Str("file"); file != "" {
		bp.GDBPath = file
	}
	return bp
}

// reconcileBreakpoints re-reads the whole list from gdb.
func (s *Session) reconcileBreakpoints(ctx context.Context) {
	rec, werr := s.send(ctx, "-break-list")
	if werr != nil {
		s.logf("-break-list failed: %s", werr.Message)
		return
	}
	table, ok := rec.Results.Get("BreakpointTable")
	if !ok {
		return
	}
	body, ok := table.Get("body")
	if !ok {
		return
	}

	next := map[int]wire.Breakpoint{}
	for _, v := range body.All("bkpt") {
		bp := s.parseBreakpoint(v.Items)
		if bp.Number == 0 {
			continue
		}
		// Filter breakpoints gdb invented. -exec-run --start injects a
		// temporary one at main; showing it would put a marker in the gutter
		// that the user cannot delete because they never created it.
		if bp.Temporary && !s.st.ours[bp.Number] {
			continue
		}
		next[bp.Number] = bp
	}
	s.st.breakpoints = next
	s.broadcastBreakpoints()
}

func (s *Session) breakpointList() []wire.Breakpoint {
	out := make([]wire.Breakpoint, 0, len(s.st.breakpoints))
	for _, bp := range s.st.breakpoints {
		out = append(out, bp)
	}
	// Sorted by number so the UI order is stable across refreshes.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].Number < out[j-1].Number; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

func (s *Session) breakpointListPayload() wire.BreakpointList {
	return wire.BreakpointList{Breakpoints: s.breakpointList()}
}

func (s *Session) broadcastBreakpoints() {
	s.emit(wire.EventBreakpointsChanged, s.breakpointListPayload())
}

func atoiSafe(s string) int {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return 0
	}
	return n
}
