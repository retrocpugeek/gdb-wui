package ghidra

// Kuna as the decompiler, in place of a Ghidra.
//
// Kuna (https://github.com/Noelo-Lab/kuna) is Ghidra's decompiler ported to
// Rust. It needs no JVM, no project and no analysis pass: one function of a
// stripped busybox decompiles in under half a second, starting from nothing.
// That makes it the decompiler for the machines Ghidra cannot reach — and,
// compiled to WebAssembly, for a browser tab, which is where the emulated
// guests of gdb-wui's try-online pages run.
//
// It is driven through kuna_wasm, the entry point kuna's own browser page
// uses: `kuna_wasm <binary> <specs> list` for the function inventory and
// `inspect 0x<entry>` for one function with its line, token, variable and
// frame maps. The same commands run either as a native kuna_wasm here
// (KunaCommand) or in WebAssembly elsewhere, over a socket (DialKuna) — the
// emulator pages answer one from the page hosting the guest.
//
// The far end of the Client's socket is a goroutine, as with an export
// (StartExport): it answers the server's protocol, decompiling on demand and
// translating kuna's records into DecompJson's. Read-only, because kuna keeps
// no database for an edit to change.

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"debug/elf"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// KunaGenerator is the generator a kuna-backed Client reports as its sidecar
// (Ready.Sidecar), which is how the debugger tells the two decompilers apart.
const KunaGenerator = "kuna"

// KunaEngine runs kuna_wasm commands against one binary: the arguments after
// `kuna_wasm <binary> <specs>`, and the JSON document the command prints.
type KunaEngine interface {
	Run(ctx context.Context, args ...string) ([]byte, error)
	// Version describes the kuna build, when it is known.
	Version() string
	Close() error
}

// KunaCommand runs a native kuna_wasm (built with `cargo build --release -p
// kuna-wasm` in kuna's decompiler/ directory) with the SLEIGH specs in specs
// (kuna's specs/, after `make specs`).
func KunaCommand(bin, specs, binary string) KunaEngine {
	return &kunaCommand{bin: bin, specs: specs, binary: binary}
}

type kunaCommand struct{ bin, specs, binary string }

func (k *kunaCommand) Run(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, k.bin, append([]string{k.binary, k.specs}, args...)...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return nil, errors.New(msg)
	}
	return out, nil
}

func (k *kunaCommand) Version() string { return "" }

func (k *kunaCommand) Close() error { return nil }

// DialKuna connects to a kuna served over TCP — by the page an emulated
// guest runs in — and hands it the binary.
//
// The protocol is lines of JSON, one request and one reply at a time:
//
//	{"op":"load","sha256":S,"size":N,"name":B}  -> {"ok":true,"have":bool,"version":V}
//	{"op":"upload","sha256":S,"size":N} + N bytes -> {"ok":true}
//	{"op":"run","sha256":S,"args":[...]}         -> {"ok":true,"result":DOC}
//
// A binary the far end already holds, by hash, is not sent again.
func DialKuna(ctx context.Context, addr, binary string) (KunaEngine, error) {
	data, err := os.ReadFile(binary)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(data)
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("kuna at %s: %w", addr, err)
	}
	k := &kunaRemote{addr: addr, conn: conn, r: bufio.NewReaderSize(conn, 1<<20),
		sha: hex.EncodeToString(sum[:])}
	var have struct {
		Have    bool   `json:"have"`
		Version string `json:"version"`
	}
	if err := k.call(ctx, map[string]any{"op": "load", "sha256": k.sha, "size": len(data),
		"name": filepath.Base(binary)}, nil, &have); err != nil {
		conn.Close()
		return nil, err
	}
	k.version = have.Version
	if !have.Have {
		if err := k.call(ctx, map[string]any{"op": "upload", "sha256": k.sha, "size": len(data)},
			data, nil); err != nil {
			conn.Close()
			return nil, err
		}
	}
	return k, nil
}

type kunaRemote struct {
	addr    string
	sha     string
	version string
	mu      sync.Mutex
	conn    net.Conn
	r       *bufio.Reader
}

