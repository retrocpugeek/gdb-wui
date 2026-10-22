//go:build integration

package debugger_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/retrocpugeek/gdb-wui/internal/debugger"
	"github.com/retrocpugeek/gdb-wui/internal/ghidra"
	"github.com/retrocpugeek/gdb-wui/internal/wire"
)

// Sidecars instead of a Ghidra: the decompilations of two of the harness's
// programs, written beforehand, and a session with no Ghidra configured at all.
// Loading a program shows its own sidecar; loading one with none says so;
// loading the first again goes back to it.
func TestDecompFromSidecars(t *testing.T) {
	var version string
	kit := decompHarnessWith(t, func(cfg *debugger.DecompConfig) {
		in := cfg.Install
		version = in.Version
		dir := filepath.Dir(cfg.CacheRoot)
		sidecars := filepath.Join(dir, "sidecars")
		if err := os.MkdirAll(sidecars, 0o755); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
		defer cancel()
		logf := func(f string, a ...any) { t.Logf(f, a...) }
		for _, prog := range []string{"demo", "stripped"} {
			proj := t.TempDir()
			if err := ghidra.Import(ctx, in, proj, "p", filepath.Join(dir, prog),
				ghidra.ImportOptions{Analysis: ghidra.AnalysisFull}, logf); err != nil {
				t.Fatalf("Import %s: %v", prog, err)
			}
			if err := ghidra.ExportSidecar(ctx, in, proj, "p", prog,
				filepath.Join(sidecars, prog+".decomp.json"), logf); err != nil {
				t.Fatalf("ExportSidecar %s: %v", prog, err)
			}
		}
		*cfg = debugger.DecompConfig{Sidecars: sidecars}
	})
	do := kit.do

	do(wire.TypeExeLoad, wire.ExeLoadRequest{Path: "demo"})
	st := waitReady(t, do)
	if st.Program == nil || st.Program.Name != "demo" || st.GhidraVersion != version || st.Editable {
		t.Fatalf("demo: status %+v", st)
	}
	fn := do(wire.TypeDecompFunction, wire.DecompFunctionRequest{Target: "accumulate"}).(wire.DecompFunction)
	if fn.Name != "accumulate" || !strings.Contains(fn.Text, "accumulate") {
		t.Errorf("accumulate from the sidecar: %s %.200q", fn.Name, fn.Text)
	}

	do(wire.TypeExeLoad, wire.ExeLoadRequest{Path: "stripped"})
	st = waitReady(t, do)
	if st.Program == nil || st.Program.Name != "stripped" || st.Mismatch != "" {
		t.Fatalf("stripped: status %+v", st)
	}

	do(wire.TypeExeLoad, wire.ExeLoadRequest{Path: "opt"})
	st = do(wire.TypeDecompStatus, nil).(wire.DecompStatus)
	if st.State != wire.DecompFailed || !strings.Contains(st.Error, "no decompilation of opt") {
		t.Errorf("opt, which has no sidecar: status %+v", st)
	}

	do(wire.TypeExeLoad, wire.ExeLoadRequest{Path: "demo"})
	st = waitReady(t, do)
	if st.Program == nil || st.Program.Name != "demo" {
		t.Fatalf("demo again: status %+v", st)
	}
}
