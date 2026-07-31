package mi

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// Handler receives every record that is not the reply to an outstanding
// command: async notifications, stream output, prompts, and inferior garbage.
//
// There is exactly one Handler, called from a single goroutine, so records
// arrive in the order gdb emitted them. It is not a broadcaster on purpose:
// ordering is the whole value here, and fan-out to N subscribers belongs one
// layer up where connection lifetimes are known.
type Handler func(Record)

// DefaultHandshake is issued, in order, before Start returns. Each command is
// awaited, so a failure surfaces as a startup error rather than as inexplicable
// behaviour later.
//
// mi-async is first and is not optional: it defaults to off, and without it
// -exec-interrupt does not work, which means the UI's Pause button cannot work.
// startup-with-shell off keeps the inferior's pid and process group
// deterministic. filename-display absolute makes source resolution tractable.
var DefaultHandshake = []string{
	"-gdb-set mi-async on",
	"-gdb-set non-stop off",
	"-gdb-set confirm off",
	"-gdb-set pagination off",
	"-gdb-set height 0",
	"-gdb-set width 0",
	"-gdb-set breakpoint pending on",
	"-gdb-set startup-with-shell off",
	"-gdb-set print object on",
	"-gdb-set print elements 200",
	"-gdb-set print repeats 10",
	"-gdb-set filename-display absolute",
	"-enable-pretty-printing",
}

// Errors returned by the client.
var (
	// ErrDead means the gdb process exited or its pipe closed. Every
	// outstanding and subsequent command fails with it.
	ErrDead = errors.New("mi: gdb is not running")
	// ErrClosed means Close was called.
	ErrClosed = errors.New("mi: client is closed")
)

// Error is a ^error reply. Send returns it as the error so that the ordinary
// `if err != nil` shape is correct; the record is still available for callers
// that want the raw payload.
type Error struct {
	Msg    string
	Code   string
	Record Record
}

func (e *Error) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("gdb: %s (%s)", e.Msg, e.Code)
	}
	return "gdb: " + e.Msg
}

// AsError returns the *Error in err's chain, if any.
func AsError(err error) (*Error, bool) {
	var e *Error
	ok := errors.As(err, &e)
	return e, ok
}

// Options configures Start.
type Options struct {
	// Path is the gdb executable; defaults to "gdb" resolved on PATH.
	Path string
	// Args are extra arguments appended after the mandatory ones.
	Args []string
	// Dir is gdb's working directory.
	Dir string
	// ExtraEnv is appended to the scrubbed child environment.
	ExtraEnv []string

	// Handshake overrides DefaultHandshake. A non-nil empty slice skips the
	// handshake entirely, which is what tests against a scripted fake want.
	Handshake []string

	// Handler receives out-of-band records. Required.
	Handler Handler

	// Logf receives diagnostics. Optional.
	Logf func(format string, args ...any)

	// Stdin and Stdout are the test seam: set both and no process is spawned,
	// so the state machine can be driven by a scripted fake in milliseconds
	// instead of by a real debugger.
	Stdin  io.WriteCloser
	Stdout io.Reader

	// QueueSize bounds the event queue; defaults to 4096.
	QueueSize int
}

// Client speaks MI to one gdb process.
type Client struct {
	opts Options
	logf func(string, ...any)

	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.Reader

	// writeMu serialises writes to gdb's stdin. It is separate from sendSem
	// because SendUnlocked bypasses the semaphore but must not interleave bytes
	// with another writer.
	writeMu sync.Mutex

	// sendSem has capacity 1 and serialises command/reply pairs: GDB/MI is not
	// concurrency-safe, and two commands in flight produce replies that cannot
	// be told apart by anything except their tokens — which works right up
	// until one of them is a console command that changes state.
	sendSem chan struct{}

	nextTok atomic.Uint64

	mu      sync.Mutex
	pending map[uint64]chan Record
	// tombs remembers tokens whose caller gave up. A late reply for one is
	// dropped rather than surfaced as a spurious event.
	tombs     map[uint64]struct{}
	tombOrder []uint64

	events chan Record

	deadOnce sync.Once
	dead     chan struct{}
	deadErr  error

	stderr *ringBuffer

	features []string

	closeOnce sync.Once
	closeErr  error
	wg        sync.WaitGroup

	// killOnClose decides whether shutdown kills the inferior. True is right
	// for a program gdb started; it is destructive for a remote target we
	// merely attached to. See SetKillOnClose.
	killOnClose atomic.Bool
}

const maxTombstones = 1024