func (k *kunaRemote) call(ctx context.Context, req map[string]any, payload []byte, out any) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	if dl, ok := ctx.Deadline(); ok {
		_ = k.conn.SetDeadline(dl)
		defer k.conn.SetDeadline(time.Time{})
	}
	line, err := json.Marshal(req)
	if err != nil {
		return err
	}
	if _, err := k.conn.Write(append(line, '\n')); err != nil {
		return fmt.Errorf("kuna at %s: %w", k.addr, err)
	}
	if payload != nil {
		if _, err := k.conn.Write(payload); err != nil {
			return fmt.Errorf("kuna at %s: %w", k.addr, err)
		}
	}
	reply, err := k.r.ReadBytes('\n')
	if err != nil {
		return fmt.Errorf("kuna at %s: %w", k.addr, err)
	}
	var head struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(reply, &head); err != nil {
		return fmt.Errorf("kuna at %s: unparseable reply: %w", k.addr, err)
	}
	if !head.OK {
		return errors.New(head.Error)
	}
	if out != nil {
		return json.Unmarshal(reply, out)
	}
	return nil
}

func (k *kunaRemote) Run(ctx context.Context, args ...string) ([]byte, error) {
	var res struct {
		Result json.RawMessage `json:"result"`
	}
	if err := k.call(ctx, map[string]any{"op": "run", "sha256": k.sha, "args": args}, nil, &res); err != nil {
		return nil, err
	}
	return res.Result, nil
}

func (k *kunaRemote) Version() string { return k.version }

func (k *kunaRemote) Close() error { return k.conn.Close() }

// kunaList is kuna_wasm's `list` document.
type kunaList struct {
	Functions []struct {
		Name    string `json:"name"`
		Address uint64 `json:"address"`
		Size    uint64 `json:"size"`
		Kind    string `json:"kind"`
	} `json:"functions"`
}

// kunaInspect is the part of kuna_wasm's `inspect` document used here.
type kunaInspect struct {
	Target struct {
		ArchID    string `json:"archid"`
		Processor string `json:"processor"`
		Bits      int    `json:"bits"`
	} `json:"target"`
	Function kunaFunction `json:"function"`
}

type kunaFunction struct {
	Name         string  `json:"name"`
	Address      uint64  `json:"address"`
	Kind         string  `json:"kind"`
	Size         uint64  `json:"size"`
	Code         *string `json:"code"`
	Error        *string `json:"error"`
	Proto        *string `json:"proto"`
	LineMappings []struct {
		Line      int      `json:"line_number"`
		Addresses []uint64 `json:"addresses"`
	} `json:"line_mappings"`
	Variables []struct {
		Name        string `json:"name"`
		Type        string `json:"type"`
		Kind        string `json:"kind"`
		ArgIndex    *int   `json:"arg_index"`
		StackOffset *int   `json:"stack_offset"`
		Size        int    `json:"size"`
	} `json:"variables"`
	Globals []struct {
		Name        string `json:"name"`
		Address     uint64 `json:"address"`
		Declaration string `json:"declaration"`
		Size        int    `json:"size"`
	} `json:"globals"`
	Tokens []struct {
		Line    int     `json:"line"`
		Text    string  `json:"text"`
		Kind    string  `json:"kind"`
		Address *uint64 `json:"address"`
	} `json:"tokens"`
	Instructions []struct {
		Address uint64 `json:"address"`
		Size    uint64 `json:"size"`
	} `json:"instructions"`
	Frame struct {
		StackPointer *struct {
			Register string `json:"register"`
			Offset   int    `json:"offset"`
		} `json:"stack_pointer"`
	} `json:"frame"`
}

// kunaServer answers the decompiler protocol from a KunaEngine.
type kunaServer struct {
	engine  KunaEngine
	program Program
	version string
	symbols map[string]uint64 // the ELF's own names, for Source

	// mu guards the index, which grows as functions the inventory missed are
	// decompiled; decompiling holds run instead, so names keep being answered
	// while kuna works.
	mu     sync.Mutex
	funcs  []exportFunc
	ranges []exportRange
	byAddr map[uint64]int
	data   map[uint64]DataRef
	cache  map[uint64]json.RawMessage

	run sync.Mutex
}

