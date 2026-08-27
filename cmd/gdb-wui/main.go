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
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/retrocpugeek/gdb-wui/internal/assets"
	"github.com/retrocpugeek/gdb-wui/internal/config"
	"github.com/retrocpugeek/gdb-wui/internal/debugger"
	"github.com/retrocpugeek/gdb-wui/internal/ghidra"
	"github.com/retrocpugeek/gdb-wui/internal/httpapi"
	"github.com/retrocpugeek/gdb-wui/internal/hub"
	"github.com/retrocpugeek/gdb-wui/internal/mcp"
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

	gdbPath string
	exe     string
	// gdbCommands run at startup, after -exe. The way in for a target that
	// has to be reached by hand: an emulator's stub needs an architecture set
	// and an address connected to before there is anything to debug.
	gdbCommands gdbCommands
	noGDB       bool
	miLog       bool
	printURL    bool
	idleExit    time.Duration

	// The MCP bridge, which joins a running server rather than starting one.
	// Three flags rather than one because reading a binary, writing into the
	// decompiler and running the program are three different things to agree
	// to, and bundling them would make the cautious answer to any of them no
	// to all three.
	mcp         bool
	mcpAnnotate bool
	mcpRun      bool

	// Where the settings came from. Not settable in a config file: a file that
	// chose which file to read would be its own puzzle.
	configPath string
	noConfig   bool
	saveConfig savePath

	// Decompilation. Optional throughout: Ghidra is a large dependency and
	// most sessions never want one.
	ghidraDir      string
	ghidraProject  string
	ghidraProgram  string
	ghidraAnalysis ghidra.Analysis
	ghidraSymbols  string
	// The binary Ghidra reverses, when it is not the one gdb loaded, and the
	// two facts a file with no format cannot supply about itself.
	ghidraBinary    string
	ghidraProcessor string
	ghidraBase      string
	decompDir       string
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
	flag.Var(&opt.gdbCommands, "gdb-command",
		"a gdb `command` to run at startup, after -exe; repeat for each, and they "+
			"run in the order given. gdb's -ex by another name, and how a target "+
			"that has to be reached by hand is reached: -gdb-command 'set "+
			"architecture arm' -gdb-command 'target remote 127.0.0.1:9999'")
	flag.BoolVar(&opt.noGDB, "no-gdb", false, "browse the project without starting a debugger")
	flag.BoolVar(&opt.miLog, "mi-log", false, "stream raw MI traffic to the browser's log pane")
	flag.BoolVar(&opt.printURL, "print-url", false, "print a fresh login URL for an already-running gdb-wui and exit")
	flag.BoolVar(&opt.mcp, "mcp", false,
		"serve MCP on stdio for an already-running gdb-wui, so an agent can read the program")
	flag.BoolVar(&opt.mcpAnnotate, "mcp-annotate", false,
		"with -mcp: also let the agent write names, types and comments into the decompiler")
	flag.BoolVar(&opt.mcpRun, "mcp-run", false,
		"with -mcp: also let the agent set breakpoints and run the program")
	flag.StringVar(&opt.ghidraDir, "ghidra", "", "Ghidra installation directory for decompilation (default $"+ghidra.EnvInstall+", then the usual locations)")
	flag.StringVar(&opt.ghidraProject, "ghidra-project", "", "existing Ghidra project (.gpr) to read, opened read-only")
	flag.StringVar(&opt.ghidraProgram, "ghidra-program", "", "which program inside -ghidra-project to decompile")
	flag.Var(&opt.ghidraAnalysis, "ghidra-analysis",
		"how much of the binary Ghidra analyses at import: `mode` is auto (the default), "+
			"full, lean or none. Past "+strconv.Itoa(ghidra.AutoAnalysisLimit>>20)+
			" MB of code the analysis cannot finish, so auto takes none for an image "+
			"whose symbols say where the functions are, and lean — the analyzers that "+
			"find functions, without the ones that cost the memory — for a stripped one")
	flag.StringVar(&opt.ghidraSymbols, "ghidra-symbols", "",
		"a `file` of 'addr [type] name' lines naming functions the binary does not name "+
			"itself, as nm and /proc/kallsyms write them")
	flag.StringVar(&opt.ghidraBinary, "ghidra-binary", "",
		"a `file` for Ghidra to reverse instead of the program gdb loaded, for a target "+
			"gdb will not take a file for — an emulator running a raw kernel image")
	flag.StringVar(&opt.ghidraProcessor, "ghidra-processor", "",
		"Ghidra language `id`, as ARM:LE:32:v7, saying what the bytes are. Required with "+
			"-ghidra-base, since a raw image says nothing about itself")
	flag.StringVar(&opt.ghidraBase, "ghidra-base", "",
		"the `address` a raw -ghidra-binary is loaded at. It imports through Ghidra's "+
			"binary loader, and it is the whole of the mapping between the debugger's "+
			"addresses and the decompiler's")
	flag.StringVar(&opt.decompDir, "decomp-dir", "", "where to cache Ghidra projects gdb-wui creates (default <project>/gdb-wui-decomp)")
	flag.DurationVar(&opt.idleExit, "idle-exit", 0,
		"exit after this long with no browser connected (0 disables)")
	flag.StringVar(&opt.configPath, "config", "",
		"read settings from this file instead of searching for one")
	flag.BoolVar(&opt.noConfig, "no-config", false,
		"ignore any config file")
	flag.Var(&opt.saveConfig, config.SaveFlag,
		"write the current settings to a config file and exit; "+
			"-save-config=PATH chooses where (default ./"+config.FileName+")")
	flag.Usage = usage
	flag.Parse()

	// After Parse, so that a flag given on the command line wins: config.Load
	// asks the flag set which flags were actually set and leaves those alone.
	//
	// The file that was used is logged unconditionally rather than under -v.
	// The support cost of config files is not knowing which one is in effect,
	// and one line at startup answers it — including when the file chose a
	// different gdb than the reader expects.
	used, err := config.Load(flag.CommandLine, opt.configPath, opt.noConfig)
	if err != nil {
		log.Fatal(err)
	}
	if used != "" {
		log.Printf("config: %s", used)
	}

	// After Load, so that what is written is the effective configuration
	// rather than only what was typed. See config.Save.
	if opt.saveConfig.set {
		written, backup, err := config.Save(flag.CommandLine, opt.saveConfig.path)
		if err != nil {
			log.Fatal(err)
		}
		if backup != "" {
			log.Printf("kept the previous file as %s", backup)
		}
		log.Printf("wrote %s", written)
		return
	}

	if opt.showVersion {
		fmt.Println("gdb-wui", version)
		return
	}
	if opt.mcp {
		// stderr, because stdout is the protocol: one JSON object per line,
		// and a stray log line in the middle of it is a client that
		// disconnects with a parse error nobody can place.
		logf := func(format string, args ...any) { log.Printf(format, args...) }
		if !opt.verbose {
			logf = func(string, ...any) {}
		}
		if err := mcp.Run(context.Background(), mcp.Options{
			Addr:     addrForLookup(opt.addr),
			Annotate: opt.mcpAnnotate,
			Run:      opt.mcpRun,
			Version:  version,
			Logf:     logf,
		}); err != nil {
			log.Fatal(err)
		}
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

// savePath backs -save-config, which takes an optional value: bare it writes
// the default file, and -save-config=PATH writes that one.
//
// The flag package spells "optional value" as a Value that reports
// IsBoolFlag, which makes `-save-config` legal without an argument and stops
// it from swallowing the next one. A plain string flag would read
// `-save-config -project .` as saving to a file named "-project".
type savePath struct {
	set  bool
	path string
}

func (s *savePath) String() string {
	if s == nil || !s.set {
		return ""
	}
	return s.path
}

func (s *savePath) Set(v string) error {
	s.set = true
	if v != config.DefaultSave {
		s.path = v
	}
	return nil
}

func (s *savePath) IsBoolFlag() bool { return true }

// gdbCommands is a repeatable flag: each -gdb-command adds one line, and they
// run in the order given.
//
// One command per occurrence rather than one string of many, because gdb's own
// vocabulary has no separator that is not also legal inside a command —
// semicolons appear in expressions, and a newline inside a shell argument is
// awkward to write. Repeating the flag is how gdb spells the same thing with
// -ex, and the order on the command line is the order they run in.
type gdbCommands []string

func (c *gdbCommands) String() string {
	if c == nil {
		return ""
	}
	// Only ever read back by -help and by the "is this still the default"
	// check in config.Save; the config file round-trips through Get, which
	// keeps the list a list.
	return strings.Join(*c, " ; ")
}

func (c *gdbCommands) Set(v string) error {
	line := strings.TrimSpace(v)
	if line == "" {
		return errors.New("empty command")
	}
	if strings.ContainsAny(line, "\r\n") {
		return errors.New("one command per -gdb-command; repeat the flag for more")
	}
	*c = append(*c, line)
	return nil
}

// Get makes this a flag.Getter, which is how config.Save learns it is a list
// and writes a JSON array rather than failing on the type.
func (c *gdbCommands) Get() any { return []string(*c) }

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

	// After -exe, matching `gdb prog -ex ...`: loading the program is what
	// tells gdb the architecture, and a command that assumes it has to come
	// second. Through console.exec rather than straight down the wire, because
	// these are the same commands somebody would type — `target remote` has to
	// register as a connection, and everything has to be followed by the same
	// resync a typed command gets.
	for _, line := range opt.gdbCommands {
		log.Printf("gdb command: %s", line)
		payload, err := json.Marshal(wire.ConsoleExecRequest{Line: line})
		if err != nil {
			return nil, err
		}
		// Its own deadline per command, not the startup context's: connecting
		// to a stub that is not listening yet, or a command that runs the
		// program, is not the same budget as bringing gdb up.
		cctx, ccancel := context.WithTimeout(context.Background(), gdbCommandTimeout)
		_, werr := session.Handle(cctx, wire.Request{
			Type:    wire.TypeConsoleExec,
			Payload: payload,
		})
		ccancel()
		if werr != nil {
			// Not fatal, and the sequence continues: the commands are the
			// user's, a later one may well fix what an earlier one got wrong,
			// and refusing to start would leave them no console to fix it at.
			log.Printf("gdb command %q: %s", line, werr.Message)
		}
	}
	if len(opt.gdbCommands) > 0 {
		// gdb's own errors go to the Console tab, where console output belongs,
		// and are easy to miss from a terminal. This line is what says whether
		// the sequence arrived anywhere: a `target remote` that was refused
		// leaves no program and no connection, and says so here.
		snap := session.Snapshot()
		// A stop arrives asynchronously: `target remote` returns once gdb has
		// answered it, and the *stopped saying the machine halted follows. Read
		// immediately, the line says "noProgram" about a target sitting at its
		// reset vector — measured against an emulator's stub, where the truth a
		// moment later was "stopped". Only worth waiting for when something
		// that should produce a stop actually happened, so a sequence that
		// merely configured gdb pays nothing.
		if r := snap.Remote; r != nil && r.Connected {
			deadline := time.Now().Add(2 * time.Second)
			for snap.RunState == wire.RunStateNoProgram && time.Now().Before(deadline) {
				time.Sleep(50 * time.Millisecond)
				snap = session.Snapshot()
			}
		}
		if r := snap.Remote; r != nil && r.Connected && r.Address != "" {
			log.Printf("after %d gdb command(s): %s, connected to %s",
				len(opt.gdbCommands), snap.RunState, r.Address)
		} else {
			log.Printf("after %d gdb command(s): %s",
				len(opt.gdbCommands), snap.RunState)
		}
	}
	return session, nil
}

