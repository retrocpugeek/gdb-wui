package hub_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/retrocpugeek/gdb-wui/internal/hub"
	"github.com/retrocpugeek/gdb-wui/internal/wire"
)

// client is a test WebSocket client with helpers for the envelope.
type client struct {
	t  *testing.T
	ws *websocket.Conn
}

// testLogf is t.Logf that falls silent once the test ends.
//
// A connection goroutine outlives the test body — httptest.Server.Close does
// not wait for a hijacked WebSocket connection — and calling t.Logf after
// tRunner has marked the test done is a data race inside the testing package
// itself. Passing t.Logf directly is the bug; this is the fix.
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

func serve(t *testing.T, cfg hub.Config) (*hub.Hub, *httptest.Server) {
	t.Helper()
	if cfg.Logf == nil {
		cfg.Logf = testLogf(t)
	}
	h := hub.New(cfg)
	ts := httptest.NewServer(h.Handler())
	t.Cleanup(ts.Close)
	return h, ts
}

func dial(t *testing.T, ts *httptest.Server) *client {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	ws, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(ts.URL, "http")+"/ws", nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = ws.CloseNow() })
	return &client{t: t, ws: ws}
}

func (c *client) readRaw() []byte {
	c.t.Helper()
	ctx, cancel := context.WithTimeout(c.t.Context(), 5*time.Second)
	defer cancel()
	typ, data, err := c.ws.Read(ctx)
	if err != nil {
		c.t.Fatalf("read: %v", err)
	}
	if typ != websocket.MessageText {
		c.t.Fatalf("frame type = %v, want text", typ)
	}
	return data
}

func (c *client) readEvent() wire.Event {
	c.t.Helper()
	var ev wire.Event
	if err := json.Unmarshal(c.readRaw(), &ev); err != nil {
		c.t.Fatalf("decoding event: %v", err)
	}
	if ev.Event == "" {
		c.t.Fatalf("expected an event, got %s", c.readRaw())
	}
	return ev
}

func (c *client) readResponse() wire.Response {
	c.t.Helper()
	var res wire.Response
	if err := json.Unmarshal(c.readRaw(), &res); err != nil {
		c.t.Fatalf("decoding response: %v", err)
	}
	return res
}

func (c *client) send(id uint64, typ string, payload any) {
	c.t.Helper()
	req := wire.Request{ID: id, Type: typ}
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			c.t.Fatal(err)
		}
		req.Payload = b
	}
	b, err := json.Marshal(req)
	if err != nil {
		c.t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(c.t.Context(), 5*time.Second)
	defer cancel()
	if err := c.ws.Write(ctx, websocket.MessageText, b); err != nil {
		c.t.Fatalf("write: %v", err)
	}
}

func (c *client) sendRaw(s string) {
	c.t.Helper()
	ctx, cancel := context.WithTimeout(c.t.Context(), 5*time.Second)
	defer cancel()
	if err := c.ws.Write(ctx, websocket.MessageText, []byte(s)); err != nil {
		c.t.Fatalf("write: %v", err)
	}
}

// TestHelloOnConnect is the load-bearing behaviour: a connection receives a
// full snapshot without asking. Reconnect, page reload and a second tab are all
// this one code path.
func TestHelloOnConnect(t *testing.T) {
	_, ts := serve(t, hub.Config{ProjectRoot: "/tmp/project", Version: "test"})
	c := dial(t, ts)

	ev := c.readEvent()
	if ev.Event != wire.EventHello {
		t.Fatalf("first event = %q, want hello", ev.Event)
	}
	if ev.Seq == 0 {
		t.Error("seq = 0; sequence numbers start at 1 so a gap is detectable")
	}

	var hello wire.Hello
	if err := json.Unmarshal(ev.Payload, &hello); err != nil {
		t.Fatal(err)
	}
	if hello.Protocol != wire.Protocol {
		t.Errorf("protocol = %d, want %d", hello.Protocol, wire.Protocol)
	}
	if hello.ProjectRoot != "/tmp/project" {
		t.Errorf("projectRoot = %q", hello.ProjectRoot)
	}
	if hello.RunState != wire.RunStateNoProgram {
		t.Errorf("runState = %q, want noProgram", hello.RunState)
	}
}

