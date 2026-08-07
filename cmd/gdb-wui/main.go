// Command gdb-wui serves a debugger UI for a local project.
//
//	gdb-wui --project /path/to/repo
//
// It binds loopback, prints a single-use URL, and opens a browser at it.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/retrocpugeek/gdb-wui/internal/assets"
	"github.com/retrocpugeek/gdb-wui/internal/debugger"
	"github.com/retrocpugeek/gdb-wui/internal/ghidra"
	"github.com/retrocpugeek/gdb-wui/internal/httpapi"
	"github.com/retrocpugeek/gdb-wui/internal/hub"
	"github.com/retrocpugeek/gdb-wui/internal/mi"
	"github.com/retrocpugeek/gdb-wui/internal/runfile"
	"github.com/retrocpugeek/gdb-wui/internal/srcfs"
	"github.com/retrocpugeek/gdb-wui/internal/wire"
)

// version is overridden at build time with -ldflags "-X main.version=...".
var version = "dev"

type options struct {
	project        string
	addr           string
	assetsDir      string
	open           bool
	listenAnywhere bool
	verbose        bool
	showVersion    bool

	gdbPath  string
	exe      string
	noGDB    bool
	miLog    bool
	printURL bool
	idleExit time.Duration

	// Decompilation. Optional throughout: Ghidra is a large dependency and
	// most sessions never want one.
	ghidraDir     string
	ghidraProject string
	ghidraProgram string
	decompDir     string
}

func main() {
	log.SetFlags(0)
	log.SetPrefix("gdb-wui: ")

	var opt options
	flag.StringVar(&opt.project, "project", ".", "project directory to browse")
	flag.StringVar(&opt.addr, "addr", "127.0.0.1:0", "listen address; must be loopback unless -listen-anywhere")
	flag.StringVar(&opt.assetsDir, "assets-dir", "", "serve the frontend from this directory instead of the binary (dev loop)")
	flag.BoolVar(&opt.open, "open", true, "open a browser at the URL")
	flag.BoolVar(&opt.listenAnywhere, "listen-anywhere", false, "allow binding a non-loopback address (dangerous)")
	flag.BoolVar(&opt.verbose, "v", false, "verbose logging")
	flag.BoolVar(&opt.showVersion, "version", false, "print the version and exit")
	flag.StringVar(&opt.gdbPath, "gdb", "gdb", "gdb executable")
	flag.StringVar(&opt.exe, "exe", "", "program to load at startup, relative to -project")
	flag.BoolVar(&opt.noGDB, "no-gdb", false, "browse the project without starting a debugger")
	flag.BoolVar(&opt.miLog, "mi-log", false, "stream raw MI traffic to the browser's log pane")
	flag.BoolVar(&opt.printURL, "print-url", false, "print a fresh login URL for an already-running gdb-wui and exit")
	flag.StringVar(&opt.ghidraDir, "ghidra", "", "Ghidra installation directory for decompilation (default $"+ghidra.EnvInstall+", then the usual locations)")
	flag.StringVar(&opt.ghidraProject, "ghidra-project", "", "existing Ghidra project (.gpr) to read, opened read-only")
	flag.StringVar(&opt.ghidraProgram, "ghidra-program", "", "which program inside -ghidra-project to decompile")
	flag.StringVar(&opt.decompDir, "decomp-dir", "", "where to cache Ghidra projects gdb-wui creates (default <project>/.gdb-wui/ghidra)")
	flag.DurationVar(&opt.idleExit, "idle-exit", 0,
		"exit after this long with no browser connected (0 disables)")
	flag.Usage = usage
	flag.Parse()

	if opt.showVersion {
		fmt.Println("gdb-wui", version)
		return
	}
	if opt.printURL {
		if err := printURL(opt.addr); err != nil {
			log.Fatal(err)
		}
		return
	}
	if err := run(opt); err != nil {
		log.Fatal(err)
	}
}

func usage() {
	fmt.Fprintf(os.Stderr, `gdb-wui %s — a web UI for GDB

usage: gdb-wui [flags]

WARNING: gdb-wui runs arbitrary programs with your full privileges. That is
what a debugger does. Never expose it to a network you do not control.

flags:
`, version)
	flag.PrintDefaults()
}

