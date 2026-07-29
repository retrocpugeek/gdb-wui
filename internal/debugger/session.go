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
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/retrocpugeek/gdb-wui/internal/mi"
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
}

// Session is one debug session: one gdb, one project, any number of browsers.
type Session struct {
	cfg    Config
	client *mi.Client
	files  *srcfs.FS
	logf   func(string, ...any)

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
}

// state is the actor's private world.
type state struct {
	runState string
	stopSeq  uint64

	exePath string

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
func (s *Session) HandleRecord(rec mi.Record) { s.intake.push(rec) }

// Close stops the actor and the gdb process.
func (s *Session) Close(ctx context.Context) error {
	var err error
	s.closeOne.Do(func() {
		close(s.done)
		<-s.stopped
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
	s.cfg.Events.Broadcast(wire.EventGDBDead, wire.GDBDead{
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
	if req.Type == wire.TypeExecPause {
		return s.pause(ctx)
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
	if s.client.DeadErr() != nil {
		return wire.NewError(wire.CodeGDBDead, "gdb is not running")
	}

	switch typ {
	// Always allowed, whatever the inferior is doing.
	case wire.TypeSessionHello, wire.TypeSessionInfo, wire.TypeSessionPing,
		wire.TypeExecPause, wire.TypeExecKill, wire.TypeBpList,
		// Listing and removing watches is bookkeeping over expressions the
		// user typed; neither needs the inferior to be stopped, and refusing
		// them while running would strand the panel.
		wire.TypeWatchList, wire.TypeWatchRemove:
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
		wire.TypeRegsNames, wire.TypeRegsValues:
		if s.st.runState != wire.RunStateStopped {
			return wire.NewError(wire.CodeNotReady,
				"no stopped inferior; load a program and run it first")
		}
	case wire.TypeExecRun:
		if s.st.exePath == "" {
			return wire.NewError(wire.CodeNotReady, "no program is loaded")
		}
	case wire.TypeBpSetSource, wire.TypeBpDelete, wire.TypeBpSetEnabled:
		if s.st.exePath == "" {
			return wire.NewError(wire.CodeNotReady, "no program is loaded")
		}
	}
	return nil
}

// send issues one command with the session's timeout, from inside the actor.
func (s *Session) send(ctx context.Context, cmd string) (mi.Record, *wire.Error) {
	if s.cfg.MILog {
		s.cfg.Events.Broadcast(wire.EventMI, wire.MILogEntry{Direction: "out", Text: cmd})
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
