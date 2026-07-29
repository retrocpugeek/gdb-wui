// Package httpapi is the HTTP surface: routing, authentication, the security
// headers, and the two bulk-read endpoints.
package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/retrocpugeek/gdb-wui/internal/assets"
	"github.com/retrocpugeek/gdb-wui/internal/srcfs"
	"github.com/retrocpugeek/gdb-wui/internal/wire"
)

// Config configures a Server.
type Config struct {
	// Files is the project browser. Required.
	Files *srcfs.FS
	// Assets serves the frontend. Required.
	Assets *assets.Assets
	// WebSocket handles /ws. It is invoked only after authorization passes.
	WebSocket http.Handler
	// Addr is the listener's address, used to build the Host and Origin
	// allowlists. Required.
	Addr net.Addr
	// ListenAnywhere disables the loopback Host and Origin checks. It exists
	// for the -listen-anywhere escape hatch and is loudly discouraged.
	ListenAnywhere bool
	// Logf receives diagnostics.
	Logf func(format string, args ...any)
}

// Server is the HTTP surface.
type Server struct {
	files  *srcfs.FS
	assets *assets.Assets
	ws     http.Handler
	tokens *tokens
	logf   func(string, ...any)

	listenAnywhere bool
	allowedHosts   []string
	allowedOrigins []string

	mux *http.ServeMux
	// csp is precomputed because it names the port.
	csp string
}

// New builds the server.
func New(cfg Config) (*Server, error) {
	if cfg.Files == nil || cfg.Assets == nil || cfg.Addr == nil {
		return nil, errors.New("httpapi: Files, Assets and Addr are required")
	}
	tok, err := newTokens()
	if err != nil {
		return nil, err
	}
	hosts, origins := loopbackHosts(cfg.Addr)
	s := &Server{
		files:          cfg.Files,
		assets:         cfg.Assets,
		ws:             cfg.WebSocket,
		tokens:         tok,
		logf:           cfg.Logf,
		listenAnywhere: cfg.ListenAnywhere,
		allowedHosts:   hosts,
		allowedOrigins: origins,
	}
	if s.logf == nil {
		s.logf = func(string, ...any) {}
	}
	s.csp = buildCSP(origins)
	s.routes()
	return s, nil
}

// BootstrapURL is the one-shot URL to open in a browser.
func (s *Server) BootstrapURL() string {
	return fmt.Sprintf("http://%s/?t=%s", s.primaryHost(), s.tokens.bootstrapToken())
}

// MintSecret is the credential for /api/bootstrap-url. The caller writes it to
// the run file, which is mode 0600; it must not be logged or put in a URL.
func (s *Server) MintSecret() string { return s.tokens.mintSecret() }

// MintPath is the endpoint that issues a fresh bootstrap URL.
const MintPath = "/api/bootstrap-url"

// SessionCookie returns a cookie that authenticates as this session. Tests use
// it; nothing in the running program does.
func (s *Server) SessionCookie() *http.Cookie {
	return &http.Cookie{Name: cookieName, Value: s.tokens.sessionToken()}
}

func (s *Server) routes() {
	s.mux = http.NewServeMux()
	s.mux.HandleFunc("GET /api/tree", s.handleTree)
	s.mux.HandleFunc("GET /api/file", s.handleFile)
	s.mux.Handle("/ws", http.HandlerFunc(s.handleWS))
	s.mux.Handle("/", s.assets.Handler())
}