func TestPing(t *testing.T) {
	_, ts := serve(t, hub.Config{})
	c := dial(t, ts)
	c.readEvent() // hello

	c.send(7, wire.TypeSessionPing, nil)
	res := c.readResponse()
	if res.ID != 7 {
		t.Errorf("id = %d, want 7", res.ID)
	}
	if !res.OK {
		t.Errorf("ok = false: %v", res.Error)
	}
	if res.Type != wire.TypeSessionPing {
		t.Errorf("type = %q", res.Type)
	}
}

// TestUnknownTypeIsUnsupported: a newer frontend against an older server must
// get an error it can render, not a dropped connection.
func TestUnknownTypeIsUnsupported(t *testing.T) {
	_, ts := serve(t, hub.Config{})
	c := dial(t, ts)
	c.readEvent()

	c.send(1, "exec.warpDrive", nil)
	res := c.readResponse()
	if res.OK {
		t.Fatal("an unknown type succeeded")
	}
	if res.Error.Code != wire.CodeUnsupported {
		t.Errorf("code = %q, want %q", res.Error.Code, wire.CodeUnsupported)
	}

	// The connection must still work.
	c.send(2, wire.TypeSessionPing, nil)
	if next := c.readResponse(); !next.OK || next.ID != 2 {
		t.Errorf("connection unusable after an unknown type: %+v", next)
	}
}

// TestEveryDeclaredTypeIsAnswered is the other half of the docs-honesty check.
// assets.TestProtocolDocumented proves every declared type is written down;
// this proves the server actually implements it, so a type cannot be documented
// and reserved at the same time.
func TestEveryDeclaredTypeIsAnswered(t *testing.T) {
	_, ts := serve(t, hub.Config{})
	c := dial(t, ts)
	c.readEvent()

	for i, typ := range wire.RequestTypes {
		id := uint64(i + 1)
		c.send(id, typ, nil)
		res := c.readResponse()
		if res.ID != id {
			t.Fatalf("id = %d, want %d", res.ID, id)
		}
		if !res.OK {
			t.Errorf("%s: ok = false (%v); it is in wire.RequestTypes, so it must work",
				typ, res.Error)
		}
	}
}

// TestMalformedFrameDoesNotClose: same reasoning, for a frame that is not even
// JSON.
func TestMalformedFrameDoesNotClose(t *testing.T) {
	_, ts := serve(t, hub.Config{})
	c := dial(t, ts)
	c.readEvent()

	c.sendRaw("{not json")
	res := c.readResponse()
	if res.OK || res.Error.Code != wire.CodeBadRequest {
		t.Errorf("got %+v, want a bad_request error", res)
	}

	c.send(3, wire.TypeSessionPing, nil)
	if next := c.readResponse(); !next.OK {
		t.Errorf("connection unusable after malformed input: %+v", next)
	}
}

// TestSeqIsMonotonicAcrossConnections: seq is server-wide so a client can tell
// it missed something.
func TestSeqIsMonotonicAcrossConnections(t *testing.T) {
	h, ts := serve(t, hub.Config{})
	a := dial(t, ts)
	firstSeq := a.readEvent().Seq

	b := dial(t, ts)
	secondSeq := b.readEvent().Seq
	if secondSeq <= firstSeq {
		t.Errorf("second connection's hello seq %d is not after the first's %d",
			secondSeq, firstSeq)
	}

	waitForConns(t, h, 2)
	h.Broadcast("console", map[string]string{"text": "hi"})

	seqA := a.readEvent().Seq
	seqB := b.readEvent().Seq
	if seqA != seqB {
		t.Errorf("the same broadcast arrived with different seq: %d and %d", seqA, seqB)
	}
	if seqA <= secondSeq {
		t.Errorf("broadcast seq %d is not after the last hello %d", seqA, secondSeq)
	}
}