// Start launches gdb (or attaches to the pipes in opts) and runs the handshake.
func Start(ctx context.Context, opts Options) (*Client, error) {
	if opts.Handler == nil {
		return nil, errors.New("mi: Options.Handler is required")
	}
	if opts.QueueSize <= 0 {
		opts.QueueSize = 4096
	}
	c := &Client{
		opts:    opts,
		logf:    opts.Logf,
		sendSem: make(chan struct{}, 1),
		pending: make(map[uint64]chan Record),
		tombs:   make(map[uint64]struct{}),
		events:  make(chan Record, opts.QueueSize),
		dead:    make(chan struct{}),
		stderr:  newRingBuffer(64),
	}
	if c.logf == nil {
		c.logf = func(string, ...any) {}
	}
	c.killOnClose.Store(true)

	if opts.Stdin != nil && opts.Stdout != nil {
		c.stdin, c.stdout = opts.Stdin, opts.Stdout
	} else if err := c.spawn(); err != nil {
		return nil, err
	}

	c.wg.Add(2)
	go c.readLoop()
	go c.dispatchLoop()

	if err := c.handshake(ctx); err != nil {
		_ = c.Close(context.Background())
		return nil, err
	}
	return c, nil
}

func (c *Client) spawn() error {
	path := c.opts.Path
	if path == "" {
		path = "gdb"
	}
	// --nx is mandatory, not a preference: a developer's ~/.gdbinit is
	// invisible to this program, can change MI behaviour arbitrarily, and the
	// resulting bug reports would be unreproducible.
	args := append([]string{"--nx", "-q", "--interpreter=mi3"}, c.opts.Args...)
	cmd := exec.Command(path, args...)
	cmd.Dir = c.opts.Dir

	// A scrubbed environment with LC_ALL=C: MI error messages are translatable,
	// and the run-state gate keys on their exact text.
	env := []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + os.Getenv("HOME"),
		"LC_ALL=C",
		"LANG=C",
		"TERM=dumb",
	}
	cmd.Env = append(env, c.opts.ExtraEnv...)

	// Setpgid puts gdb and the inferior in one process group, so a single
	// Kill(-pgid) reaps both. Without it a wedged inferior outlives us.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = 3 * time.Second

	// Pipes, not a pty: a pty echoes our own commands back into the MI stream
	// and re-enables the readline behaviour the handshake just turned off.
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("mi: stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("mi: stdout pipe: %w", err)
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("mi: stderr pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("mi: start %s: %w", path, err)
	}
	c.cmd, c.stdin, c.stdout = cmd, stdin, stdout

	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		sc := bufio.NewScanner(stderrPipe)
		for sc.Scan() {
			c.stderr.add(sc.Text())
		}
	}()
	return nil
}

func (c *Client) handshake(ctx context.Context) error {
	cmds := c.opts.Handshake
	if cmds == nil {
		cmds = DefaultHandshake
	}
	for _, cmd := range cmds {
		if _, err := c.Send(ctx, cmd); err != nil {
			return fmt.Errorf("mi: handshake %q: %w", cmd, err)
		}
	}
	if len(cmds) == 0 {
		return nil
	}
	rec, err := c.Send(ctx, "-list-features")
	if err != nil {
		return fmt.Errorf("mi: -list-features: %w", err)
	}
	if feats, ok := rec.Results.List("features"); ok {
		for _, f := range feats {
			c.features = append(c.features, f.Str)
		}
	}
	return nil
}

// Features returns the feature list reported by -list-features at startup.
func (c *Client) Features() []string { return append([]string(nil), c.features...) }

// HasFeature reports whether gdb advertised the named MI feature.
func (c *Client) HasFeature(name string) bool {
	for _, f := range c.features {
		if f == name {
			return true
		}
	}
	return false
}

// Dead is closed when gdb exits or its pipe closes.
func (c *Client) Dead() <-chan struct{} { return c.dead }

// DeadErr returns why the client died, or nil if it is alive.
func (c *Client) DeadErr() error {
	select {
	case <-c.dead:
		c.mu.Lock()
		defer c.mu.Unlock()
		return c.deadErr
	default:
		return nil
	}
}

// StderrTail returns the last lines gdb wrote to stderr, for crash reports.
func (c *Client) StderrTail() []string { return c.stderr.lines() }

// Send issues a command and waits for its reply, serialised against other
// Sends. cmd must not contain a newline or a leading token; both are added.
func (c *Client) Send(ctx context.Context, cmd string) (Record, error) {
	select {
	case c.sendSem <- struct{}{}:
	case <-ctx.Done():
		return Record{}, ctx.Err()
	case <-c.dead:
		return Record{}, c.deadError()
	}
	defer func() { <-c.sendSem }()
	return c.send(ctx, cmd)
}

