package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/retrocpugeek/gdb-wui/internal/httpapi"
	"github.com/retrocpugeek/gdb-wui/internal/runfile"
	"github.com/retrocpugeek/gdb-wui/internal/wire"
)

// The bridge's connection to a running gdb-wui.
//
// It is an ordinary client of the same WebSocket the browser uses, and that is
// the whole design of this package. Every guard in the server — the read-only
// project refusal, the run-state gate, address translation, the undo journal,
// the broadcast that repaints open tabs — applies to an agent because an agent
// is on the same road as everybody else. Reaching past the protocol would mean
// re-implementing all of it, and getting it wrong somewhere nobody looks.
//
// It also has to be a WebSocket client rather than an HTTP caller, for a reason
// that only shows up in the execution tools: a stop is an *event*, and the
// tools that run the program are synchronous to the next one. See runToStop.

// dialTimeout bounds finding and joining the server. Everything here is
// loopback and local, so a second is already generous.
const dialTimeout = 10 * time.Second

// session is the live connection.
type session struct {
	ws *websocket.Conn

	mu      sync.Mutex
	nextID  uint64
	pending map[uint64]chan wire.Response
	// watchers receive every event. A slice rather than one channel because
	// two tools can be waiting at once — a stop and a timeout race, and both
	// have to see what arrives.
	watchers map[int]chan wire.Event
	watchSeq int
	// dead is closed when the read loop stops, and err says why.
	dead chan struct{}
	err  error

	// stopSeq is the last stop the server reported, updated from events. Read
	// tools do not send it — a stale request would be dropped, and an agent
	// asking about the state it can see is not making a mistake — but tools
	// report it, so a model can notice that what it holds predates the stop.
	stopSeq uint64
	state   string
}

// connect finds the running gdb-wui and joins it.
//
// The credential path is the one -print-url already walks: the run file is mode
// 0600, so a caller that can read the mint secret is the user who started the
// server; the mint endpoint hands back a single-use URL; redeeming it yields
// the session cookie. Nothing new is added to the threat model, because nothing
// new is added at all.
func connect(ctx context.Context, addr string) (*session, error) {
	entry, err := runfile.Find(addr)
	if err != nil {
		if errors.Is(err, runfile.ErrNoServer) {
			return nil, fmt.Errorf("%w — start one with `gdb-wui -project <dir>`", err)
		}
		return nil, err
	}
	base := "http://" + entry.Addr

	client := &http.Client{
		Timeout: dialTimeout,
		// The redirect that follows a redeemed token goes to the app itself,
		// which is a page this has no use for. The cookie is on the 303.
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	mint, err := http.NewRequestWithContext(ctx, http.MethodPost, base+httpapi.MintPath, nil)
	if err != nil {
		return nil, err
	}
	mint.Header.Set(httpapi.MintHeader, entry.MintSecret)
	// The server requires an Origin on anything but a GET, and one of its own.
	mint.Header.Set("Origin", base)
	mint.Header.Set("Sec-Fetch-Site", "same-origin")
	res, err := client.Do(mint)
	if err != nil {
		return nil, fmt.Errorf("could not reach the server at %s: %w", entry.Addr, err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(res.Body, 4096))
		return nil, fmt.Errorf("the server refused to issue a link (%s): %s",
			res.Status, strings.TrimSpace(string(body)))
	}
	var minted struct {
		URL string `json:"url"`
	}
	if err := json.NewDecoder(res.Body).Decode(&minted); err != nil {
		return nil, fmt.Errorf("decoding the reply: %w", err)
	}

	cookie, err := redeem(ctx, client, minted.URL, base)
	if err != nil {
		return nil, err
	}

	header := http.Header{}
	header.Set("Cookie", cookie)
	// A WebSocket upgrade needs an Origin the server recognises, which is the
	// same anti-rebinding rule a browser is held to.
	header.Set("Origin", base)
	ws, _, err := websocket.Dial(ctx, "ws://"+entry.Addr+"/ws", &websocket.DialOptions{
		HTTPClient: &http.Client{Timeout: dialTimeout},
		HTTPHeader: header,
	})
	if err != nil {
		return nil, fmt.Errorf("connecting to %s: %w", entry.Addr, err)
	}
	// A decompiled function of a large binary is megabytes, and the default cap
	// would drop it as too large — turning "this function is big" into "the
	// connection died", which is a much worse thing to debug.
	ws.SetReadLimit(64 << 20)

	s := &session{
		ws:       ws,
		pending:  make(map[uint64]chan wire.Response),
		watchers: make(map[int]chan wire.Event),
		dead:     make(chan struct{}),
	}
	go s.read()
	return s, nil
}

// redeem trades the single-use URL for the session cookie.
func redeem(ctx context.Context, client *http.Client, url, base string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Sec-Fetch-Site", "none")
	res, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("redeeming the login link: %w", err)
	}
	defer res.Body.Close()
	for _, c := range res.Cookies() {
		if c.Value != "" {
			return c.Name + "=" + c.Value, nil
		}
	}
	return "", fmt.Errorf("the login link at %s handed back no session cookie (%s)",
		base, res.Status)
}

