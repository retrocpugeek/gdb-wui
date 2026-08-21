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
	// Hashed here rather than on demand: the decompiler's mismatch guard needs
	// it, and reading the file once at load time is cheaper and more truthful
	// than reading it later, when it may have been rebuilt underneath us.
	s.st.exeSHA256 = s.hashExe(req.Path)
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

// execRunTo runs the program until it reaches one place.
//
// A temporary breakpoint and a resume. gdb has `until` and `advance`, and
// neither is this: both stop when the current frame returns, so running to a
// line in a function that has not been called yet — the ordinary case when
// reading unfamiliar code — would stop somewhere else and call it done. A
// breakpoint is reached wherever it is, in whatever frame, which is what the
// menu item claims.
//
// The breakpoint is temporary and it is ours, so it appears in the Breakpoints
// pane while it lasts and gdb deletes it on the hit. A run that never reaches
// the place leaves it there, visible and deletable, rather than arming
// something invisible for the next run.
func (s *Session) execRunTo(r *request) (any, *wire.Error) {
	req, werr := decode[wire.ExecRunToRequest](r.req.Payload)
	if werr != nil {
		return nil, werr
	}
	spec, werr := s.runToSpec(r, req)
	if werr != nil {
		return nil, werr
	}
	// Only meaningful from a stop: the stop the client was looking at is the
	// one whose frame it means to continue from.
	if s.st.runState == wire.RunStateStopped {
		if werr := s.checkStopSeq(req.StopSeq); werr != nil {
			return nil, werr
		}
	}

	bp, werr := s.insertBreakpoint(r, breakpointSpec{location: spec, temporary: true})
	if werr != nil {
		return nil, werr
	}

	// From a stop this is a continue; with nothing running it is a run, which
	// is what makes this a way to start a session at a line rather than at
	// main. Both mirror execRun and execResume, including the varobj and
	// terminal handling a fresh process needs.
	if s.st.runState == wire.RunStateStopped {
		thread := s.thread(req.Thread)
		cmd := fmt.Sprintf("-exec-continue --thread %d", thread)
		if _, werr := s.send(r.ctx, cmd); werr != nil {
			s.dropBreakpoint(r, bp.Number)
			return nil, werr
		}
		s.setRunning(thread)
	} else {
		s.ensureTerminal(r.ctx)
		s.deleteAllVarobjs(r.ctx)
		if _, werr := s.send(r.ctx, "-exec-run"); werr != nil {
			s.dropBreakpoint(r, bp.Number)
			return nil, werr
		}
		s.setRunning(0)
	}
	return wire.ExecAck{RunState: s.st.runState, StopSeq: s.st.stopSeq}, nil
}

