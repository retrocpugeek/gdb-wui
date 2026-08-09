// Package ghidra supervises one resident Ghidra process and speaks a small
// request/response protocol to it.
//
// The shape mirrors internal/mi: a long-lived child, a scrubbed environment,
// one goroutine reading replies, requests matched by id, and a death that
// fails every outstanding and subsequent call rather than hanging.
// gdb-wui already owns a debugger this way; owning a decompiler the same way
// means one set of habits covers both.
//
// Why resident at all. analyzeHeadless keeps the JVM alive for as long as a
// postScript runs, so a script that blocks reading requests is a server.
// Measured on a 2 MB MIPS64 image: a fresh analyzeHeadless costs 3.5s per
// function, of which 3.4s is JVM startup and project open. The same request to
// a resident process is 100-200ms. That difference is what makes decompiling
// the function under the program counter a view rather than a job.
//
// Why a unix socket rather than the child's stdout. Ghidra's logging owns
// stdout and interleaves log4j records with anything a script prints;
// separating a protocol back out of that is a parser nobody should write. A
// TCP port would be reachable by anything on the machine, and this process
// decompiles whatever it is asked to. A socket in a 0700 directory is bounded
// by filesystem permissions instead.
package ghidra

import (
	"bufio"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

//go:embed scripts/*.java
var scripts embed.FS

// Errors returned by the client.
var (
	// ErrDead means the Ghidra process exited or its socket closed. Every
	// outstanding and subsequent request fails with it.
	ErrDead = errors.New("ghidra: the decompiler is not running")
	// ErrClosed means Close was called.
	ErrClosed = errors.New("ghidra: client is closed")
)

// Error is a failed request. The far end reports these instead of closing the
// connection, because one undecompilable function must not end the session.
type Error struct {
	Op  string
	Msg string
}

func (e *Error) Error() string { return "ghidra: " + e.Op + ": " + e.Msg }

// Options configures Start.
type Options struct {
	// Install is the located Ghidra. Required.
	Install *Install

	// ProjectDir and ProjectName address a Ghidra project. When Import is
	// empty the project must already exist and is opened read-only.
	ProjectDir  string
	ProjectName string

	// Program selects one program inside an existing project. It is required
	// for that mode and it is not a nicety: a real project holds several
	// programs and, in Ghidra's Debugger workflow, a pile of traces as well.
	// analyzeHeadless with no -process pattern sweeps all of them.
	Program string

	// Writable permits Rename and Retype. False for a project the user named:
	// theirs holds their names, types and comments, and gdb-wui writes only to
	// the one it imported itself.
	//
	// It is not the same thing as -readOnly, which protects nothing at all —
	// under it the sidecar can still rename a function and save the file
	// (finding 32). This flag is passed to the sidecar as well, so the refusal
	// exists on both sides of the socket rather than only in the caller.
	Writable bool

	// Timeout bounds startup. Zero means DefaultStartTimeout.
	Timeout time.Duration

	// Logf receives diagnostics, including the child's stderr. Optional.
	Logf func(format string, args ...any)

	// exec replaces spawning Ghidra, for tests. It is handed the socket path
	// and returns a function that stops whatever it started.
	exec func(ctx context.Context, socket string) (stop func(), err error)
}

// DefaultStartTimeout covers JVM startup and opening a project. Importing and
// analysing a large image takes far longer and sets its own.
const DefaultStartTimeout = 90 * time.Second

// quitGrace is how long Close waits for the far end to hang up after being
// asked to quit, before killing the process group. A clean exit releases the
// Ghidra project lock; this is short because failing to release it is
// recoverable and hanging on shutdown is not.
const quitGrace = 2 * time.Second

// Client is one resident Ghidra.
type Client struct {
	opts Options
	logf func(string, ...any)

	conn net.Conn
	stop func()

	// dir holds the socket and the extracted scripts. Removed on Close.
	dir string

	writeMu sync.Mutex
	nextID  atomic.Uint64

	mu      sync.Mutex
	pending map[uint64]chan *reply

	// ready is the greeting: program identity and function count, which the
	// caller needs before showing anything.
	ready Ready

	deadOnce sync.Once
	dead     chan struct{}
	deadErr  error

	closeOnce sync.Once
	wg        sync.WaitGroup
}

// Ready is the unsolicited greeting the server sends on connect.
type Ready struct {
	Schema        int     `json:"schema"`
	Program       Program `json:"program"`
	FunctionCount int     `json:"functionCount"`
}

type reply struct {
	OK    bool   `json:"ok"`
	Error string `json:"error"`
	// Warning is a successful reply that the caller still has to say something
	// about — a name that is now ambiguous, an edit that was not saved.
	Warning string `json:"warning"`
	// Was and Now describe an edit well enough to undo it: the value before,
	// and the name the symbol answers to after.
	Was      string          `json:"was"`
	Now      string          `json:"now"`
	Function json.RawMessage `json:"function"`
	Program  json.RawMessage `json:"program"`
	Raw      json.RawMessage `json:"-"`
}

// envelope is what arrives on the wire: either a reply with an id, or an
// unsolicited event.
type envelope struct {
	ID    *uint64 `json:"id"`
	Event string  `json:"event"`
}

// Start launches Ghidra and waits for the server to greet us.
func Start(ctx context.Context, opts Options) (*Client, error) {
	if opts.Install == nil && opts.exec == nil {
		return nil, errors.New("ghidra: Options.Install is required")
	}
	if opts.Program == "" && opts.exec == nil {
		return nil, errors.New("ghidra: Options.Program is required when opening a project")
	}
	timeout := opts.Timeout
	if timeout == 0 {
		timeout = DefaultStartTimeout
	}

	c := &Client{
		opts:    opts,
		logf:    opts.Logf,
		pending: make(map[uint64]chan *reply),
		dead:    make(chan struct{}),
	}
	if c.logf == nil {
		c.logf = func(string, ...any) {}
	}

	// 0700, and the socket inside it: the directory permission is the access
	// control on the protocol.
	dir, err := os.MkdirTemp("", "gdb-wui-ghidra-")
	if err != nil {
		return nil, fmt.Errorf("ghidra: temp dir: %w", err)
	}
	c.dir = dir

	sockPath := filepath.Join(dir, "decomp.sock")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		c.cleanup()
		return nil, fmt.Errorf("ghidra: listen: %w", err)
	}
	defer ln.Close()

	if opts.exec != nil {
		c.stop, err = opts.exec(ctx, sockPath)
	} else {
		c.stop, err = c.spawn(ctx, sockPath)
	}
	if err != nil {
		c.cleanup()
		return nil, err
	}

	// Accept with a deadline. Ghidra failing to start looks exactly like
	// Ghidra being slow, and without this the caller waits forever.
	if lnDeadline, ok := ln.(*net.UnixListener); ok {
		_ = lnDeadline.SetDeadline(time.Now().Add(timeout))
	}
	conn, err := ln.Accept()
	if err != nil {
		c.stop()
		c.cleanup()
		return nil, fmt.Errorf("ghidra: waiting for the decompiler to connect: %w", err)
	}
	c.conn = conn

	greeting := make(chan Ready, 1)
	c.wg.Add(1)
	go c.readLoop(greeting)

	select {
	case r := <-greeting:
		c.ready = r
	case <-c.dead:
		c.stop()
		c.cleanup()
		return nil, c.deadErr
	case <-time.After(timeout):
		_ = c.Close()
		return nil, errors.New("ghidra: connected but sent no greeting")
	case <-ctx.Done():
		_ = c.Close()
		return nil, ctx.Err()
	}
	return c, nil
}