// read is the one place the connection is read from. Replies go to whoever is
// waiting on that id; events go to every watcher and update the cached stop.
func (s *session) read() {
	defer close(s.dead)
	for {
		_, data, err := s.ws.Read(context.Background())
		if err != nil {
			s.mu.Lock()
			s.err = err
			for _, ch := range s.pending {
				close(ch)
			}
			s.pending = map[uint64]chan wire.Response{}
			s.mu.Unlock()
			return
		}
		// One decode to tell the two apart: a reply carries an id, an event
		// carries a name, and nothing carries both.
		var probe struct {
			ID    *uint64 `json:"id"`
			Event string  `json:"event"`
		}
		if json.Unmarshal(data, &probe) != nil {
			continue
		}
		switch {
		case probe.Event != "":
			var ev wire.Event
			if json.Unmarshal(data, &ev) != nil {
				continue
			}
			s.deliver(ev)
		case probe.ID != nil:
			var res wire.Response
			if json.Unmarshal(data, &res) != nil {
				continue
			}
			s.mu.Lock()
			ch := s.pending[res.ID]
			delete(s.pending, res.ID)
			s.mu.Unlock()
			if ch != nil {
				ch <- res
				close(ch)
			}
		}
	}
}

// deliver fans one event out and keeps the cached run state current.
func (s *session) deliver(ev wire.Event) {
	var payload struct {
		StopSeq  uint64 `json:"stopSeq"`
		RunState string `json:"runState"`
	}
	_ = json.Unmarshal(ev.Payload, &payload)

	s.mu.Lock()
	if payload.StopSeq > s.stopSeq {
		s.stopSeq = payload.StopSeq
	}
	switch ev.Event {
	case wire.EventStopped:
		s.state = wire.RunStateStopped
	case wire.EventRunning:
		s.state = wire.RunStateRunning
	case wire.EventExited:
		s.state = wire.RunStateExited
	case wire.EventHello:
		if payload.RunState != "" {
			s.state = payload.RunState
		}
	}
	watchers := make([]chan wire.Event, 0, len(s.watchers))
	for _, ch := range s.watchers {
		watchers = append(watchers, ch)
	}
	s.mu.Unlock()

	for _, ch := range watchers {
		// Never block the reader on a slow watcher: a full buffer means that
		// watcher has more events than it can use, and dropping one is better
		// than stalling every other request on the connection.
		select {
		case ch <- ev:
		default:
		}
	}
}

// watch subscribes to events until the returned function is called.
func (s *session) watch() (<-chan wire.Event, func()) {
	ch := make(chan wire.Event, 64)
	s.mu.Lock()
	s.watchSeq++
	id := s.watchSeq
	s.watchers[id] = ch
	s.mu.Unlock()
	return ch, func() {
		s.mu.Lock()
		delete(s.watchers, id)
		s.mu.Unlock()
	}
}

// call makes one request and waits for its reply.
func (s *session) call(ctx context.Context, typ string, payload any) (json.RawMessage, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	if s.err != nil {
		err := s.err
		s.mu.Unlock()
		return nil, fmt.Errorf("the connection to gdb-wui is gone: %w", err)
	}
	s.nextID++
	id := s.nextID
	ch := make(chan wire.Response, 1)
	s.pending[id] = ch
	s.mu.Unlock()

	req, err := json.Marshal(wire.Request{ID: id, Type: typ, Payload: body})
	if err != nil {
		return nil, err
	}
	if err := s.ws.Write(ctx, websocket.MessageText, req); err != nil {
		s.mu.Lock()
		delete(s.pending, id)
		s.mu.Unlock()
		return nil, err
	}

	select {
	case res, ok := <-ch:
		if !ok {
			return nil, fmt.Errorf("the connection to gdb-wui closed while waiting for %s", typ)
		}
		if res.Error != nil {
			// gdb's and Ghidra's own wording, passed through: it says which
			// name is duplicated and what is wrong with a type string, and a
			// summary of it would be less useful to a reader and to a model.
			return nil, fmt.Errorf("%s", res.Error.Message)
		}
		return res.Payload, nil
	case <-ctx.Done():
		s.mu.Lock()
		delete(s.pending, id)
		s.mu.Unlock()
		return nil, ctx.Err()
	case <-s.dead:
		return nil, fmt.Errorf("the connection to gdb-wui is gone")
	}
}

// stopState is what every execution tool answers with: where the program ended
// up, in one reply, rather than an acknowledgement the caller has to chase.
type stopState struct {
	State   string          `json:"state"`
	Reason  string          `json:"reason,omitempty"`
	StopSeq uint64          `json:"stopSeq,omitempty"`
	Frame   json.RawMessage `json:"frame,omitempty"`
	// Note explains an outcome that is not a stop, so a model does not have to
	// infer it from an absence.
	Note string `json:"note,omitempty"`
}

