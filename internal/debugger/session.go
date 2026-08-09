// Package debugger is the domain layer: it owns every piece of debug session
// state and is the only thing that decides what MI commands to send.
//
// All state lives behind a single actor goroutine. Nothing is mutex-protected,
// because nothing is touched by two goroutines: browser requests and MI events
// arrive on channels and are processed one at a time, in order. That buys three
// things — no data races by construction, deterministic event ordering, and
// tests that can drive the whole state machine through a scripted fake in
// milliseconds.
package debugger

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/retrocpugeek/gdb-wui/internal/mi"
	"github.com/retrocpugeek/gdb-wui/internal/ptyio"
	"github.com/retrocpugeek/gdb-wui/internal/srcfs"
	"github.com/retrocpugeek/gdb-wui/internal/wire"
)

// Broadcaster receives events destined for every connected browser.
type Broadcaster interface {
	Broadcast(event string, payload any)
}

// Config configures a Session.
type Config struct {
	// MI describes the gdb process to start. Handler is set by New and any
	// value here is ignored — the session must be the handler, and it must be
	// installed before the reader goroutine exists.
	//
	// Taking options rather than a started client is what removes a cycle that
	// is otherwise a data race: the client needs a handler at construction, the
	// handler is the session, and the session needs the client. Wiring one of
	// them up afterwards means a goroutine reads a variable the constructor is
	// still writing.
	MI mi.Options
	// Files is the project browser, used to resolve gdb's paths to
	// root-relative ones.
	Files *srcfs.FS
	// Events receives broadcasts. Required.
	Events Broadcaster
	// Logf receives diagnostics.
	Logf func(format string, args ...any)
	// Version is reported in the hello snapshot.
	Version string
	// GDBVersion is the debugger's version banner, for display. It is read
	// from `gdb --version` by the caller rather than over MI: -gdb-version
	// answers with console records, which are asynchronous and would have to
	// be correlated by timing.
	GDBVersion string
	// MILog enables the raw MI event stream to the browser. It is a developer
	// aid and doubles the event volume, so it is off unless asked for.
	MILog bool
	// CommandTimeout bounds one gdb round-trip. Zero means 20s.
	CommandTimeout time.Duration
	// Decomp configures the optional decompiler. A zero value disables it,
	// which is an ordinary state: Ghidra is a large dependency and most
	// sessions never want one.
	Decomp DecompConfig
}

// Session is one debug session: one gdb, one project, any number of browsers.
type Session struct {
	cfg    Config
	client *mi.Client
	files  *srcfs.FS
	logf   func(string, ...any)

	// capture diverts console output for one internal command. See runConsole.
	capture atomic.Pointer[consoleCapture]

	// decomp is the optional decompiler. It is not in st, because its cold start
	// takes seconds to minutes and must not happen on the actor.
	decomp decomp

	// reqs carries browser requests to the actor.
	reqs chan *request
	// intake is the non-blocking landing pad for MI records; see its type.
	intake *recordQueue

	done     chan struct{}
	closeOne sync.Once
	stopped  chan struct{}

	// snapshot is the only state read outside the actor. The actor publishes an
	// immutable copy after every change so that a newly connected browser can
	// be answered without a round-trip through the loop — which matters because
	// the loop may be blocked on a gdb command for seconds.
	snapshot atomic.Pointer[wire.Hello]

	// Everything below is actor-owned. No locks, and none are needed.
	st state
	// srcCache memoises path resolution. A deep stack asks about the same few
	// files over and over, and every miss is a stat.
	srcCache map[string]wire.SourceRef
	// vars is the varobj registry; see varobj.go.
	vars *varRegistry

	// term is the inferior's terminal, actor-owned. termPtr is the same
	// pointer published for the two request paths that bypass the actor —
	// keystrokes and resizes, which must not wait for a gdb round-trip.
	term    *ptyio.Terminal
	termPtr atomic.Pointer[*ptyio.Terminal]
}

