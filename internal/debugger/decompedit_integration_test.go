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

// Editing the decompiler's names and types, through a whole session.
//
// The ghidra package's tests cover whether Ghidra can be made to write at all.
// These cover the two things only this layer can get wrong: the coordinates an
// edit is addressed in, and who is allowed to make one.

// TestRenameAFunctionThroughTheSession is the feature in one test: a name the
// decompiler invented becomes a name the user chose, everywhere it appears.
func TestRenameAFunctionThroughTheSession(t *testing.T) {
	k := decompHarness(t)
	do := k.do
	do(wire.TypeExeLoad, wire.ExeLoadRequest{Path: "nodebug"})
	waitReady(t, do)

	fn := do(wire.TypeDecompFunction,
		wire.DecompFunctionRequest{Target: "accumulate"}).(wire.DecompFunction)

	out := do(wire.TypeDecompRename, wire.DecompEditRequest{
		Kind:     wire.DecompEditFunction,
		Function: fn.Entry,
		Name:     fn.Name,
		Value:    "total_up",
	}).(wire.DecompEdit)

	if out.Function.Name != "total_up" {
		t.Errorf("function is %q, want total_up", out.Function.Name)
	}
	if out.Function.Entry != fn.Entry {
		t.Errorf("entry moved from %s to %s; a different function was renamed",
			fn.Entry, out.Function.Entry)
	}
	if !strings.Contains(out.Did, "total_up") {
		t.Errorf("did = %q, which does not describe the change", out.Did)
	}
	if !out.CanUndo {
		t.Error("canUndo is false right after an edit")
	}

	// And the name the call stack would show. This is the whole point of
	// putting the name in Ghidra rather than in a table of our own: everything
	// that asks the decompiler gets the new answer.
	named := do(wire.TypeDecompNames,
		wire.DecompNamesRequest{Addresses: []string{fn.Entry}}).(wire.DecompNames)
	if len(named.Names) != 1 || named.Names[0].Name != "total_up" {
		t.Errorf("decomp.names says %+v, want total_up", named.Names)
	}
}

// TestEditsAreAddressedInRuntimeCoordinates.
//
// The client only ever sees runtime addresses, and on a PIE those differ from
// Ghidra's by however far the loader moved the program — some 0x555555554000.
// An edit that failed to translate would either miss entirely or, worse, land
// on whatever Ghidra has at the untranslated address. So this renames *after*
// the program is running, using the address the client was given.
func TestEditsAreAddressedInRuntimeCoordinates(t *testing.T) {
	k := decompHarness(t)
	do := k.do
	do(wire.TypeExeLoad, wire.ExeLoadRequest{Path: "nodebug"})
	waitReady(t, do)

	do(wire.TypeBpSetAddress, wire.BreakpointAddressRequest{Location: "accumulate"})
	do(wire.TypeExecRun, wire.ExecRequest{})
	waitStopped(t, do, 30*time.Second)

	stack := do(wire.TypeStackList, wire.StackListRequest{}).(wire.StackList)
	pc := stack.Frames[0].Address

	fn := do(wire.TypeDecompFunction,
		wire.DecompFunctionRequest{Target: pc}).(wire.DecompFunction)
	if fn.Bias == 0 {
		t.Skip("no relocation happened, so there is no translation to test")
	}
	if len(fn.Vars) == 0 {
		t.Fatal("no variables to rename")
	}

	// A local rather than the function, because a local is addressed by the
	// function's entry *and* a symbol id: both have to survive the trip.
	var target wire.DecompVar
	for _, v := range fn.Vars {
		if v.ID != "" {
			target = v
			break
		}
	}
	if target.ID == "" {
		t.Fatal("no variable carried a symbol id")
	}

	out := do(wire.TypeDecompRename, wire.DecompEditRequest{
		Kind:     wire.DecompEditVariable,
		Function: fn.Entry,
		Symbol:   target.ID,
		Name:     target.Name,
		Value:    "counted",
	}).(wire.DecompEdit)

	if !hasWireVar(out.Function.Vars, "counted") {
		t.Fatalf("no variable named counted among %v", wireVarNames(out.Function.Vars))
	}
	if out.Function.Entry != fn.Entry {
		t.Errorf("the reply is for %s, not the function asked about (%s)",
			out.Function.Entry, fn.Entry)
	}
	if hasWireVar(out.Function.Vars, target.Name) {
		t.Errorf("%s is still there as well as counted", target.Name)
	}
}