// SendUnlocked issues a command without taking the command semaphore.
//
// It exists for exactly two commands: -exec-interrupt and -gdb-exit. Both must
// work while another command is outstanding — a console `shell sleep 60` holds
// the semaphore for a minute, and "Pause" and "quit" cannot wait for it.
func (c *Client) SendUnlocked(ctx context.Context, cmd string) (Record, error) {
	return c.send(ctx, cmd)
}

func (c *Client) send(ctx context.Context, cmd string) (Record, error) {
	if strings.ContainsAny(cmd, "\r\n") {
		return Record{}, fmt.Errorf("mi: command contains a newline: %q", cmd)
	}
	select {
	case <-c.dead:
		return Record{}, c.deadError()
	default:
	}

	// Tokens start at 1: token 0 is what gdb uses for results caused by
	// console-originated activity, and it must never collide with ours.
	tok := c.nextTok.Add(1)
	ch := make(chan Record, 1)

	c.mu.Lock()
	c.pending[tok] = ch
	c.mu.Unlock()

	line := strconv.FormatUint(tok, 10) + cmd + "\n"
	if err := c.write(line); err != nil {
		c.forget(tok, false)
		return Record{}, err
	}

	select {
	case rec, ok := <-ch:
		// markDead closes pending channels, so a receive that yields !ok means
		// gdb died while this command was outstanding. Without this check the
		// zero Record would be returned with a nil error — a silent "^done"
		// that never happened.
		if !ok {
			return Record{}, c.deadError()
		}
		if rec.IsError() {
			return rec, &Error{Msg: rec.ErrorMessage(), Code: rec.ErrorCode(), Record: rec}
		}
		return rec, nil
	case <-ctx.Done():
		// Tombstone so the reply that is already in flight is dropped instead
		// of being mistaken for an unsolicited event.
		c.forget(tok, true)
		return Record{}, ctx.Err()
	case <-c.dead:
		c.forget(tok, false)
		return Record{}, c.deadError()
	}
}

func (c *Client) write(line string) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if _, err := io.WriteString(c.stdin, line); err != nil {
		return fmt.Errorf("mi: write to gdb: %w", err)
	}
	return nil
}

func (c *Client) forget(tok uint64, tombstone bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.pending, tok)
	if !tombstone {
		return
	}
	c.tombs[tok] = struct{}{}
	c.tombOrder = append(c.tombOrder, tok)
	for len(c.tombOrder) > maxTombstones {
		delete(c.tombs, c.tombOrder[0])
		c.tombOrder = c.tombOrder[1:]
	}
}

func (c *Client) readLoop() {
	defer c.wg.Done()
	defer close(c.events)

	br := bufio.NewReaderSize(c.stdout, 64*1024)
	for {
		// ReadString, not bufio.Scanner: a single -data-read-memory-bytes or
		// -var-list-children reply runs to megabytes, and Scanner's 64 KiB
		// default token cap fails in a way that looks exactly like a hang.
		line, err := br.ReadString('\n')
		if line != "" {
			c.route(ParseRecord(line))
		}
		if err != nil {
			c.markDead(err)
			return
		}
	}
}

func (c *Client) route(rec Record) {
	if rec.Type == RecResult && rec.HasToken {
		c.mu.Lock()
		ch, ok := c.pending[rec.Token]
		if ok {
			delete(c.pending, rec.Token)
		}
		_, tombed := c.tombs[rec.Token]
		if tombed {
			delete(c.tombs, rec.Token)
		}
		c.mu.Unlock()

		if ok {
			ch <- rec
			return
		}
		if tombed {
			c.logf("mi: dropping late reply for abandoned token %d", rec.Token)
			return
		}
		// Token 0, or a token we never issued: a result caused by something the
		// user typed at the console. It is an event, not an error.
	}
	c.emit(rec)
}

func (c *Client) emit(rec Record) {
	select {
	case c.events <- rec:
		return
	default:
	}
	// The queue blocks rather than drops. Dropping a *stopped desynchronises
	// the UI permanently; blocking backpressures gdb, which is correct and
	// self-limiting. A watchdog makes the stall visible.
	t := time.NewTimer(time.Second)
	defer t.Stop()
	select {
	case c.events <- rec:
	case <-t.C:
		c.logf("mi: event queue full for >1s, backpressuring gdb (handler is slow)")
		c.events <- rec
	}
}

