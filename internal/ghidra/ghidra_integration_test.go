//go:build integration

package ghidra_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/retrocpugeek/gdb-wui/internal/ghidra"
)

// Against a real Ghidra. The unit tests cover the protocol with a fake; what
// only a JVM can tell us is whether the embedded scripts actually compile,
// whether analyzeHeadless accepts the arguments we build, and whether the
// resident-server trick keeps working across Ghidra releases. Those are the
// three things that break silently when someone upgrades.

func install(t *testing.T) *ghidra.Install {
	t.Helper()
	in, err := ghidra.Locate("")
	if err != nil {
		t.Skipf("no Ghidra installation: %v", err)
	}
	return in
}

// fixture compiles a small C program to decompile.
func fixture(t *testing.T) string {
	t.Helper()
	cc, err := exec.LookPath("gcc")
	if err != nil {
		t.Skip("gcc is not installed")
	}
	dir := t.TempDir()
	src := filepath.Join(dir, "demo.c")
	// names is a table of pointers with no debug info to describe it, which is
	// what a global retype is for: Ghidra leaves the bytes undefined and
	// renders pick() as a hand-computed offset until something says what shape
	// they are.
	const body = `
#include <stdio.h>
static int accumulate(int n) {
	int total = 0;
	for (int i = 0; i < n; i++) total += i * 3;
	return total;
}
const char *names[] = { "one", "two", "three" };
const char *tail[] = { "four" };
const char *pick(int i) { return i ? names[i] : tail[0]; }
int main(void) { printf("%d %s\n", accumulate(7), pick(1)); return 0; }
`
	if err := os.WriteFile(src, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "demo")
	out, err := exec.Command(cc, "-O0", "-o", bin, src).CombinedOutput()
	if err != nil {
		t.Fatalf("gcc: %v\n%s", err, out)
	}
	return bin
}

// tableFixture compiles an optimised program whose table walk the decompiler
// has to invent a temporary for.
//
// Optimised on purpose: at -O0 every intermediate gets a stack slot, and a
// variable with real storage is not the one that strands. A name committed to a
// register or a frame offset survives a reshape, because the storage is still
// there to hang it on; a name committed for a decompiler temporary is keyed by
// the shape of the p-code, and a reshape leaves it addressing nothing.
func tableFixture(t *testing.T) string {
	t.Helper()
	cc, err := exec.LookPath("gcc")
	if err != nil {
		t.Skip("gcc is not installed")
	}
	dir := t.TempDir()
	src := filepath.Join(dir, "table.c")
	// The shape this is modelled on is busybox's applet installer: a nibble
	// packed two to a byte, indexing a table of directory strings.
	const body = `
#include <stdio.h>
static const unsigned char loc[4] = { 0x31, 0x42, 0x13, 0x24 };
const char *dirs[] = { "/", "/bin/", "/sbin/", "/usr/bin/", "/usr/sbin/" };
const char *dir_for(unsigned i) {
	unsigned n = (i & 1) ? (loc[i >> 1] >> 4) : (loc[i >> 1] & 0xf);
	return dirs[n];
}
int main(int argc, char **argv) { printf("%s\n", dir_for(argc)); return 0; }
`
	if err := os.WriteFile(src, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "table")
	out, err := exec.Command(cc, "-O2", "-o", bin, src).CombinedOutput()
	if err != nil {
		t.Fatalf("gcc: %v\n%s", err, out)
	}
	return bin
}

// start imports the fixture and returns a resident client. Import and analysis
// of a hello-world is seconds; a large image is minutes, which is why the
// caller of this in production is a job rather than a click.
func start(t *testing.T) *ghidra.Client {
	c, _ := startIn(t)
	return c
}

