//go:build integration

package debugger_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/retrocpugeek/gdb-wui/internal/testutil"
	"github.com/retrocpugeek/gdb-wui/internal/wire"
)

// addFixture compiles a second program into an existing project. realProject
// builds exactly one, and the cache-invalidation tests need two to switch
// between.
func addFixture(t *testing.T, h *harness, name string) {
	t.Helper()
	src := filepath.Join(testutil.RepoRoot(t), "testdata", "fixtures", name+".c")
	body, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(h.files.Abs(), name+".c")
	if err := os.WriteFile(dst, body, 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("gcc", "-g", "-O0", "-o",
		filepath.Join(h.files.Abs(), name), dst).CombinedOutput()
	if err != nil {
		t.Fatalf("compiling %s: %v\n%s", name, err, out)
	}
}

// The symbol pane's backend, against a real gdb. The distinction it has to get
// right is debug-vs-nondebug: one population can be jumped to in the source
// view and the other only in the disassembly, and a UI that cannot tell them
// apart offers jumps that do nothing.

func findSymbol(syms []wire.Symbol, name string) (wire.Symbol, bool) {
	for _, s := range syms {
		if s.Name == name {
			return s, true
		}
	}
	return wire.Symbol{}, false
}

func TestSymbolsListDebugFunctions(t *testing.T) {
	h := startReal(t, "hello")
	h.mustDo(wire.TypeExeLoad, wire.ExeLoadRequest{Path: "hello"})

	out := h.mustDo(wire.TypeSymbolsList, wire.SymbolsListRequest{}).(wire.SymbolsList)
	if out.Available == 0 {
		t.Fatal("no symbols at all from a -g binary")
	}

	main, ok := findSymbol(out.Symbols, "main")
	if !ok {
		t.Fatalf("main not in %d symbols", len(out.Symbols))
	}
	if main.Kind != wire.SymbolFunction {
		t.Errorf("main kind = %q, want function", main.Kind)
	}
	if !main.Debug {
		t.Error("main should carry debug info")
	}
	if main.File != "hello.c" {
		t.Errorf("main file = %q, want hello.c — the source jump needs a project path", main.File)
	}
	if main.Line == 0 {
		t.Error("main has no line; a source jump has nowhere to go")
	}
	if !strings.Contains(main.Type, "int") {
		t.Errorf("main type = %q, want something containing int", main.Type)
	}

	// A static function must be listed too: it is exactly what someone
	// reaches for the filter box to find.
	if _, ok := findSymbol(out.Symbols, "add"); !ok {
		t.Error("the static function add is missing")
	}
}

func TestSymbolsListIncludesNondebug(t *testing.T) {
	h := startReal(t, "hello")
	h.mustDo(wire.TypeExeLoad, wire.ExeLoadRequest{Path: "hello"})

	out := h.mustDo(wire.TypeSymbolsList, wire.SymbolsListRequest{Limit: 5000}).(wire.SymbolsList)

	var nondebug int
	for _, s := range out.Symbols {
		if s.Debug {
			continue
		}
		nondebug++
		if s.Address == "" {
			t.Fatalf("nondebug symbol %q has no address, so it cannot be jumped to", s.Name)
		}
		if !strings.HasPrefix(s.Address, "0x") {
			t.Errorf("address %q is not hex", s.Address)
		}
	}
	if nondebug == 0 {
		t.Error("no nondebug symbols; --include-nondebug is not reaching gdb")
	}

	// _start comes from the ELF table, not from debug info.
	if start, ok := findSymbol(out.Symbols, "_start"); ok {
		if start.Debug {
			t.Error("_start should not claim debug info")
		}
		if start.Kind != wire.SymbolFunction {
			t.Errorf("_start kind = %q, want function", start.Kind)
		}
	}
}

// Addresses come back from gdb zero-padded to the word size. Every other panel
// shows them trimmed, and two spellings of one address in one UI is a bug
// report waiting to happen.
func TestSymbolsAddressesAreTrimmed(t *testing.T) {
	h := startReal(t, "hello")
	h.mustDo(wire.TypeExeLoad, wire.ExeLoadRequest{Path: "hello"})

	out := h.mustDo(wire.TypeSymbolsList, wire.SymbolsListRequest{Limit: 5000}).(wire.SymbolsList)
	for _, s := range out.Symbols {
		if s.Address != "" && strings.HasPrefix(s.Address, "0x0") && s.Address != "0x0" {
			t.Errorf("address %q is still zero-padded", s.Address)
		}
	}
}

func TestSymbolsFilterAndRanking(t *testing.T) {
	h := startReal(t, "hello")
	h.mustDo(wire.TypeExeLoad, wire.ExeLoadRequest{Path: "hello"})

	out := h.mustDo(wire.TypeSymbolsList,
		wire.SymbolsListRequest{Filter: "main"}).(wire.SymbolsList)
	if len(out.Symbols) == 0 {
		t.Fatal("filtering for main found nothing")
	}
	if out.Symbols[0].Name != "main" {
		t.Errorf("first hit = %q, want the exact match main first", out.Symbols[0].Name)
	}
	for _, s := range out.Symbols {
		if !strings.Contains(strings.ToLower(s.Name), "main") {
			t.Errorf("%q does not match the filter", s.Name)
		}
	}

	// Case-insensitive: the user does not know how the symbol was spelled.
	upper := h.mustDo(wire.TypeSymbolsList,
		wire.SymbolsListRequest{Filter: "MAIN"}).(wire.SymbolsList)
	if upper.Matched != out.Matched {
		t.Errorf("MAIN matched %d, main matched %d — filter is case-sensitive",
			upper.Matched, out.Matched)
	}
}

func TestSymbolsKindFilter(t *testing.T) {
	h := startReal(t, "hello")
	h.mustDo(wire.TypeExeLoad, wire.ExeLoadRequest{Path: "hello"})

	out := h.mustDo(wire.TypeSymbolsList, wire.SymbolsListRequest{
		Kind: wire.SymbolVariable, Limit: 5000,
	}).(wire.SymbolsList)
	for _, s := range out.Symbols {
		if s.Kind != wire.SymbolVariable {
			t.Fatalf("%q is a %s in a variables-only reply", s.Name, s.Kind)
		}
	}
	if out.Available == out.Matched {
		t.Error("the variable filter matched the whole table; kinds are not being separated")
	}
}

// Truncation must be visible. A list that silently stops at the limit reads as
// the complete answer, and the user concludes the symbol is not there.
func TestSymbolsTruncationIsReported(t *testing.T) {
	h := startReal(t, "hello")
	h.mustDo(wire.TypeExeLoad, wire.ExeLoadRequest{Path: "hello"})

	out := h.mustDo(wire.TypeSymbolsList, wire.SymbolsListRequest{Limit: 2}).(wire.SymbolsList)
	if len(out.Symbols) != 2 {
		t.Fatalf("got %d symbols, want the limit of 2", len(out.Symbols))
	}
	if !out.Truncated {
		t.Error("truncated is false on a truncated reply")
	}
	if out.Matched <= 2 {
		t.Errorf("matched = %d, should count past the limit", out.Matched)
	}
}

// A stripped binary has no debug info. The pane must still work: the ELF
// dynamic symbols are the only thing a user has to navigate by, which is
// precisely the remote-firmware case.
func TestSymbolsOnStrippedBinary(t *testing.T) {
	h := startReal(t, "hello")
	addFixture(t, h, "nodebug")
	stripFixture(t, h, "nodebug")
	h.mustDo(wire.TypeExeLoad, wire.ExeLoadRequest{Path: "nodebug"})

	out := h.mustDo(wire.TypeSymbolsList, wire.SymbolsListRequest{Limit: 5000}).(wire.SymbolsList)
	for _, s := range out.Symbols {
		if s.Debug {
			t.Errorf("%q claims debug info in a stripped binary", s.Name)
		}
	}
	if out.Available == 0 {
		t.Skip("this toolchain left no dynamic symbols to list")
	}
}

// Loading a different program must not leave the previous one's symbols in the
// cache, or the pane lists functions that no longer exist and jumping to them
// lands nowhere.
func TestSymbolsInvalidatedOnExeLoad(t *testing.T) {
	h := startReal(t, "hello")
	addFixture(t, h, "structs")
	h.mustDo(wire.TypeExeLoad, wire.ExeLoadRequest{Path: "hello"})

	before := h.mustDo(wire.TypeSymbolsList,
		wire.SymbolsListRequest{Filter: "add"}).(wire.SymbolsList)
	if before.Matched == 0 {
		t.Fatal("add missing from hello")
	}

	h.mustDo(wire.TypeExeLoad, wire.ExeLoadRequest{Path: "structs"})
	h.rec.wait(t, wire.EventSymbolsInvalidated)

	after := h.mustDo(wire.TypeSymbolsList,
		wire.SymbolsListRequest{Filter: "inspect"}).(wire.SymbolsList)
	if _, ok := findSymbol(after.Symbols, "inspect"); !ok {
		t.Error("structs' inspect is missing; the cache was not refilled")
	}
	stale := h.mustDo(wire.TypeSymbolsList,
		wire.SymbolsListRequest{Filter: "add"}).(wire.SymbolsList)
	if _, ok := findSymbol(stale.Symbols, "add"); ok {
		t.Error("hello's add survived loading structs; the cache is stale")
	}
}

// The remote-target workflow loads symbols by typing `file …` at the console,
// never through exe.load. If that does not invalidate, the pane is empty for
// exactly the users who need it most.
func TestSymbolsInvalidatedByConsoleFileCommand(t *testing.T) {
	h := startReal(t, "hello")
	addFixture(t, h, "structs")
	h.mustDo(wire.TypeExeLoad, wire.ExeLoadRequest{Path: "hello"})
	h.mustDo(wire.TypeSymbolsList, wire.SymbolsListRequest{})

	h.mustDo(wire.TypeConsoleExec, wire.ConsoleExecRequest{
		Line: "file " + h.files.Abs() + "/structs",
	})
	h.rec.wait(t, wire.EventSymbolsInvalidated)

	out := h.mustDo(wire.TypeSymbolsList,
		wire.SymbolsListRequest{Filter: "inspect"}).(wire.SymbolsList)
	if _, ok := findSymbol(out.Symbols, "inspect"); !ok {
		t.Error("symbols did not follow a typed `file` command")
	}
}

// The symbol pane jumps by name, not by the address it displays, and this is
// why. gdb's -symbol-info-* reports link-time addresses; a PIE is relocated
// when it runs, so those addresses point at unmapped memory the moment there
// is a live process. Disassembling by name has to resolve to the *running*
// address.
func TestDisassembleBySymbolNameFollowsRelocation(t *testing.T) {
	h := startReal(t, "hello")
	h.mustDo(wire.TypeExeLoad, wire.ExeLoadRequest{Path: "hello"})

	syms := h.mustDo(wire.TypeSymbolsList,
		wire.SymbolsListRequest{Filter: "_start", Limit: 5000}).(wire.SymbolsList)
	start, ok := findSymbol(syms.Symbols, "_start")
	if !ok {
		t.Skip("no _start symbol in this toolchain's output")
	}
	linkTime, err := strconv.ParseUint(strings.TrimPrefix(start.Address, "0x"), 16, 64)
	if err != nil {
		t.Fatalf("parsing %q: %v", start.Address, err)
	}

	h.mustDo(wire.TypeBpSetSource, wire.BreakpointRequest{Path: "hello.c", Line: lineMainInit})
	h.mustDo(wire.TypeExecRun, wire.ExecRequest{})
	h.rec.wait(t, wire.EventStopped)

	out := h.mustDo(wire.TypeDisasmFunction,
		wire.DisasmFunctionRequest{Address: "_start"}).(wire.Disassembly)
	if len(out.Instructions) == 0 {
		t.Fatal("disassembling _start by name produced nothing")
	}
	got := out.Instructions[0].Addr
	if got == linkTime {
		t.Fatalf("got the link-time address 0x%x; a relocated PIE should not "+
			"disassemble there", got)
	}
	if got < linkTime {
		t.Errorf("address 0x%x is below the link-time 0x%x", got, linkTime)
	}

	// And the address the pane displays really is the unusable one, which is
	// the whole reason the jump goes by name.
	if _, werr := h.do(wire.TypeDisasmRange, wire.DisasmRangeRequest{
		Start: start.Address,
		End:   "0x" + strconv.FormatUint(linkTime+16, 16),
	}); werr == nil {
		t.Log("note: the link-time range was readable; relocation may be disabled here")
	}
}

// Shared libraries bring their symbols with them. A pane that still lists only
// the 27 symbols the executable had before it ran is not showing what the
// program contains, which is the one thing it claims to show.
func TestSymbolsPickUpSharedLibraries(t *testing.T) {
	h := startReal(t, "hello")
	h.mustDo(wire.TypeExeLoad, wire.ExeLoadRequest{Path: "hello"})

	before := h.mustDo(wire.TypeSymbolsList, wire.SymbolsListRequest{}).(wire.SymbolsList)

	h.mustDo(wire.TypeBpSetSource, wire.BreakpointRequest{Path: "hello.c", Line: lineMainInit})
	h.mustDo(wire.TypeExecRun, wire.ExecRequest{})
	// The invalidation goes out just before the stop: by the time a client has
	// been told the program stopped, the symbol table it is about to query is
	// already the new one.
	h.rec.wait(t, wire.EventSymbolsInvalidated)
	h.rec.wait(t, wire.EventStopped)

	after := h.mustDo(wire.TypeSymbolsList, wire.SymbolsListRequest{}).(wire.SymbolsList)
	if after.Available <= before.Available {
		t.Errorf("available went %d -> %d; libc's symbols were not picked up",
			before.Available, after.Available)
	}

	// The program's own symbols must survive the refill, and an exact match
	// must still outrank the crowd of libc names that now contains it.
	hit := h.mustDo(wire.TypeSymbolsList,
		wire.SymbolsListRequest{Filter: "add"}).(wire.SymbolsList)
	if len(hit.Symbols) == 0 || hit.Symbols[0].Name != "add" {
		t.Fatalf("add is no longer the top hit among %d matches", hit.Matched)
	}
	if hit.Symbols[0].File != "hello.c" {
		t.Errorf("top hit is %q from %q, not the program's own add",
			hit.Symbols[0].Name, hit.Symbols[0].File)
	}
}

// addFixtureNoDebug compiles a fixture without -g and leaves it unstripped, so
// its symbols reach the ELF table with an address but no type.
func addFixtureNoDebug(t *testing.T, h *harness, name string) {
	t.Helper()
	src := filepath.Join(testutil.RepoRoot(t), "testdata", "fixtures", name+".c")
	body, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(h.files.Abs(), name+".c")
	if err := os.WriteFile(dst, body, 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("gcc", "-O0", "-o",
		filepath.Join(h.files.Abs(), name), dst).CombinedOutput()
	if err != nil {
		t.Fatalf("compiling %s: %v\n%s", name, err, out)
	}
}

// A symbol with an address but no type is what a binary built without -g is
// made of. gdb refuses to evaluate one — "'LogType' has unknown type; cast it
// to its declared type" — so anything that asks for its *value* fails. Its
// address is not in doubt, and the address is what the symbol pane wants.
func TestResolveMinimalSymbolAddress(t *testing.T) {
	h := startReal(t, "hello")
	addFixtureNoDebug(t, h, "minsym")
	h.mustDo(wire.TypeExeLoad, wire.ExeLoadRequest{Path: "minsym"})

	syms := h.mustDo(wire.TypeSymbolsList,
		wire.SymbolsListRequest{Filter: "LogType"}).(wire.SymbolsList)
	sym, ok := findSymbol(syms.Symbols, "LogType")
	if !ok {
		t.Fatalf("LogType missing from %d matches", syms.Matched)
	}
	if sym.Debug {
		t.Error("LogType should have no debug info")
	}
	if sym.Kind != wire.SymbolVariable {
		t.Errorf("LogType kind = %q, want variable", sym.Kind)
	}
	if sym.Address == "" {
		t.Fatal("LogType has no address")
	}

	// No -g means no line table, so there is no source line to break on.
	// starti is the documented way into a binary like this.
	h.mustDo(wire.TypeExecRun, wire.ExecRequest{StopAtEntry: true})
	h.rec.wait(t, wire.EventStopped)

	// This is the request the symbol pane sends for a variable. Before the
	// address-of fallback it failed outright with gdb's unknown-type message.
	out := h.mustDo(wire.TypeMemRead, wire.MemReadRequest{
		Address: "&(LogType)", Count: 16,
	}).(wire.Memory)
	if out.Addr == 0 {
		t.Fatal("&(LogType) resolved to 0")
	}

	// The bare name must work too: the memory address bar accepts anything the
	// user types, and typing a symbol's name there is the obvious thing to do.
	bare := h.mustDo(wire.TypeMemRead, wire.MemReadRequest{
		Address: "LogType", Count: 16,
	}).(wire.Memory)
	if bare.Addr != out.Addr {
		t.Errorf("LogType resolved to 0x%x but &(LogType) to 0x%x",
			bare.Addr, out.Addr)
	}
}

// The same symbol, reached the way the pane reaches a function: by name,
// through the disassembler. gdb's -a option rejects a typeless symbol too, so
// this exercises the fallback rather than the fast path.
func TestDisassembleMinimalSymbolFunction(t *testing.T) {
	h := startReal(t, "hello")
	addFixtureNoDebug(t, h, "minsym")
	h.mustDo(wire.TypeExeLoad, wire.ExeLoadRequest{Path: "minsym"})
	// No -g means no line table, so there is no source line to break on.
	// starti is the documented way into a binary like this.
	h.mustDo(wire.TypeExecRun, wire.ExecRequest{StopAtEntry: true})
	h.rec.wait(t, wire.EventStopped)

	out := h.mustDo(wire.TypeDisasmFunction,
		wire.DisasmFunctionRequest{Address: "LogWrite"}).(wire.Disassembly)
	if len(out.Instructions) == 0 {
		t.Fatal("no instructions for LogWrite")
	}
}

// symbols.load exists because loading a program and loading its symbols are
// different acts. Against a target already running the code, only the second
// applies — and declaring an exec file would leave the UI offering to Run a
// second, local copy.

func TestSymbolsLoadReplaceSetsNoExecFile(t *testing.T) {
	h := startReal(t, "hello")
	out := h.mustDo(wire.TypeSymbolsLoad, wire.SymbolsLoadRequest{
		Path: "hello", Mode: wire.SymbolsReplace,
	}).(wire.SymbolsLoaded)
	if out.Available == 0 {
		t.Fatal("no symbols after loading a -g binary")
	}

	syms := h.mustDo(wire.TypeSymbolsList, wire.SymbolsListRequest{}).(wire.SymbolsList)
	if _, ok := findSymbol(syms.Symbols, "main"); !ok {
		t.Error("main is missing; symbols were not installed")
	}

	// The point of the exercise: no program was declared, so there is nothing
	// to run. exe.load would have set one.
	if snap := h.sess.Snapshot(); snap.ExePath != "" {
		t.Errorf("ExePath = %q; symbols.load must not declare a program to run",
			snap.ExePath)
	}
	if werr := runShouldFail(h); werr == nil {
		t.Error("exec.run succeeded after symbols.load; an exec file was set")
	}
}

func runShouldFail(h *harness) *wire.Error {
	_, werr := h.do(wire.TypeExecRun, wire.ExecRequest{})
	return werr
}

// The bare-metal case: an image that does not run where it was linked. Every
// address has to be biased, or the symbols point at the wrong code and the
// debugger lies confidently.
func TestSymbolsLoadAddAppliesOffset(t *testing.T) {
	h := startReal(t, "hello")

	base := h.mustDo(wire.TypeSymbolsLoad, wire.SymbolsLoadRequest{
		Path: "hello", Mode: wire.SymbolsReplace,
	}).(wire.SymbolsLoaded)
	if base.Available == 0 {
		t.Fatal("no symbols")
	}
	unbiased := h.mustDo(wire.TypeSymbolsList,
		wire.SymbolsListRequest{Filter: "_start", Limit: 5000}).(wire.SymbolsList)
	before, ok := findSymbol(unbiased.Symbols, "_start")
	if !ok {
		t.Skip("no _start in this toolchain's output")
	}
	linkAddr, err := strconv.ParseUint(strings.TrimPrefix(before.Address, "0x"), 16, 64)
	if err != nil {
		t.Fatalf("parsing %q: %v", before.Address, err)
	}

	const bias = 0x80000000
	h.mustDo(wire.TypeSymbolsLoad, wire.SymbolsLoadRequest{
		Path: "hello", Mode: wire.SymbolsAdd, Offset: "0x80000000",
	})

	biased := h.mustDo(wire.TypeSymbolsList,
		wire.SymbolsListRequest{Filter: "_start", Limit: 5000}).(wire.SymbolsList)
	var found bool
	for _, s := range biased.Symbols {
		if s.Name != "_start" || s.Address == "" {
			continue
		}
		got, err := strconv.ParseUint(strings.TrimPrefix(s.Address, "0x"), 16, 64)
		if err != nil {
			continue
		}
		if got == linkAddr+bias {
			found = true
		}
	}
	if !found {
		t.Errorf("no _start at the biased address 0x%x; -o was not applied",
			linkAddr+bias)
	}
}

// Containment still applies: a symbol file is opened through the project root
// like every other path in the protocol.
func TestSymbolsLoadRefusesEscapingPath(t *testing.T) {
	h := startReal(t, "hello")
	for _, p := range []string{"../../etc/hostname", "/etc/hostname"} {
		if _, werr := h.do(wire.TypeSymbolsLoad, wire.SymbolsLoadRequest{Path: p}); werr == nil {
			t.Errorf("symbols.load accepted %q, which is outside the project", p)
		}
	}
}

func TestSymbolsLoadRejectsNonELF(t *testing.T) {
	h := startReal(t, "hello")
	if _, werr := h.do(wire.TypeSymbolsLoad,
		wire.SymbolsLoadRequest{Path: "hello.c"}); werr == nil {
		t.Error("symbols.load accepted a C source file as a symbol table")
	}
}

func TestSymbolsLoadRejectsBadMode(t *testing.T) {
	h := startReal(t, "hello")
	if _, werr := h.do(wire.TypeSymbolsLoad,
		wire.SymbolsLoadRequest{Path: "hello", Mode: "sideways"}); werr == nil {
		t.Error("symbols.load accepted an unknown mode")
	}
}