// StartKuna serves kuna's decompilation of binary to a Client. The engine is
// closed with the client.
func StartKuna(ctx context.Context, engine KunaEngine, binary string, opts Options) (*Client, error) {
	k, err := newKunaServer(ctx, engine, binary)
	if err != nil {
		engine.Close()
		return nil, err
	}
	opts.Writable = false
	opts.exec = func(ctx context.Context, sockPath string) (func(), error) {
		conn, err := net.Dial("unix", sockPath)
		if err != nil {
			engine.Close()
			return nil, err
		}
		go k.serve(conn)
		return func() {
			conn.Close()
			engine.Close()
		}, nil
	}
	return Start(ctx, opts)
}

func newKunaServer(ctx context.Context, engine KunaEngine, binary string) (*kunaServer, error) {
	data, err := os.ReadFile(binary)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(data)
	k := &kunaServer{engine: engine, version: engine.Version(), byAddr: map[uint64]int{},
		data: map[uint64]DataRef{}, cache: map[uint64]json.RawMessage{},
		symbols: map[string]uint64{}}
	k.program = Program{Name: filepath.Base(binary), Path: binary,
		SHA256: hex.EncodeToString(sum[:])}
	if f, err := elf.NewFile(bytes.NewReader(data)); err == nil {
		k.program.Format = "Executable and Linking Format (ELF)"
		k.program.PointerSize = 4
		if f.Class == elf.ELFCLASS64 {
			k.program.PointerSize = 8
		}
		// kuna's addresses are the file's own, so its "image base" is the
		// lowest address the file asks to be loaded at; the debugger's bias
		// arithmetic (biasFromEntryPoint) then needs nothing kuna-specific.
		base := ^uint64(0)
		for _, p := range f.Progs {
			if p.Type == elf.PT_LOAD && p.Vaddr < base {
				base = p.Vaddr
			}
		}
		if base == ^uint64(0) {
			base = 0
		}
		k.program.ImageBase = fmt.Sprintf("0x%x", base)
		syms, _ := f.Symbols()
		dyn, _ := f.DynamicSymbols()
		for _, s := range append(syms, dyn...) {
			if elf.ST_TYPE(s.Info) == elf.STT_FUNC && s.Value != 0 {
				k.symbols[s.Name] = s.Value
			}
		}
		f.Close()
	} else {
		k.program.Format = "binary"
		k.program.ImageBase = "0x0"
	}

	out, err := engine.Run(ctx, "list")
	if err != nil {
		return nil, fmt.Errorf("kuna: list: %w", err)
	}
	var list kunaList
	if err := json.Unmarshal(out, &list); err != nil {
		return nil, fmt.Errorf("kuna: list: %w", err)
	}
	for _, f := range list.Functions {
		k.add(f.Name, f.Address, f.Size, f.Kind != "" && f.Kind != "func")
	}
	k.sortRanges()
	// The language comes from decompiling something; the entry point is the
	// one function every executable has.
	if len(k.funcs) > 0 {
		if doc, err := engine.Run(ctx, "inspect", fmt.Sprintf("0x%x", k.funcs[0].entry)); err == nil {
			var in kunaInspect
			if json.Unmarshal(doc, &in) == nil {
				k.program.LanguageID = in.Target.ArchID
			}
		}
	}
	return k, nil
}

// add records one function in the index, under mu or before serving.
func (k *kunaServer) add(name string, entry, size uint64, thunk bool) {
	if _, ok := k.byAddr[entry]; ok {
		return
	}
	hexEntry := fmt.Sprintf("0x%x", entry)
	k.byAddr[entry] = len(k.funcs)
	k.funcs = append(k.funcs, exportFunc{
		FunctionName: FunctionName{Name: name, Entry: hexEntry, Thunk: thunk},
		entry:        entry,
	})
	if size == 0 {
		size = 1
	}
	k.ranges = append(k.ranges, exportRange{start: entry, end: entry + size - 1, fn: k.byAddr[entry]})
}