// startIn is start, also returning the project directory. That path is unique
// to this test and appears on the spawned JVM's command line, which is what
// lets a caller pick its own process out of a machine that may be running
// other Ghidras — another test's, or the developer's own gdb-wui.
func startIn(t *testing.T) (*ghidra.Client, string) {
	t.Helper()
	in := install(t)
	t.Logf("ghidra %s at %s", in.Version, in.Dir)

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	t.Cleanup(cancel)

	// Two phases, because they cannot be one: see ghidra.Import.
	projectDir := t.TempDir()
	bin := fixture(t)
	logf := func(f string, a ...any) { t.Logf(f, a...) }
	if err := ghidra.Import(ctx, in, projectDir, "itest", bin, ghidra.AnalysisFull, logf); err != nil {
		t.Fatalf("Import: %v", err)
	}
	c, err := ghidra.Start(ctx, ghidra.Options{
		Install:     in,
		ProjectDir:  projectDir,
		ProjectName: "itest",
		Program:     filepath.Base(bin),
		Timeout:     4 * time.Minute,
		Logf:        logf,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c, projectDir
}

// TestResidentServerAnswers is the load-bearing one. If the embedded scripts do
// not compile, or analyzeHeadless stops keeping the JVM alive for a blocking
// postScript, this is where it shows — and the whole design rests on it.
func TestResidentServerAnswers(t *testing.T) {
	c := start(t)

	ready := c.Ready()
	if ready.Schema != ghidra.Schema {
		t.Errorf("schema = %d, want %d", ready.Schema, ghidra.Schema)
	}
	if ready.Program.SHA256 == "" {
		t.Error("greeting carried no sha256; the mismatch guard needs it")
	}
	if !strings.HasPrefix(ready.Program.LanguageID, "x86") {
		t.Errorf("languageId = %q, want an x86 one", ready.Program.LanguageID)
	}
	if ready.FunctionCount == 0 {
		t.Error("functionCount = 0")
	}
}

// TestDecompileByNameAndAddress: both spellings must work, because a caller
// holding a program counter has an address and a caller clicking a symbol has
// a name, and neither should have to convert.
func TestDecompileByNameAndAddress(t *testing.T) {
	c := start(t)
	ctx := context.Background()

	byName, err := c.Decompile(ctx, "accumulate")
	if err != nil {
		t.Fatalf("Decompile by name: %v", err)
	}
	if byName.Text == "" || !strings.Contains(byName.Text, "return") {
		t.Errorf("text does not look like C:\n%s", byName.Text)
	}
	if len(byName.Lines) == 0 {
		t.Fatal("no line map; the whole feature rests on it")
	}

	byAddr, err := c.Decompile(ctx, byName.Entry)
	if err != nil {
		t.Fatalf("Decompile by address: %v", err)
	}
	if byAddr.Name != byName.Name {
		t.Errorf("by address gave %q, by name gave %q", byAddr.Name, byName.Name)
	}
}

// TestDecompileByInteriorAddress: a program counter is rarely a function entry.
func TestDecompileByInteriorAddress(t *testing.T) {
	c := start(t)
	ctx := context.Background()

	fn, err := c.Decompile(ctx, "accumulate")
	if err != nil {
		t.Fatalf("Decompile: %v", err)
	}
	// Any address the line map mentions is inside the body by construction.
	var interior string
	for _, l := range fn.Lines {
		if len(l.Addrs) > 0 && l.Addrs[0] != fn.Entry {
			interior = l.Addrs[0]
			break
		}
	}
	if interior == "" {
		t.Skip("no interior address in the line map for this build")
	}
	got, err := c.Decompile(ctx, interior)
	if err != nil {
		t.Fatalf("Decompile at interior %s: %v", interior, err)
	}
	if got.Name != fn.Name {
		t.Errorf("address %s resolved to %q, want %q", interior, got.Name, fn.Name)
	}
}

// TestLineMapAddressesAreInsideTheBody catches a class of coordinate confusion.
// A map whose addresses fall outside the function it describes still looks
// authoritative.
func TestLineMapAddressesAreInsideTheBody(t *testing.T) {
	c := start(t)
	fn, err := c.Decompile(context.Background(), "accumulate")
	if err != nil {
		t.Fatalf("Decompile: %v", err)
	}
	lo, hi := parseAddr(t, fn.BodyStart), parseAddr(t, fn.BodyEnd)
	for _, l := range fn.Lines {
		for _, a := range l.Addrs {
			v := parseAddr(t, a)
			if v < lo || v > hi {
				t.Errorf("line %d claims %s, outside the body %s..%s",
					l.N, a, fn.BodyStart, fn.BodyEnd)
			}
		}
	}
}

// TestUnknownFunctionIsAnErrorNotADisconnect. A UI will ask for things that do
// not exist; losing the resident process each time would mean paying startup
// again for every typo.
func TestUnknownFunctionIsAnErrorNotADisconnect(t *testing.T) {
	c := start(t)
	ctx := context.Background()

	if _, err := c.Decompile(ctx, "no_such_function_xyz"); err == nil {
		t.Fatal("expected an error")
	}
	if _, err := c.Decompile(ctx, "accumulate"); err != nil {
		t.Fatalf("connection did not survive a failed request: %v", err)
	}
}

func TestFunctionsListsWithoutDecompiling(t *testing.T) {
	c := start(t)
	list, err := c.Functions(context.Background(), 0, 500, "")
	if err != nil {
		t.Fatalf("Functions: %v", err)
	}
	if list.Total == 0 {
		t.Fatal("no functions listed")
	}
	var found bool
	for _, f := range list.Functions {
		if f.Name == "accumulate" {
			found = true
		}
	}
	if !found {
		t.Errorf("accumulate missing from %d listed functions", len(list.Functions))
	}

	filtered, err := c.Functions(context.Background(), 0, 500, "accumul")
	if err != nil {
		t.Fatalf("Functions filtered: %v", err)
	}
	if filtered.Total == 0 || filtered.Total >= list.Total {
		t.Errorf("filter did nothing: %d of %d", filtered.Total, list.Total)
	}
}

// TestDataListsGlobalsAndNotCode is the other half of the browsable index.
//
// The list has to be the program's module-scope data and nothing else. Ghidra's
// symbol table also holds a LAB_ for every jump target it found, and there are
// far more of those than there are globals; letting them in would bury the
// twenty names somebody is looking for under two thousand nobody is.
func TestDataListsGlobalsAndNotCode(t *testing.T) {
	c := start(t)
	list, err := c.Data(context.Background(), 0, 5000, "")
	if err != nil {
		t.Fatalf("Data: %v", err)
	}
	if list.Total == 0 {
		t.Fatal("no data symbols listed; the fixture defines names[] and tail[]")
	}

	byName := map[string]bool{}
	for _, d := range list.Data {
		byName[d.Name] = true
		if d.Address == "" {
			t.Errorf("%s has no address, which is the only thing a global is good for", d.Name)
		}
	}
	for _, want := range []string{"names", "tail"} {
		if !byName[want] {
			t.Errorf("%s missing from %d data symbols", want, len(list.Data))
		}
	}
	// A function is not data. It has its own list, and appearing in both would
	// show every function twice in a pane that merges them.
	if byName["accumulate"] || byName["pick"] {
		t.Error("a function was listed as data")
	}

	filtered, err := c.Data(context.Background(), 0, 5000, "tail")
	if err != nil {
		t.Fatalf("Data filtered: %v", err)
	}
	if filtered.Total == 0 || filtered.Total >= list.Total {
		t.Errorf("filter did nothing: %d of %d", filtered.Total, list.Total)
	}
}

// TestCloseStopsTheProcess: a 2 GB JVM outliving the session is not
// acceptable, and Setpgid plus a group kill is what prevents it.
//
// This counts JVMs rather than trusting the call. An earlier version of this
// test only checked that requests failed after Close, which they do the moment
// the socket shuts — it would have passed with the process still running, and
// a leaked JVM was in fact observed by hand while that version was green.
//
// It counts only the processes carrying this test's own project directory. An
// earlier version compared totals across every gdb-wui Ghidra on the machine,
// which made it a lie in both directions: a previous test's JVM still dying
// while this one started cancelled out to no change ("no JVM appeared"), and a
// developer's own session running alongside would have masked a real leak.
func TestCloseStopsTheProcess(t *testing.T) {
	c, projectDir := startIn(t)
	// A live request proves the JVM is up before we ask it to go away.
	if _, err := c.Decompile(context.Background(), "accumulate"); err != nil {
		t.Fatalf("Decompile: %v", err)
	}
	if n := countHeadlessJVMs(t, projectDir); n == 0 {
		t.Fatal("no JVM for this project; this test cannot prove anything")
	}
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// After Close every request must fail rather than block.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := c.Decompile(ctx, "accumulate"); err == nil {
		t.Error("a request succeeded after Close")
	} else if ctx.Err() != nil {
		t.Error("a request after Close blocked until the context expired")
	}

	// And the process itself is gone. A JVM takes a moment to die.
	deadline := time.Now().Add(20 * time.Second)
	for {
		if countHeadlessJVMs(t, projectDir) == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d Ghidra process(es) for %s outlived Close",
				countHeadlessJVMs(t, projectDir), projectDir)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// countHeadlessJVMs counts the Ghidra processes serving one project. Both the
// project directory and the script name are required: the directory alone also
// matches the import that ran before, and the script alone matches every other
// Ghidra on the machine.
func countHeadlessJVMs(t *testing.T, projectDir string) int {
	t.Helper()
	out, err := exec.Command("ps", "-eo", "args").Output()
	if err != nil {
		t.Skipf("cannot list processes: %v", err)
	}
	var n int
	for _, line := range strings.Split(string(out), "\n") {
		if strings.Contains(line, projectDir) && strings.Contains(line, "DecompServer") {
			n++
		}
	}
	return n
}

func parseAddr(t *testing.T, s string) uint64 {
	t.Helper()
	var v uint64
	s = strings.TrimPrefix(s, "0x")
	for _, r := range s {
		var d uint64
		switch {
		case r >= '0' && r <= '9':
			d = uint64(r - '0')
		case r >= 'a' && r <= 'f':
			d = uint64(r-'a') + 10
		case r >= 'A' && r <= 'F':
			d = uint64(r-'A') + 10
		default:
			t.Fatalf("unparseable address %q", s)
		}
		v = v*16 + d
	}
	return v
}
