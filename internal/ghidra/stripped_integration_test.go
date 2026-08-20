//go:build integration

package ghidra_test

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/retrocpugeek/gdb-wui/internal/ghidra"
)

// An image with no symbol table. The kernel case once someone has run strip
// over it, and the one place the lazy import from AnalysisNone has nothing to
// work with: the ELF loader creates a function per symbol, so with no symbols
// it creates none, and every program counter resolves to nothing.
//
// Two ways back, and both are tested here against a binary this test strips
// itself. Ghidra can find the functions, and then they are called FUN_ and an
// address; or something else can say what they are called, which for a kernel
// is its own kallsyms table, still inside the image because stripping an ELF
// does not touch the data a kernel keeps for its oops traces.

// pair compiles one program twice over: as it was built, and stripped. Both
// halves are needed, because the way back from a stripped image is a symbol
// list from somewhere else, and the unstripped twin is where this test gets
// one.
//
// -no-pie so that the addresses nm prints are the addresses Ghidra loads at. A
// position-independent executable's symbols are offsets from a base chosen at
// load time, and feeding those to Ghidra names eleven addresses that are not
// in the image. A kernel is not position-independent, which is why the case
// this is modelled on does not have the problem.
func pair(t *testing.T) (unstripped, stripped string) {
	t.Helper()
	cc, err := exec.LookPath("gcc")
	if err != nil {
		t.Skip("gcc is not installed")
	}
	if _, err := exec.LookPath("strip"); err != nil {
		t.Skip("strip is not installed")
	}
	dir := t.TempDir()
	src := filepath.Join(dir, "demo.c")
	const body = `
#include <stdio.h>
int accumulate(int n) {
	int total = 0;
	for (int i = 0; i < n; i++) total += i * 3;
	return total;
}
int main(void) { printf("%d\n", accumulate(7)); return 0; }
`
	if err := os.WriteFile(src, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	unstripped = filepath.Join(dir, "demo")
	out, err := exec.Command(cc, "-O0", "-no-pie", "-o", unstripped, src).CombinedOutput()
	if err != nil {
		t.Fatalf("gcc: %v\n%s", err, out)
	}
	stripped = filepath.Join(dir, "demo-stripped")
	if out, err := exec.Command("cp", unstripped, stripped).CombinedOutput(); err != nil {
		t.Fatalf("cp: %v\n%s", err, out)
	}
	if out, err := exec.Command("strip", stripped).CombinedOutput(); err != nil {
		t.Fatalf("strip: %v\n%s", err, out)
	}
	if !ghidra.HasFunctionSymbols(unstripped) {
		t.Fatal("the unstripped half has no function symbols")
	}
	if ghidra.HasFunctionSymbols(stripped) {
		t.Fatal("strip left function symbols behind; this tests nothing")
	}
	return unstripped, stripped
}

// symbolAddress is where a function lives, according to the unstripped twin.
func symbolAddress(t *testing.T, bin, want string) string {
	t.Helper()
	if _, err := exec.LookPath("nm"); err != nil {
		t.Skip("nm is not installed")
	}
	out, err := exec.Command("nm", "--defined-only", bin).Output()
	if err != nil {
		t.Fatalf("nm: %v", err)
	}
	for _, line := range strings.Split(string(out), "\n") {
		if f := strings.Fields(line); len(f) == 3 && f[2] == want {
			return "0x" + strings.TrimLeft(strings.ToLower(f[0]), "0")
		}
	}
	t.Fatalf("nm does not report %s", want)
	return ""
}

func startOn(t *testing.T, bin string, opts ghidra.ImportOptions) *ghidra.Client {
	t.Helper()
	in := install(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	t.Cleanup(cancel)

	projectDir := t.TempDir()
	logf := func(f string, a ...any) { t.Logf(f, a...) }
	started := time.Now()
	if err := ghidra.Import(ctx, in, projectDir, "itest", bin, opts, logf); err != nil {
		t.Fatalf("Import: %v", err)
	}
	t.Logf("imported (%s) in %v", opts.Analysis,
		time.Since(started).Round(time.Millisecond))

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
	return c
}

// TestStrippedWithoutAnalysisIsEmpty is the reason the other two exist. Not a
// bug being pinned but a limit: AnalysisNone reads the symbol table, and a
// stripped image has none, so the mode that rescues a large kernel produces
// nothing at all for a stripped one.
func TestStrippedWithoutAnalysisIsEmpty(t *testing.T) {
	unstripped, stripped := pair(t)
	at := symbolAddress(t, unstripped, "accumulate")
	c := startOn(t, stripped, ghidra.ImportOptions{Analysis: ghidra.AnalysisNone})
	ctx := context.Background()

	// A handful of PLT thunks and the entry point are the loader's own doing
	// and are there whatever happens. What is not there is any of the
	// program's own code, by name or by address — and the address is the one
	// that matters, because it is what a program counter is.
	if _, err := c.Decompile(ctx, "accumulate"); err == nil {
		t.Error("accumulate decompiled on a stripped program imported with no " +
			"analysis; nothing should know that name")
	}
	if _, err := c.Decompile(ctx, at); err == nil {
		t.Errorf("%s decompiled, so something found a function there without "+
			"either a symbol table or an analysis", at)
	}
	list, err := c.Functions(ctx, 0, 5000, "")
	if err != nil {
		t.Fatalf("Functions: %v", err)
	}
	t.Logf("a stripped no-analysis import has %d functions, all of them the "+
		"loader's", len(list.Functions))
}

// TestLeanAnalysisFindsFunctionsInAStrippedImage: the analyzers are the only
// thing that can find them, and the full set is what does not fit on a large
// image. What survives the trimming has to still find functions.
func TestLeanAnalysisFindsFunctionsInAStrippedImage(t *testing.T) {
	unstripped, stripped := pair(t)
	at := symbolAddress(t, unstripped, "accumulate")
	c := startOn(t, stripped, ghidra.ImportOptions{Analysis: ghidra.AnalysisLean})
	ctx := context.Background()

	// The address the symbol table would have given, found without one.
	fnAt, err := c.Decompile(ctx, at)
	if err != nil {
		t.Fatalf("Decompile %s after a lean analysis: %v — the analyzers that "+
			"find functions did not find this one", at, err)
	}
	if fnAt.Entry != at {
		t.Errorf("found a function at %s covering %s, want one starting there",
			fnAt.Entry, at)
	}

	if n := c.Ready().FunctionCount; n < 5 {
		t.Fatalf("functionCount = %d after a lean analysis of a stripped binary; "+
			"the analyzers that find functions are not running", n)
	}
	list, err := c.Functions(ctx, 0, 5000, "")
	if err != nil {
		t.Fatalf("Functions: %v", err)
	}
	// main is called from the ELF entry point, so a run that discovers
	// anything discovers this. Its name is gone with the symbol table, so it
	// is addressed by what the analysis calls it.
	var named int
	for _, f := range list.Functions {
		if strings.HasPrefix(f.Name, "FUN_") {
			named++
		}
	}
	if named == 0 {
		t.Errorf("no FUN_ functions among %d; a stripped image has no names to "+
			"recover, so the discovered ones should be named after their addresses",
			len(list.Functions))
	}
	// And they decompile, which is the point of finding them.
	fn, err := c.Decompile(ctx, list.Functions[0].Entry)
	if err != nil {
		t.Fatalf("Decompile %s: %v", list.Functions[0].Entry, err)
	}
	if fn.Text == "" {
		t.Error("the decompiled text is empty")
	}
}

// TestSymbolsNameAStrippedImage is the kallsyms route, in miniature: take the
// names from somewhere else and put them back. The file is written in the
// format /proc/kallsyms uses, from the symbols of the unstripped build, which
// is exactly what a kernel hands over at runtime.
func TestSymbolsNameAStrippedImage(t *testing.T) {
	unstripped, stripped := pair(t)
	syms := kallsymsFrom(t, unstripped)

	c := startOn(t, stripped, ghidra.ImportOptions{
		Analysis: ghidra.AnalysisNone,
		Symbols:  syms,
	})
	ctx := context.Background()

	fn, err := c.Decompile(ctx, "accumulate")
	if err != nil {
		t.Fatalf("Decompile accumulate on a stripped image named from a symbol "+
			"file: %v", err)
	}
	if fn.Name != "accumulate" {
		t.Errorf("name = %q, want accumulate", fn.Name)
	}
	// The injected function has a one-byte body, the same shape the ELF loader
	// leaves. If the lazy disassembly does not treat it the same way, the
	// stack depth is the thing that quietly goes missing.
	if fn.Frame.SPDepth == nil {
		t.Error("no spDepth on a function created from a symbol file; the injected " +
			"function is not taking the same path as a symbol-table one")
	}

	// And an interior address, which is what a program counter is.
	inside, err := plus(fn.Entry, 4)
	if err != nil {
		t.Fatal(err)
	}
	again, err := c.Decompile(ctx, inside)
	if err != nil {
		t.Fatalf("Decompile at %s: %v", inside, err)
	}
	if again.Name != "accumulate" {
		t.Errorf("decompiling %s gave %q, want accumulate", inside, again.Name)
	}
}

// kallsymsFrom writes the function symbols of a binary in /proc/kallsyms's
// format: lowercase hex address, one-letter type, name.
func kallsymsFrom(t *testing.T, bin string) string {
	t.Helper()
	if _, err := exec.LookPath("nm"); err != nil {
		t.Skip("nm is not installed")
	}
	out, err := exec.Command("nm", "--defined-only", bin).Output()
	if err != nil {
		t.Fatalf("nm: %v", err)
	}
	path := filepath.Join(t.TempDir(), "kallsyms")
	var b strings.Builder
	var n int
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) != 3 || len(f[1]) != 1 {
			continue
		}
		b.WriteString(fmt.Sprintf("%s %s %s\n", strings.ToLower(f[0]), f[1], f[2]))
		n++
	}
	if n == 0 {
		t.Fatal("nm produced no symbols to write")
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Logf("wrote %d symbols in kallsyms format", n)
	return path
}
