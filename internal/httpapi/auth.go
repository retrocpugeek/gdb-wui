package httpapi

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Threat model, stated plainly because everything in this file follows from it.
//
// gdb-wui runs arbitrary binaries with the user's full privileges. That is the
// product, not a flaw, and sandboxing the debuggee is not a goal. What is in
// scope: other local users and processes reaching the loopback port, hostile
// web pages the user happens to visit, and path traversal.
//
// Binding 127.0.0.1 is not sufficient on its own. Any page in the user's
// browser can fetch a loopback URL, and for this service a successful
// cross-origin request means remote code execution. Hence three independent
// layers below, each covering something the others miss.

const (
	// cookieName is the session cookie.
	cookieName = "gdbwui"
	// bootstrapTTL bounds how long the URL token is usable. It only has to
	// survive the gap between printing the URL and the browser opening it.
	bootstrapTTL = 60 * time.Second
)

// tokens holds the two secrets.
//
// Two, not one, because the bootstrap token necessarily appears in a URL — the
// URL is handed to xdg-open, so it lands in the process table where any local
// user can read it with ps, in browser history, and in a Referer header. A
// token with those exposures must be single-use and short-lived. The session
// token, which is the one that actually authorises requests, never appears in
// a URL or in argv.
type tokens struct {
	mu sync.Mutex

	bootstrap     string
	bootstrapExp  time.Time
	bootstrapUsed bool

	session string

	// mint authorises /api/bootstrap-url. It is held only in the run file,
	// which is mode 0600, and never appears in a URL, in argv, or in a log.
	mint string
}

func newTokens() (*tokens, error) {
	boot, err := randomToken()
	if err != nil {
		return nil, err
	}
	sess, err := randomToken()
	if err != nil {
		return nil, err
	}
	mint, err := randomToken()
	if err != nil {
		return nil, err
	}
	return &tokens{
		bootstrap:    boot,
		bootstrapExp: time.Now().Add(bootstrapTTL),
		session:      sess,
		mint:         mint,
	}, nil
}

func randomToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("httpapi: generating token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// redeemBootstrap validates and burns the bootstrap token.
func (t *tokens) redeemBootstrap(candidate string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.bootstrapUsed || candidate == "" {
		return false
	}
	if time.Now().After(t.bootstrapExp) {
		return false
	}
	// Constant time: the comparison is against a secret, and an early-exit
	// comparison leaks it a byte at a time to a caller that can measure.
	if subtle.ConstantTimeCompare([]byte(candidate), []byte(t.bootstrap)) != 1 {
		return false
	}
	t.bootstrapUsed = true
	return true
}

func (t *tokens) checkSession(candidate string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if candidate == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(candidate), []byte(t.session)) == 1
}

func (t *tokens) bootstrapToken() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.bootstrap
}

func (t *tokens) sessionToken() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.session
}

func (t *tokens) mintSecret() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.mint
}

func (t *tokens) checkMint(candidate string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if candidate == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(candidate), []byte(t.mint)) == 1
}