// spawn runs analyzeHeadless with the resident server script.
func (c *Client) spawn(ctx context.Context, sockPath string) (func(), error) {
	scriptDir := filepath.Join(c.dir, "scripts")
	if err := c.extractScripts(scriptDir); err != nil {
		return nil, err
	}

	// Always an existing program. Importing here would not work: analyzeHeadless
	// commits an imported program only after the postScript returns, and this
	// script never returns — it is the server. An import that way is analysed,
	// served, and then thrown away, leaving an empty project for the next run
	// to fail on. See Import below.
	//
	// -noanalysis because re-analysing someone's curated program would undo
	// their work, and -readOnly so that analyzeHeadless commits nothing of its
	// own when the sidecar exits.
	//
	// -readOnly is *not* what keeps a user's project safe, though it reads that
	// way. Under it the sidecar can still rename a function and save the file,
	// and the change is there on the next open: finding 32. What keeps a
	// project safe is Options.Writable, which is false for one the user named,
	// and the sidecar's own refusal to answer an edit without it.
	args := []string{
		c.opts.ProjectDir, c.opts.ProjectName,
		"-process", c.opts.Program, "-noanalysis", "-readOnly",
	}
	args = append(args,
		"-scriptPath", scriptDir,
		"-postScript", "DecompServer.java", sockPath,
	)
	if c.opts.Writable {
		args = append(args, "writable")
	}

	cmd := exec.Command(c.opts.Install.Headless, args...)
	// A scrubbed environment, as for gdb. Ghidra needs a JDK: it does not ship
	// one, so PATH and JAVA_HOME are how it is found.
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + os.Getenv("HOME"),
		"JAVA_HOME=" + os.Getenv("JAVA_HOME"),
		"LC_ALL=C",
		"LANG=C",
	}
	// Its own process group, so one Kill(-pgid) reaps the JVM and the shell
	// wrapper together. analyzeHeadless is a script that execs java; killing
	// only the script would leave a 2 GB JVM behind.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = 5 * time.Second

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("ghidra: stdout pipe: %w", err)
	}
	cmd.Stderr = cmd.Stdout

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("ghidra: start %s: %w", c.opts.Install.Headless, err)
	}

	// Ghidra's log is the only place a startup failure explains itself, so it
	// goes to the caller's log rather than to /dev/null.
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		sc := bufio.NewScanner(stdout)
		sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for sc.Scan() {
			c.logf("ghidra: %s", sc.Text())
		}
	}()

	// Reap, and report an exit as death so a startup failure is not a timeout.
	go func() {
		err := cmd.Wait()
		c.die(fmt.Errorf("%w: %v", ErrDead, err))
	}()

	return func() {
		if cmd.Process != nil {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
	}, nil
}

