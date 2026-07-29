package httpapi_test

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/retrocpugeek/gdb-wui/internal/assets"
	"github.com/retrocpugeek/gdb-wui/internal/httpapi"
	"github.com/retrocpugeek/gdb-wui/internal/srcfs"
	"github.com/retrocpugeek/gdb-wui/internal/wire"
)

// fixture builds a server over a small project, on a real loopback listener so
// the Host and Origin allowlists are the real ones.
type fixture struct {
	t      *testing.T
	api    *httpapi.Server
	ts     *httptest.Server
	host   string // "127.0.0.1:PORT"
	origin string // "http://127.0.0.1:PORT"
}

// newFixture builds a server with a fixed WebSocket handler.
func newFixture(t *testing.T, wsHandler http.Handler) *fixture {
	t.Helper()
	return newFixtureFactory(t, func(net.Addr) http.Handler { return wsHandler })
}

// newFixtureFactory is newFixture for a handler that needs the listener's
// address to be built — the hub, whose allowed origins name the port.
func newFixtureFactory(t *testing.T, makeWS func(net.Addr) http.Handler) *fixture {
	t.Helper()

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "main.c"), []byte("int main(void){return 0;}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "src", "util.c"), []byte("// util\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "blob.bin"), []byte("\x00\x01\x02"), 0o644); err != nil {
		t.Fatal(err)
	}

	files, err := srcfs.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = files.Close() })

	assetTree, err := assets.Embedded()
	if err != nil {
		t.Fatal(err)
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	api, err := httpapi.New(httpapi.Config{
		Files:     files,
		Assets:    assetTree,
		WebSocket: makeWS(listener.Addr()),
		Addr:      listener.Addr(),
		Logf:      t.Logf,
	})
	if err != nil {
		t.Fatal(err)
	}

	ts := &httptest.Server{
		Listener: listener,
		Config:   httpapi.NewHTTPServer(api),
	}
	ts.Start()
	t.Cleanup(ts.Close)

	host := listener.Addr().String()
	return &fixture{t: t, api: api, ts: ts, host: host, origin: "http://" + host}
}

// do issues a request with full control over the headers the security layer
// reads. It deliberately does not follow redirects: the bootstrap flow's 303 is
// something tests assert on.
func (f *fixture) do(method, path string, mutate ...func(*http.Request)) *http.Response {
	f.t.Helper()
	req, err := http.NewRequest(method, f.ts.URL+path, nil)
	if err != nil {
		f.t.Fatal(err)
	}
	req.Host = f.host
	for _, m := range mutate {
		m(req)
	}
	client := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	res, err := client.Do(req)
	if err != nil {
		f.t.Fatalf("%s %s: %v", method, path, err)
	}
	f.t.Cleanup(func() { _ = res.Body.Close() })
	return res
}

func withCookie(f *fixture) func(*http.Request) {
	return func(r *http.Request) { r.AddCookie(f.api.SessionCookie()) }
}

func header(name, value string) func(*http.Request) {
	return func(r *http.Request) { r.Header.Set(name, value) }
}

func withHost(h string) func(*http.Request) {
	return func(r *http.Request) { r.Host = h }
}

