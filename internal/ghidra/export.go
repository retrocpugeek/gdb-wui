package ghidra

// Serving a decompilation that ExportDecomp.java wrote, in place of a resident
// Ghidra.
//
// The export is the same JSON the server sends — DecompJson writes both — so
// the far end of the socket can be a goroutine reading a file instead of a
// JVM. The Client, and everything above it, cannot tell the difference: the
// greeting, the ops and the replies are the server's own. What an export
// cannot do is change, so the edits fail, and only what was decompiled when
// it was written can be shown.
//
// Why bother. Ghidra wants a desktop: 900 MB installed, a JVM, a gigabyte or
// two of heap, and seconds to minutes of analysis. The machine being debugged
// is often none of those — a single-board computer, a router, an emulator in
// a browser tab. Exported once on a workstation, the decompilation travels as
// a file and costs nothing to serve.
//
// The functions are not decoded up front. A large program's export is tens of
// megabytes of text, which would be most of a small machine's memory; the
// file is read once to note where each function's record lies, and a record
// is read back from disk when asked for.

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// Export is an opened ExportDecomp document.
type Export struct {
	Path string
	// Schema is the document's version (DecompJson.SCHEMA).
	Schema int
	// Program is what was decompiled; Program.SHA256 relates it to a binary.
	Program Program
	// Version is the Ghidra version that wrote it, when the document says.
	Version string

	// funcs is every function, in entry order; ranges is their bodies sorted
	// by start, for finding the function an address is in.
	funcs  []exportFunc
	ranges []exportRange
	data   []DataRef
	// records is where each decompiled function's record lies in the file,
	// by entry.
	records map[uint64]span

	mu sync.Mutex
	f  *os.File
}

type exportFunc struct {
	FunctionName
	entry uint64
}

type exportRange struct {
	start, end uint64 // inclusive
	fn         int    // index into funcs
}

type span struct{ off, len int64 }

// exportIndexEntry is one element of the document's "index".
type exportIndexEntry struct {
	Name      string      `json:"name"`
	Entry     string      `json:"entry"`
	Thunk     bool        `json:"thunk"`
	Signature string      `json:"signature"`
	Ranges    [][2]string `json:"ranges"`
}

// exportRecordHead is the part of a decompiled function's record that the
// index needs, decoded without keeping its text.
type exportRecordHead struct {
	Name      string `json:"name"`
	Entry     string `json:"entry"`
	BodyStart string `json:"bodyStart"`
	BodyEnd   string `json:"bodyEnd"`
	Signature string `json:"signature"`
}

// parseAddr reads a hex address as DecompJson writes it ("0x10be50").
func parseAddr(s string) (uint64, bool) {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(strings.TrimPrefix(s, "0x"), "0X")
	if s == "" {
		return 0, false
	}
	v, err := strconv.ParseUint(s, 16, 64)
	return v, err == nil
}

// OpenExport reads an export's header and indexes its functions.
func OpenExport(path string) (*Export, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	e := &Export{Path: path, f: f, records: map[uint64]span{}}
	if err := e.index(); err != nil {
		f.Close()
		return nil, fmt.Errorf("ghidra: export %s: %w", path, err)
	}
	return e, nil
}

// Close releases the file.
func (e *Export) Close() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.f == nil {
		return nil
	}
	err := e.f.Close()
	e.f = nil
	return err
}