// gdbCommandTimeout bounds one startup command. Generous because the
// commonest one is `target remote`, which waits on something outside this
// process, and a stub started in another terminal a moment ago is the normal
// case rather than an error.
const gdbCommandTimeout = 60 * time.Second

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
// It exists because the bootstrap token is single-use with a
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
	// Every ghidra flag counts as asking, including the two that only modify
	// an import. Leaving them out would mean -ghidra-base on its own returned
	// here, and the complaint that it needs a binary would never be printed.
	if opt.ghidraDir == "" && opt.ghidraProject == "" && opt.ghidraBinary == "" &&
		opt.ghidraProcessor == "" && opt.ghidraBase == "" &&
		os.Getenv(ghidra.EnvInstall) == "" {
		return debugger.DecompConfig{}
	}
	install, err := ghidra.Locate(opt.ghidraDir)
	if err != nil {
		logf("decompilation unavailable: %v", err)
		return debugger.DecompConfig{}
	}
	cfg := debugger.DecompConfig{
		Install:   install,
		CacheRoot: opt.decompDir,
		Analysis:  opt.ghidraAnalysis,
		Symbols:   opt.ghidraSymbols,
		Processor: opt.ghidraProcessor,
	}
	if opt.ghidraBinary != "" {
		// Absolute, because nothing downstream resolves it: it is deliberately
		// not a path inside -project, the point being to name a file the
		// debugger has no relationship with.
		abs, err := filepath.Abs(opt.ghidraBinary)
		if err == nil {
			_, err = os.Stat(abs)
		}
		if err != nil {
			// Fatal to the feature rather than dropped like -ghidra-symbols.
			// A named binary is the whole subject; carrying on would reverse
			// gdb's program instead, or nothing, without saying so.
			logf("decompilation unavailable: -ghidra-binary: %v", err)
			return debugger.DecompConfig{}
		}
		cfg.Binary = abs
	}
	if opt.ghidraBase != "" {
		switch {
		case cfg.Binary == "":
			logf("decompilation unavailable: -ghidra-base needs -ghidra-binary, " +
				"which is the image it is the base of")
			return debugger.DecompConfig{}
		case cfg.Processor == "":
			logf("decompilation unavailable: -ghidra-base needs -ghidra-processor, " +
				"because a raw image says nothing about what its bytes are")
			return debugger.DecompConfig{}
		}
		n, err := strconv.ParseUint(opt.ghidraBase, 0, 64)
		if err != nil {
			logf("decompilation unavailable: -ghidra-base %q is not an address",
				opt.ghidraBase)
			return debugger.DecompConfig{}
		}
		// Normalised, because it names the cache directory and 0xC0108000 and
		// 0xc0108000 are the same program.
		cfg.Base = fmt.Sprintf("%#x", n)
	}
	if cfg.Symbols != "" {
		// Named but unreadable is worth saying now. Otherwise the first sign
		// is a Ghidra script logging a count of zero, one JVM start later.
		if _, err := os.Stat(cfg.Symbols); err != nil {
			logf("decompilation: -ghidra-symbols: %v", err)
			cfg.Symbols = ""
		}
	}
	if cfg.CacheRoot != "" {
		// An explicit path that Ghidra will refuse is worth saying so about
		// now, rather than one JVM start later in a message that names
		// neither the path nor the reason.
		if err := ghidra.CheckProjectPath(cfg.CacheRoot); err != nil {
			logf("decompilation unavailable: %v", err)
			return debugger.DecompConfig{}
		}
	}
	if cfg.CacheRoot == "" {
		// Beside the project by default, so the analysis travels with it.
		// -decomp-dir moves it, which is what a read-only or network-mounted
		// project needs.
		//
		// Not a dotted name, and not for want of trying: Ghidra refuses any
		// path element beginning with a dot, which rules out both a hidden
		// directory here and every conventional cache location, $XDG_CACHE_HOME
		// being ~/.cache. Visible it is.
		cfg.CacheRoot = filepath.Join(projectAbs, "gdb-wui-decomp")
		if err := ghidra.CheckProjectPath(cfg.CacheRoot); err != nil {
			// The project itself lives under a dotted directory, so nothing
			// inside it can hold the cache. Fall back somewhere Ghidra will
			// accept and say so, because the analysis will then be redone
			// after a reboot.
			cfg.CacheRoot = filepath.Join(os.TempDir(), "gdb-wui-decomp")
			logf("decompilation: %v", err)
			logf("decompilation: caching in %s instead; pass -decomp-dir to choose",
				cfg.CacheRoot)
		}
	}

	if opt.ghidraProject != "" {
		if err := ghidra.CheckProjectPath(opt.ghidraProject); err != nil {
			logf("decompilation unavailable: %v", err)
			return debugger.DecompConfig{}
		}
	}

	if opt.ghidraProject != "" && cfg.Binary != "" {
		// One says open this project, the other says import this file. Both
		// cannot be honoured, and guessing which was meant would silently
		// throw away work the user asked for.
		logf("decompilation unavailable: -ghidra-project and -ghidra-binary " +
			"name two different programs; pass one")
		return debugger.DecompConfig{}
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