// TestSecurityMatrix is the highest-value single test in the repo. Every row is
// an attack that this server must refuse, or a legitimate request it must
// allow; a regression in any of them means a hostile web page can run programs
// as the user.
func TestSecurityMatrix(t *testing.T) {
	f := newFixture(t, stubWS{})

	cases := []struct {
		name   string
		method string
		path   string
		mutate []func(*http.Request)
		want   int
		why    string
	}{
		{
			name: "no credential", method: "GET", path: "/api/tree",
			want: http.StatusUnauthorized,
			why:  "an unauthenticated request must never reach the filesystem",
		},
		{
			name: "valid cookie", method: "GET", path: "/api/tree",
			mutate: []func(*http.Request){withCookie(f)},
			want:   http.StatusOK,
			why:    "the ordinary case must work, or the rest of the matrix proves nothing",
		},
		{
			name: "wrong cookie value", method: "GET", path: "/api/tree",
			mutate: []func(*http.Request){func(r *http.Request) {
				r.AddCookie(&http.Cookie{Name: "gdbwui", Value: "not-the-token"})
			}},
			want: http.StatusUnauthorized,
			why:  "a guessed cookie must not authenticate",
		},
		{
			name: "rebound Host", method: "GET", path: "/api/tree",
			mutate: []func(*http.Request){withCookie(f), withHost("evil.example")},
			want:   http.StatusForbidden,
			why:    "DNS rebinding arrives on loopback but carries the attacker's Host",
		},
		{
			name: "rebound Host on the asset root", method: "GET", path: "/",
			mutate: []func(*http.Request){withCookie(f), withHost("evil.example")},
			want:   http.StatusForbidden,
			why:    "the asset routes are as sensitive as the API ones",
		},
		{
			name: "cross-origin fetch", method: "GET", path: "/api/tree",
			mutate: []func(*http.Request){withCookie(f), header("Origin", "http://evil.example")},
			want:   http.StatusForbidden,
			why:    "a hostile page fetching loopback announces its origin",
		},
		{
			name: "cross-site fetch metadata", method: "GET", path: "/api/tree",
			mutate: []func(*http.Request){withCookie(f), header("Sec-Fetch-Site", "cross-site")},
			want:   http.StatusForbidden,
			why:    "Sec-Fetch-Site is an independent read of the same fact as Origin",
		},
		{
			name: "same-origin fetch metadata", method: "GET", path: "/api/tree",
			mutate: []func(*http.Request){withCookie(f), header("Sec-Fetch-Site", "same-origin")},
			want:   http.StatusOK,
			why:    "the frontend's own fetches carry same-origin",
		},
		{
			name: "user-typed navigation", method: "GET", path: "/api/tree",
			mutate: []func(*http.Request){withCookie(f), header("Sec-Fetch-Site", "none")},
			want:   http.StatusOK,
			why:    "'none' is a user-initiated navigation, not an attack",
		},
		{
			name: "own origin", method: "GET", path: "/api/tree",
			mutate: []func(*http.Request){withCookie(f), header("Origin", f.origin)},
			want:   http.StatusOK,
			why:    "our own origin must be accepted",
		},
		{
			name: "websocket upgrade without a cookie", method: "GET", path: "/ws",
			mutate: []func(*http.Request){
				header("Upgrade", "websocket"), header("Connection", "Upgrade"),
				header("Origin", f.origin),
			},
			want: http.StatusUnauthorized,
			why:  "the upgrade must be authorized before websocket.Accept writes a 101",
		},
		{
			name: "websocket upgrade with a cross-origin Origin", method: "GET", path: "/ws",
			mutate: []func(*http.Request){
				withCookie(f), header("Upgrade", "websocket"),
				header("Connection", "Upgrade"), header("Origin", "http://evil.example"),
			},
			want: http.StatusForbidden,
			why:  "a cross-origin socket is the most valuable thing an attacker could get",
		},
		{
			name: "websocket upgrade with no Origin at all", method: "GET", path: "/ws",
			mutate: []func(*http.Request){
				withCookie(f), header("Upgrade", "websocket"), header("Connection", "Upgrade"),
			},
			want: http.StatusForbidden,
			why:  "browsers always send Origin on a WebSocket handshake; its absence is not a browser",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := f.do(tc.method, tc.path, tc.mutate...)
			if res.StatusCode != tc.want {
				t.Errorf("status = %d, want %d\n  %s", res.StatusCode, tc.want, tc.why)
			}
		})
	}
}

// TestBootstrapFlow covers the one route reachable without a cookie.
func TestBootstrapFlow(t *testing.T) {
	f := newFixture(t, stubWS{})

	token := bootstrapToken(t, f.api.BootstrapURL())
	res := f.do("GET", "/?t="+url.QueryEscape(token))

	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", res.StatusCode)
	}
	if loc := res.Header.Get("Location"); loc != "/" {
		t.Errorf("Location = %q, want /; the token must not survive the redirect", loc)
	}

	var set *http.Cookie
	for _, c := range res.Cookies() {
		if c.Name == "gdbwui" {
			set = c
		}
	}
	if set == nil {
		t.Fatal("no session cookie was set")
	}
	if !set.HttpOnly {
		t.Error("cookie is not HttpOnly; script in a rendered source file could steal it")
	}
	if set.SameSite != http.SameSiteStrictMode {
		t.Errorf("SameSite = %v, want Strict", set.SameSite)
	}
	if set.Value == token {
		t.Error("the session cookie is the bootstrap token; they must be distinct secrets")
	}

	// The cookie it set must actually authenticate.
	authed := f.do("GET", "/api/tree", func(r *http.Request) { r.AddCookie(set) })
	if authed.StatusCode != http.StatusOK {
		t.Errorf("the issued cookie does not authenticate: status %d", authed.StatusCode)
	}
}

// TestBootstrapTokenIsSingleUse: the token appears in argv and browser history,
// so replaying it must fail.
func TestBootstrapTokenIsSingleUse(t *testing.T) {
	f := newFixture(t, stubWS{})
	token := bootstrapToken(t, f.api.BootstrapURL())

	if got := f.do("GET", "/?t="+url.QueryEscape(token)).StatusCode; got != http.StatusSeeOther {
		t.Fatalf("first use: status = %d, want 303", got)
	}
	if got := f.do("GET", "/?t="+url.QueryEscape(token)).StatusCode; got != http.StatusUnauthorized {
		t.Errorf("second use: status = %d, want 401", got)
	}
}