// extractScripts writes the embedded Ghidra-side sources to a directory for
// -scriptPath. Embedding rather than reading from the repo means a built
// binary carries its own decompiler glue and works from anywhere.
func (c *Client) extractScripts(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("ghidra: script dir: %w", err)
	}
	entries, err := scripts.ReadDir("scripts")
	if err != nil {
		return err
	}
	for _, e := range entries {
		body, err := scripts.ReadFile("scripts/" + e.Name())
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dir, e.Name()), body, 0o600); err != nil {
			return fmt.Errorf("ghidra: writing %s: %w", e.Name(), err)
		}
	}
	return nil
}

func (c *Client) readLoop(greeting chan<- Ready) {
	defer c.wg.Done()
	sc := bufio.NewScanner(c.conn)
	// A decompiled function is easily a megabyte of JSON; the default 64 KiB
	// would truncate one and look like a protocol error.
	sc.Buffer(make([]byte, 0, 256*1024), 32*1024*1024)

	greeted := false
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var env envelope
		if err := json.Unmarshal(line, &env); err != nil {
			c.logf("ghidra: unparseable message: %v", err)
			continue
		}
		if env.ID == nil {
			if env.Event == "ready" && !greeted {
				var r Ready
				if err := json.Unmarshal(line, &r); err == nil {
					greeted = true
					greeting <- r
				}
			}
			continue
		}
		var rep reply
		if err := json.Unmarshal(line, &rep); err != nil {
			c.logf("ghidra: unparseable reply: %v", err)
			continue
		}
		rep.Raw = append(json.RawMessage(nil), line...)

		c.mu.Lock()
		ch := c.pending[*env.ID]
		delete(c.pending, *env.ID)
		c.mu.Unlock()
		if ch != nil {
			ch <- &rep
		}
	}
	err := sc.Err()
	if err == nil {
		err = ErrDead
	} else {
		err = fmt.Errorf("%w: %v", ErrDead, err)
	}
	c.die(err)
}