// runToSpec turns the request's place into a location gdb takes.
func (s *Session) runToSpec(r *request, req wire.ExecRunToRequest) (string, *wire.Error) {
	loc := strings.TrimSpace(req.Location)
	if (req.Path == "") == (loc == "") {
		return "", wire.NewError(wire.CodeBadRequest,
			"give either a path and a line, or a location")
	}
	if loc != "" {
		return s.locationSpec(r, loc), nil
	}
	if req.Line <= 0 {
		return "", wire.NewError(wire.CodeBadRequest, "a positive line is required")
	}
	if s.files == nil {
		return "", wire.NewError(wire.CodeInternal, "no project is configured")
	}
	abs, err := s.files.AbsPath(req.Path)
	if err != nil {
		return "", fsError(err)
	}
	return fmt.Sprintf("%s:%d", abs, req.Line), nil
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

	// Pending, because a line in a shared library that has not loaded yet is a
	// breakpoint someone means to keep; see insertBreakpoint.
	bp, werr := s.insertBreakpoint(r, breakpointSpec{
		location:  fmt.Sprintf("%s:%d", abs, req.Line),
		temporary: req.Temporary,
		condition: req.Condition,
		pending:   true,
	})
	if werr != nil {
		return nil, werr
	}
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

// bpSetAddress sets a breakpoint by address or by symbol.
//
// The counterpart to bpSetSource for everything without source: a decompiled
// line, a disassembly row, a function in the symbol pane. In a stripped binary
// it is the only way to break at all.
//
// A location that is not a bare number goes to gdb verbatim, so a function name
// keeps gdb's prologue skipping — `break process_packet` stops at entry+24,
// past the register spills, which is where a user means. Prefixing a name with
// `*` would defeat that and stop on the first instruction instead.
func (s *Session) bpSetAddress(r *request) (any, *wire.Error) {
	req, werr := decode[wire.BreakpointAddressRequest](r.req.Payload)
	if werr != nil {
		return nil, werr
	}
	loc := strings.TrimSpace(req.Location)
	if loc == "" {
		return nil, wire.NewError(wire.CodeBadRequest, "location is required")
	}

	spec := s.locationSpec(r, loc)
	// A name gdb cannot resolve becomes a pending breakpoint below, which is
	// right for a shared library that has not loaded yet and wrong when there
	// is no program at all: nothing will ever define the symbol, so it would
	// sit there looking set and never fire.
	//
	// gdb decides whether the name is known, not exePath, because attaching to
	// a process gives gdb a symbol table that this session never recorded a
	// path for — `break main` has to keep working there.
	//
	// An address is exempt from the question. It needs no symbols at all,
	// which is why this is a check on the location rather than a rule about
	// the session: attached to an emulator running a raw image, gdb has no
	// file and an address is the only location there is.
	if s.st.exePath == "" && !strings.HasPrefix(spec, "*") && !s.gdbKnowsSymbol(r, spec) {
		return nil, wire.NewError(wire.CodeNotReady, fmt.Sprintf(
			"no program is loaded and gdb has no symbol called %s; "+
				"break at an address instead", loc))
	}

	bp, werr := s.insertBreakpoint(r, breakpointSpec{
		location:  spec,
		temporary: req.Temporary,
		condition: req.Condition,
		pending:   true,
	})
	if werr != nil {
		return nil, werr
	}
	return bp, nil
}

// locationSpec turns an address or a name into something gdb resolves.
//
// An address needs the `*` form and a name must not have it. A decompiler name
// — FUN_0010e2dc, or whatever it has been renamed to — is neither: gdb has
// never heard of it, so it is resolved to an address here. Left alone it would
// become a *pending* breakpoint, which is right for a shared library that has
// not loaded yet and wrong for a name nothing will ever define: the breakpoint
// would sit there looking set and never fire.
func (s *Session) locationSpec(r *request, loc string) string {
	if _, err := parseAddress(loc); err == nil {
		return "*" + loc
	}
	if plausibleDecompName(loc) && !s.gdbKnowsSymbol(r, loc) {
		if addr, ok := s.decompAddressOf(r, loc); ok {
			return fmt.Sprintf("*0x%x", addr)
		}
	}
	return loc
}

// breakpointSpec is one breakpoint to insert.
type breakpointSpec struct {
	// location is in gdb's own vocabulary: `file:line`, `*0xaddr`, or a name.
	location  string
	temporary bool
	condition string
	// pending allows a location gdb cannot resolve yet, which is right for a
	// breakpoint someone is placing and wrong for one the server is about to
	// run to: a run-to whose location never resolves would run the program to
	// completion instead of saying it could not find the place.
	pending bool
}

// insertBreakpoint inserts one breakpoint and puts it in the mirror.
//
// Shared by the two ways of naming a place and by exec.runTo, so that all three
// register the breakpoint as ours — which is what keeps a temporary one
// visible, since the mirror hides temporary breakpoints gdb invented for
// itself.
func (s *Session) insertBreakpoint(r *request, spec breakpointSpec) (wire.Breakpoint, *wire.Error) {
	cmd := "-break-insert"
	if spec.pending {
		cmd += " -f"
	}
	if spec.temporary {
		cmd += " -t"
	}
	if spec.condition != "" {
		cmd += " -c " + quote(spec.condition)
	}
	cmd += " " + quote(spec.location)

	rec, werr := s.send(r.ctx, cmd)
	if werr != nil {
		return wire.Breakpoint{}, werr
	}
	bkpt, ok := rec.Results.Tuple("bkpt")
	if !ok {
		return wire.Breakpoint{}, wire.NewError(wire.CodeInternal,
			"gdb accepted the breakpoint but reported none")
	}
	bp := s.parseBreakpoint(bkpt)
	s.st.ours[bp.Number] = true
	s.st.breakpoints[bp.Number] = bp
	s.broadcastBreakpoints()
	return bp, nil
}

// dropBreakpoint removes one without reporting failure.
//
// For the cleanup path: a run-to that could not resume must not leave its
// temporary breakpoint behind, and the reason the resume failed is what the
// caller is about to report.
func (s *Session) dropBreakpoint(r *request, number int) {
	if _, werr := s.send(r.ctx, fmt.Sprintf("-break-delete %d", number)); werr != nil {
		s.logf("removing the run-to breakpoint %d: %s", number, werr.Message)
		return
	}
	delete(s.st.breakpoints, number)
	delete(s.st.ours, number)
	s.broadcastBreakpoints()
}