func (e *Export) index() error {
	dec := json.NewDecoder(bufio.NewReaderSize(e.f, 1<<20))
	if err := expectDelim(dec, '{'); err != nil {
		return err
	}
	var index []exportIndexEntry
	haveIndex := false
	var heads []exportRecordHead
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return err
		}
		key, _ := tok.(string)
		switch key {
		case "schema":
			if err := dec.Decode(&e.Schema); err != nil {
				return err
			}
		case "generator":
			var g struct {
				Version string `json:"version"`
			}
			if err := dec.Decode(&g); err != nil {
				return err
			}
			e.Version = g.Version
		case "program":
			if err := dec.Decode(&e.Program); err != nil {
				return err
			}
		case "index":
			if err := dec.Decode(&index); err != nil {
				return err
			}
			haveIndex = true
		case "data":
			if err := dec.Decode(&e.data); err != nil {
				return err
			}
		case "functions":
			if err := expectDelim(dec, '['); err != nil {
				return err
			}
			for dec.More() {
				var raw json.RawMessage
				if err := dec.Decode(&raw); err != nil {
					return err
				}
				// The record ends where the decoder is now; RawMessage holds
				// exactly its bytes, so that is where it starts as well.
				end := dec.InputOffset()
				var head exportRecordHead
				if err := json.Unmarshal(raw, &head); err != nil {
					return err
				}
				entry, ok := parseAddr(head.Entry)
				if !ok {
					return fmt.Errorf("function %q has no entry address", head.Name)
				}
				e.records[entry] = span{off: end - int64(len(raw)), len: int64(len(raw))}
				heads = append(heads, head)
			}
			if err := expectDelim(dec, ']'); err != nil {
				return err
			}
		default:
			var skip json.RawMessage
			if err := dec.Decode(&skip); err != nil {
				return err
			}
		}
	}
	if e.Schema == 0 {
		return errors.New("not an ExportDecomp document (no schema)")
	}
	if e.Schema > Schema {
		return fmt.Errorf("schema %d is newer than this gdb-wui understands (%d)", e.Schema, Schema)
	}
	if !haveIndex {
		// Written before the index existed: what the decompiled records say
		// about themselves is all there is, so thunks and anything the
		// decompiler refused are missing from the lists and from naming.
		for _, h := range heads {
			index = append(index, exportIndexEntry{Name: h.Name, Entry: h.Entry,
				Signature: h.Signature, Ranges: [][2]string{{h.BodyStart, h.BodyEnd}}})
		}
	}
	for _, ie := range index {
		entry, ok := parseAddr(ie.Entry)
		if !ok {
			continue
		}
		e.funcs = append(e.funcs, exportFunc{
			FunctionName: FunctionName{Name: ie.Name, Entry: ie.Entry, Signature: ie.Signature, Thunk: ie.Thunk},
			entry:        entry,
		})
	}
	sort.SliceStable(e.funcs, func(i, j int) bool { return e.funcs[i].entry < e.funcs[j].entry })
	// The bodies point at funcs by position, so they come after sorting it.
	byEntry := make(map[uint64]int, len(e.funcs))
	for i, f := range e.funcs {
		byEntry[f.entry] = i
	}
	for _, ie := range index {
		entry, ok := parseAddr(ie.Entry)
		if !ok {
			continue
		}
		for _, r := range ie.Ranges {
			lo, ok1 := parseAddr(r[0])
			hi, ok2 := parseAddr(r[1])
			if ok1 && ok2 && lo <= hi {
				e.ranges = append(e.ranges, exportRange{start: lo, end: hi, fn: byEntry[entry]})
			}
		}
	}
	sort.Slice(e.ranges, func(i, j int) bool { return e.ranges[i].start < e.ranges[j].start })
	return nil
}

func expectDelim(dec *json.Decoder, want json.Delim) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	if d, ok := tok.(json.Delim); !ok || d != want {
		return fmt.Errorf("expected %q, found %v", want, tok)
	}
	return nil
}

// containing is the function an address is in, as the server's containing()
// answers it for an analysed program: an entry point, or inside a body.
func (e *Export) containing(addr uint64) (exportFunc, bool) {
	// The ranges starting at or below addr, nearest first. One that ends
	// before addr says nothing about those before it, which may be longer
	// (bodies can interleave), so the search goes on until no range that far
	// back could reach addr.
	i := sort.Search(len(e.ranges), func(i int) bool { return e.ranges[i].start > addr }) - 1
	for ; i >= 0 && e.ranges[i].start+maxBody >= addr; i-- {
		if addr <= e.ranges[i].end {
			return e.funcs[e.ranges[i].fn], true
		}
	}
	return exportFunc{}, false
}

// maxBody bounds how far back containing looks for a body still reaching an
// address. Generous: a function is rarely a megabyte, and looking further
// costs only on a miss.
const maxBody = 1 << 20

// resolve is the server's resolve(): an address (the function containing it)
// or else a name.
func (e *Export) resolve(which string) (exportFunc, bool) {
	if a, ok := parseAddr(which); ok {
		if f, ok := e.containing(a); ok {
			return f, true
		}
	}
	for _, f := range e.funcs {
		if f.Name == which {
			return f, true
		}
	}
	return exportFunc{}, false
}

// record reads a decompiled function's record back from the file.
func (e *Export) record(entry uint64) (json.RawMessage, bool, error) {
	sp, ok := e.records[entry]
	if !ok {
		return nil, false, nil
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.f == nil {
		return nil, false, ErrClosed
	}
	buf := make([]byte, sp.len)
	if _, err := e.f.ReadAt(buf, sp.off); err != nil {
		return nil, false, err
	}
	return buf, true, nil
}

// FindExport looks in dir for the export of the binary whose sha256 is given,
// reading only each document's header. "" when there is none.
func FindExport(dir, sha256 string) (string, error) {
	if sha256 == "" {
		return "", nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", err
	}
	for _, de := range entries {
		if de.IsDir() || !strings.HasSuffix(de.Name(), ".json") {
			continue
		}
		path := filepath.Join(dir, de.Name())
		if p, err := exportProgram(path); err == nil && strings.EqualFold(p.SHA256, sha256) {
			return path, nil
		}
	}
	return "", nil
}

// exportProgram reads an export's "program" object, which ExportDecomp writes
// before anything large.
func exportProgram(path string) (Program, error) {
	f, err := os.Open(path)
	if err != nil {
		return Program{}, err
	}
	defer f.Close()
	dec := json.NewDecoder(bufio.NewReader(f))
	if err := expectDelim(dec, '{'); err != nil {
		return Program{}, err
	}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return Program{}, err
		}
		if key, _ := tok.(string); key == "program" {
			var p Program
			err := dec.Decode(&p)
			return p, err
		}
		// Anything before it is small: schema and generator.
		var skip json.RawMessage
		if err := dec.Decode(&skip); err != nil {
			return Program{}, err
		}
	}
	return Program{}, io.ErrUnexpectedEOF
}

