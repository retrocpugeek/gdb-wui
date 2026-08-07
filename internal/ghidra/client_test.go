package ghidra

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

// These drive the whole protocol through the exec seam, with a fake server in
// Go instead of a JVM. That is the point: the interesting behaviour here is id
// matching, death propagation and cancellation, none of which needs Ghidra to
// exercise and all of which would be untestable if the only way in were a
// 3.5-second process launch.

// fake speaks the server side of the protocol. handler returns the JSON line
// to reply with, or "" to send nothing at all.
type fake struct {
	t       *testing.T
	handler func(op string, id uint64, req map[string]any) string
	// greeting is sent on connect unless empty.
	greeting string

	mu    sync.Mutex
	conn  net.Conn
	calls []string
}

func newFake(t *testing.T, handler func(op string, id uint64, req map[string]any) string) *fake {
	return &fake{
		t:       t,
		handler: handler,
		greeting: `{"event":"ready","schema":1,"functionCount":3,` +
			`"program":{"name":"demo","sha256":"abc123","languageId":"MIPS:BE:64:default",` +
			`"imageBase":"0x120000000","pointerSize":8}}`,
	}
}

// options returns Options wired to this fake.
func (f *fake) options() Options {
	return Options{
		Timeout: 5 * time.Second,
		exec: func(ctx context.Context, socket string) (func(), error) {
			done := make(chan struct{})
			go f.serve(socket, done)
			return func() { close(done) }, nil
		},
	}
}

func (f *fake) serve(socket string, done <-chan struct{}) {
	conn, err := net.Dial("unix", socket)
	if err != nil {
		return
	}
	f.mu.Lock()
	f.conn = conn
	f.mu.Unlock()
	defer conn.Close()

	go func() {
		<-done
		conn.Close()
	}()

	if f.greeting != "" {
		fmt.Fprintln(conn, f.greeting)
	}
	sc := bufio.NewScanner(conn)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for sc.Scan() {
		var req map[string]any
		if err := json.Unmarshal(sc.Bytes(), &req); err != nil {
			continue
		}
		op, _ := req["op"].(string)
		idf, _ := req["id"].(float64)
		f.mu.Lock()
		f.calls = append(f.calls, op)
		f.mu.Unlock()
		if reply := f.handler(op, uint64(idf), req); reply != "" {
			fmt.Fprintln(conn, reply)
		}
		// The real server acts on quit and hangs up without replying. The fake
		// must too, or Close's shutdown path is never exercised as it ships.
		if op == "quit" {
			return
		}
	}
}

func (f *fake) hangUp() {
	f.mu.Lock()
	c := f.conn
	f.mu.Unlock()
	if c != nil {
		c.Close()
	}
}

func okReply(id uint64, body string) string {
	return fmt.Sprintf(`{"id":%d,"ok":true,%s}`, id, body)
}

const demoFunction = `"function":{"name":"csum16","entry":"0x120005080",` +
	`"bodyStart":"0x120005080","bodyEnd":"0x120005118",` +
	`"signature":"ulong csum16(ushort *, long)",` +
	`"frame":{"size":0,"localSize":0,"paramOffset":0,"returnAddressOffset":0,"growsNegative":true},` +
	`"variables":[{"name":"local_10","type":"int","size":4,"param":false,"pc":null,` +
	`"storage":{"kind":"stack","offset":-16}},` +
	`{"name":"lVar1","type":"long","size":8,"param":false,"pc":"0x1200050a4",` +
	`"storage":{"kind":"unique"}}],` +
	`"lineCount":23,"text":"ulong csum16(ushort *p,long n)\n{\n  return 0;\n}\n",` +
	`"lines":[{"n":3,"addrs":["0x120005088","0x12000508c"]}]}`