func (k *kunaServer) sortRanges() {
	sort.Slice(k.ranges, func(i, j int) bool { return k.ranges[i].start < k.ranges[j].start })
}

func (k *kunaServer) containing(addr uint64) (exportFunc, bool) {
	i := sort.Search(len(k.ranges), func(i int) bool { return k.ranges[i].start > addr }) - 1
	for ; i >= 0 && k.ranges[i].start+maxBody >= addr; i-- {
		if addr <= k.ranges[i].end {
			return k.funcs[k.ranges[i].fn], true
		}
	}
	return exportFunc{}, false
}

// resolve finds a function by address or name. kuna's inventory misses
// functions that are only ever called — it lists what it can prove a function
// from the file alone — and its decompilations name them sub_<address>, so
// such a name is taken at its word and decompiled at that address.
func (k *kunaServer) resolve(which string) (uint64, bool) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if a, ok := parseAddr(which); ok {
		if f, ok := k.containing(a); ok {
			return f.entry, true
		}
		return 0, false
	}
	for _, f := range k.funcs {
		if f.Name == which {
			return f.entry, true
		}
	}
	if rest, ok := strings.CutPrefix(which, "sub_"); ok {
		if a, err := strconv.ParseUint(rest, 16, 64); err == nil {
			return a, true
		}
	}
	return 0, false
}

func (k *kunaServer) serve(conn net.Conn) {
	defer conn.Close()
	var wmu sync.Mutex
	w := bufio.NewWriter(conn)
	send := func(v any) bool {
		b, err := json.Marshal(v)
		if err != nil {
			return false
		}
		wmu.Lock()
		defer wmu.Unlock()
		if _, err := w.Write(append(b, '\n')); err != nil {
			return false
		}
		return w.Flush() == nil
	}
	k.mu.Lock()
	n := len(k.funcs)
	k.mu.Unlock()
	if !send(map[string]any{"event": "ready", "schema": Schema, "program": k.program,
		"functionCount": n, "sidecar": KunaGenerator, "sidecarVersion": k.version}) {
		return
	}
	sc := bufio.NewScanner(conn)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	var wg sync.WaitGroup
	defer wg.Wait()
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
		// Concurrently, because a decompile takes kuna a second and the
		// names the stack pane asks for on every stop must not queue behind
		// it.
		wg.Add(1)
		go func() {
			defer wg.Done()
			send(k.handle(req.ID, req.Op, req.Offset, req.Limit, req.Filter, req.Function, req.Addresses))
		}()
	}
}

func (k *kunaServer) handle(id uint64, op string, offset, limit int, filter, function, addresses string) any {
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
		k.mu.Lock()
		defer k.mu.Unlock()
		return map[string]any{"id": id, "ok": true, "program": k.program, "functionCount": len(k.funcs)}
	case "functions":
		k.mu.Lock()
		defer k.mu.Unlock()
		sorted := append([]exportFunc(nil), k.funcs...)
		sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].entry < sorted[j].entry })
		var all []FunctionRef
		for _, f := range sorted {
			if needle == "" || strings.Contains(strings.ToLower(f.Name), needle) {
				all = append(all, FunctionRef{Name: f.Name, Entry: f.Entry, Thunk: f.Thunk})
			}
		}
		lo, hi := page(len(all))
		return map[string]any{"id": id, "ok": true, "total": len(all), "offset": lo,
			"functions": nonNil(all[lo:hi])}
	case "data":
		k.mu.Lock()
		defer k.mu.Unlock()
		var all []DataRef
		for _, d := range k.data {
			if needle == "" || strings.Contains(strings.ToLower(d.Name), needle) {
				all = append(all, d)
			}
		}
		sort.Slice(all, func(i, j int) bool { return all[i].Address < all[j].Address })
		lo, hi := page(len(all))
		return map[string]any{"id": id, "ok": true, "total": len(all), "offset": lo,
			"data": nonNil(all[lo:hi])}
	case "decompile":
		which := strings.TrimSpace(function)
		if which == "" {
			return fail("decompile needs a function name or address")
		}
		entry, ok := k.resolve(which)
		if !ok {
			return fail("kuna knows no function " + which)
		}
		rec, err := k.decompile(entry)
		if err != nil {
			return fail(err.Error())
		}
		return map[string]any{"id": id, "ok": true, "function": rec}
	case "names":
		k.mu.Lock()
		defer k.mu.Unlock()
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
			if f, ok := k.containing(a); ok {
				n := f.FunctionName
				n.Addr = want
				names = append(names, n)
			}
		}
		return map[string]any{"id": id, "ok": true, "names": names}
	case "rename", "retype", "comment":
		return fail("kuna's decompilation cannot be changed: it keeps no database for an edit to go into")
	default:
		return fail("unknown op " + op)
	}
}