// StartExport serves an export to a Client, as Start serves a resident
// Ghidra. The client is read-only: edits fail with the reason.
func StartExport(ctx context.Context, path string, opts Options) (*Client, error) {
	e, err := OpenExport(path)
	if err != nil {
		return nil, err
	}
	opts.Writable = false
	opts.exec = func(ctx context.Context, sockPath string) (func(), error) {
		conn, err := net.Dial("unix", sockPath)
		if err != nil {
			e.Close()
			return nil, err
		}
		go e.serve(conn)
		return func() {
			conn.Close()
			e.Close()
		}, nil
	}
	return Start(ctx, opts)
}

// serve answers the server's protocol on conn until it closes or says quit.
func (e *Export) serve(conn net.Conn) {
	defer conn.Close()
	w := bufio.NewWriter(conn)
	send := func(v any) bool {
		b, err := json.Marshal(v)
		if err != nil {
			return false
		}
		if _, err := w.Write(append(b, '\n')); err != nil {
			return false
		}
		return w.Flush() == nil
	}
	if !send(map[string]any{"event": "ready", "schema": e.Schema,
		"program": e.Program, "functionCount": len(e.funcs),
		"sidecar": filepath.Base(e.Path), "sidecarVersion": e.Version}) {
		return
	}
	sc := bufio.NewScanner(conn)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		var req struct {
			ID        uint64 `json:"id"`
			Op        string `json:"op"`
			Offset    int    `json:"offset"`
			Limit     int    `json:"limit"`
			Filter    string `json:"filter"`
			Function  string `json:"function"`
			Addresses string `json:"addresses"`
		}
		if err := json.Unmarshal(sc.Bytes(), &req); err != nil {
			continue
		}
		if req.Op == "quit" {
			return
		}
		if !send(e.handle(req.ID, req.Op, req.Offset, req.Limit, req.Filter, req.Function, req.Addresses)) {
			return
		}
	}
}

// handle answers one request, as DecompServer.handle does.
func (e *Export) handle(id uint64, op string, offset, limit int, filter, function, addresses string) any {
	fail := func(msg string) any { return map[string]any{"id": id, "ok": false, "error": msg} }
	page := func(n int) (int, int) {
		if limit <= 0 || limit > 5000 {
			limit = 500
		}
		lo := min(max(offset, 0), n)
		return lo, min(n, lo+limit)
	}
	needle := strings.ToLower(filter)
	switch op {
	case "info":
		return map[string]any{"id": id, "ok": true, "program": e.Program, "functionCount": len(e.funcs)}
	case "functions":
		var all []FunctionRef
		for _, f := range e.funcs {
			if needle == "" || strings.Contains(strings.ToLower(f.Name), needle) {
				all = append(all, FunctionRef{Name: f.Name, Entry: f.Entry, Thunk: f.Thunk})
			}
		}
		lo, hi := page(len(all))
		return map[string]any{"id": id, "ok": true, "total": len(all), "offset": lo,
			"functions": nonNil(all[lo:hi])}
	case "data":
		var all []DataRef
		for _, d := range e.data {
			if needle == "" || strings.Contains(strings.ToLower(d.Name), needle) {
				all = append(all, d)
			}
		}
		lo, hi := page(len(all))
		return map[string]any{"id": id, "ok": true, "total": len(all), "offset": lo,
			"data": nonNil(all[lo:hi])}
	case "decompile":
		if strings.TrimSpace(function) == "" {
			return fail("decompile needs a function name or address")
		}
		f, ok := e.resolve(strings.TrimSpace(function))
		if !ok {
			return fail("no function " + function)
		}
		rec, ok, err := e.record(f.entry)
		if err != nil {
			return fail(err.Error())
		}
		if !ok {
			why := "it was not decompiled when the export was written"
			if f.Thunk {
				why = "it is a thunk, which the export leaves out"
			}
			return fail(fmt.Sprintf("%s is in %s, but %s", f.Name, filepath.Base(e.Path), why))
		}
		return map[string]any{"id": id, "ok": true, "function": rec}
	case "names":
		names := []FunctionName{}
		for _, raw := range strings.Split(addresses, ",") {
			want := strings.TrimSpace(raw)
			if want == "" || len(names) >= 256 {
				continue
			}
			a, ok := parseAddr(want)
			if !ok {
				continue
			}
			if f, ok := e.containing(a); ok {
				n := f.FunctionName
				n.Addr = want
				names = append(names, n)
			}
		}
		return map[string]any{"id": id, "ok": true, "names": names}
	case "rename", "retype", "comment":
		return fail("this decompilation is an export (" + filepath.Base(e.Path) +
			"), which cannot be changed")
	default:
		return fail("unknown op " + op)
	}
}

func nonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}
