//go:build integration

package debugger_test

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/retrocpugeek/gdb-wui/internal/debugger"
	"github.com/retrocpugeek/gdb-wui/internal/ghidra"
	"github.com/retrocpugeek/gdb-wui/internal/wire"
)

// TestDecompFromKuna decompiles with kuna beside a sidecar: the sidecar first,
// then kuna when chosen, and kuna alone for a program with no sidecar.
// KUNA_WASM names a native kuna_wasm and KUNA_SPECS its specs.
func TestDecompFromKuna(t *testing.T) {
	bin, specs := os.Getenv("KUNA_WASM"), os.Getenv("KUNA_SPECS")
	if bin == "" || specs == "" {
		t.Skip("KUNA_WASM and KUNA_SPECS are not set")
	}
	kit := decompHarnessWith(t, func(cfg *debugger.DecompConfig) {
		dir := filepath.Dir(cfg.CacheRoot)
		sidecars := filepath.Join(dir, "sidecars")
		if err := os.MkdirAll(sidecars, 0o755); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
		defer cancel()
		logf := func(f string, a ...any) { t.Logf(f, a...) }
		proj := t.TempDir()
		if err := ghidra.Import(ctx, cfg.Install, proj, "p", filepath.Join(dir, "demo"),
			ghidra.ImportOptions{Analysis: ghidra.AnalysisFull}, logf); err != nil {
			t.Fatalf("Import: %v", err)
		}
		if err := ghidra.ExportSidecar(ctx, cfg.Install, proj, "p", "demo",
			filepath.Join(sidecars, "demo.decomp.json"), logf); err != nil {
			t.Fatalf("ExportSidecar: %v", err)
		}
		*cfg = debugger.DecompConfig{Sidecars: sidecars, KunaWasm: bin, KunaSpecs: specs}
	})
	do, try := kit.do, kit.try

	do(wire.TypeExeLoad, wire.ExeLoadRequest{Path: "demo"})
	st := waitReady(t, do)
	if st.Decompiler != wire.DecompilerGhidra ||
		!slices.Equal(st.Decompilers, []string{wire.DecompilerGhidra, wire.DecompilerKuna}) {
		t.Fatalf("demo, with a sidecar: status %+v", st)
	}

	do(wire.TypeDecompUse, wire.DecompUseRequest{Decompiler: wire.DecompilerKuna})
	st = waitReady(t, do)
	if st.Decompiler != wire.DecompilerKuna || st.Editable || st.Program == nil || st.Program.Name != "demo" {
		t.Fatalf("demo, kuna: status %+v", st)
	}
	fn := do(wire.TypeDecompFunction, wire.DecompFunctionRequest{Target: "accumulate"}).(wire.DecompFunction)
	if fn.Name != "accumulate" || !strings.Contains(fn.Text, "accumulate(int") || len(fn.Lines) == 0 {
		t.Errorf("accumulate from kuna: %s %.300q", fn.Name, fn.Text)
	}
	if _, werr := try(wire.TypeDecompRename, wire.DecompEditRequest{Kind: wire.DecompEditFunction,
		Function: fn.Entry, Name: "accumulate", Value: "x"}); werr == nil {
		t.Error("kuna accepted a rename")
	}

	// No sidecar: kuna, the choice standing, and nothing else on offer.
	do(wire.TypeExeLoad, wire.ExeLoadRequest{Path: "stripped"})
	st = waitReady(t, do)
	if st.Decompiler != wire.DecompilerKuna || st.Program == nil || st.Program.Name != "stripped" ||
		len(st.Decompilers) != 0 {
		t.Fatalf("stripped: status %+v", st)
	}
	fn = do(wire.TypeDecompFunction, wire.DecompFunctionRequest{Target: "main"}).(wire.DecompFunction)
	if !strings.Contains(fn.Text, "printf") {
		t.Errorf("stripped main from kuna: %.300q", fn.Text)
	}
	if _, werr := try(wire.TypeDecompUse, wire.DecompUseRequest{Decompiler: wire.DecompilerGhidra}); werr == nil {
		t.Error("decomp.use offered Ghidra for a program it has no sidecar for")
	}

	// And back to the sidecar's program, and to Ghidra.
	do(wire.TypeExeLoad, wire.ExeLoadRequest{Path: "demo"})
	if st = waitReady(t, do); st.Decompiler != wire.DecompilerKuna {
		t.Fatalf("demo again: status %+v", st)
	}
	do(wire.TypeDecompUse, wire.DecompUseRequest{Decompiler: wire.DecompilerGhidra})
	if st = waitReady(t, do); st.Decompiler != wire.DecompilerGhidra || st.GhidraVersion == "" {
		t.Fatalf("demo, Ghidra again: status %+v", st)
	}
}
