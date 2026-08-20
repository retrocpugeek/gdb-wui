//go:build integration

package ghidra_test

import (
	"context"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/retrocpugeek/gdb-wui/internal/ghidra"
)

// A program imported with no analysis at all. The mode a kernel gets: Ghidra's
// auto-analysis cannot finish on 6.9 MB of code, so gdb-wui imports the image
// as it stands and disassembles each function as it is opened.
//
// Two things have to survive that, and neither announces itself when it stops
// working. A program counter has to find its function, when nothing has given
// any function a body beyond its entry point; and the decompiled function has
// to carry a stack depth, because the frame rule turns a Ghidra stack offset
// into an address with it and a nil one shows every local as no value at all.
// The decompiled C, which is what a reader would check, looks perfect either
// way.

func startUnanalysed(t *testing.T) (*ghidra.Client, string) {
	t.Helper()
	in := install(t)

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	t.Cleanup(cancel)

	projectDir := t.TempDir()
	bin := fixture(t)
	logf := func(f string, a ...any) { t.Logf(f, a...) }

	started := time.Now()
	if err := ghidra.Import(ctx, in, projectDir, "itest", bin,
		ghidra.ImportOptions{Analysis: ghidra.AnalysisNone}, logf); err != nil {
		t.Fatalf("Import: %v", err)
	}
	t.Logf("imported without analysis in %v", time.Since(started).Round(time.Millisecond))

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

func startUnanalysedWritable(t *testing.T) *ghidra.Client {
	t.Helper()
	in := install(t)

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	t.Cleanup(cancel)

	projectDir := t.TempDir()
	bin := fixture(t)
	logf := func(f string, a ...any) { t.Logf(f, a...) }
	if err := ghidra.Import(ctx, in, projectDir, "itest", bin,
		ghidra.ImportOptions{Analysis: ghidra.AnalysisNone}, logf); err != nil {
		t.Fatalf("Import: %v", err)
	}
	c, err := ghidra.Start(ctx, ghidra.Options{
		Install:     in,
		ProjectDir:  projectDir,
		ProjectName: "itest",
		Program:     filepath.Base(bin),
		Writable:    true,
		Timeout:     4 * time.Minute,
		Logf:        logf,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// TestUnanalysedProgramStillHasItsFunctions: the ELF symbol table is what the
// function list is made of, and the loader reads it whether or not anything
// analyses afterwards. If this is empty the mode is useless — there would be
// nothing to open.
func TestUnanalysedProgramStillHasItsFunctions(t *testing.T) {
	c, _ := startUnanalysed(t)

	if n := c.Ready().FunctionCount; n == 0 {
		t.Fatal("functionCount = 0 on an unanalysed program; the symbol table went unread")
	}
	list, err := c.Functions(context.Background(), 0, 5000, "accumulate")
	if err != nil {
		t.Fatalf("Functions: %v", err)
	}
	if len(list.Functions) == 0 {
		t.Fatal("accumulate is not in the function list")
	}
}

// TestDecompileWithoutAnalysisFromAnInteriorAddress is the load-bearing one.
//
// The address is deliberately not the entry point, and deliberately the first
// thing asked of this function: with no analysis every body is one byte long,
// so getFunctionContaining answers nothing and the sidecar has to fall back to
// the greatest entry at or below the address. Ask for the entry instead, or ask
// twice, and the fallback is never exercised.
func TestDecompileWithoutAnalysisFromAnInteriorAddress(t *testing.T) {
	c, _ := startUnanalysed(t)
	ctx := context.Background()

	list, err := c.Functions(ctx, 0, 5000, "accumulate")
	if err != nil {
		t.Fatalf("Functions: %v", err)
	}
	if len(list.Functions) == 0 {
		t.Fatal("accumulate is not in the function list")
	}
	entry := list.Functions[0].Entry
	inside, err := plus(entry, 4)
	if err != nil {
		t.Fatalf("%v", err)
	}

	started := time.Now()
	fn, err := c.Decompile(ctx, inside)
	if err != nil {
		t.Fatalf("Decompile at %s, four bytes into accumulate: %v", inside, err)
	}
	t.Logf("first decompile of a never-disassembled function: %v",
		time.Since(started).Round(time.Millisecond))

	if fn.Name != "accumulate" {
		t.Errorf("decompiling %s gave %q, want accumulate", inside, fn.Name)
	}
	if !strings.Contains(fn.Text, "accumulate") {
		t.Errorf("the decompiled text does not mention the function:\n%s", fn.Text)
	}
	if fn.Frame.SPDepth == nil {
		t.Error("no spDepth on an unanalysed program: the function was decompiled " +
			"without being disassembled first, so CallDepthChangeInfo had no " +
			"instructions to walk. Every stack local would show no value, and the " +
			"decompiled C would look perfectly fine")
	}
	if len(fn.Variables) == 0 {
		t.Error("the function has no variables at all")
	}
}

// TestUnanalysedAgreesWithAnalysed pins the two modes together on the numbers
// the frame rule uses. The unanalysed program is missing cross-references and
// recovered signatures, which is the deal; where the stack pointer sits is not
// something it is allowed to be vaguer about.
func TestUnanalysedAgreesWithAnalysed(t *testing.T) {
	ctx := context.Background()

	lazy, _ := startUnanalysed(t)
	full := start(t)

	a, err := lazy.Decompile(ctx, "accumulate")
	if err != nil {
		t.Fatalf("Decompile without analysis: %v", err)
	}
	b, err := full.Decompile(ctx, "accumulate")
	if err != nil {
		t.Fatalf("Decompile with analysis: %v", err)
	}

	if a.Frame.SPDepth == nil || b.Frame.SPDepth == nil {
		t.Fatalf("spDepth unanalysed=%v analysed=%v; both modes need one",
			deref(a.Frame.SPDepth), deref(b.Frame.SPDepth))
	}
	if *a.Frame.SPDepth != *b.Frame.SPDepth {
		t.Errorf("spDepth = %d without analysis and %d with it; the frame rule "+
			"would put stack variables in two different places",
			*a.Frame.SPDepth, *b.Frame.SPDepth)
	}
	if a.Entry != b.Entry {
		t.Errorf("entry = %s without analysis and %s with it", a.Entry, b.Entry)
	}
}

// TestEditsWorkWithoutAnalysis: an edit opens a transaction and then decompiles
// inside it, so on an unanalysed program the lazy disassembly starts a
// transaction while one is already open. Nested transactions are a thing
// Ghidra supports rather than a thing this code should assume, and a rename
// that leaves the program locked or half-committed would show up here.
func TestEditsWorkWithoutAnalysis(t *testing.T) {
	c := startUnanalysedWritable(t)
	ctx := context.Background()

	fn, err := c.Decompile(ctx, "accumulate")
	if err != nil {
		t.Fatalf("Decompile: %v", err)
	}
	if len(fn.Variables) == 0 {
		t.Fatal("no variables to rename")
	}
	v := fn.Variables[0]

	res, err := c.Rename(ctx, ghidra.Edit{
		Kind:     ghidra.EditVariable,
		Function: fn.Entry,
		Symbol:   v.ID,
		Name:     v.Name,
		Value:    "running_total",
	})
	if err != nil {
		t.Fatalf("Rename on an unanalysed program: %v", err)
	}
	if !hasVar(res.Function.Variables, "running_total") {
		t.Fatalf("no running_total among %v", names(res.Function.Variables))
	}
	// The re-decompile that comes back with the edit has to be as complete as
	// the one before it. A dropped inner transaction would take the
	// disassembly with it.
	if res.Function.Frame.SPDepth == nil {
		t.Error("the edit's re-decompile lost the stack depth")
	}
}

func plus(addr string, n int64) (string, error) {
	v, err := strconv.ParseUint(strings.TrimPrefix(addr, "0x"), 16, 64)
	if err != nil {
		return "", fmt.Errorf("parsing address %q: %w", addr, err)
	}
	return "0x" + strconv.FormatUint(v+uint64(n), 16), nil
}

func deref(p *int) any {
	if p == nil {
		return "nil"
	}
	return *p
}