func run(opt options) error {
	files, err := srcfs.Open(opt.project)
	if err != nil {
		return err
	}
	defer files.Close()

	assetTree, err := loadAssets(opt.assetsDir)
	if err != nil {
		return err
	}

	if err := checkAddr(opt.addr, opt.listenAnywhere); err != nil {
		return err
	}
	listener, err := net.Listen("tcp", opt.addr)
	if err != nil {
		return fmt.Errorf("listening on %s: %w", opt.addr, err)
	}
	defer listener.Close()

	logf := func(string, ...any) {}
	if opt.verbose {
		logf = log.Printf
	}

	_, origins := loopbackOrigins(listener.Addr())
	hubCfg := hub.Config{
		AllowedOrigins: origins,
		Logf:           logf,
		ProjectRoot:    files.Abs(),
		Version:        version,
	}
	h := hub.New(hubCfg)

	if !opt.noGDB {
		session, err := startDebugger(opt, files, h, logf)
		if err != nil {
			return err
		}
		defer func() {
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := session.Close(shutdownCtx); err != nil {
				log.Printf("closing gdb: %v", err)
			}
		}()
		h.SetSession(session)
	}

	api, err := httpapi.New(httpapi.Config{
		Files:          files,
		Assets:         assetTree,
		WebSocket:      h.Handler(),
		Addr:           listener.Addr(),
		ListenAnywhere: opt.listenAnywhere,
		Logf:           logf,
	})
	if err != nil {
		return err
	}

	// Record where this server can be reached so `gdb-wui -print-url` can ask
	// it for a fresh link. The file is 0600 and holds the mint secret.
	runPath, err := runfile.Write(runfile.Entry{
		PID:        os.Getpid(),
		Addr:       listener.Addr().String(),
		Project:    files.Abs(),
		MintSecret: api.MintSecret(),
		Started:    time.Now(),
	})
	if err != nil {
		// Not fatal: the server works fine, -print-url just will not find it.
		log.Printf("could not record the run file: %v", err)
	} else {
		defer func() { _ = runfile.Remove(runPath) }()
	}

	srv := httpapi.NewHTTPServer(api)
	url := api.BootstrapURL()

	// Printed to stdout, always, and treated as the primary path. The browser
	// launch is a convenience that can fail — over SSH, in a container, with no
	// desktop session — and the tool must remain usable when it does.
	fmt.Println(url)
	log.Printf("serving %s on %s", files.Abs(), listener.Addr())
	log.Printf("the link above is single-use and expires in 60s; "+
		"run `gdb-wui -print-url%s` for another", addrHint(listener.Addr().String()))
	if assetTree.Dev() {
		log.Printf("assets from %s (no caching)", opt.assetsDir)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	serveErr := make(chan error, 1)
	go func() {
		err := srv.Serve(listener)
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		serveErr <- err
	}()

	if opt.open {
		go openBrowser(url, logf)
	}
	if opt.idleExit > 0 {
		go watchIdle(ctx, h, opt.idleExit, stop, logf)
	}

	select {
	case err := <-serveErr:
		return err
	case <-ctx.Done():
		log.Println("shutting down")
	}

	h.Shutdown()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}
	return <-serveErr
}

// startDebugger brings up gdb and the session that drives it.
func startDebugger(opt options, files *srcfs.FS, h *hub.Hub, logf func(string, ...any)) (*debugger.Session, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	session, err := debugger.New(ctx, debugger.Config{
		MI: mi.Options{
			Path: opt.gdbPath,
			Dir:  files.Abs(),
			Logf: logf,
		},
		Files:      files,
		Events:     h,
		Logf:       logf,
		Version:    version,
		GDBVersion: gdbVersion(opt.gdbPath),
		MILog:      opt.miLog,
		Decomp:     decompConfig(opt, files.Abs(), logf),
	})
	if err != nil {
		return nil, err
	}
	log.Printf("gdb ready")

	if opt.exe != "" {
		payload, err := json.Marshal(wire.ExeLoadRequest{Path: opt.exe})
		if err != nil {
			return nil, err
		}
		if _, werr := session.Handle(ctx, wire.Request{
			Type:    wire.TypeExeLoad,
			Payload: payload,
		}); werr != nil {
			// Not fatal: the UI can load a different program. Refusing to start
			// over a bad -exe would be a worse experience than saying so.
			log.Printf("loading %s: %s", opt.exe, werr.Message)
		} else {
			log.Printf("loaded %s", opt.exe)
		}
	}
	return session, nil
}