// state is the actor's private world.
type state struct {
	runState string
	stopSeq  uint64

	exePath string
	// stepping is a step-by-line walk in progress. Not a request that blocks:
	// exec commands are acknowledgements and the stop arrives later on this
	// same actor, so a handler that waited for one would deadlock against
	// itself.
	stepping *stepLine

	// exeSHA256 is the loaded executable's hash. It exists for the decompiler
	// mismatch guard: showing a decompilation of a different build than the
	// one being debugged is a confidently wrong answer, and the hash is the
	// only thing that notices.
	exeSHA256 string

	threads   []wire.Thread
	frames    []wire.Frame
	locals    []wire.Variable
	selThread int
	selFrame  int

	lastStopReason string
	exitCode       *int
	exitSignal     string

	// breakpoints is the mirror, keyed by gdb's number.
	breakpoints map[int]wire.Breakpoint
	// ours records the breakpoints this server created. gdb invents its own —
	// -exec-run --start injects a temporary one — and a mirror that shows them
	// would put phantom markers in the gutter.
	ours map[int]bool

	gdbVersion string
	features   []string

	// watches are the user's expressions. They outlive the varobjs behind
	// them, which are deleted wholesale on every re-run.
	watches  []watch
	watchSeq int

	// registerNames is cached per program; it never changes within one.
	registerNames []string

	// symbols is the program's symbol table, cached per program. symbolsRead
	// records that it has been looked at, which is not the same as it being
	// non-empty: a stripped binary legitimately has none, and without the flag
	// every keystroke in the filter box would re-ask gdb for nothing.
	symbols     []wire.Symbol
	symbolsRead bool
	// symbolsDirty records that a shared library came or went since the last
	// stop, so one invalidation can be sent at the stop instead of one per
	// library during the run.
	symbolsDirty bool

	// inferiorPID is the debuggee's process id, from =thread-group-started. It
	// is what inferior.signal targets.
	inferiorPID int

	// substitutions and sourceDirs are what gdb has been told about where
	// source lives, so the UI can show it and duplicates can be avoided.
	substitutions []wire.Substitution
	sourceDirs    []string

	// remoteTarget records that gdb is attached to something this server did
	// not start — a gdbserver, an emulator's stub. It changes what shutdown is
	// allowed to do: killing a target we merely connected to destroys somebody
	// else's session.
	remoteTarget bool
	remoteAddr   string

	// gdbSelThread is the thread gdb itself currently has selected. Tracked so
	// -thread-select can be skipped when it would be a no-op: it makes gdb send
	// a T ("is thread alive?") packet to the target, which is pure noise on a
	// single-threaded one and which minimal stubs log as unsupported.
	gdbSelThread int
}

// request is one browser request awaiting the actor.
type request struct {
	ctx   context.Context
	req   wire.Request
	reply chan result
}

type result struct {
	payload any
	err     *wire.Error
}

const defaultCommandTimeout = 20 * time.Second

// New starts gdb and the session that drives it. The caller must call Close.
func New(ctx context.Context, cfg Config) (*Session, error) {
	if cfg.Events == nil {
		return nil, errors.New("debugger: Events is required")
	}
	if cfg.CommandTimeout == 0 {
		cfg.CommandTimeout = defaultCommandTimeout
	}
	s := &Session{
		cfg:     cfg,
		files:   cfg.Files,
		logf:    cfg.Logf,
		reqs:    make(chan *request),
		intake:  newRecordQueue(),
		done:    make(chan struct{}),
		stopped: make(chan struct{}),
	}
	if s.logf == nil {
		s.logf = func(string, ...any) {}
	}

	// The session exists before the client, so the handler is a bound method on
	// a fully constructed object. HandleRecord touches only the intake queue,
	// which is already initialised, so a record arriving during Start is safe.
	opts := cfg.MI
	opts.Handler = s.HandleRecord
	if opts.Logf == nil {
		opts.Logf = s.logf
	}
	client, err := mi.Start(ctx, opts)
	if err != nil {
		return nil, fmt.Errorf("debugger: starting gdb: %w", err)
	}
	s.client = client

	s.vars = newVarRegistry()
	s.st = state{
		runState:    wire.RunStateNoProgram,
		breakpoints: map[int]wire.Breakpoint{},
		ours:        map[int]bool{},
		selThread:   1,
		features:    client.Features(),
		gdbVersion:  cfg.GDBVersion,
	}
	s.publish()

	go s.run()
	go s.watchGDB()
	return s, nil
}