// TestUndoPutsTheNameBack. Ghidra's own undo cannot be used, because saving
// clears it and every edit is saved (finding 33) — so this is exercising a
// journal of inverse edits, and the thing most likely to be wrong about one is
// that it addresses the symbol by an id the edit has just invalidated.
func TestUndoPutsTheNameBack(t *testing.T) {
	k := decompHarness(t)
	do := k.do
	do(wire.TypeExeLoad, wire.ExeLoadRequest{Path: "nodebug"})
	waitReady(t, do)

	fn := do(wire.TypeDecompFunction,
		wire.DecompFunctionRequest{Target: "accumulate"}).(wire.DecompFunction)
	var v wire.DecompVar
	for _, c := range fn.Vars {
		if c.ID != "" {
			v = c
			break
		}
	}
	if v.ID == "" {
		t.Fatal("no variable carried a symbol id")
	}

	do(wire.TypeDecompRename, wire.DecompEditRequest{
		Kind:     wire.DecompEditVariable,
		Function: fn.Entry,
		Symbol:   v.ID,
		Name:     v.Name,
		Value:    "misnamed",
	})
	out := do(wire.TypeDecompUndo, wire.DecompUndoRequest{}).(wire.DecompEdit)

	if !hasWireVar(out.Function.Vars, v.Name) {
		t.Errorf("after undo the variables are %v, want %s back",
			wireVarNames(out.Function.Vars), v.Name)
	}
	if hasWireVar(out.Function.Vars, "misnamed") {
		t.Error("misnamed is still there after the undo")
	}
	if out.CanUndo {
		t.Error("canUndo is still true after the only edit was undone")
	}
	// And the journal is empty rather than a toggle: undoing again is an
	// error, not a redo of the rename.
	if _, werr := k.try(wire.TypeDecompUndo, wire.DecompUndoRequest{}); werr == nil {
		t.Error("a second undo succeeded; the journal is behaving as a toggle")
	}
}

// TestTheUsersOwnProjectIsNotWritten is the guard.
//
// -readOnly does not provide it: under that flag the sidecar can still rename
// a function and save the file (finding 32). What provides it is this refusal,
// so deleting it has to fail a test.
func TestTheUsersOwnProjectIsNotWritten(t *testing.T) {
	install, err := ghidra.Locate("")
	if err != nil {
		t.Skipf("no Ghidra installation: %v", err)
	}
	// A project of the user's own: imported here, exactly as someone would
	// have imported it in the Ghidra GUI, and named with -ghidra-project.
	scratch := t.TempDir()
	projectDir := filepath.Join(scratch, "theirs")
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatal(err)
	}

	k := decompHarnessWith(t, func(cfg *debugger.DecompConfig) {
		cfg.ProjectDir = projectDir
		cfg.ProjectName = "theirs"
		cfg.Program = "nodebug"
	})
	// Imported after the session exists and before anything asks it to
	// decompile, which is when the sidecar is first started.
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	if err := ghidra.Import(ctx, install, projectDir, "theirs",
		filepath.Join(k.dir, "nodebug"),
		func(f string, a ...any) { t.Logf(f, a...) }); err != nil {
		t.Fatalf("Import: %v", err)
	}

	do := k.do
	do(wire.TypeExeLoad, wire.ExeLoadRequest{Path: "nodebug"})
	st := waitReady(t, do)
	if st.Editable {
		t.Fatal("a project named with -ghidra-project reports itself editable")
	}

	fn := do(wire.TypeDecompFunction,
		wire.DecompFunctionRequest{Target: "accumulate"}).(wire.DecompFunction)
	_, werr := k.try(wire.TypeDecompRename, wire.DecompEditRequest{
		Kind:     wire.DecompEditFunction,
		Function: fn.Entry,
		Name:     fn.Name,
		Value:    "not_allowed",
	})
	if werr == nil {
		t.Fatal("an edit to the user's own project was accepted")
	}
	// The wording matters, and so does which layer produced it. Removing this
	// layer's guard still gets a refusal — the sidecar has its own — but the
	// message becomes the sidecar's, which talks about how it was opened
	// rather than about whose project it is.
	if !strings.Contains(werr.Message, "yours") {
		t.Errorf("message = %q. Either the guard in this layer is gone and the "+
			"sidecar refused instead, or the wording no longer says whose "+
			"project it is.", werr.Message)
	}
	// And nothing happened to it.
	after := do(wire.TypeDecompFunction,
		wire.DecompFunctionRequest{Target: "accumulate"}).(wire.DecompFunction)
	if after.Name != fn.Name {
		t.Errorf("the function is now called %q; the refusal did not hold", after.Name)
	}
}

// TestNamesAreChecked. Ghidra accepts almost anything as a symbol name, spaces
// included, and the result decompiles into text that cannot be pasted into gdb.
func TestNamesAreChecked(t *testing.T) {
	k := decompHarness(t)
	do := k.do
	do(wire.TypeExeLoad, wire.ExeLoadRequest{Path: "nodebug"})
	waitReady(t, do)
	fn := do(wire.TypeDecompFunction,
		wire.DecompFunctionRequest{Target: "accumulate"}).(wire.DecompFunction)

	// Not "has a space": Ghidra refuses that one itself, so it would pass with
	// this check deleted and would prove nothing about it. These two Ghidra
	// accepts, and the result decompiles into text nobody can paste into gdb.
	for _, bad := range []string{"2fast", "curly{}"} {
		_, werr := k.try(wire.TypeDecompRename, wire.DecompEditRequest{
			Kind:     wire.DecompEditFunction,
			Function: fn.Entry,
			Name:     fn.Name,
			Value:    bad,
		})
		if werr == nil {
			t.Errorf("%q was accepted as a name", bad)
		}
	}
}

func hasWireVar(vars []wire.DecompVar, name string) bool {
	for _, v := range vars {
		if v.Name == name {
			return true
		}
	}
	return false
}

func wireVarNames(vars []wire.DecompVar) []string {
	out := make([]string, 0, len(vars))
	for _, v := range vars {
		out = append(out, v.Name)
	}
	return out
}
