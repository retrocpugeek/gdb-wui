package httpapi_test

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/retrocpugeek/gdb-wui/internal/hub"
	"github.com/retrocpugeek/gdb-wui/internal/wire"
)

// The unit tests either exercise the security layer with a stub socket or the
// hub with no security layer. This file is the seam between them: a real
// WebSocket, through the real authorization gate, over a real listener. The bug
// it catches is the one neither half can — the upgrade being authorized in the
// wrong place, or the cookie not reaching Accept.

// hubFixture wires a real hub behind the real server. The hub's allowed origins
// name the port, so it cannot be built until the listener exists.
func hubFixture(t *testing.T) (*fixture, *hub.Hub) {
	t.Helper()
	var h *hub.Hub
	f := newFixtureFactory(t, func(addr net.Addr) http.Handler {
		_, port, err := net.SplitHostPort(addr.String())
		if err != nil {
			t.Fatal(err)
		}
		h = hub.New(hub.Config{
			AllowedOrigins: []string{"127.0.0.1:" + port, "localhost:" + port},
			Logf:           t.Logf,
			ProjectRoot:    "/tmp/project",
			Version:        "test",
		})
		return h.Handler()
	})
	return f, h
}

func (f *fixture) dialWS(t *testing.T, ctx context.Context, headers http.Header) (*websocket.Conn, *http.Response, error) {
	t.Helper()
	return websocket.Dial(ctx, "ws://"+f.host+"/ws", &websocket.DialOptions{HTTPHeader: headers})
}

func (f *fixture) authHeaders() http.Header {
	return http.Header{
		"Cookie": []string{f.api.SessionCookie().String()},
		"Origin": []string{f.origin},
	}
}

func TestWebSocketThroughTheFullStack(t *testing.T) {
	f, _ := hubFixture(t)

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	ws, _, err := f.dialWS(t, ctx, f.authHeaders())
	if err != nil {
		t.Fatalf("dial through the full stack: %v", err)
	}
	defer ws.CloseNow()

	_, data, err := ws.Read(ctx)
	if err != nil {
		t.Fatalf("reading hello: %v", err)
	}
	var ev wire.Event
	if err := json.Unmarshal(data, &ev); err != nil {
		t.Fatal(err)
	}
	if ev.Event != wire.EventHello {
		t.Fatalf("first message = %q, want hello", ev.Event)
	}
	var hello wire.Hello
	if err := json.Unmarshal(ev.Payload, &hello); err != nil {
		t.Fatal(err)
	}
	if hello.Protocol != wire.Protocol || hello.RunState != wire.RunStateNoProgram {
		t.Errorf("hello = %+v", hello)
	}

	// A round-trip, to prove the connection is usable and not merely accepted.
	req, _ := json.Marshal(wire.Request{ID: 1, Type: wire.TypeSessionPing})
	if err := ws.Write(ctx, websocket.MessageText, req); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, data, err = ws.Read(ctx)
	if err != nil {
		t.Fatalf("reading pong: %v", err)
	}
	var res wire.Response
	if err := json.Unmarshal(data, &res); err != nil {
		t.Fatal(err)
	}
	if !res.OK || res.ID != 1 {
		t.Errorf("response = %+v, want a successful reply to id 1", res)
	}
}

// TestWebSocketRefusedWithoutCookie must fail with a 401 at the HTTP layer, not
// with a successful upgrade followed by a close, which is what happens
// when authorization lives inside the WebSocket handler instead of in front of
// it.
func TestWebSocketRefusedWithoutCookie(t *testing.T) {
	f, h := hubFixture(t)

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	_, res, err := f.dialWS(t, ctx, http.Header{"Origin": []string{f.origin}})
	if err == nil {
		t.Fatal("the upgrade succeeded without a session cookie")
	}
	if res == nil {
		t.Fatalf("no HTTP response: %v", err)
	}
	if res.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 (error was %v)", res.StatusCode, err)
	}
	if got := h.Count(); got != 0 {
		t.Errorf("the hub registered %d connections for a rejected upgrade", got)
	}
}

// TestWebSocketRefusedCrossOrigin is the attack that matters most: a hostile
// page opening a socket to the debugger.
func TestWebSocketRefusedCrossOrigin(t *testing.T) {
	f, h := hubFixture(t)

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	headers := f.authHeaders()
	headers.Set("Origin", "http://evil.example")
	_, res, err := f.dialWS(t, ctx, headers)
	if err == nil {
		t.Fatal("a cross-origin upgrade succeeded")
	}
	if res != nil && res.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d, want 403", res.StatusCode)
	}
	if got := h.Count(); got != 0 {
		t.Errorf("the hub registered %d connections for a cross-origin upgrade", got)
	}
}

// TestConcurrentClients: two browser tabs are a supported configuration, and
// the reason hello carries a full snapshot.
func TestConcurrentClients(t *testing.T) {
	f, h := hubFixture(t)

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	dialOne := func() *websocket.Conn {
		ws, _, err := f.dialWS(t, ctx, f.authHeaders())
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		t.Cleanup(func() { _ = ws.CloseNow() })
		return ws
	}

	for i, ws := range []*websocket.Conn{dialOne(), dialOne()} {
		_, data, err := ws.Read(ctx)
		if err != nil {
			t.Fatalf("client %d: %v", i, err)
		}
		if !strings.Contains(string(data), `"hello"`) {
			t.Errorf("client %d first message = %s", i, data)
		}
	}

	deadline := time.Now().Add(2 * time.Second)
	for h.Count() != 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := h.Count(); got != 2 {
		t.Errorf("hub has %d connections, want 2", got)
	}
}