// HandleRecord is the mi.Handler. It must never block: see recordQueue.
//
// A capture, when one is active, takes console text here rather than from the
// actor's queue. That is the only place it can be taken synchronously: send()
// blocks the actor until the reply arrives, so records produced by the command
// sit in the queue until afterwards, and a command whose *answer* is its
// console output could not be read at all. Draining the queue mid-handler
// instead would run a stop event from inside another request.
func (s *Session) HandleRecord(rec mi.Record) {
	if rec.Type == mi.RecConsole {
		if buf := s.capture.Load(); buf != nil {
			buf.mu.Lock()
			buf.text.WriteString(rec.Text)
			buf.mu.Unlock()
			// Swallowed on purpose: this is gdb-wui asking gdb something for
			// its own reasons, and the user did not type it.
			return
		}
	}
	s.intake.push(rec)
}

// consoleCapture collects the console output of one internal command.
type consoleCapture struct {
	mu   sync.Mutex
	text strings.Builder
}

// runConsole issues a console command and returns what it printed.
//
// For the handful of things gdb will only say in prose. `info files` is the
// one that matters: it names the runtime entry point, which is the only way to
// locate a stripped position-independent executable, since it has no symbol
// either side could anchor to.
func (s *Session) runConsole(ctx context.Context, line string) (string, *wire.Error) {
	buf := &consoleCapture{}
	if !s.capture.CompareAndSwap(nil, buf) {
		return "", wire.NewError(wire.CodeBusy, "another console capture is in flight")
	}
	defer s.capture.Store(nil)

	if _, werr := s.send(ctx, "-interpreter-exec console "+quote(line)); werr != nil {
		return "", werr
	}
	buf.mu.Lock()
	defer buf.mu.Unlock()
	return buf.text.String(), nil
}

// Close stops the actor and the gdb process.
func (s *Session) Close(ctx context.Context) error {
	var err error
	s.closeOne.Do(func() {
		close(s.done)
		<-s.stopped
		s.closeTerminal()

		// A remote target is somebody else's process. Detach so it survives us,
		// and stop the client's teardown from killing it — gdb kills a
		// `target remote` connection on plain quit, not only on an explicit
		// kill.
		if s.st.remoteTarget && s.client.DeadErr() == nil {
			detach, cancel := context.WithTimeout(ctx, 3*time.Second)
			if _, werr := s.send(detach, "-target-detach"); werr != nil {
				s.logf("detaching from %s: %s", s.st.remoteAddr, werr.Message)
			} else {
				s.logf("detached from %s", s.st.remoteAddr)
			}
			cancel()
			s.client.SetKillOnClose(false)
		}
		// Before gdb, because the decompiler is a separate process with its own
		// JVM and a 2 GB heap ceiling: leaving one behind is the worst kind of
		// leak, invisible until the machine swaps.
		s.closeDecomp()
		err = s.client.Close(ctx)
	})
	return err
}

func (s *Session) run() {
	defer close(s.stopped)
	for {
		select {
		case <-s.done:
			return
		case req := <-s.reqs:
			s.serve(req)
		case <-s.intake.ready():
			for _, rec := range s.intake.drain() {
				s.onRecord(rec)
			}
		}
	}
}

// watchGDB turns the client dying into an event, once.
func (s *Session) watchGDB() {
	select {
	case <-s.done:
		return
	case <-s.client.Dead():
	}
	reason := "gdb exited"
	if err := s.client.DeadErr(); err != nil {
		reason = err.Error()
	}
	s.emitOffActor(wire.EventGDBDead, wire.GDBDead{
		Reason: reason,
		Stderr: s.client.StderrTail(),
	})
}

// Snapshot implements hub.Session. It reads the published copy rather than
// asking the actor, so a browser can connect and repaint while the actor is
// mid-command.
func (s *Session) Snapshot() wire.Hello {
	if snap := s.snapshot.Load(); snap != nil {
		return *snap
	}
	return wire.Hello{Protocol: wire.Protocol, RunState: wire.RunStateNoProgram}
}