func startFake(t *testing.T, f *fake) *Client {
	t.Helper()
	c, err := Start(context.Background(), f.options())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// TestStartWaitsForGreeting is the startup contract: Start does not return
// until the far end has said what program it has. A caller that got a Client
// before that would have to poll for identity, and would happily show a
// decompilation of the wrong binary in the meantime.
func TestStartWaitsForGreeting(t *testing.T) {
	f := newFake(t, func(op string, id uint64, req map[string]any) string { return "" })
	c := startFake(t, f)

	ready := c.Ready()
	if ready.Schema != Schema {
		t.Errorf("schema = %d, want %d", ready.Schema, Schema)
	}
	if ready.Program.SHA256 != "abc123" {
		t.Errorf("sha256 = %q", ready.Program.SHA256)
	}
	if ready.Program.LanguageID != "MIPS:BE:64:default" {
		t.Errorf("languageId = %q", ready.Program.LanguageID)
	}
	if ready.FunctionCount != 3 {
		t.Errorf("functionCount = %d", ready.FunctionCount)
	}
}

// TestStartFailsWithoutGreeting: a process that connects and says nothing is a
// broken one, and must not leave the caller blocked forever.
func TestStartFailsWithoutGreeting(t *testing.T) {
	f := newFake(t, func(string, uint64, map[string]any) string { return "" })
	f.greeting = ""
	opts := f.options()
	opts.Timeout = 300 * time.Millisecond

	if _, err := Start(context.Background(), opts); err == nil {
		t.Fatal("Start succeeded with no greeting; it should have timed out")
	}
}

func TestDecompileRoundTrip(t *testing.T) {
	f := newFake(t, func(op string, id uint64, req map[string]any) string {
		if op != "decompile" {
			return ""
		}
		if req["function"] != "0x120005080" {
			t.Errorf("function = %v", req["function"])
		}
		return okReply(id, demoFunction)
	})
	c := startFake(t, f)

	fn, err := c.Decompile(context.Background(), "0x120005080")
	if err != nil {
		t.Fatalf("Decompile: %v", err)
	}
	if fn.Name != "csum16" || fn.Entry != "0x120005080" {
		t.Errorf("got %q at %q", fn.Name, fn.Entry)
	}
	if len(fn.Lines) != 1 || len(fn.Lines[0].Addrs) != 2 {
		t.Fatalf("line map = %+v", fn.Lines)
	}
	if fn.Lines[0].N != 3 {
		t.Errorf("line n = %d, want 3", fn.Lines[0].N)
	}
	// The storage split is the part a UI cannot fake: one variable is
	// addressable and the other never will be.
	if len(fn.Variables) != 2 {
		t.Fatalf("variables = %d", len(fn.Variables))
	}
	if fn.Variables[0].Storage.Kind != StorageStack || fn.Variables[0].Storage.Offset != -16 {
		t.Errorf("var0 storage = %+v", fn.Variables[0].Storage)
	}
	if fn.Variables[1].Storage.Kind != StorageUnique {
		t.Errorf("var1 storage = %+v", fn.Variables[1].Storage)
	}
}

// TestFailedRequestKeepsTheConnection is the reason errors are a reply rather
// than a disconnect. One undecompilable function is ordinary; losing the whole
// resident process over it would mean a 3.5-second restart for a typo.
func TestFailedRequestKeepsTheConnection(t *testing.T) {
	f := newFake(t, func(op string, id uint64, req map[string]any) string {
		if req["function"] == "nosuch" {
			return fmt.Sprintf(`{"id":%d,"ok":false,"error":"no function nosuch"}`, id)
		}
		return okReply(id, demoFunction)
	})
	c := startFake(t, f)

	_, err := c.Decompile(context.Background(), "nosuch")
	if err == nil {
		t.Fatal("expected an error for an unknown function")
	}
	var gerr *Error
	if !errors.As(err, &gerr) {
		t.Fatalf("error is %T, want *ghidra.Error", err)
	}
	if !strings.Contains(gerr.Msg, "no function") {
		t.Errorf("message = %q", gerr.Msg)
	}

	// The connection must still work.
	if _, err := c.Decompile(context.Background(), "0x120005080"); err != nil {
		t.Fatalf("second request after a failure: %v", err)
	}
}

// TestRepliesMatchByID drives replies back out of order. Without id matching
// this passes by luck when the server is sequential and fails the moment it
// is not — so the fake answers backwards on purpose.
func TestRepliesMatchByID(t *testing.T) {
	var mu sync.Mutex
	held := map[uint64]string{}
	release := make(chan struct{})

	f := newFake(t, func(op string, id uint64, req map[string]any) string {
		if op != "decompile" {
			return "" // Close sends quit; it is not one of the three
		}
		which, _ := req["function"].(string)
		mu.Lock()
		held[id] = which
		n := len(held)
		mu.Unlock()
		if n == 3 {
			// All three are in hand; let the test flush them backwards.
			close(release)
		}
		return ""
	})
	c := startFake(t, f)

	results := make(chan string, 3)
	for _, which := range []string{"a", "b", "c"} {
		go func(w string) {
			fn, err := c.Decompile(context.Background(), w)
			if err != nil {
				results <- "err:" + err.Error()
				return
			}
			results <- w + "->" + fn.Name
		}(which)
	}

	<-release
	mu.Lock()
	ids := make([]uint64, 0, len(held))
	for id := range held {
		ids = append(ids, id)
	}
	mu.Unlock()
	// Reverse order, and each reply names the function it belongs to so a
	// mismatch is visible rather than merely a count.
	for i := len(ids) - 1; i >= 0; i-- {
		id := ids[i]
		mu.Lock()
		which := held[id]
		mu.Unlock()
		body := strings.Replace(demoFunction, `"name":"csum16"`, `"name":"fn_`+which+`"`, 1)
		f.mu.Lock()
		conn := f.conn
		f.mu.Unlock()
		fmt.Fprintln(conn, okReply(id, body))
	}

	got := map[string]bool{}
	for range 3 {
		select {
		case r := <-results:
			got[r] = true
		case <-time.After(5 * time.Second):
			t.Fatal("timed out waiting for replies")
		}
	}
	for _, want := range []string{"a->fn_a", "b->fn_b", "c->fn_c"} {
		if !got[want] {
			t.Errorf("missing %q; got %v", want, got)
		}
	}
}

// TestDeathFailsPendingRequests: when the process goes away, a caller blocked
// on a reply must be told, not left hanging. This is the failure mode that
// turns a crashed decompiler into a frozen UI.
func TestDeathFailsPendingRequests(t *testing.T) {
	gate := make(chan struct{})
	f := newFake(t, func(op string, id uint64, req map[string]any) string {
		close(gate)
		return "" // never reply
	})
	c := startFake(t, f)

	errCh := make(chan error, 1)
	go func() {
		_, err := c.Decompile(context.Background(), "0x1")
		errCh <- err
	}()

	<-gate
	f.hangUp()

	select {
	case err := <-errCh:
		if !errors.Is(err, ErrDead) {
			t.Errorf("error = %v, want ErrDead", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a pending request survived the process dying")
	}

	// And a later request fails immediately rather than blocking.
	if _, err := c.Decompile(context.Background(), "0x1"); !errors.Is(err, ErrDead) {
		t.Errorf("post-death request: %v, want ErrDead", err)
	}
}

// TestDeathDrainsPendingMap covers what nothing else does.
//
// Callers are woken by call()'s select on c.dead, so a client that never
// cleaned up its pending map would still behave correctly and pass
// TestDeathFailsPendingRequests — verified by deleting the drain loop and
// watching that test stay green. What leaks in that case is the map itself:
// one entry per request in flight when the process died, holding a channel
// nobody will ever read.
func TestDeathDrainsPendingMap(t *testing.T) {
	gate := make(chan struct{}, 4)
	f := newFake(t, func(op string, id uint64, req map[string]any) string {
		gate <- struct{}{}
		return "" // never reply
	})
	c := startFake(t, f)

	for range 3 {
		go func() { _, _ = c.Decompile(context.Background(), "0x1") }()
	}
	for range 3 {
		select {
		case <-gate:
		case <-time.After(5 * time.Second):
			t.Fatal("requests never reached the server")
		}
	}
	f.hangUp()

	deadline := time.After(5 * time.Second)
	for {
		c.mu.Lock()
		n := len(c.pending)
		c.mu.Unlock()
		if n == 0 {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("%d pending entries survived the process dying", n)
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// TestContextCancelsWithoutLeakingTheID. A cancelled request must not leave a
// pending entry that a later, unrelated reply could satisfy.
func TestContextCancels(t *testing.T) {
	f := newFake(t, func(op string, id uint64, req map[string]any) string { return "" })
	c := startFake(t, f)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err := c.Decompile(ctx, "0x1")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want DeadlineExceeded", err)
	}

	c.mu.Lock()
	n := len(c.pending)
	c.mu.Unlock()
	if n != 0 {
		t.Errorf("%d pending entries left after cancellation", n)
	}
}

func TestFunctionsList(t *testing.T) {
	f := newFake(t, func(op string, id uint64, req map[string]any) string {
		if op != "functions" {
			return ""
		}
		if req["filter"] != "csum" {
			t.Errorf("filter = %v", req["filter"])
		}
		return okReply(id, `"total":2,"offset":0,"functions":[`+
			`{"name":"csum16","entry":"0x120005080","thunk":false},`+
			`{"name":"csum16_update","entry":"0x120005038","thunk":false}]`)
	})
	c := startFake(t, f)

	list, err := c.Functions(context.Background(), 0, 100, "csum")
	if err != nil {
		t.Fatalf("Functions: %v", err)
	}
	if list.Total != 2 || len(list.Functions) != 2 {
		t.Fatalf("got total=%d n=%d", list.Total, len(list.Functions))
	}
	if list.Functions[0].Name != "csum16" || list.Functions[0].Entry != "0x120005080" {
		t.Errorf("first = %+v", list.Functions[0])
	}
}

func TestCloseIsIdempotent(t *testing.T) {
	f := newFake(t, func(op string, id uint64, req map[string]any) string {
		if op == "quit" {
			return okReply(id, `"bye":true`)
		}
		return ""
	})
	c, err := Start(context.Background(), f.options())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Errorf("first Close: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
}

// TestStartRequiresProgram guards the mistake that a real project makes easy.
// A Ghidra project holds several programs, and in the Debugger workflow a pile
// of traces as well; analyzeHeadless with no -process pattern sweeps all of
// them. Requiring the name means that cannot happen by omission.
func TestStartRequiresProgram(t *testing.T) {
	_, err := Start(context.Background(), Options{
		Install:     &Install{Dir: "/nowhere", Headless: "/nowhere/support/analyzeHeadless"},
		ProjectDir:  "/tmp",
		ProjectName: "proj",
	})
	if err == nil {
		t.Fatal("Start succeeded with no Program")
	}
	if !strings.Contains(err.Error(), "Program is required") {
		t.Errorf("error = %v", err)
	}
}