// ServeHTTP applies the security headers, then the bootstrap flow, then
// authorization, then routes. The order is the point: nothing reaches a handler
// without having passed the gate, and there is one gate rather than one per
// route, because the route somebody forgets to protect is the one that matters.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.setSecurityHeaders(w)

	// The bootstrap redirect is the only route reachable without a cookie. It
	// still goes through the Host and Origin checks first.
	if r.URL.Path == "/" && r.Method == http.MethodGet {
		if !s.checkHost(w, r) || !s.checkOrigin(w, r) {
			return
		}
		if s.handleBootstrap(w, r) {
			return
		}
	}

	// The mint endpoint carries its own credential instead of the session
	// cookie — the whole point of it is to hand out a way to *get* a cookie.
	// It still passes the Host and Origin checks first.
	if r.URL.Path == MintPath {
		if !s.checkHost(w, r) || !s.checkOrigin(w, r) {
			return
		}
		s.handleMint(w, r)
		return
	}

	if !s.authorize(w, r) {
		return
	}
	s.mux.ServeHTTP(w, r)
}

// handleMint issues a fresh single-use bootstrap URL to a caller that can prove
// it is the same local user, by presenting the secret from the 0600 run file.
func (s *Server) handleMint(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		s.writeError(w, http.StatusMethodNotAllowed, wire.CodeBadRequest, "use POST")
		return
	}
	if !s.tokens.checkMint(r.Header.Get(MintHeader)) {
		s.deny(w, r, http.StatusUnauthorized, "unauthorized",
			"missing or invalid mint credential")
		return
	}
	token, err := s.tokens.newBootstrap()
	if err != nil {
		s.logf("minting a bootstrap token: %v", err)
		s.writeError(w, http.StatusInternalServerError, wire.CodeInternal, "could not mint a token")
		return
	}
	s.logf("issued a fresh bootstrap token %s", redact(token))
	s.writeJSON(w, http.StatusOK, map[string]string{
		"url": fmt.Sprintf("http://%s/?t=%s", s.primaryHost(), token),
	})
}

// MintHeader carries the mint credential.
const MintHeader = "X-Gdb-Wui-Mint"

func (s *Server) primaryHost() string {
	if len(s.allowedHosts) > 0 {
		return s.allowedHosts[0]
	}
	return "127.0.0.1"
}

// setSecurityHeaders applies headers to every response, including errors.
func (s *Server) setSecurityHeaders(w http.ResponseWriter) {
	h := w.Header()
	// No CORS headers are ever set. Their absence is what makes a cross-origin
	// fetch unreadable even if it were somehow allowed to be sent.
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Cross-Origin-Resource-Policy", "same-origin")
	h.Set("Cross-Origin-Opener-Policy", "same-origin")
	h.Set("Content-Security-Policy", s.csp)
}

func buildCSP(origins []string) string {
	// connect-src has to name the WebSocket origins explicitly: 'self' does not
	// cover ws:// URLs in every browser.
	var ws []string
	for _, o := range origins {
		ws = append(ws, "ws://"+strings.TrimPrefix(o, "http://"))
	}
	connect := "'self'"
	if len(ws) > 0 {
		connect += " " + strings.Join(ws, " ")
	}
	return strings.Join([]string{
		"default-src 'none'",
		"script-src 'self'",
		// 'unsafe-inline' for styles only, and only because xterm injects a
		// <style> element at runtime (M5). Revisit once that is in and drop it
		// if it turns out to be unnecessary.
		"style-src 'self' 'unsafe-inline'",
		"img-src 'self' data:",
		"font-src 'self'",
		"connect-src " + connect,
		"frame-ancestors 'none'",
		"base-uri 'none'",
		"form-action 'none'",
	}, "; ")
}

// handleWS authorizes and then hands off. Authorization happens here, before
// the handler can call websocket.Accept.
func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	if s.ws == nil {
		s.writeError(w, http.StatusNotFound, wire.CodeNotFound, "websocket endpoint is not configured")
		return
	}
	s.ws.ServeHTTP(w, r)
}