func (c *Client) dispatchLoop() {
	defer c.wg.Done()
	for rec := range c.events {
		c.opts.Handler(rec)
	}
}

func (c *Client) markDead(cause error) {
	c.deadOnce.Do(func() {
		c.mu.Lock()
		if cause != nil && !errors.Is(cause, io.EOF) {
			c.deadErr = fmt.Errorf("%w: %v", ErrDead, cause)
		} else {
			c.deadErr = ErrDead
		}
		pending := c.pending
		c.pending = make(map[uint64]chan Record)
		c.mu.Unlock()

		close(c.dead)
		// Unblock every caller waiting on a reply that will never come.
		for _, ch := range pending {
			close(ch)
		}
	})
}

func (c *Client) deadError() error {
	if err := c.DeadErr(); err != nil {
		return err
	}
	return ErrDead
}

// SetKillOnClose controls whether shutdown kills the inferior.
//
// It must be false for a remote target. gdb sends a kill packet to a
// `target remote` connection both on an explicit `kill` and on plain quit —
// verified against gdb 17.1 and a qemu stub — which terminates a process this
// server did not start and has no business ending. Detaching first is the
// caller's job; this stops the teardown from undoing it.
func (c *Client) SetKillOnClose(kill bool) { c.killOnClose.Store(kill) }

// Close shuts gdb down, escalating as needed, and waits for the process to be
// gone.
//
// The escalation exists because each step can legitimately fail: -exec-interrupt
// errors when nothing is running, `kill` errors when there is no inferior, and
// ^exit means only that gdb *accepted* the request — teardown records still
// arrive after it, and only Wait returning means the process is actually gone.
func (c *Client) Close(ctx context.Context) error {
	c.closeOnce.Do(func() { c.closeErr = c.doClose(ctx) })
	return c.closeErr
}

func (c *Client) doClose(ctx context.Context) error {
	shutdown, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	if c.DeadErr() == nil {
		// Best effort, in order, ignoring errors: stop the inferior if it is
		// running, kill it, then ask gdb to exit.
		step, stepCancel := context.WithTimeout(shutdown, 500*time.Millisecond)
		_, _ = c.SendUnlocked(step, "-exec-interrupt")
		stepCancel()

		if c.killOnClose.Load() {
			step, stepCancel = context.WithTimeout(shutdown, 500*time.Millisecond)
			_, _ = c.SendUnlocked(step, `-interpreter-exec console "kill"`)
			stepCancel()
		}

		step, stepCancel = context.WithTimeout(shutdown, 500*time.Millisecond)
		_, _ = c.SendUnlocked(step, "-gdb-exit")
		stepCancel()
	}

	// Closing stdin gives gdb EOF even if -gdb-exit never landed.
	if c.stdin != nil {
		_ = c.stdin.Close()
	}

	var waitErr error
	if c.cmd != nil {
		done := make(chan error, 1)
		go func() { done <- c.cmd.Wait() }()
		select {
		case waitErr = <-done:
		case <-shutdown.Done():
			c.killGroup()
			waitErr = <-done
		}
	}

	c.markDead(io.EOF)
	c.wg.Wait()

	if waitErr != nil {
		var ee *exec.ExitError
		if errors.As(waitErr, &ee) {
			// gdb exiting non-zero during teardown is normal enough not to be
			// worth surfacing as a failure.
			return nil
		}
		return waitErr
	}
	return nil
}

func (c *Client) killGroup() {
	if c.cmd == nil || c.cmd.Process == nil {
		return
	}
	pid := c.cmd.Process.Pid
	// Negative pid targets the whole process group, which is why Setpgid was
	// set: this is what reaps a wedged inferior along with gdb.
	if err := syscall.Kill(-pid, syscall.SIGKILL); err != nil {
		c.logf("mi: kill process group %d: %v", pid, err)
		_ = c.cmd.Process.Kill()
	}
}

// ringBuffer keeps the last n lines of gdb's stderr for crash diagnostics.
type ringBuffer struct {
	mu   sync.Mutex
	buf  []string
	n    int
	next int
	full bool
}

func newRingBuffer(n int) *ringBuffer {
	return &ringBuffer{buf: make([]string, n), n: n}
}

func (r *ringBuffer) add(line string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.buf[r.next] = line
	r.next = (r.next + 1) % r.n
	if r.next == 0 {
		r.full = true
	}
}

func (r *ringBuffer) lines() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.full {
		return append([]string(nil), r.buf[:r.next]...)
	}
	out := make([]string, 0, r.n)
	out = append(out, r.buf[r.next:]...)
	return append(out, r.buf[:r.next]...)
}