// decompile asks kuna for one function, once.
func (k *kunaServer) decompile(entry uint64) (json.RawMessage, error) {
	k.mu.Lock()
	rec, ok := k.cache[entry]
	k.mu.Unlock()
	if ok {
		return rec, nil
	}
	k.run.Lock()
	defer k.run.Unlock()
	k.mu.Lock()
	rec, ok = k.cache[entry]
	k.mu.Unlock()
	if ok {
		return rec, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	doc, err := k.engine.Run(ctx, "inspect", fmt.Sprintf("0x%x", entry))
	if err != nil {
		return nil, fmt.Errorf("kuna: %v", err)
	}
	var in kunaInspect
	if err := json.Unmarshal(doc, &in); err != nil {
		return nil, fmt.Errorf("kuna: unparseable inspect: %v", err)
	}
	if in.Function.Error != nil && in.Function.Code == nil {
		return nil, fmt.Errorf("kuna could not decompile %s: %s", in.Function.Name, *in.Function.Error)
	}
	fn := kunaToFunction(&in, k.symbols)
	rec, err = json.Marshal(fn)
	if err != nil {
		return nil, err
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	k.cache[entry] = rec
	if _, known := k.byAddr[entry]; !known {
		bodyEnd, _ := parseAddr(fn.BodyEnd)
		k.add(fn.Name, entry, bodyEnd-entry+1, false)
		k.sortRanges()
	}
	for _, g := range fn.Globals {
		a, ok := parseAddr(g.Address)
		if ok {
			k.data[a] = DataRef{Name: g.Name, Address: g.Address, Type: g.Type, Length: g.Size}
		}
	}
	return rec, nil
}

// kunaDecl is one declaration at the top of a kuna decompilation: `char *v1;
// // x0`, the comment saying where the variable lives.
var kunaDecl = regexp.MustCompile(`^\s+([^=();/]*?[\s*])([A-Za-z_]\w*)(\[[^\]]*\])?;\s*//\s*(.+?)\s*$`)

// kunaStorage is one location in a declaration's comment.
var kunaStorage = regexp.MustCompile(`^(?:stack ([+-]) 0x([0-9a-f]+)|ram 0x[0-9a-f]+|tmp|([a-z]+[0-9]*))$`)

// kunaToFunction translates one kuna_wasm inspect document.
//
// What kuna says about a variable is spread over three places that do not
// always agree — its variables list can name a parameter param_1 that the
// text calls a0 — so the text, which is what the user reads, is the authority
// for names, and each declaration's comment for where the variable lives.
func kunaToFunction(in *kunaInspect, symbols map[string]uint64) *Function {
	f := &in.Function
	code := ""
	if f.Code != nil {
		code = *f.Code
	}
	entry := f.Address
	end := entry + f.Size
	for _, ins := range f.Instructions {
		end = max(end, ins.Address+ins.Size)
	}
	if end <= entry {
		end = entry + 1
	}
	signature := strings.TrimSuffix(strings.TrimSpace(deref(f.Proto)), ";")
	lines := strings.Split(code, "\n")
	if signature == "" && len(lines) > 0 {
		signature = strings.TrimSpace(lines[0])
	}
	source := SourceDefault
	if !strings.HasPrefix(f.Name, "sub_") {
		source = SourceAnalysis
		if a, ok := symbols[f.Name]; ok && a == entry {
			source = SourceImported
		}
	}
	fn := &Function{
		Name:      f.Name,
		Entry:     fmt.Sprintf("0x%x", entry),
		BodyStart: fmt.Sprintf("0x%x", entry),
		BodyEnd:   fmt.Sprintf("0x%x", end-1),
		Signature: signature,
		Source:    source,
		Frame:     Frame{GrowsNegative: true},
		Text:      code,
		LineCount: len(lines),
	}
	if sp := f.Frame.StackPointer; sp != nil {
		depth := sp.Offset
		fn.Frame.SPDepth = &depth
		fn.Frame.Size = -depth
	}
	for _, m := range f.LineMappings {
		l := Line{N: m.Line}
		for _, a := range m.Addresses {
			l.Addrs = append(l.Addrs, fmt.Sprintf("0x%x", a))
		}
		fn.Lines = append(fn.Lines, l)
	}
	for i, l := range lines {
		if strings.HasPrefix(strings.TrimSpace(l), "//") {
			fn.CommentLines = append(fn.CommentLines, CommentLine{N: i + 1})
		}
	}

	// The first address each name is used at, for a register variable's PC.
	firstUse := map[string]uint64{}
	for _, t := range f.Tokens {
		if t.Address == nil || t.Kind != "variable" {
			continue
		}
		if a, ok := firstUse[t.Text]; !ok || *t.Address < a {
			firstUse[t.Text] = *t.Address
		}
	}

	// Parameters, from the signature; kuna's list supplies their types by
	// position, the ABI their registers.
	for i, p := range kunaParams(signature) {
		v := Var{Name: p.name, Type: p.typ, Param: true, Size: in.Target.Bits / 8,
			PC: fn.Entry, Storage: Storage{Kind: StorageOther}}
		for _, kv := range f.Variables {
			if kv.Kind == "arg" && kv.ArgIndex != nil && *kv.ArgIndex == i {
				v.Size = kv.Size
				if kv.StackOffset != nil {
					v.Storage = Storage{Kind: StorageStack, Offset: *kv.StackOffset}
					v.PC = ""
				}
			}
		}
		if v.Storage.Kind == StorageOther {
			if reg := kunaArgRegister(in.Target.Processor, i, p.typ, v.Size); reg != "" {
				v.Storage = Storage{Kind: StorageRegister, Register: reg}
			} else {
				v.Storage.Text = "parameter " + strconv.Itoa(i+1)
			}
		}
		fn.Variables = append(fn.Variables, v)
	}

	// Locals: the declarations after the opening brace, up to the first line
	// that is not one.
	for _, l := range lines[min(2, len(lines)):] {
		m := kunaDecl.FindStringSubmatch(l)
		if m == nil {
			break
		}
		storages := strings.Split(m[4], " | ")
		ok := true
		for _, s := range storages {
			if !kunaStorage.MatchString(strings.TrimSpace(s)) {
				ok = false
			}
		}
		if !ok {
			// A statement with a trailing comment — `foo(); // tail-call` —
			// and not a declaration: the declarations are over.
			break
		}
		typ := strings.TrimSpace(m[1]) + m[3]
		v := Var{Name: m[2], Type: typ, Size: kunaTypeSize(typ, in.Target.Bits/8),
			Storage: Storage{Kind: StorageOther, Text: m[4]}}
		// kuna merges what it decided is one variable across every place it
		// lives, and does not say which holds the value at a given pc. One
		// stack slot among them is its home — unoptimised code keeps every
		// variable in one and loads it into registers to use it — and is
		// what is read. Otherwise only a lone register is: of several, none
		// can be picked, and a wrong one would read as a value.
		var stack, regs []string
		for _, st := range storages {
			s := kunaStorage.FindStringSubmatch(strings.TrimSpace(st))
			switch {
			case s[2] != "":
				stack = append(stack, st)
			case s[3] != "" && s[3] != "tmp":
				regs = append(regs, s[3])
			case s[3] == "tmp":
				regs = append(regs, "")
			}
		}
		switch {
		case len(stack) == 1:
			s := kunaStorage.FindStringSubmatch(strings.TrimSpace(stack[0]))
			off, _ := strconv.ParseInt(s[2], 16, 64)
			if s[1] == "-" {
				off = -off
			}
			v.Storage = Storage{Kind: StorageStack, Offset: int(off)}
		case len(stack) == 0 && len(regs) == 1 && regs[0] != "" && len(storages) == 1:
			v.Storage = Storage{Kind: StorageRegister, Register: regs[0]}
			if a, ok := firstUse[v.Name]; ok {
				v.PC = fmt.Sprintf("0x%x", a)
			} else {
				v.PC = fn.Entry
			}
		}
		fn.Variables = append(fn.Variables, v)
	}

	seen := map[uint64]bool{}
	for _, g := range f.Globals {
		if seen[g.Address] {
			continue
		}
		seen[g.Address] = true
		typ := strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(g.Declaration), g.Name))
		fn.Globals = append(fn.Globals, Global{Name: g.Name, Type: typ, Size: g.Size,
			Address: fmt.Sprintf("0x%x", g.Address)})
	}
	return fn
}