// die records the first cause of death and wakes everything waiting.
func (c *Client) die(err error) {
	c.deadOnce.Do(func() {
		c.deadErr = err
		close(c.dead)
		// Fail every outstanding request rather than leaving callers blocked
		// on a process that is gone.
		c.mu.Lock()
		for id, ch := range c.pending {
			delete(c.pending, id)
			close(ch)
		}
		c.mu.Unlock()
	})
}

// call sends one request and waits for its reply.
func (c *Client) call(ctx context.Context, op string, req map[string]any) (*reply, error) {
	select {
	case <-c.dead:
		return nil, c.deadErr
	default:
	}

	id := c.nextID.Add(1)
	if req == nil {
		req = map[string]any{}
	}
	req["id"] = id
	req["op"] = op
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}

	ch := make(chan *reply, 1)
	c.mu.Lock()
	c.pending[id] = ch
	c.mu.Unlock()

	c.writeMu.Lock()
	_, err = c.conn.Write(append(body, '\n'))
	c.writeMu.Unlock()
	if err != nil {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return nil, fmt.Errorf("%w: %v", ErrDead, err)
	}

	select {
	case rep, ok := <-ch:
		if !ok || rep == nil {
			return nil, c.deadErr
		}
		if !rep.OK {
			return nil, &Error{Op: op, Msg: rep.Error}
		}
		return rep, nil
	case <-ctx.Done():
		// Forget the id. A late reply is dropped rather than delivered to
		// whoever next gets this channel.
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return nil, ctx.Err()
	case <-c.dead:
		return nil, c.deadErr
	}
}

// Ready returns the greeting: which program is loaded and how many functions
// it has. Valid as soon as Start returns.
func (c *Client) Ready() Ready { return c.ready }

// Decompile returns one function, addressed by name or by any address inside
// it. An address is the useful form: a caller holding a program counter should
// not have to work out which function it is in.
func (c *Client) Decompile(ctx context.Context, which string) (*Function, error) {
	rep, err := c.call(ctx, "decompile", map[string]any{"function": which})
	if err != nil {
		return nil, err
	}
	var fn Function
	if err := json.Unmarshal(rep.Function, &fn); err != nil {
		return nil, fmt.Errorf("ghidra: decoding function: %w", err)
	}
	return &fn, nil
}

// Functions lists the program's functions without decompiling them, which is
// what a browsable index needs: decompiling 1415 functions to draw a list
// would be two minutes of work nobody asked for.
func (c *Client) Functions(ctx context.Context, offset, limit int, filter string) (*FunctionList, error) {
	rep, err := c.call(ctx, "functions", map[string]any{
		"offset": offset, "limit": limit, "filter": filter,
	})
	if err != nil {
		return nil, err
	}
	var out FunctionList
	if err := json.Unmarshal(rep.Raw, &out); err != nil {
		return nil, fmt.Errorf("ghidra: decoding function list: %w", err)
	}
	return &out, nil
}

// Names says which function each address falls in.
//
// One round trip for a whole call stack, and no decompilation: the sidecar
// answers from the function manager. That is what makes it usable on the path
// where a UI has just been handed a stack of "?? ()" and wants it filled in.
//
// The addresses go over as one comma-separated string. The sidecar's JSON
// parser is deliberately hand-rolled and has no array reader; a list of hex
// numbers needs neither.
func (c *Client) Names(ctx context.Context, addrs []string) ([]FunctionName, error) {
	if len(addrs) == 0 {
		return nil, nil
	}
	rep, err := c.call(ctx, "names", map[string]any{"addresses": strings.Join(addrs, ",")})
	if err != nil {
		return nil, err
	}
	var out NameList
	if err := json.Unmarshal(rep.Raw, &out); err != nil {
		return nil, fmt.Errorf("ghidra: decoding names: %w", err)
	}
	return out.Names, nil
}

// Rename gives a function, local or global a new name.
//
// Retype is the same call with a type instead, and both return the function
// decompiled *again* rather than an acknowledgement. That is not politeness: an
// edit renumbers the ids of the symbols it did not touch (finding 34) and a
// retype reshapes the body around it, so a caller that patched its own copy
// would be holding keys that no longer address anything.
//
// EditResult also carries what an undo needs, because only the far end can see
// which symbol the edit landed on when the id was stale and the name matched
// instead.
func (c *Client) Rename(ctx context.Context, e Edit) (*EditResult, error) {
	return c.edit(ctx, "rename", e, map[string]any{"newName": e.Value})
}