func TestBootstrapRejectsWrongToken(t *testing.T) {
	f := newFixture(t, stubWS{})
	if got := f.do("GET", "/?t=wrong").StatusCode; got != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", got)
	}
}

// TestSecurityHeaders checks the headers that limit the damage of anything that
// does get through.
// TestMintEndpoint covers the -print-url path: a caller holding the secret from
// the 0600 run file can get a fresh login link without a cookie.
func TestMintEndpoint(t *testing.T) {
	f := newFixture(t, stubWS{})

	mint := func(mutate ...func(*http.Request)) *http.Response {
		return f.do("POST", httpapi.MintPath, append([]func(*http.Request){
			header("Origin", f.origin),
		}, mutate...)...)
	}

	t.Run("no credential", func(t *testing.T) {
		if got := mint().StatusCode; got != http.StatusUnauthorized {
			t.Errorf("status = %d, want 401", got)
		}
	})

	t.Run("wrong credential", func(t *testing.T) {
		res := mint(header(httpapi.MintHeader, "not-the-secret"))
		if res.StatusCode != http.StatusUnauthorized {
			t.Errorf("status = %d, want 401", res.StatusCode)
		}
	})

	t.Run("session cookie is not enough", func(t *testing.T) {
		// The cookie authenticates ordinary requests but must not substitute
		// for the mint secret: a compromised browser tab should not be able to
		// mint fresh credentials.
		if got := mint(withCookie(f)).StatusCode; got != http.StatusUnauthorized {
			t.Errorf("status = %d, want 401", got)
		}
	})

	t.Run("cross-origin", func(t *testing.T) {
		res := mint(
			header(httpapi.MintHeader, f.api.MintSecret()),
			header("Origin", "http://evil.example"),
		)
		if res.StatusCode != http.StatusForbidden {
			t.Errorf("status = %d, want 403", res.StatusCode)
		}
	})

	t.Run("valid credential", func(t *testing.T) {
		res := mint(header(httpapi.MintHeader, f.api.MintSecret()))
		if res.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", res.StatusCode)
		}
		var body struct {
			URL string `json:"url"`
		}
		if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		token := bootstrapToken(t, body.URL)

		// The minted token must actually work, and only once.
		first := f.do("GET", "/?t="+url.QueryEscape(token))
		if first.StatusCode != http.StatusSeeOther {
			t.Errorf("minted token: status = %d, want 303", first.StatusCode)
		}
		second := f.do("GET", "/?t="+url.QueryEscape(token))
		if second.StatusCode != http.StatusUnauthorized {
			t.Errorf("minted token reuse: status = %d, want 401", second.StatusCode)
		}
	})

	t.Run("invalidates the previous token", func(t *testing.T) {
		fresh := newFixture(t, stubWS{})
		original := bootstrapToken(t, fresh.api.BootstrapURL())

		res := fresh.do("POST", httpapi.MintPath,
			header("Origin", fresh.origin),
			header(httpapi.MintHeader, fresh.api.MintSecret()))
		if res.StatusCode != http.StatusOK {
			t.Fatalf("mint: status = %d", res.StatusCode)
		}
		// Minting replaces the outstanding token rather than adding a second
		// valid one, so a URL someone screenshotted stops working.
		if got := fresh.do("GET", "/?t="+url.QueryEscape(original)).StatusCode; got != http.StatusUnauthorized {
			t.Errorf("the superseded token still works: status = %d, want 401", got)
		}
	})

	t.Run("GET is refused", func(t *testing.T) {
		res := f.do("GET", httpapi.MintPath, header(httpapi.MintHeader, f.api.MintSecret()))
		if res.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("status = %d, want 405", res.StatusCode)
		}
	})
}

func TestSecurityHeaders(t *testing.T) {
	f := newFixture(t, stubWS{})
	res := f.do("GET", "/api/tree", withCookie(f))

	for name, want := range map[string]string{
		"X-Content-Type-Options":       "nosniff",
		"Referrer-Policy":              "no-referrer",
		"Cross-Origin-Resource-Policy": "same-origin",
	} {
		if got := res.Header.Get(name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}

	csp := res.Header.Get("Content-Security-Policy")
	for _, want := range []string{
		"default-src 'none'", "script-src 'self'", "frame-ancestors 'none'",
		"base-uri 'none'", "form-action 'none'",
	} {
		if !strings.Contains(csp, want) {
			t.Errorf("CSP is missing %q\n  got: %s", want, csp)
		}
	}
	// The socket must be reachable under the policy, or the app cannot work.
	if !strings.Contains(csp, "ws://"+f.host) {
		t.Errorf("CSP connect-src does not permit the WebSocket origin\n  got: %s", csp)
	}
	// No CORS header may ever be set.
	if got := res.Header.Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("Access-Control-Allow-Origin = %q; CORS must never be enabled", got)
	}
}