type kunaParam struct{ typ, name string }

// kunaParams splits a prototype's parameter list, `int f(int argc,char
// **argv)`, into types and names.
func kunaParams(signature string) []kunaParam {
	open := strings.Index(signature, "(")
	close := strings.LastIndex(signature, ")")
	if open < 0 || close <= open {
		return nil
	}
	inner := strings.TrimSpace(signature[open+1 : close])
	if inner == "" || inner == "void" {
		return nil
	}
	var out []kunaParam
	depth, start := 0, 0
	for i := 0; i <= len(inner); i++ {
		if i < len(inner) {
			switch inner[i] {
			case '(', '[':
				depth++
				continue
			case ')', ']':
				depth--
				continue
			case ',':
				if depth > 0 {
					continue
				}
			default:
				continue
			}
		}
		p := strings.TrimSpace(inner[start:i])
		start = i + 1
		j := len(p)
		for j > 0 && (p[j-1] == '_' || p[j-1] >= '0' && p[j-1] <= '9' ||
			p[j-1] >= 'a' && p[j-1] <= 'z' || p[j-1] >= 'A' && p[j-1] <= 'Z') {
			j--
		}
		if j == 0 || j == len(p) || p == "..." {
			// A bare type, or a variadic tail: nothing nameable.
			continue
		}
		out = append(out, kunaParam{typ: strings.TrimSpace(p[:j]), name: p[j:]})
	}
	return out
}

// kunaArgRegister is the register the i-th integer argument arrives in, or ""
// where the ABI puts it elsewhere.
func kunaArgRegister(processor string, i int, typ string, size int) string {
	if i >= 8 || strings.Contains(typ, "float") || strings.Contains(typ, "double") {
		return ""
	}
	switch processor {
	case "RISCV":
		return fmt.Sprintf("a%d", i)
	case "AARCH64":
		if size > 0 && size <= 4 && !strings.Contains(typ, "*") {
			return fmt.Sprintf("w%d", i)
		}
		return fmt.Sprintf("x%d", i)
	}
	return ""
}

// kunaTypeSize guesses a declared type's size, which kuna's text does not
// carry: a pointer is a pointer, and the C names have their usual sizes.
func kunaTypeSize(typ string, pointer int) int {
	if strings.Contains(typ, "*") {
		return pointer
	}
	switch strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(typ, "unsigned "), "signed ")) {
	case "char", "bool":
		return 1
	case "short":
		return 2
	case "int", "float":
		return 4
	case "long", "double":
		return pointer
	case "long long":
		return 8
	}
	return 0
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