// Handle implements hub.Session.
func (s *Session) Handle(ctx context.Context, req wire.Request) (any, *wire.Error) {
	// exec.pause must not queue behind the actor. The actor is frequently
	// blocked in a gdb round-trip, and while the inferior is running that is
	// exactly when the user presses Pause — routing it through the loop would
	// mean the button only works when it is not needed.
	switch req.Type {
	case wire.TypeExecPause:
		return s.pause(ctx)
	case wire.TypeInferiorStdin:
		// A keystroke must not queue behind a gdb round-trip: the actor is
		// frequently blocked, and typing that waits for it feels like a hang.
		// Writing to the pty touches no session state.
		return s.inferiorStdin(ctx, req)
	case wire.TypeInferiorResize:
		return s.inferiorResize(ctx, req)
	}

	r := &request{ctx: ctx, req: req, reply: make(chan result, 1)}
	select {
	case s.reqs <- r:
	case <-ctx.Done():
		return nil, wire.NewError(wire.CodeTimeout, "request cancelled before it was accepted")
	case <-s.done:
		return nil, wire.NewError(wire.CodeGDBDead, "session is closed")
	}

	select {
	case res := <-r.reply:
		return res.payload, res.err
	case <-ctx.Done():
		return nil, wire.NewError(wire.CodeTimeout, "request timed out")
	case <-s.done:
		return nil, wire.NewError(wire.CodeGDBDead, "session is closed")
	}
}

// serve dispatches one request inside the actor.
func (s *Session) serve(r *request) {
	payload, err := s.dispatch(r)
	select {
	case r.reply <- result{payload, err}:
	default:
		// The caller gave up. Its request still ran, which is correct: a
		// breakpoint the user asked for should exist even if the response was
		// too slow to be read.
	}
	s.publish()
}

func (s *Session) dispatch(r *request) (any, *wire.Error) {
	if err := s.gate(r.req.Type); err != nil {
		return nil, err
	}
	switch r.req.Type {
	case wire.TypeSessionHello, wire.TypeSessionInfo:
		return s.buildSnapshot(), nil
	case wire.TypeSessionPing:
		return map[string]any{"pong": true}, nil
	case wire.TypeSessionRestart:
		return s.restartGDB(r)

	case wire.TypeExeLoad:
		return s.exeLoad(r)

	case wire.TypeExecRun:
		return s.execRun(r)
	case wire.TypeExecContinue:
		return s.execResume(r, "-exec-continue")
	case wire.TypeExecStep:
		return s.execResume(r, "-exec-step")
	case wire.TypeExecNext:
		return s.execResume(r, "-exec-next")
	case wire.TypeExecFinish:
		return s.execFinish(r)
	case wire.TypeExecKill:
		return s.execKill(r)

	case wire.TypeBpSetSource:
		return s.bpSetSource(r)
	case wire.TypeBpSetAddress:
		return s.bpSetAddress(r)
	case wire.TypeBpDelete:
		return s.bpDelete(r)
	case wire.TypeBpSetEnabled:
		return s.bpSetEnabled(r)
	case wire.TypeBpList:
		return s.bpList(r)

	case wire.TypeStackList:
		return s.stackList(r)
	case wire.TypeFrameSelect:
		return s.frameSelect(r)

	case wire.TypeVarsLocals:
		return s.varsLocals(r)
	case wire.TypeVarsExpand:
		return s.varsExpand(r)
	case wire.TypeVarsAssign:
		return s.varsAssign(r)

	case wire.TypeWatchAdd:
		return s.watchAdd(r)
	case wire.TypeWatchRemove:
		return s.watchRemove(r)
	case wire.TypeWatchList:
		return s.watchListRequest(r)

	case wire.TypeRegsNames:
		return s.regsNames(r)
	case wire.TypeRegsValues:
		return s.regsValues(r)
	case wire.TypeRegsWrite:
		return s.regsWrite(r)

	case wire.TypeConsoleExec:
		return s.consoleExec(r)
	case wire.TypeConsoleComplete:
		return s.consoleComplete(r)

	case wire.TypeInferiorSignal:
		return s.inferiorSignal(r)

	case wire.TypeThreadsList:
		return s.threadsList(r)
	case wire.TypeThreadSelect:
		return s.threadSelect(r)

	case wire.TypeDisasmFunction:
		return s.disasmFunction(r)
	case wire.TypeDisasmRange:
		return s.disasmRange(r)

	case wire.TypeExecStepLine:
		return s.execStepLine(r)
	case wire.TypeExecStepI:
		return s.execStepI(r)
	case wire.TypeExecNextI:
		return s.execNextI(r)

	case wire.TypeMemRead:
		return s.memRead(r)
	case wire.TypeEvalExpr:
		return s.evalExpr(r)
	case wire.TypeMemSymbols:
		return s.memSymbols(r)
	case wire.TypeMemWrite:
		return s.memWrite(r)

	case wire.TypeGotoLocate:
		return s.gotoLocate(r)

	case wire.TypeSymbolsList:
		return s.symbolsList(r)
	case wire.TypeSymbolsLoad:
		return s.symbolsLoad(r)

	case wire.TypeDecompStatus:
		return s.decompStatus(r)
	case wire.TypeDecompFunction:
		return s.decompFunction(r)
	case wire.TypeDecompNames:
		return s.decompNames(r)
	case wire.TypeDecompRename:
		return s.decompRename(r)
	case wire.TypeDecompRetype:
		return s.decompRetype(r)
	case wire.TypeDecompUndo:
		return s.decompUndoLast(r)

	case wire.TypePathSubstitute:
		return s.pathSubstitute(r)
	case wire.TypePathAddDir:
		return s.pathAddDir(r)
	case wire.TypePathList:
		return s.pathList(r)
	}
	return nil, wire.NewError(wire.CodeUnsupported,
		fmt.Sprintf("%q is not supported by this server", r.req.Type))
}