// TestErrorsAreAlsoProtected: a rejected request must not leak through the
// error path either.
func TestErrorResponsesCarrySecurityHeaders(t *testing.T) {
	f := newFixture(t, stubWS{})
	res := f.do("GET", "/api/tree")
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", res.StatusCode)
	}
	if res.Header.Get("Content-Security-Policy") == "" {
		t.Error("error responses have no CSP")
	}
	var body wire.ErrorBody
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		t.Fatalf("error body is not JSON: %v", err)
	}
	if body.Error.Code == "" {
		t.Error("error body has no code")
	}
}

func TestTreeEndpoint(t *testing.T) {
	f := newFixture(t, stubWS{})
	res := f.do("GET", "/api/tree", withCookie(f))
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", res.StatusCode)
	}
	var tree wire.Tree
	if err := json.NewDecoder(res.Body).Decode(&tree); err != nil {
		t.Fatal(err)
	}
	names := map[string]wire.TreeEntry{}
	for _, e := range tree.Entries {
		names[e.Name] = e
	}
	if _, ok := names["main.c"]; !ok {
		t.Errorf("main.c missing from %v", tree.Entries)
	}
	if e, ok := names["src"]; !ok || !e.Dir {
		t.Errorf("src = %+v, want a directory", e)
	}
}

func TestTreeRejectsTraversal(t *testing.T) {
	f := newFixture(t, stubWS{})
	res := f.do("GET", "/api/tree?path=../..", withCookie(f))
	if res.StatusCode != http.StatusForbidden && res.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 403 or 404", res.StatusCode)
	}
}

func TestFileEndpoint(t *testing.T) {
	f := newFixture(t, stubWS{})
	res := f.do("GET", "/api/file?path=src/util.c", withCookie(f))
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", res.StatusCode)
	}
	if ct := res.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("Content-Type = %q, want text/plain; a .html file in the project "+
			"must never be served as a document", ct)
	}
	etag := res.Header.Get("ETag")
	if etag == "" {
		t.Error("no ETag")
	}

	// The conditional request must be honoured, or every stop re-sends the file.
	cached := f.do("GET", "/api/file?path=src/util.c", withCookie(f), header("If-None-Match", etag))
	if cached.StatusCode != http.StatusNotModified {
		t.Errorf("conditional request: status = %d, want 304", cached.StatusCode)
	}
}

func TestFileEndpointErrors(t *testing.T) {
	f := newFixture(t, stubWS{})
	for _, tc := range []struct {
		name, query string
		want        int
	}{
		{"missing path", "", http.StatusBadRequest},
		{"not found", "?path=nope.c", http.StatusNotFound},
		{"traversal", "?path=../../etc/passwd", http.StatusForbidden},
		{"directory", "?path=src", http.StatusBadRequest},
		{"binary", "?path=blob.bin", http.StatusUnsupportedMediaType},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := f.do("GET", "/api/file"+tc.query, withCookie(f))
			if res.StatusCode != tc.want {
				t.Errorf("status = %d, want %d", res.StatusCode, tc.want)
			}
		})
	}
}

func TestAssetsAreServed(t *testing.T) {
	f := newFixture(t, stubWS{})
	for _, path := range []string{"/", "/js/main.js", "/css/tokens.css"} {
		res := f.do("GET", path, withCookie(f))
		if res.StatusCode != http.StatusOK {
			t.Errorf("GET %s: status = %d", path, res.StatusCode)
		}
	}
	// Module scripts must arrive with a JavaScript MIME type or the browser
	// refuses them and the page is blank with no useful error.
	res := f.do("GET", "/js/main.js", withCookie(f))
	if ct := res.Header.Get("Content-Type"); !strings.Contains(ct, "javascript") {
		t.Errorf("Content-Type = %q, want a JavaScript type", ct)
	}
}

// TestNoSPACatchAll: a typo'd module path must 404 rather than quietly return
// index.html with a 200.
func TestNoSPACatchAll(t *testing.T) {
	f := newFixture(t, stubWS{})
	res := f.do("GET", "/js/does-not-exist.js", withCookie(f))
	if res.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", res.StatusCode)
	}
}

// stubWS stands in for the hub: reaching it means authorization passed.
type stubWS struct{}

func (stubWS) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusTeapot)
}

func bootstrapToken(t *testing.T, bootstrapURL string) string {
	t.Helper()
	u, err := url.Parse(bootstrapURL)
	if err != nil {
		t.Fatal(err)
	}
	tok := u.Query().Get("t")
	if tok == "" {
		t.Fatalf("no token in %q", bootstrapURL)
	}
	return tok
}