// Retype sets a variable's type, or a function's whole prototype — which in
// Ghidra also renames it, because a prototype carries a name.
func (c *Client) Retype(ctx context.Context, e Edit) (*EditResult, error) {
	return c.edit(ctx, "retype", e, map[string]any{"type": e.Value})
}

func (c *Client) edit(ctx context.Context, op string, e Edit, extra map[string]any) (
	*EditResult, error) {
	req := map[string]any{
		"kind":     e.Kind,
		"function": e.Function,
		"symbol":   e.Symbol,
		"name":     e.Name,
		"address":  e.Address,
	}
	for k, v := range extra {
		req[k] = v
	}
	rep, err := c.call(ctx, op, req)
	if err != nil {
		return nil, err
	}
	var fn Function
	if err := json.Unmarshal(rep.Function, &fn); err != nil {
		return nil, fmt.Errorf("ghidra: decoding function: %w", err)
	}
	return &EditResult{Function: &fn, Warning: rep.Warning, Was: rep.Was, Now: rep.Now}, nil
}

// Dead returns a channel closed when the process goes away, and the reason.
func (c *Client) Dead() (<-chan struct{}, func() error) {
	return c.dead, func() error { return c.deadErr }
}

// Close stops Ghidra and removes the socket and extracted scripts.
func (c *Client) Close() error {
	c.closeOnce.Do(func() {
		// Ask politely first: a clean exit lets Ghidra release the project
		// lock, which matters when the user has it open in the GUI too.
		//
		// Fire-and-forget, because quit has no reply — the server acts on it
		// and closes the connection. Waiting for one would stall every
		// shutdown for the full timeout, which is what it did until the unit
		// suite's runtime made it apparent.
		if c.conn != nil {
			c.writeMu.Lock()
			_, _ = c.conn.Write([]byte("{\"id\":0,\"op\":\"quit\"}\n"))
			c.writeMu.Unlock()
			select {
			case <-c.dead:
				// The far end hung up, which is the acknowledgement.
			case <-time.After(quitGrace):
				// It did not. Fall through and kill it.
			}
			_ = c.conn.Close()
		}
		c.die(ErrClosed)
		if c.stop != nil {
			c.stop()
		}
		c.wg.Wait()
		c.cleanup()
	})
	return nil
}

func (c *Client) cleanup() {
	if c.dir != "" {
		_ = os.RemoveAll(c.dir)
	}
}

// Import analyses a binary into a project and saves it, so a later Start can
// open it.
//
// A separate invocation from Start, and it has to be. analyzeHeadless writes an
// imported program to the project only once the postScript returns; the
// resident server never returns, so importing and serving together analyses the
// binary, serves it, and then discards it — leaving an empty project that the
// next start fails to open. Found the hard way: the first run worked and every
// run after it reported "Requested project program file(s) not found".
//
// This blocks for the length of the analysis, which is seconds for a
// hello-world and minutes for firmware. The caller is expected to be a
// background job.
func Import(ctx context.Context, install *Install, projectDir, projectName, binary string,
	logf func(string, ...any)) error {
	if install == nil {
		return errors.New("ghidra: no installation")
	}
	if logf == nil {
		logf = func(string, ...any) {}
	}
	cmd := exec.CommandContext(ctx, install.Headless,
		projectDir, projectName, "-import", binary)
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + os.Getenv("HOME"),
		"JAVA_HOME=" + os.Getenv("JAVA_HOME"),
		"LC_ALL=C",
		"LANG=C",
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	out, err := cmd.CombinedOutput()
	for _, line := range strings.Split(string(out), "\n") {
		if line != "" {
			logf("ghidra import: %s", line)
		}
	}
	if err != nil {
		return fmt.Errorf("ghidra: importing %s: %w", binary, err)
	}
	// analyzeHeadless exits 0 on some failures and says so only in its log, so
	// the absence of the success line is the real check.
	if !strings.Contains(string(out), "REPORT: Import succeeded") {
		return fmt.Errorf("ghidra: importing %s did not report success", binary)
	}
	return nil
}