// gate is the run-state gate.
//
// gdb rejects most commands while the inferior runs, with messages like
// "Selected thread is running." — verified against gdb 17.1 and pinned by an
// integration test. Forwarding those requests would surface gdb's wording as
// an error in the UI; refusing them here turns an undocumented behaviour into a
// documented contract, and costs a round-trip less.
func (s *Session) gate(typ string) *wire.Error {
	// Restarting is the one thing that must work *because* gdb is dead.
	if typ == wire.TypeSessionRestart {
		return nil
	}
	if s.client.DeadErr() != nil {
		return wire.NewError(wire.CodeGDBDead, "gdb is not running")
	}

	switch typ {
	// Always allowed, whatever the inferior is doing.
	case wire.TypeSessionHello, wire.TypeSessionInfo, wire.TypeSessionPing,
		wire.TypeSessionRestart,
		wire.TypeExecPause, wire.TypeExecKill, wire.TypeBpList,
		// Listing and removing watches is bookkeeping over expressions the
		// user typed; neither needs the inferior to be stopped, and refusing
		// them while running would strand the panel.
		wire.TypeWatchList, wire.TypeWatchRemove,
		// The console is the escape hatch: refusing it while running would
		// remove the one way out of a situation the UI does not model.
		wire.TypeConsoleExec, wire.TypeConsoleComplete,
		// Typing into the program and signalling it are what the terminal is for,
		// and both only make sense while it runs.
		wire.TypeInferiorStdin, wire.TypeInferiorSignal, wire.TypeInferiorResize,
		// Telling gdb where source lives is configuration, not a state query,
		// and is exactly what a user reaches for when a frame has no source.
		wire.TypePathSubstitute, wire.TypePathAddDir, wire.TypePathList,
		// The symbol table is a property of the file, not of the inferior.
		// Refusing to search it while the program runs would disable the one
		// panel that has a useful answer at that moment. Loading is allowed
		// then too: telling gdb what the addresses mean is configuration, and
		// it is exactly what someone does after attaching to a running target.
		wire.TypeSymbolsList, wire.TypeSymbolsLoad,
		// Decompilation is a property of the file, not of the inferior, and
		// the status request must answer even when nothing is loaded — it is
		// how the UI learns the feature exists at all.
		wire.TypeDecompStatus, wire.TypeDecompFunction,
		// Naming frames is a question about the program's code rather than
		// about the process, and the panel that asks has already drawn the
		// stack it wants to correct.
		wire.TypeDecompNames,
		// Naming things in the decompiler's own database changes nothing about
		// the inferior, and reading a running program's disassembly is exactly
		// when someone works out what a function is called.
		wire.TypeDecompRename, wire.TypeDecompRetype, wire.TypeDecompUndo:
		return nil
	}

	if s.st.runState == wire.RunStateRunning {
		return wire.NewError(wire.CodeBusy,
			"the inferior is running; pause it first")
	}

	switch typ {
	case wire.TypeExecContinue, wire.TypeExecStep, wire.TypeExecNext,
		wire.TypeExecFinish, wire.TypeStackList, wire.TypeFrameSelect,
		wire.TypeVarsLocals, wire.TypeVarsExpand, wire.TypeWatchAdd,
		wire.TypeRegsNames, wire.TypeRegsValues,
		wire.TypeThreadsList, wire.TypeThreadSelect,
		wire.TypeDisasmFunction, wire.TypeDisasmRange,
		wire.TypeExecStepI, wire.TypeExecNextI, wire.TypeExecStepLine,
		wire.TypeMemRead, wire.TypeEvalExpr, wire.TypeMemSymbols,
		// The writes sit with the reads: gdb needs a stopped inferior to
		// resolve an expression in a frame, and a value written into a
		// half-executed instruction stream is not one anybody asked for.
		wire.TypeVarsAssign, wire.TypeRegsWrite, wire.TypeMemWrite:
		if s.st.runState != wire.RunStateStopped {
			return wire.NewError(wire.CodeNotReady,
				"no stopped inferior; load a program and run it first")
		}
	case wire.TypeExecRun:
		if s.st.exePath == "" {
			return wire.NewError(wire.CodeNotReady, "no program is loaded")
		}
	case wire.TypeBpSetSource, wire.TypeBpSetAddress,
		wire.TypeBpDelete, wire.TypeBpSetEnabled,
		// Locating a name needs symbols, not a running process: opening the
		// source at a function is a reasonable first thing to do, before
		// deciding where to put a breakpoint.
		wire.TypeGotoLocate:
		if s.st.exePath == "" {
			return wire.NewError(wire.CodeNotReady, "no program is loaded")
		}
	}
	return nil
}

