package ghidra

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/retrocpugeek/gdb-wui/internal/testutil"
)

func TestDecideOnTheThreshold(t *testing.T) {
	for _, tc := range []struct {
		name    string
		bytes   int64
		symbols bool
		want    Analysis
	}{
		{"a hello world", 2 << 10, true, AnalysisFull},
		{"a 2 MB firmware image, which analyses in 71s", 2 << 20, true, AnalysisFull},
		{"exactly the limit", AutoAnalysisLimit, true, AnalysisFull},
		{"one byte over", AutoAnalysisLimit + 1, true, AnalysisNone},
		{"a MIPS64 kernel", 6_800_000, true, AnalysisNone},
		// Stripped, so nothing else knows where the functions are and the
		// analyzers are the only thing that can find them. Below the limit it
		// makes no difference: the full analysis finds them and more.
		{"a small stripped binary", 2 << 10, false, AnalysisFull},
		{"a stripped MIPS64 kernel", 6_800_000, false, AnalysisLean},
	} {
		got, why := decide(tc.bytes, tc.symbols)
		if got != tc.want {
			t.Errorf("%s (%d bytes, symbols=%v): decide = %s, want %s",
				tc.name, tc.bytes, tc.symbols, got, tc.want)
		}
		// The reason exists to be logged, so an empty one where a decision was
		// taken against the caller's expectation is a silent surprise.
		if got != AnalysisFull && why == "" {
			t.Errorf("%s: chose %s without saying why", tc.name, got)
		}
		if got == AnalysisFull && why != "" {
			t.Errorf("%s: explained a decision it did not take: %q", tc.name, why)
		}
	}
}

func TestStrippedIsDetected(t *testing.T) {
	bin := smallELF(t)
	if !HasFunctionSymbols(bin) {
		t.Error("a freshly compiled binary has no function symbols?")
	}
	stripped := filepath.Join(t.TempDir(), "stripped")
	body, err := os.ReadFile(bin)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stripped, body, 0o755); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("strip", stripped).CombinedOutput(); err != nil {
		t.Skipf("strip: %v\n%s", err, out)
	}
	if HasFunctionSymbols(stripped) {
		t.Error("a stripped binary still reports function symbols, so auto would " +
			"import it with no analysis and produce an empty program")
	}
	if mode, _ := AnalysisAuto.Resolve(stripped); mode != AnalysisFull {
		t.Errorf("auto chose %s for a small stripped binary, want %s", mode, AnalysisFull)
	}
}

// An explicit mode is the user's, and must not be second-guessed by measuring
// anything. Proved with a path that does not exist: if Resolve reads the file
// at all, it cannot answer.
func TestResolveDoesNotMeasureWhenTold(t *testing.T) {
	for _, mode := range []Analysis{AnalysisFull, AnalysisNone} {
		got, why := mode.Resolve(filepath.Join(t.TempDir(), "no-such-binary"))
		if got != mode {
			t.Errorf("%s.Resolve = %s, want it unchanged", mode, got)
		}
		if why != "" {
			t.Errorf("%s.Resolve explained itself (%q); only auto has anything to explain",
				mode, why)
		}
	}
}

// Idempotent, because Import resolves again after the debugger already has.
func TestResolveIsIdempotent(t *testing.T) {
	bin := smallELF(t)
	once, _ := AnalysisAuto.Resolve(bin)
	twice, _ := once.Resolve(bin)
	if once != twice {
		t.Errorf("resolving twice moved from %s to %s", once, twice)
	}
}

func TestCodeBytesCountsCodeOnly(t *testing.T) {
	bin := smallELF(t)
	n, err := CodeBytes(bin)
	if err != nil {
		t.Fatalf("CodeBytes: %v", err)
	}
	st, err := os.Stat(bin)
	if err != nil {
		t.Fatal(err)
	}
	if n <= 0 {
		t.Errorf("CodeBytes = %d, want the .text it certainly has", n)
	}
	// The point of measuring sections rather than the file: a binary carries
	// symbol tables, relocations and rodata that cost nothing to analyse.
	if n >= st.Size() {
		t.Errorf("CodeBytes = %d for a %d byte file; that is the whole file, "+
			"so something is counting more than the executable sections", n, st.Size())
	}
	if mode, _ := AnalysisAuto.Resolve(bin); mode != AnalysisFull {
		t.Errorf("auto chose %s for a hello world", mode)
	}
}

// Not an ELF is not an error the user should feel: Ghidra takes PE, Mach-O and
// raw images too, and the behaviour that predates this switch is to analyse.
func TestAutoFallsBackToAnalysingWhatItCannotMeasure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "not-an-elf")
	if err := os.WriteFile(path, []byte("MZ\x00\x00 this is not an ELF"), 0o644); err != nil {
		t.Fatal(err)
	}
	if mode, _ := AnalysisAuto.Resolve(path); mode != AnalysisFull {
		t.Errorf("auto chose %s for a file it cannot measure, want %s", mode, AnalysisFull)
	}
	if _, err := CodeBytes(path); err == nil {
		t.Error("CodeBytes accepted a file that is not an ELF")
	}
}

func TestAnalysisRejectsAMisspelling(t *testing.T) {
	var a Analysis
	if a.String() != string(AnalysisAuto) {
		t.Errorf("the zero value prints as %q, want %q", a.String(), AnalysisAuto)
	}
	if err := a.Set("none"); err != nil || a != AnalysisNone {
		t.Errorf("Set(none) = %v, leaving %q", err, a)
	}
	err := a.Set("noanalysis")
	if err == nil {
		t.Fatal("Set accepted 'noanalysis'; a misspelt mode must not run as auto")
	}
	for _, want := range []string{"auto", "full", "none"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not name %q: %v", want, err)
		}
	}
	if a != AnalysisNone {
		t.Errorf("a rejected Set changed the value to %q", a)
	}
}

// The heap was unreachable before this: Import builds the child's environment
// from an allowlist, and GHIDRA_MAXMEM was not on it, so exporting it did
// nothing at all and the 2 GB default could not be raised from outside.
func TestTheHeapCanBeRaisedFromOutside(t *testing.T) {
	if got := heapEnv(); len(got) != 0 {
		t.Fatalf("heapEnv = %v with nothing set, want nothing", got)
	}
	t.Setenv(EnvMaxMem, "8G")
	if got := heapEnv(); len(got) != 1 || got[0] != EnvMaxMem+"=8G" {
		t.Errorf("heapEnv = %v, want [%s=8G]", got, EnvMaxMem)
	}
	t.Setenv(EnvHeadlessMaxMem, "12G")
	if got := heapEnv(); len(got) != 2 {
		t.Errorf("heapEnv = %v, want both settings", got)
	}
}

// smallELF is any ELF this machine can produce. Not the test binary itself:
// a Go binary is megabytes of runtime, and small is the property under test.
func smallELF(t *testing.T) string {
	t.Helper()
	return testutil.Fixture(t, "hello")
}
