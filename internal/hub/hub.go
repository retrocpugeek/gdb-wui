// Package hub owns the WebSocket connections: accepting them, pumping frames,
// dispatching requests, and fanning events out.
//
// It knows the envelope and nothing about debugging. Domain requests are handed
// to a Session, which M3 implements with the real debugger; in M2 there is no
// session and those requests come back as "unsupported".
package hub

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/retrocpugeek/gdb-wui/internal/wire"
)

// Session is the domain behind the socket.
type Session interface {
	// Snapshot returns the state a newly connected client needs. It is called
	// once per connection, before anything else is sent.
	Snapshot() wire.Hello
	// Handle executes one request. Returning a *wire.Error produces a failed
	// response; returning a nil error and a payload produces a successful one.
	Handle(ctx context.Context, req wire.Request) (any, *wire.Error)
}

// Config configures a Hub.
type Config struct {
	// Session handles domain requests. May be nil, in which case only the
	// session.* group works.
	Session Session
	// AllowedOrigins is passed to websocket.Accept. Setting it explicitly means
	// behaviour never depends on a library default changing.
	AllowedOrigins []string
	// Logf receives diagnostics.
	Logf func(format string, args ...any)
	// ProjectRoot is reported in the hello when there is no Session.
	ProjectRoot string
	// Version is the server build version.
	Version string
}

// Limits.
const (
	// readLimit caps one inbound frame. Requests are small; a megabyte is
	// already absurd and anything larger is a bug or an attack.
	readLimit = 1 << 20
	// outboundBuffer is how many events may queue for one browser before it is
	// disconnected.
	outboundBuffer = 256
	// writeTimeout bounds a single frame write.
	writeTimeout = 10 * time.Second
	// pingInterval detects a browser that vanished without closing.
	pingInterval = 30 * time.Second
)

// Hub tracks the live connections.
type Hub struct {
	cfg  Config
	logf func(string, ...any)

	mu    sync.Mutex
	conns map[*conn]struct{}

	// session is swapped in after construction; see SetSession.
	session atomic.Pointer[Session]

	// seq is server-monotonic across every event on every connection, so a
	// client can detect a gap and tests can assert ordering.
	seq atomic.Uint64

	closed atomic.Bool
}

// New builds a Hub.
func New(cfg Config) *Hub {
	h := &Hub{cfg: cfg, logf: cfg.Logf, conns: map[*conn]struct{}{}}
	if h.logf == nil {
		h.logf = func(string, ...any) {}
	}
	if cfg.Session != nil {
		h.session.Store(&cfg.Session)
	}
	return h
}

// SetSession attaches the debugger after construction.
//
// The three-way cycle is the reason this exists: the MI client needs an event
// handler, the handler is the debugger session, and the session broadcasts
// through the hub. Something has to be wired up second.
func (h *Hub) SetSession(s Session) {
	h.session.Store(&s)
}

// currentSession returns the attached session, or nil.
func (h *Hub) currentSession() Session {
	if p := h.session.Load(); p != nil {
		return *p
	}
	return nil
}

// conn is one browser connection.
type conn struct {
	ws  *websocket.Conn
	out chan []byte
	// dropped is set when the outbound queue overflowed, so the close reason
	// says why.
	dropped atomic.Bool
}

// Handler returns the /ws handler.
//
// Authentication has already happened: httpapi.authorize runs before this, and
// must, because websocket.Accept writes the 101 response and there is no way to
// retract it afterwards.
func (h *Hub) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if h.closed.Load() {
			http.Error(w, "server is shutting down", http.StatusServiceUnavailable)
			return
		}
		ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{
			OriginPatterns: h.cfg.AllowedOrigins,
			// Compression off: the traffic is small JSON on loopback, where
			// compression costs CPU on the step hot path and saves nothing.
			CompressionMode: websocket.CompressionDisabled,
		})
		if err != nil {
			h.logf("websocket accept: %v", err)
			return
		}
		ws.SetReadLimit(readLimit)
		h.serve(r.Context(), ws)
	})
}

func (h *Hub) serve(ctx context.Context, ws *websocket.Conn) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	c := &conn{ws: ws, out: make(chan []byte, outboundBuffer)}
	h.add(c)
	defer h.remove(c)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer cancel()
		h.writePump(ctx, c)
	}()

	// The hello goes out before the first read, so a client that sends nothing
	// still receives the full snapshot. Everything about reconnect, reload and
	// second-tab support depends on this being unconditional.
	h.sendTo(c, h.helloEvent())

	h.readPump(ctx, c)
	cancel()
	wg.Wait()

	_ = ws.Close(websocket.StatusNormalClosure, "")
}