// send issues one command with the session's timeout, from inside the actor.
func (s *Session) send(ctx context.Context, cmd string) (mi.Record, *wire.Error) {
	if s.cfg.MILog {
		s.emit(wire.EventMI, wire.MILogEntry{Direction: "out", Text: cmd})
	}
	cctx, cancel := context.WithTimeout(ctx, s.cfg.CommandTimeout)
	defer cancel()

	rec, err := s.client.Send(cctx, cmd)
	if err == nil {
		return rec, nil
	}
	return rec, s.wireError(cmd, err)
}

func (s *Session) wireError(cmd string, err error) *wire.Error {
	if gerr, ok := mi.AsError(err); ok {
		return wire.NewError(wire.CodeGDBError, gerr.Msg)
	}
	if errors.Is(err, mi.ErrDead) {
		return wire.NewError(wire.CodeGDBDead, "gdb is not running")
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return wire.NewError(wire.CodeTimeout,
			fmt.Sprintf("gdb did not answer %q in time", cmd))
	}
	s.logf("command %q failed: %v", cmd, err)
	return wire.NewError(wire.CodeInternal, err.Error())
}

// checkStopSeq drops a request aimed at a stop that has already been
// superseded.
//
// This one mechanism covers three bugs at once: a double-clicked step, a panel
// refreshing against state that changed while its request was in flight, and
// (from M4) a variable tree built from a frame that no longer exists.
func (s *Session) checkStopSeq(got uint64) *wire.Error {
	if got == 0 || got == s.st.stopSeq {
		return nil
	}
	return wire.NewError(wire.CodeBusy,
		fmt.Sprintf("stale request: it targets stop %d, the current stop is %d",
			got, s.st.stopSeq))
}

// publish snapshots actor state for readers outside the loop.
func (s *Session) publish() {
	snap := s.buildSnapshot()
	s.snapshot.Store(&snap)
}

