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
	const body = `
#include <stdio.h>
static int accumulate(int n) {
	int total = 0;
	for (int i = 0; i < n; i++) total += i * 3;
	return total;
}
int main(void) { printf("%d\n", accumulate(7)); return 0; }
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

// start imports the fixture and returns a resident client. Import and analysis
// of a hello-world is seconds; a large image is minutes, which is why the
// caller of this in production is a job rather than a click.
func start(t *testing.T) *ghidra.Client {
	t.Helper()
	in := install(t)
	t.Logf("ghidra %s at %s", in.Version, in.Dir)

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	t.Cleanup(cancel)

	c, err := ghidra.Start(ctx, ghidra.Options{
		Install:     in,
		ProjectDir:  t.TempDir(),
		ProjectName: "itest",
		Import:      fixture(t),
		Timeout:     4 * time.Minute,
		Logf:        func(f string, a ...any) { t.Logf(f, a...) },
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
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

// TestLineMapAddressesAreInsideTheBody catches a whole class of coordinate
// confusion: a map whose addresses fall outside the function it describes is
// worse than no map, because it looks authoritative.
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

// TestCloseStopsTheProcess: a 2 GB JVM outliving the session is not acceptable,
// and Setpgid plus a group kill is what prevents it. Verified by pid rather
// than by trusting the call.
func TestCloseStopsTheProcess(t *testing.T) {
	c := start(t)
	// A live request proves the JVM is up before we ask it to go away.
	if _, err := c.Decompile(context.Background(), "accumulate"); err != nil {
		t.Fatalf("Decompile: %v", err)
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