// watchIdle shuts the server down once nobody has been connected for a while.
//
// A debugger left running holds a gdb process and an inferior, which can be
// holding a lock, a port or a device. The grace period starts only after the
// first client has connected and gone: exiting because nobody arrived in the
// first thirty seconds would kill a server whose browser was slow to start.
func watchIdle(ctx context.Context, h *hub.Hub, after time.Duration,
	stop func(), logf func(string, ...any)) {

	const tick = 5 * time.Second
	var everConnected bool
	var idleSince time.Time

	ticker := time.NewTicker(tick)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		if h.Count() > 0 {
			everConnected = true
			idleSince = time.Time{}
			continue
		}
		if !everConnected {
			continue
		}
		if idleSince.IsZero() {
			idleSince = time.Now()
			continue
		}
		if time.Since(idleSince) >= after {
			log.Printf("no browser connected for %s; exiting", after)
			logf("idle exit after %s", after)
			stop()
			return
		}
	}
}

// printURL asks a running server for a fresh login link.
//
// It exists because the bootstrap token is deliberately single-use with a
// 60-second TTL — it ends up in argv and browser history, so a long-lived one
// would be a standing credential in `ps` output. Making a new one cheap is the
// right answer to "the link expired"; making the old one last longer is not.
func printURL(addr string) error {
	entry, err := runfile.Find(addrForLookup(addr))
	if err != nil {
		if errors.Is(err, runfile.ErrNoServer) {
			return fmt.Errorf("%w — start one with `gdb-wui -project <dir>`", err)
		}
		return err
	}

	target := "http://" + entry.Addr + httpapi.MintPath
	req, err := http.NewRequest(http.MethodPost, target, nil)
	if err != nil {
		return err
	}
	req.Header.Set(httpapi.MintHeader, entry.MintSecret)
	// The server requires an Origin on non-GET requests, and it must be one of
	// its own; a browser could not forge this, and neither can a hostile page.
	req.Header.Set("Origin", "http://"+entry.Addr)
	req.Header.Set("Sec-Fetch-Site", "same-origin")

	client := &http.Client{Timeout: 5 * time.Second}
	res, err := client.Do(req)
	if err != nil {
		// A live pid whose port refuses connections means the run file is
		// stale in a way the liveness check cannot see.
		_ = runfile.Remove(entry.Path())
		return fmt.Errorf("could not reach the server at %s: %w", entry.Addr, err)
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(res.Body, 4096))
		return fmt.Errorf("server refused to issue a link (%s): %s",
			res.Status, strings.TrimSpace(string(body)))
	}
	var out struct {
		URL string `json:"url"`
	}
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		return fmt.Errorf("decoding the reply: %w", err)
	}
	if out.URL == "" {
		return errors.New("server returned an empty URL")
	}

	fmt.Println(out.URL)
	log.Printf("serving %s; this link is single-use and expires in 60s", entry.Project)
	return nil
}

// addrForLookup treats the default -addr as "unspecified", so -print-url with
// no arguments finds the only running server rather than looking for one bound
// to port 0.
func addrForLookup(addr string) string {
	if addr == "" || addr == "127.0.0.1:0" {
		return ""
	}
	return addr
}

// addrHint suggests the -addr argument only when it would be needed.
func addrHint(addr string) string {
	entries, err := runfile.List()
	if err != nil || len(entries) <= 1 {
		return ""
	}
	return " -addr " + addr
}

// gdbVersion reads the banner. It is display-only, so a failure is not an
// error — MI has no synchronous way to ask.
func gdbVersion(path string) string {
	out, err := exec.Command(path, "--version").Output()
	if err != nil {
		return ""
	}
	line, _, _ := strings.Cut(string(out), "\n")
	return strings.TrimSpace(line)
}

func loadAssets(dir string) (*assets.Assets, error) {
	if dir == "" {
		return assets.Embedded()
	}
	return assets.Dir(dir)
}