// emit refreshes the snapshot and then broadcasts. Actor goroutine only.
//
// The order matters and is not an optimisation detail. serve() publishes only
// after dispatch returns, so a handler that broadcast
// directly would announce a change the snapshot did not yet carry. A client
// acting on the event — or a browser connecting in that window and getting
// `hello` — would be told the program had loaded and simultaneously handed a
// snapshot saying none had. Rare, but it is the sort of disagreement that
// costs an afternoon to track down; CI caught it as a flaky exePath.
//
// Building a snapshot reads the actor's state, so this is safe only on the
// actor goroutine. Anything else must use emitOffActor.
func (s *Session) emit(event string, payload any) {
	s.publish()
	s.cfg.Events.Broadcast(event, payload)
}

// emitOffActor broadcasts from a goroutine that does not own the state.
//
// It cannot publish, because building a snapshot means reading everything the
// actor owns and nothing here holds that goroutine still. That is a real
// constraint rather than a caution: routing the terminal pump through emit
// tripped the race detector immediately.
//
// Only valid for events whose payload is self-contained and absent from the
// snapshot — the debuggee's terminal output, and gdb having died. An event
// that carries session state must be emitted by the actor.
func (s *Session) emitOffActor(event string, payload any) {
	s.cfg.Events.Broadcast(event, payload)
}

func (s *Session) buildSnapshot() wire.Hello {
	h := wire.Hello{
		Protocol:       wire.Protocol,
		Server:         s.cfg.Version,
		GDBVersion:     s.st.gdbVersion,
		Features:       s.st.features,
		RunState:       s.st.runState,
		StopSeq:        s.st.stopSeq,
		ExePath:        s.st.exePath,
		Breakpoints:    s.breakpointList(),
		Threads:        append([]wire.Thread(nil), s.st.threads...),
		LastStopReason: s.st.lastStopReason,
	}
	if s.files != nil {
		h.ProjectRoot = s.files.Abs()
	}
	if s.st.remoteTarget {
		h.Remote = &wire.RemoteTarget{Connected: true, Address: s.st.remoteAddr}
	}
	if s.st.runState == wire.RunStateStopped {
		h.Frames = append([]wire.Frame(nil), s.st.frames...)
		h.Locals = append([]wire.Variable(nil), s.st.locals...)
		h.Selection = &wire.Selection{
			ThreadID: s.st.selThread,
			Frame:    s.st.selFrame,
			StopSeq:  s.st.stopSeq,
		}
	}
	return h
}

// pause runs outside the actor. It writes straight to gdb.
func (s *Session) pause(ctx context.Context) (any, *wire.Error) {
	if s.client.DeadErr() != nil {
		return nil, wire.NewError(wire.CodeGDBDead, "gdb is not running")
	}
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	// SendUnlocked, because the actor may be holding the command semaphore
	// waiting for something that will not finish until the inferior stops.
	if _, err := s.client.SendUnlocked(cctx, "-exec-interrupt"); err != nil {
		if gerr, ok := mi.AsError(err); ok {
			// "not being run" is the common case of pressing Pause when
			// nothing is running. It is not worth an error dialog.
			if strings.Contains(gerr.Msg, "not being run") {
				return map[string]any{"paused": false, "reason": gerr.Msg}, nil
			}
			return nil, wire.NewError(wire.CodeGDBError, gerr.Msg)
		}
		return nil, s.wireError("-exec-interrupt", err)
	}
	// The stop itself arrives as an event; this is only an acknowledgement.
	return map[string]any{"paused": true}, nil
}

// recordQueue is an unbounded landing pad for MI records.
//
// Unbounded is a deliberate and slightly uncomfortable choice. The actor
// processes events and issues commands from the same goroutine, so while it
// waits for a reply it is not draining events. If this queue could block, the
// mi client's dispatch goroutine would block on it, then the mi reader would
// block on its own queue, and the reply the actor is waiting for would never be
// read — a deadlock that needs a specific interleaving to reproduce and would
// look like a hang in the field.
//
// Bounding it is not an option either: dropping a *stopped desynchronises the
// UI permanently. So it grows, and the backpressure that keeps it finite lives
// one layer down, in the mi client's own bounded queue, which throttles gdb
// itself.
type recordQueue struct {
	mu     sync.Mutex
	items  []mi.Record
	signal chan struct{}
}