func (h *Hub) readPump(ctx context.Context, c *conn) {
	for {
		typ, data, err := c.ws.Read(ctx)
		if err != nil {
			status := websocket.CloseStatus(err)
			if status != websocket.StatusNormalClosure && status != websocket.StatusGoingAway &&
				!errors.Is(err, context.Canceled) {
				h.logf("read: %v", err)
			}
			return
		}
		if typ != websocket.MessageText {
			h.sendTo(c, mustMarshal(wire.Response{
				OK:    false,
				Error: wire.NewError(wire.CodeBadRequest, "binary frames are not accepted"),
			}))
			continue
		}

		var req wire.Request
		if err := json.Unmarshal(data, &req); err != nil {
			h.sendTo(c, mustMarshal(wire.Response{
				OK:    false,
				Error: wire.NewError(wire.CodeBadRequest, "malformed request: "+err.Error()),
			}))
			continue
		}
		h.sendTo(c, mustMarshal(h.dispatch(ctx, req)))
	}
}

// dispatch routes one request.
func (h *Hub) dispatch(ctx context.Context, req wire.Request) wire.Response {
	res := wire.Response{ID: req.ID, Type: req.Type}

	switch req.Type {
	case wire.TypeSessionPing:
		res.OK = true
		res.Payload = mustMarshal(map[string]any{"pong": true})
		return res

	case wire.TypeSessionHello, wire.TypeSessionInfo:
		res.OK = true
		res.Payload = mustMarshal(h.snapshot())
		return res
	}

	session := h.currentSession()
	if session == nil {
		// Unknown or not-yet-implemented types get an error response, never a
		// closed connection: a newer frontend talking to an older server should
		// degrade, not disconnect.
		res.Error = wire.NewError(wire.CodeUnsupported,
			fmt.Sprintf("%q is not supported by this server", req.Type))
		return res
	}

	payload, werr := session.Handle(ctx, req)
	if werr != nil {
		res.Error = werr
		return res
	}
	res.OK = true
	if payload != nil {
		res.Payload = mustMarshal(payload)
	}
	return res
}

func (h *Hub) snapshot() wire.Hello {
	if session := h.currentSession(); session != nil {
		return session.Snapshot()
	}
	return wire.Hello{
		Protocol:    wire.Protocol,
		Server:      h.cfg.Version,
		ProjectRoot: h.cfg.ProjectRoot,
		RunState:    wire.RunStateNoProgram,
	}
}

func (h *Hub) helloEvent() []byte {
	return mustMarshal(wire.Event{
		Event:   wire.EventHello,
		Seq:     h.seq.Add(1),
		Payload: mustMarshal(h.snapshot()),
	})
}

func (h *Hub) writePump(ctx context.Context, c *conn) {
	ticker := time.NewTicker(pingInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case msg := <-c.out:
			writeCtx, cancel := context.WithTimeout(ctx, writeTimeout)
			err := c.ws.Write(writeCtx, websocket.MessageText, msg)
			cancel()
			if err != nil {
				h.logf("write: %v", err)
				return
			}
		case <-ticker.C:
			pingCtx, cancel := context.WithTimeout(ctx, writeTimeout)
			err := c.ws.Ping(pingCtx)
			cancel()
			if err != nil {
				return
			}
		}
	}
}

// sendTo queues one message for one connection.
//
// A full queue closes the connection rather than blocking. This is the opposite
// of the policy in internal/mi, where blocking backpressures gdb, and is
// correct there. Here, blocking would let one wedged browser tab stall the
// debugger for every other client, so the wedged tab is dropped instead. It
// will reconnect and receive a fresh snapshot, which is the recovery path
// hello already provides.
func (h *Hub) sendTo(c *conn, msg []byte) {
	select {
	case c.out <- msg:
	default:
		if c.dropped.CompareAndSwap(false, true) {
			h.logf("outbound queue full; disconnecting a slow client")
			go func() {
				_ = c.ws.Close(websocket.StatusPolicyViolation, "client too slow")
			}()
		}
	}
}

// Broadcast fans an event out to every connection.
func (h *Hub) Broadcast(event string, payload any) {
	msg := mustMarshal(wire.Event{
		Event:   event,
		Seq:     h.seq.Add(1),
		Payload: mustMarshal(payload),
	})
	h.mu.Lock()
	conns := make([]*conn, 0, len(h.conns))
	for c := range h.conns {
		conns = append(conns, c)
	}
	h.mu.Unlock()

	for _, c := range conns {
		h.sendTo(c, msg)
	}
}

// Count returns the number of live connections.
func (h *Hub) Count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.conns)
}

// Shutdown tells every client the server is going away and closes them.
func (h *Hub) Shutdown() {
	h.closed.Store(true)
	h.Broadcast(wire.EventShuttingDown, map[string]any{})

	h.mu.Lock()
	conns := make([]*conn, 0, len(h.conns))
	for c := range h.conns {
		conns = append(conns, c)
	}
	h.mu.Unlock()

	// A brief grace period so the shuttingDown event has a chance to leave
	// before the close frame follows it.
	time.Sleep(50 * time.Millisecond)
	for _, c := range conns {
		_ = c.ws.Close(websocket.StatusGoingAway, "server shutting down")
	}
}

func (h *Hub) add(c *conn) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.conns[c] = struct{}{}
}

func (h *Hub) remove(c *conn) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.conns, c)
}

// mustMarshal panics only on a programming error: every type marshalled here is
// a wire DTO built from strings and numbers.
func mustMarshal(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(fmt.Sprintf("hub: marshalling %T: %v", v, err))
	}
	return b
}
