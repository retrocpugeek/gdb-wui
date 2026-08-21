//go:build integration

package debugger_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/retrocpugeek/gdb-wui/internal/debugger"
	"github.com/retrocpugeek/gdb-wui/internal/wire"
)

// The target gdb cannot take a file for.
//
// An emulator booting a kernel Image has no ELF anywhere: `file` on the image
// answers "data", and gdb answers "not in executable format: file format not
// recognized". Nothing gdb knows names a binary, so the decompiler has to be
// told which file to reverse, what its bytes are and where they are loaded —
// and the three of those together are the only mapping there is between the
// addresses the debugger reports and the addresses Ghidra holds.

// rawImage is a couple of functions with everything but the code taken off,
// which is what a kernel Image is. Self-contained functions, because this is
// an unlinked object's .text and a call between them would carry an unapplied
// relocation.
func rawImage(t *testing.T, dir string) string {
	t.Helper()
	cc, err := exec.LookPath("gcc")
	if err != nil {
		t.Skip("gcc is not installed")
	}
	objcopy, err := exec.LookPath("objcopy")
	if err != nil {
		t.Skip("objcopy is not installed")
	}
	src := filepath.Join(dir, "image.c")
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
	obj := filepath.Join(dir, "image.o")
	if out, err := exec.Command(cc, "-O0", "-ffreestanding", "-c", "-o", obj, src).
		CombinedOutput(); err != nil {
		t.Fatalf("gcc: %v\n%s", err, out)
	}
	img := filepath.Join(dir, "vmlinux.img")
	if out, err := exec.Command(objcopy, "-O", "binary", "--only-section=.text", obj, img).
		CombinedOutput(); err != nil {
		t.Fatalf("objcopy: %v\n%s", err, out)
	}
	body2, err := os.ReadFile(img)
	if err != nil {
		t.Fatal(err)
	}
	if len(body2) == 0 || string(body2[:4]) == "\x7fELF" {
		t.Fatal("that is not a raw image; this tests nothing")
	}
	return img
}

// TestGDBWillNotTakeARawImage is why the rest of this file exists. If gdb ever
// learns to load one, the flags below stop being the only way in.
func TestGDBWillNotTakeARawImage(t *testing.T) {
	k := decompHarness(t)
	// Inside the project, because exe.load takes a path relative to it and
	// "gdb cannot find it" would prove nothing.
	img := rawImage(t, k.dir)
	_, werr := k.try(wire.TypeExeLoad,
		wire.ExeLoadRequest{Path: filepath.Base(img)})
	if werr == nil {
		t.Fatalf("gdb loaded %s as a program", filepath.Base(img))
	}
	msg := strings.ToLower(werr.Message)
	if !strings.Contains(msg, "elf") && !strings.Contains(msg, "format") {
		t.Errorf("refused for an unexpected reason: %s", werr.Message)
	}
}

// TestRawImageDecompilesWithNoProgramInGDB is the emulated-kernel case end to
// end: gdb has nothing loaded, and the decompiler still comes up on the file
// it was pointed at, at the addresses it was told to put it.
func TestRawImageDecompilesWithNoProgramInGDB(t *testing.T) {
	img := rawImage(t, t.TempDir())
	const (
		base      = "0x40000000"
		processor = "x86:LE:64:default"
	)
	k := decompHarnessWith(t, func(cfg *debugger.DecompConfig) {
		cfg.Binary = img
		cfg.Processor = processor
		cfg.Base = base
	})
	do := k.do

	st := waitReady(t, do)
	if st.Program == nil {
		t.Fatal("ready with no program")
	}
	if st.Program.LanguageID != processor {
		t.Errorf("language %s, want %s", st.Program.LanguageID, processor)
	}
	// Nothing was loaded into gdb, so there is no build to disagree with.
	if st.Mismatch != "" {
		t.Errorf("warned about a mismatch with nothing: %s", st.Mismatch)
	}

	fn := do(wire.TypeDecompFunction,
		wire.DecompFunctionRequest{Target: base}).(wire.DecompFunction)
	if want := "FUN_40000000"; fn.Name != want {
		t.Errorf("the function at %s is %s, want %s", base, fn.Name, want)
	}
	// The bias is what would silently ruin this: with no ELF to anchor on and
	// no symbol shared with gdb, the only correct answer is zero, and the base
	// given at startup is what makes zero correct.
	if fn.Bias != 0 {
		t.Errorf("bias %d for a raw image; the base is the mapping", fn.Bias)
	}
	if fn.Entry != base {
		t.Errorf("entry %s, want %s", fn.Entry, base)
	}
	if len(fn.Lines) < 4 {
		t.Errorf("%s decompiled to %d lines, which is not a function body",
			fn.Name, len(fn.Lines))
	}
}