func TestBroadcastReachesEveryClient(t *testing.T) {
	h, ts := serve(t, hub.Config{})
	clients := []*client{dial(t, ts), dial(t, ts), dial(t, ts)}
	for _, c := range clients {
		c.readEvent()
	}
	waitForConns(t, h, 3)

	h.Broadcast(wire.EventConsole, map[string]string{"text": "shared"})
	for i, c := range clients {
		ev := c.readEvent()
		if ev.Event != wire.EventConsole {
			t.Errorf("client %d got event %q", i, ev.Event)
		}
	}
}

// fakeSession stands in for the debugger, which arrives in M3.
type fakeSession struct {
	hello    wire.Hello
	handled  chan wire.Request
	response any
	failWith *wire.Error
}

func (f *fakeSession) Snapshot() wire.Hello { return f.hello }

func (f *fakeSession) Handle(_ context.Context, req wire.Request) (any, *wire.Error) {
	select {
	case f.handled <- req:
	default:
	}
	if f.failWith != nil {
		return nil, f.failWith
	}
	return f.response, nil
}

func TestSessionHandlesDomainRequests(t *testing.T) {
	sess := &fakeSession{
		hello:    wire.Hello{Protocol: wire.Protocol, RunState: wire.RunStateStopped, StopSeq: 42},
		handled:  make(chan wire.Request, 1),
		response: map[string]any{"ok": "yes"},
	}
	_, ts := serve(t, hub.Config{Session: sess})
	c := dial(t, ts)

	// The snapshot must come from the session, not the placeholder.
	var hello wire.Hello
	if err := json.Unmarshal(c.readEvent().Payload, &hello); err != nil {
		t.Fatal(err)
	}
	if hello.RunState != wire.RunStateStopped || hello.StopSeq != 42 {
		t.Errorf("hello = %+v, want the session's snapshot", hello)
	}

	c.send(9, "exec.step", map[string]int{"thread": 1})
	res := c.readResponse()
	if !res.OK {
		t.Fatalf("ok = false: %v", res.Error)
	}
	select {
	case got := <-sess.handled:
		if got.Type != "exec.step" {
			t.Errorf("session saw type %q", got.Type)
		}
		if !strings.Contains(string(got.Payload), `"thread":1`) {
			t.Errorf("payload = %s", got.Payload)
		}
	default:
		t.Error("the session never saw the request")
	}
}

func TestSessionErrorBecomesFailedResponse(t *testing.T) {
	sess := &fakeSession{
		handled:  make(chan wire.Request, 1),
		failWith: wire.NewError(wire.CodeBusy, "the inferior is running"),
	}
	_, ts := serve(t, hub.Config{Session: sess})
	c := dial(t, ts)
	c.readEvent()

	c.send(1, "stack.list", nil)
	res := c.readResponse()
	if res.OK {
		t.Fatal("ok = true for a failing handler")
	}
	if res.Error.Code != wire.CodeBusy {
		t.Errorf("code = %q, want busy", res.Error.Code)
	}
}

func TestShutdownNotifiesClients(t *testing.T) {
	h, ts := serve(t, hub.Config{})
	c := dial(t, ts)
	c.readEvent()
	waitForConns(t, h, 1)

	done := make(chan struct{})
	go func() { defer close(done); h.Shutdown() }()

	ev := c.readEvent()
	if ev.Event != wire.EventShuttingDown {
		t.Errorf("event = %q, want shuttingDown", ev.Event)
	}
	<-done

	// The connection should now be closed by the server.
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	if _, _, err := c.ws.Read(ctx); err == nil {
		t.Error("connection still open after shutdown")
	} else if errors.Is(err, context.DeadlineExceeded) {
		t.Error("server never closed the connection")
	}
}

func TestConnectionsAreTracked(t *testing.T) {
	h, ts := serve(t, hub.Config{})
	if got := h.Count(); got != 0 {
		t.Fatalf("count = %d before any connection", got)
	}
	c := dial(t, ts)
	c.readEvent()
	waitForConns(t, h, 1)

	_ = c.ws.Close(websocket.StatusNormalClosure, "")
	waitForConns(t, h, 0)
}

func waitForConns(t *testing.T, h *hub.Hub, want int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if h.Count() == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("connection count = %d, want %d", h.Count(), want)
}
