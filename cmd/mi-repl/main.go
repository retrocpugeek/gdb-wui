// Command mi-repl is a development tool for looking at GDB/MI.
//
// Two modes:
//
//	mi-repl < captured.mi              parse MI text, print canonical JSON
//	mi-repl -gdb ./prog                drive a real gdb, commands on stdin
//
// The first is the parser under a microscope: pipe anything gdb ever printed
// through it and read back exactly what the rest of the program will see. The
// second is the M1 demo — it exercises the process supervisor, the handshake,
// command correlation and the event stream against a real debugger.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/retrocpugeek/gdb-wui/internal/mi"
)

func main() {
	var (
		useGDB  = flag.Bool("gdb", false, "spawn gdb and treat stdin lines as MI commands")
		gdbPath = flag.String("gdb-path", "gdb", "gdb executable")
		pretty  = flag.Bool("pretty", false, "indent the JSON")
		raw     = flag.Bool("raw", false, "also echo the raw MI line")
	)
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: mi-repl [flags] [program]\n\n")
		fmt.Fprintf(os.Stderr, "  mi-repl < captured.mi     parse MI text into canonical JSON\n")
		fmt.Fprintf(os.Stderr, "  mi-repl -gdb ./prog       drive a real gdb, MI commands on stdin\n\n")
		flag.PrintDefaults()
	}
	flag.Parse()

	if err := run(*useGDB, *gdbPath, *pretty, *raw, flag.Args()); err != nil {
		fmt.Fprintln(os.Stderr, "mi-repl:", err)
		os.Exit(1)
	}
}

func run(useGDB bool, gdbPath string, pretty, raw bool, args []string) error {
	w := newWriter(os.Stdout, pretty, raw)
	defer w.flush()

	if !useGDB {
		return filter(w)
	}
	return drive(w, gdbPath, args)
}

// filter is the parse-only mode: one line in, one JSON object out.
func filter(w *writer) error {
	br := bufio.NewReaderSize(os.Stdin, 64*1024)
	for {
		line, err := br.ReadString('\n')
		if line != "" {
			w.emit("", mi.ParseRecord(line))
		}
		if err != nil {
			return nil //nolint:nilerr // EOF and read errors both mean "done"
		}
	}
}

// drive spawns gdb and forwards stdin lines to it as commands.
func drive(w *writer, gdbPath string, args []string) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	client, err := mi.Start(ctx, mi.Options{
		Path:    gdbPath,
		Args:    args,
		Handler: func(r mi.Record) { w.emit("event", r) },
		Logf:    func(f string, a ...any) { fmt.Fprintf(os.Stderr, "mi-repl: "+f+"\n", a...) },
	})
	if err != nil {
		return err
	}
	defer func() {
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := client.Close(shutdown); err != nil {
			fmt.Fprintln(os.Stderr, "mi-repl: close:", err)
		}
	}()

	fmt.Fprintf(os.Stderr, "mi-repl: gdb ready, %d features\n", len(client.Features()))

	lines := make(chan string)
	go func() {
		defer close(lines)
		sc := bufio.NewScanner(os.Stdin)
		sc.Buffer(make([]byte, 0, 64*1024), 4<<20)
		for sc.Scan() {
			lines <- sc.Text()
		}
	}()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-client.Dead():
			for _, l := range client.StderrTail() {
				fmt.Fprintln(os.Stderr, "gdb stderr:", l)
			}
			return client.DeadErr()
		case line, ok := <-lines:
			if !ok {
				return nil
			}
			cmd := strings.TrimSpace(line)
			if cmd == "" || strings.HasPrefix(cmd, "#") {
				continue
			}
			// A bare word is a console command; MI commands start with '-'.
			if !strings.HasPrefix(cmd, "-") {
				cmd = `-interpreter-exec console "` + strings.ReplaceAll(cmd, `"`, `\"`) + `"`
			}
			// -exec-interrupt must not queue behind whatever is running.
			send := client.Send
			if strings.HasPrefix(cmd, "-exec-interrupt") || strings.HasPrefix(cmd, "-gdb-exit") {
				send = client.SendUnlocked
			}
			rec, err := send(ctx, cmd)
			if err != nil {
				if _, isGDB := mi.AsError(err); !isGDB {
					return err
				}
			}
			w.emit("reply", rec)
		}
	}
}

type writer struct {
	bw     *bufio.Writer
	enc    *json.Encoder
	raw    bool
	pretty bool
}

func newWriter(f *os.File, pretty, raw bool) *writer {
	bw := bufio.NewWriter(f)
	enc := json.NewEncoder(bw)
	enc.SetEscapeHTML(false)
	if pretty {
		enc.SetIndent("", "  ")
	}
	return &writer{bw: bw, enc: enc, raw: raw, pretty: pretty}
}

func (w *writer) emit(origin string, r mi.Record) {
	if w.raw && r.Raw != "" {
		fmt.Fprintf(w.bw, "// %s\n", r.Raw)
	}
	b, err := json.Marshal(r)
	if err != nil {
		fmt.Fprintln(os.Stderr, "mi-repl: encode:", err)
		return
	}
	// Splice rather than embed: embedding mi.Record in a wrapper struct
	// promotes its MarshalJSON to the wrapper, which would silently drop
	// "origin" and emit the bare record.
	if origin != "" {
		key, _ := json.Marshal(origin)
		prefix := `{"origin":` + string(key)
		if string(b) == "{}" {
			b = []byte(prefix + "}")
		} else {
			b = append([]byte(prefix+","), b[1:]...)
		}
	}
	if err := w.enc.Encode(json.RawMessage(b)); err != nil {
		fmt.Fprintln(os.Stderr, "mi-repl: encode:", err)
	}
	w.bw.Flush()
}

func (w *writer) flush() { _ = w.bw.Flush() }