func newRecordQueue() *recordQueue {
	return &recordQueue{signal: make(chan struct{}, 1)}
}

func (q *recordQueue) push(rec mi.Record) {
	q.mu.Lock()
	q.items = append(q.items, rec)
	q.mu.Unlock()
	select {
	case q.signal <- struct{}{}:
	default:
	}
}

func (q *recordQueue) ready() <-chan struct{} { return q.signal }

func (q *recordQueue) drain() []mi.Record {
	q.mu.Lock()
	defer q.mu.Unlock()
	items := q.items
	q.items = nil
	return items
}

// restartGDB replaces a dead gdb with a fresh one.
//
// Explicit, never automatic. gdb dying means something went wrong — a crash, an
// OOM kill, a `kill -9` — and silently starting another would hide that while
// throwing away the breakpoints and program state the user had. So the UI
// offers a button and this is what it calls.
//
// Breakpoints and watches are re-created from the mirror, because those are the
// user's work and re-typing them is the part that would actually hurt.
func (s *Session) restartGDB(r *request) (any, *wire.Error) {
	if s.client.DeadErr() == nil {
		return nil, wire.NewError(wire.CodeBadRequest,
			"gdb is still running; kill the program instead of restarting the debugger")
	}

	// Remember the user's work before the old state is discarded.
	oldBreakpoints := s.breakpointList()
	oldWatches := append([]watch(nil), s.st.watches...)
	exePath := s.st.exePath

	opts := s.cfg.MI
	opts.Handler = s.HandleRecord
	if opts.Logf == nil {
		opts.Logf = s.logf
	}
	client, err := mi.Start(r.ctx, opts)
	if err != nil {
		return nil, wire.NewError(wire.CodeGDBDead, "could not start gdb: "+err.Error())
	}

	old := s.client
	s.client = client
	go func() {
		// The old process is already gone; this only reaps its goroutines.
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = old.Close(ctx)
	}()

	// A brand-new gdb knows nothing: reset everything derived from the old one.
	s.vars = newVarRegistry()
	s.srcCache = nil
	s.st.runState = wire.RunStateNoProgram
	s.st.threads, s.st.frames, s.st.locals = nil, nil, nil
	s.st.breakpoints = map[int]wire.Breakpoint{}
	s.st.ours = map[int]bool{}
	s.st.registerNames = nil
	s.st.symbols, s.st.symbolsRead = nil, false
	s.st.substitutions, s.st.sourceDirs = nil, nil
	s.st.inferiorPID = 0
	s.st.features = client.Features()
	s.st.watches = oldWatches
	// The terminal belonged to the old process group.
	if s.term != nil {
		_ = s.term.Close()
		s.term = nil
		s.termPtr.Store(nil)
	}

	// Re-load the program, then re-create the breakpoints the user had.
	var restored int
	if exePath != "" {
		payload, _ := json.Marshal(wire.ExeLoadRequest{Path: exePath})
		if _, werr := s.dispatch(&request{
			ctx: r.ctx,
			req: wire.Request{Type: wire.TypeExeLoad, Payload: payload},
		}); werr != nil {
			s.logf("re-loading %s after restart: %s", exePath, werr.Message)
		} else {
			for _, bp := range oldBreakpoints {
				if bp.Path == "" || bp.Line == 0 {
					continue
				}
				payload, _ := json.Marshal(wire.BreakpointRequest{
					Path: bp.Path, Line: bp.Line, Condition: bp.Condition,
				})
				if _, werr := s.dispatch(&request{
					ctx: r.ctx,
					req: wire.Request{Type: wire.TypeBpSetSource, Payload: payload},
				}); werr == nil {
					restored++
				}
			}
		}
	}

	s.emit(wire.EventVarsInvalidated, map[string]any{})
	s.broadcastBreakpoints()
	go s.watchGDB()

	return map[string]any{
		"restarted":           true,
		"exePath":             exePath,
		"breakpointsRestored": restored,
	}, nil
}
