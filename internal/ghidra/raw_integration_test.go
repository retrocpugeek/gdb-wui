//go:build integration

package ghidra_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/retrocpugeek/gdb-wui/internal/ghidra"
)

// An image with no format at all: no ELF header, no sections, no symbols, no
// entry point. A kernel Image carved out of firmware and booted by an emulator
// is the case — gdb will not take such a file ("not in executable format"), so
// the decompiler has to be told about it separately, and told the two things
// the bytes cannot say for themselves: what processor they are for and where
// they are loaded.

// rawBlob compiles a couple of functions and strips everything that is not the
// code, leaving the first function at offset zero.
//
// Self-contained functions on purpose: this is an unlinked object's .text, so
// a call between them would carry an unapplied relocation and disassemble into
// a jump to nowhere.
func rawBlob(t *testing.T) string {
	t.Helper()
	cc, err := exec.LookPath("gcc")
	if err != nil {
		t.Skip("gcc is not installed")
	}
	objcopy, err := exec.LookPath("objcopy")
	if err != nil {
		t.Skip("objcopy is not installed")
	}
	dir := t.TempDir()
	src := filepath.Join(dir, "blob.c")
	const body = `
int accumulate(int n) {
	int total = 0;
	for (int i = 0; i < n; i++) total += i * 3;
	return total;
}
int scale(int n) {
	int out = n;
	for (int i = 0; i < 4; i++) out = out * 2 + 1;
	return out;
}
`
	if err := os.WriteFile(src, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	obj := filepath.Join(dir, "blob.o")
	if out, err := exec.Command(cc, "-O0", "-ffreestanding", "-c", "-o", obj, src).
		CombinedOutput(); err != nil {
		t.Fatalf("gcc: %v\n%s", err, out)
	}
	// The first function has to be at offset zero for the base address to be
	// its address, which is what the whole test turns on.
	if at := symbolOffset(t, obj, "accumulate"); at != 0 {
		t.Skipf("gcc put accumulate at %#x of .text, not the start", at)
	}
	blob := filepath.Join(dir, "blob.bin")
	if out, err := exec.Command(objcopy, "-O", "binary", "--only-section=.text", obj, blob).
		CombinedOutput(); err != nil {
		t.Fatalf("objcopy: %v\n%s", err, out)
	}
	info, err := os.Stat(blob)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() == 0 {
		t.Fatal("objcopy produced an empty blob")
	}
	if elfHeader(t, blob) {
		t.Fatal("the blob still looks like an ELF; this tests nothing")
	}
	return blob
}

// symbolOffset is where a symbol sits inside its section, per nm.
func symbolOffset(t *testing.T, obj, want string) uint64 {
	t.Helper()
	if _, err := exec.LookPath("nm"); err != nil {
		t.Skip("nm is not installed")
	}
	out, err := exec.Command("nm", "--defined-only", obj).Output()
	if err != nil {
		t.Fatalf("nm: %v", err)
	}
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) == 3 && f[2] == want {
			var n uint64
			for _, c := range strings.ToLower(f[0]) {
				n = n*16 + uint64(strings.IndexRune("0123456789abcdef", c))
			}
			return n
		}
	}
	t.Fatalf("nm does not report %s", want)
	return 0
}

func elfHeader(t *testing.T, path string) bool {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return len(body) >= 4 && string(body[:4]) == "\x7fELF"
}

// The base a raw image is imported at. Arbitrary, which is the point: the
// bytes do not know, so whatever is passed becomes the truth.
const rawBase = "0x40000000"

const rawProcessor = "x86:LE:64:default"

// TestRawImageIsSeededAtItsBase is the one that has to hold for an emulated
// kernel to be readable at all.
//
// With no analysis nothing else creates a function — measured on a 12 MB ARM
// kernel Image, which imported that way holds exactly zero — so a function at
// the base is SeedEntry's doing and nothing else's. That address matters more
// than any other: it is the only one the user stated, and for a kernel Image
// it is where execution begins.
func TestRawImageIsSeededAtItsBase(t *testing.T) {
	blob := rawBlob(t)
	c := startOn(t, blob, ghidra.ImportOptions{
		Analysis:  ghidra.AnalysisNone,
		Processor: rawProcessor,
		Base:      rawBase,
	})
	if got := c.Ready().Program.LanguageID; got != rawProcessor {
		t.Errorf("language %s, want %s — -processor did not take", got, rawProcessor)
	}
	fn, err := c.Decompile(context.Background(), rawBase)
	if err != nil {
		t.Fatalf("decompiling the base of a raw image: %v", err)
	}
	if want := "FUN_40000000"; fn.Name != want {
		t.Errorf("the function at %s is %s, want %s", rawBase, fn.Name, want)
	}
	if len(fn.Lines) < 4 {
		t.Errorf("%s decompiled to %d lines, which is not a function body",
			fn.Name, len(fn.Lines))
	}
}

// TestRawImageAnalysesFromItsBase is the same image imported the way auto
// imports a small one, and the check that the loader options do not disturb
// anything downstream: the analysis runs, finds functions, and they sit at the
// addresses the base put them at rather than at zero.
func TestRawImageAnalysesFromItsBase(t *testing.T) {
	blob := rawBlob(t)
	c := startOn(t, blob, ghidra.ImportOptions{
		Analysis:  ghidra.AnalysisFull,
		Processor: rawProcessor,
		Base:      rawBase,
	})
	ctx := context.Background()
	list, err := c.Functions(ctx, 0, 50, "")
	if err != nil {
		t.Fatalf("Functions: %v", err)
	}
	if len(list.Functions) == 0 {
		t.Fatal("no functions in an analysed raw image")
	}
	for _, f := range list.Functions {
		if !strings.HasPrefix(f.Entry, "0x4000") {
			t.Errorf("%s is at %s, which is not inside the image based at %s",
				f.Name, f.Entry, rawBase)
		}
	}
	if _, err := c.Decompile(ctx, rawBase); err != nil {
		t.Errorf("decompiling the base of an analysed raw image: %v", err)
	}
}