// newBootstrap issues a fresh single-use token, invalidating any previous one.
//
// The 60-second TTL only has to cover the gap between printing a URL and a
// browser opening it. When that gap turns out to be longer — the operator
// walked away, or started the server with -open=false — the answer is a new
// token, not a longer-lived one.
func (t *tokens) newBootstrap() (string, error) {
	tok, err := randomToken()
	if err != nil {
		return "", err
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.bootstrap = tok
	t.bootstrapExp = time.Now().Add(bootstrapTTL)
	t.bootstrapUsed = false
	return tok, nil
}

// redact keeps a token out of the logs while leaving enough to correlate.
func redact(tok string) string {
	if len(tok) <= 4 {
		return "…"
	}
	return tok[:4] + "…"
}

// authorize is the single gate. Every route goes through it, including the
// WebSocket upgrade, and for the upgrade it must run *before*
// websocket.Accept — an Accept that has already written the 101 response
// cannot be un-accepted.
func (s *Server) authorize(w http.ResponseWriter, r *http.Request) bool {
	if !s.checkHost(w, r) {
		return false
	}
	if !s.checkOrigin(w, r) {
		return false
	}

	c, err := r.Cookie(cookieName)
	if err != nil || !s.tokens.checkSession(c.Value) {
		s.deny(w, r, http.StatusUnauthorized, "unauthorized",
			"no valid session cookie; open the URL printed by gdb-wui")
		return false
	}
	return true
}

// checkHost is anti-rebinding layer one.
//
// Under DNS rebinding the attacker's page resolves evil.example to 127.0.0.1
// and requests http://evil.example:PORT/. The connection genuinely arrives on
// loopback, but the Host header still says evil.example. Requiring an exact
// match against the addresses we actually serve rejects it before anything else
// runs.
func (s *Server) checkHost(w http.ResponseWriter, r *http.Request) bool {
	host := r.Host
	if host == "" {
		s.deny(w, r, http.StatusForbidden, "bad_host", "missing Host header")
		return false
	}
	if s.listenAnywhere {
		return true
	}
	for _, allowed := range s.allowedHosts {
		if strings.EqualFold(host, allowed) {
			return true
		}
	}
	s.deny(w, r, http.StatusForbidden, "bad_host",
		fmt.Sprintf("Host %q is not one of this server's addresses", host))
	return false
}

// checkOrigin is anti-rebinding layers two and three.
//
// Origin is the strongest signal available: under rebinding the page's origin
// remains http://evil.example, so the browser both withholds our SameSite
// cookie and announces the true origin here. Sec-Fetch-Site is a second,
// independent read of the same fact that does not depend on Origin being sent.
//
// Origin is absent on ordinary top-level navigations, which is why it is
// required only for the WebSocket upgrade and for non-GET requests.
func (s *Server) checkOrigin(w http.ResponseWriter, r *http.Request) bool {
	if site := r.Header.Get("Sec-Fetch-Site"); site != "" {
		switch site {
		case "same-origin", "same-site", "none":
			// "none" is a user-initiated navigation: typing the URL, or the
			// browser we launched opening it.
		default:
			s.deny(w, r, http.StatusForbidden, "cross_site",
				fmt.Sprintf("Sec-Fetch-Site: %s", site))
			return false
		}
	}

	origin := r.Header.Get("Origin")
	needsOrigin := r.Method != http.MethodGet && r.Method != http.MethodHead
	if isWebSocketUpgrade(r) {
		needsOrigin = true
	}

	if origin == "" {
		if needsOrigin {
			s.deny(w, r, http.StatusForbidden, "missing_origin",
				"this request requires an Origin header")
			return false
		}
		return true
	}
	if s.originAllowed(origin) {
		return true
	}
	s.deny(w, r, http.StatusForbidden, "bad_origin",
		fmt.Sprintf("Origin %q is not allowed", origin))
	return false
}

// originAllowed matches an Origin against the addresses we serve.
func (s *Server) originAllowed(origin string) bool {
	if s.listenAnywhere {
		return true
	}
	for _, allowed := range s.allowedOrigins {
		if strings.EqualFold(origin, allowed) {
			return true
		}
	}
	return false
}

func isWebSocketUpgrade(r *http.Request) bool {
	return strings.EqualFold(r.Header.Get("Upgrade"), "websocket")
}

// handleBootstrap implements GET /?t=<bootstrap>: validate, burn, set the
// session cookie, and redirect to a clean URL so the token leaves the address
// bar and never reaches history or a Referer.
func (s *Server) handleBootstrap(w http.ResponseWriter, r *http.Request) bool {
	token := r.URL.Query().Get("t")
	if token == "" {
		return false
	}
	if !s.tokens.redeemBootstrap(token) {
		s.logf("rejected bootstrap token %s (expired, already used, or wrong)", redact(token))
		s.deny(w, r, http.StatusUnauthorized, "unauthorized",
			"that link has expired or was already used; restart gdb-wui for a new one")
		return true
	}

	http.SetCookie(w, &http.Cookie{
		Name:  cookieName,
		Value: s.tokens.sessionToken(),
		Path:  "/",
		// HttpOnly: script never needs it, and keeping it out of reach means an
		// XSS in a rendered source file cannot exfiltrate the session.
		HttpOnly: true,
		// Strict: the cookie is not sent on any cross-site request at all, so a
		// hostile page cannot ride it even if the other checks were bypassed.
		SameSite: http.SameSiteStrictMode,
		// No Secure: this is plain http on loopback, and setting it would stop
		// the cookie being stored at all.
	})
	// 303 so the browser re-requests with GET and the token disappears.
	http.Redirect(w, r, "/", http.StatusSeeOther)
	return true
}

// loopbackHosts builds the Host and Origin allowlists for an address.
func loopbackHosts(addr net.Addr) (hosts, origins []string) {
	_, port, err := net.SplitHostPort(addr.String())
	if err != nil {
		return nil, nil
	}
	for _, h := range []string{"127.0.0.1", "localhost", "[::1]"} {
		hostPort := h + ":" + port
		hosts = append(hosts, hostPort)
		origins = append(origins, "http://"+hostPort)
	}
	return hosts, origins
}