func (s *Server) handleTree(w http.ResponseWriter, r *http.Request) {
	listing, err := s.files.Tree(r.URL.Query().Get("path"))
	if err != nil {
		s.writeFSError(w, err)
		return
	}
	out := wire.Tree{
		Path:      listing.Path,
		Entries:   make([]wire.TreeEntry, 0, len(listing.Entries)),
		Truncated: listing.Truncated,
	}
	for _, e := range listing.Entries {
		out.Entries = append(out.Entries, wire.TreeEntry{
			Name: e.Name, Path: e.Path, Dir: e.Dir, Size: e.Size, Symlink: e.Symlink,
		})
	}
	w.Header().Set("Cache-Control", "no-store")
	s.writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleFile(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Query().Get("path")
	if path == "" {
		s.writeError(w, http.StatusBadRequest, wire.CodeBadRequest, "path is required")
		return
	}
	file, err := s.files.ReadFile(path)
	if err != nil {
		s.writeFSError(w, err)
		return
	}

	w.Header().Set("ETag", file.ETag)
	w.Header().Set("Cache-Control", "no-cache")
	if match := r.Header.Get("If-None-Match"); match != "" && strings.Contains(match, file.ETag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	// text/plain with an explicit charset, and nosniff from the global headers:
	// a .html file in the project must never be interpreted as a document.
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Length", fmt.Sprint(len(file.Content)))
	_, _ = w.Write(file.Content)
}

// writeFSError maps srcfs errors onto status codes and protocol error codes.
// The mapping is here, once, rather than at each call site.
func (s *Server) writeFSError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, srcfs.ErrNotFound):
		s.writeError(w, http.StatusNotFound, wire.CodeNotFound, "no such file or directory")
	case errors.Is(err, srcfs.ErrDenied):
		// Deliberately identical in shape to not-found so a probe cannot use
		// the distinction to map the filesystem outside the root.
		s.writeError(w, http.StatusForbidden, wire.CodePathDenied, "path is outside the project root")
	case errors.Is(err, srcfs.ErrTooLarge):
		s.writeError(w, http.StatusRequestEntityTooLarge, wire.CodeTooLarge, err.Error())
	case errors.Is(err, srcfs.ErrBinary):
		s.writeError(w, http.StatusUnsupportedMediaType, wire.CodeBadRequest,
			"file is not text; use the memory viewer")
	case errors.Is(err, srcfs.ErrIsDir):
		s.writeError(w, http.StatusBadRequest, wire.CodeBadRequest, "path is a directory")
	default:
		s.logf("filesystem error: %v", err)
		s.writeError(w, http.StatusInternalServerError, wire.CodeInternal, "internal error")
	}
}

func (s *Server) writeJSON(w http.ResponseWriter, status int, body any) {
	buf, err := json.Marshal(body)
	if err != nil {
		s.logf("marshalling response: %v", err)
		http.Error(w, `{"error":{"code":"internal","message":"encoding failed"}}`,
			http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Length", fmt.Sprint(len(buf)))
	w.WriteHeader(status)
	_, _ = w.Write(buf)
}

func (s *Server) writeError(w http.ResponseWriter, status int, code, message string) {
	s.writeJSON(w, status, wire.ErrorBody{Error: wire.Error{Code: code, Message: message}})
}

// deny logs and refuses. Every rejection goes through it so that adding a
// counter or a rate limit later is a one-line change.
func (s *Server) deny(w http.ResponseWriter, r *http.Request, status int, code, message string) {
	s.logf("denied %s %s from %s: %s (%s)", r.Method, r.URL.Path, r.RemoteAddr, code, message)
	s.writeError(w, status, code, message)
}

// NewHTTPServer wraps a Server in an *http.Server with the timeouts and limits
// that keep a stuck or hostile client from holding resources.
func NewHTTPServer(s *Server) *http.Server {
	return &http.Server{
		Handler: http.MaxBytesHandler(s, 1<<20),
		// A slowloris that never finishes its headers costs one goroutine
		// without this.
		ReadHeaderTimeout: 5 * time.Second,
		MaxHeaderBytes:    1 << 16,
		// No WriteTimeout: the WebSocket connection is long-lived and a write
		// deadline would sever it. Per-message deadlines live in the hub.
		IdleTimeout: 120 * time.Second,
	}
}