// checkAddr refuses a non-loopback bind unless it was asked for explicitly, and
// says so loudly when it was.
func checkAddr(addr string, listenAnywhere bool) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("bad -addr %q: %w", addr, err)
	}
	if host == "" && !listenAnywhere {
		return fmt.Errorf("-addr %q binds every interface; use 127.0.0.1:PORT, "+
			"or -listen-anywhere if you really mean it", addr)
	}
	ip := net.ParseIP(host)
	loopback := host == "localhost" || (ip != nil && ip.IsLoopback())
	if loopback {
		return nil
	}
	if !listenAnywhere {
		return fmt.Errorf("-addr %q is not a loopback address; gdb-wui runs arbitrary "+
			"programs with your privileges, so it refuses to listen off-host without "+
			"-listen-anywhere", addr)
	}
	log.Printf("WARNING: listening on %s, which is reachable from other machines.", addr)
	log.Printf("WARNING: anyone who can reach this port can run programs as you.")
	return nil
}

func loopbackOrigins(addr net.Addr) (hosts, origins []string) {
	_, port, err := net.SplitHostPort(addr.String())
	if err != nil {
		return nil, nil
	}
	for _, h := range []string{"127.0.0.1", "localhost", "[::1]"} {
		hosts = append(hosts, h+":"+port)
		origins = append(origins, "http://"+h+":"+port)
	}
	return hosts, origins
}

// openBrowser launches a browser with a fixed argv.
//
// exec.Command with separate arguments, never a shell: the URL contains a
// token, and handing it to a shell would put it through word splitting and
// expansion for no benefit whatsoever.
func openBrowser(url string, logf func(string, ...any)) {
	candidates := []string{"xdg-open", "gio", "x-www-browser", "sensible-browser"}
	for _, name := range candidates {
		path, err := exec.LookPath(name)
		if err != nil {
			continue
		}
		args := []string{url}
		if name == "gio" {
			args = []string{"open", url}
		}
		cmd := exec.Command(path, args...)
		// Detached: the browser must outlive nothing here, and its stdio must
		// not scribble over ours.
		cmd.Stdout, cmd.Stderr = nil, nil
		if err := cmd.Start(); err != nil {
			logf("launching %s: %v", name, err)
			continue
		}
		go func() { _ = cmd.Wait() }()
		return
	}
	logf("no browser launcher found (tried %s); open the URL above yourself",
		strings.Join(candidates, ", "))
}

// decompConfig resolves the decompiler options, or returns a zero value.
//
// Every failure here is soft. Decompilation is an extra view: a missing or
// misconfigured Ghidra must leave a working debugger and an explanation in the
// log, not a refusal to start. The UI learns the feature is unavailable from
// decomp.status and says how to enable it.
func decompConfig(opt options, projectAbs string, logf func(string, ...any)) debugger.DecompConfig {
	// Only go looking when asked. Discovering a Ghidra nobody mentioned and
	// silently offering to spawn a JVM from it is too much initiative.
	if opt.ghidraDir == "" && opt.ghidraProject == "" && os.Getenv(ghidra.EnvInstall) == "" {
		return debugger.DecompConfig{}
	}
	install, err := ghidra.Locate(opt.ghidraDir)
	if err != nil {
		logf("decompilation unavailable: %v", err)
		return debugger.DecompConfig{}
	}
	cfg := debugger.DecompConfig{Install: install, CacheRoot: opt.decompDir}
	if cfg.CacheRoot == "" {
		// Beside the project by default, so the analysis travels with it and
		// is visible rather than hidden in a cache directory nobody finds.
		// -decomp-dir moves it, which is what a read-only or network-mounted
		// project needs.
		cfg.CacheRoot = filepath.Join(projectAbs, ".gdb-wui", "ghidra")
	}

	if opt.ghidraProject != "" {
		// A .gpr is addressed by the directory holding it plus its bare name,
		// which is not how anyone thinks of a project, so accept the path to
		// the .gpr itself and split it.
		path := strings.TrimSuffix(opt.ghidraProject, ".gpr")
		cfg.ProjectDir = filepath.Dir(path)
		cfg.ProjectName = filepath.Base(path)
		cfg.Program = opt.ghidraProgram
		if cfg.Program == "" {
			// Not a default worth guessing. A project holds several programs
			// and, in Ghidra's Debugger workflow, a pile of traces; picking
			// one would be picking wrong.
			logf("decompilation unavailable: -ghidra-project needs -ghidra-program " +
				"naming one program inside it")
			return debugger.DecompConfig{}
		}
	}
	logf("decompilation: ghidra %s at %s", install.Version, install.Dir)
	return cfg
}
