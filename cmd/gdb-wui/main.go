// Command gdb-wui serves a debugger UI for a local project.
//
//	gdb-wui --project /path/to/repo
//
// It binds loopback, prints a single-use URL, and opens a browser at it.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/retrocpugeek/gdb-wui/internal/assets"
	"github.com/retrocpugeek/gdb-wui/internal/httpapi"
	"github.com/retrocpugeek/gdb-wui/internal/hub"
	"github.com/retrocpugeek/gdb-wui/internal/srcfs"
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
	flag.Usage = usage
	flag.Parse()

	if opt.showVersion {
		fmt.Println("gdb-wui", version)
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
	h := hub.New(hub.Config{
		AllowedOrigins: origins,
		Logf:           logf,
		ProjectRoot:    files.Abs(),
		Version:        version,
	})

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

	srv := httpapi.NewHTTPServer(api)
	url := api.BootstrapURL()

	// Printed to stdout, always, and treated as the primary path. The browser
	// launch is a convenience that can fail — over SSH, in a container, with no
	// desktop session — and the tool must remain usable when it does.
	fmt.Println(url)
	log.Printf("serving %s on %s", files.Abs(), listener.Addr())
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
