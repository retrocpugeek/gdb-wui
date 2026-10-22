//go:build integration

package ghidra_test

import (
	"context"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/retrocpugeek/gdb-wui/internal/ghidra"
)

// TestSidecarAnswersAsTheServerDoes exports the fixture's project and checks
// that every op the client makes gets the same answer from the sidecar as from
// the resident server on the same project: the export is meant to be a
// drop-in for it, and a difference here is a decompiled view that changes
// depending on where it came from.
func TestSidecarAnswersAsTheServerDoes(t *testing.T) {
	in := install(t)
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()
	logf := func(f string, a ...any) { t.Logf(f, a...) }

	// Exported before the server starts: a running server holds the
	// project's lock, and analyzeHeadless will not open it beside one.
	projectDir := t.TempDir()
	bin := fixture(t)
	if err := ghidra.Import(ctx, in, projectDir, "itest", bin,
		ghidra.ImportOptions{Analysis: ghidra.AnalysisFull}, logf); err != nil {
		t.Fatalf("Import: %v", err)
	}
	sidecar := filepath.Join(t.TempDir(), "demo.decomp.json")
	if err := ghidra.ExportSidecar(ctx, in, projectDir, "itest", filepath.Base(bin), sidecar, logf); err != nil {
		t.Fatalf("ExportSidecar: %v", err)
	}
	live, err := ghidra.Start(ctx, ghidra.Options{Install: in, ProjectDir: projectDir,
		ProjectName: "itest", Program: filepath.Base(bin), Timeout: 4 * time.Minute, Logf: logf})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer live.Close()
	found, err := ghidra.FindExport(filepath.Dir(sidecar), live.Ready().Program.SHA256)
	if err != nil || found != sidecar {
		t.Fatalf("FindExport = %q, %v", found, err)
	}
	exp, err := ghidra.StartExport(ctx, sidecar, ghidra.Options{})
	if err != nil {
		t.Fatalf("StartExport: %v", err)
	}
	defer exp.Close()

	got, want := exp.Ready(), live.Ready()
	if got.Sidecar != filepath.Base(sidecar) || got.SidecarVersion != in.Version {
		t.Errorf("greeting names sidecar %q, version %q", got.Sidecar, got.SidecarVersion)
	}
	got.Sidecar, got.SidecarVersion = "", ""
	if !reflect.DeepEqual(got, want) {
		t.Errorf("greeting:\n sidecar %+v\n server  %+v", got, want)
	}

	lf, err := live.Functions(ctx, 0, 5000, "")
	if err != nil {
		t.Fatal(err)
	}
	ef, err := exp.Functions(ctx, 0, 5000, "")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(ef, lf) {
		t.Errorf("functions:\n sidecar %+v\n server  %+v", ef, lf)
	}
	ld, err := live.Data(ctx, 0, 5000, "")
	if err != nil {
		t.Fatal(err)
	}
	ed, err := exp.Data(ctx, 0, 5000, "")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(ed, ld) {
		t.Errorf("data:\n sidecar %+v\n server  %+v", ed, ld)
	}

	// Every function by name, and by an address inside it and one past it:
	// the decompiled ones must match, and the rest (thunks) must fail in
	// both.
	var addrs []string
	for _, f := range lf.Functions {
		lfn, lerr := live.Decompile(ctx, f.Name)
		efn, eerr := exp.Decompile(ctx, f.Name)
		if (lerr == nil) != (eerr == nil) {
			if !f.Thunk {
				t.Errorf("Decompile(%s): server %v, sidecar %v", f.Name, lerr, eerr)
			}
			continue
		}
		if lerr != nil {
			continue
		}
		if !reflect.DeepEqual(efn, lfn) {
			t.Errorf("Decompile(%s) differs:\n sidecar %.300q\n server  %.300q", f.Name, efn.Text, lfn.Text)
		}
		start := parseAddr(t, lfn.BodyStart)
		inside := fmt.Sprintf("0x%x", start+(parseAddr(t, lfn.BodyEnd)-start)/2)
		efn2, err := exp.Decompile(ctx, inside)
		if err != nil || efn2.Name != lfn.Name {
			t.Errorf("Decompile(%s, inside %s) = %v, %v", inside, f.Name, efn2, err)
		}
		addrs = append(addrs, f.Entry, inside, fmt.Sprintf("0x%x", parseAddr(t, lfn.BodyEnd)+0x100000))
	}
	ln, err := live.Names(ctx, addrs)
	if err != nil {
		t.Fatal(err)
	}
	en, err := exp.Names(ctx, addrs)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(en, ln) {
		t.Errorf("names:\n sidecar %+v\n server  %+v", en, ln)
	}

	// And it cannot be edited
	if _, err := exp.Rename(ctx, ghidra.Edit{Kind: ghidra.EditFunction, Function: lf.Functions[0].Entry,
		Name: lf.Functions[0].Name, Value: "renamed"}); err == nil || !strings.Contains(err.Error(), "cannot be changed") {
		t.Errorf("Rename on a sidecar: %v", err)
	}
}