// runToStop sends an execution request and waits for the program to stop.
//
// This is the reason the bridge holds a WebSocket. gdb answers ^running as soon
// as it accepts the command, and the stop arrives later as an event; meanwhile
// every read request is refused with "the inferior is running". An agent given
// the raw asynchrony would poll — burning a round trip and a model's attention
// on a question the connection already knows the answer to — and would race.
//
// The subscription is taken *before* the request goes out, because the stop can
// beat the acknowledgement back.
func (s *session) runToStop(ctx context.Context, typ string, payload any,
	timeout time.Duration) (*stopState, error) {

	events, stop := s.watch()
	defer stop()

	if _, err := s.call(ctx, typ, payload); err != nil {
		return nil, err
	}

	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	for {
		select {
		case ev := <-events:
			switch ev.Event {
			case wire.EventStopped:
				return s.stopped(ev), nil
			case wire.EventExited:
				return &stopState{
					State: wire.RunStateExited,
					Note:  "the program exited; there is nothing left to inspect",
				}, nil
			case wire.EventGDBDead:
				return nil, fmt.Errorf("gdb died")
			}
		case <-deadline.C:
			return &stopState{
				State: wire.RunStateRunning,
				Note: fmt.Sprintf(
					"still running after %s. It may be waiting for input on the "+
						"program's terminal, or in a loop; `pause` stops it.",
					timeout),
			}, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-s.dead:
			return nil, fmt.Errorf("the connection to gdb-wui is gone")
		}
	}
}

// stopped turns a stop event into the answer a tool returns.
func (s *session) stopped(ev wire.Event) *stopState {
	var payload struct {
		Reason  string          `json:"reason"`
		StopSeq uint64          `json:"stopSeq"`
		Frame   json.RawMessage `json:"frame"`
	}
	_ = json.Unmarshal(ev.Payload, &payload)
	return &stopState{
		State:   wire.RunStateStopped,
		Reason:  payload.Reason,
		StopSeq: payload.StopSeq,
		Frame:   payload.Frame,
	}
}

// waitForStop is runToStop without the request, for when something else — a
// person at the browser, or a continue the agent did not send — resumed the
// program.
func (s *session) waitForStop(ctx context.Context, timeout time.Duration) (*stopState, error) {
	events, stop := s.watch()
	defer stop()

	s.mu.Lock()
	state := s.state
	seq := s.stopSeq
	s.mu.Unlock()
	if state == wire.RunStateStopped {
		return &stopState{State: wire.RunStateStopped, StopSeq: seq,
			Note: "already stopped"}, nil
	}
	if state == wire.RunStateExited {
		return &stopState{State: wire.RunStateExited,
			Note: "the program has exited"}, nil
	}

	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	for {
		select {
		case ev := <-events:
			switch ev.Event {
			case wire.EventStopped:
				return s.stopped(ev), nil
			case wire.EventExited:
				return &stopState{State: wire.RunStateExited,
					Note: "the program exited while waiting"}, nil
			}
		case <-deadline.C:
			return &stopState{State: wire.RunStateRunning,
				Note: fmt.Sprintf("still running after %s", timeout)}, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-s.dead:
			return nil, fmt.Errorf("the connection to gdb-wui is gone")
		}
	}
}

// waitForDecompiler blocks until Ghidra is ready, failed, or absent.
//
// The same argument as runToStop, for the same reason: importing and analysing
// a binary is seconds for a small program and minutes for firmware, and an
// agent that had to poll would spend a turn on each attempt. The server
// broadcasts decompChanged when the state moves, so waiting is free here and
// expensive anywhere else.
//
// It also starts the decompiler, because asking for its status is what starts
// it — deliberately, so that a session which never opens the pane never spawns
// a JVM.
func (s *session) waitForDecompiler(ctx context.Context, timeout time.Duration) (any, error) {
	events, stop := s.watch()
	defer stop()

	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	for {
		raw, err := s.call(ctx, wire.TypeDecompStatus, struct{}{})
		if err != nil {
			return nil, err
		}
		var status struct {
			State string `json:"state"`
		}
		_ = json.Unmarshal(raw, &status)
		switch status.State {
		case wire.DecompReady, wire.DecompFailed, wire.DecompOff, "":
			return json.RawMessage(raw), nil
		}

		select {
		case ev := <-events:
			// Any decompiler event is a reason to look again; a log line is
			// how a slow import announces progress, and the state change
			// itself arrives as decompChanged.
			if ev.Event != wire.EventDecompChanged && ev.Event != wire.EventDecompLog {
				continue
			}
		case <-deadline.C:
			return json.RawMessage(raw), nil
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-s.dead:
			return nil, fmt.Errorf("the connection to gdb-wui is gone")
		}
	}
}

func (s *session) close() {
	_ = s.ws.Close(websocket.StatusNormalClosure, "")
}
