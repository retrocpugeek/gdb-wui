//go:build integration

package ghidra_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/retrocpugeek/gdb-wui/internal/ghidra"
)

// Editing the decompiler's database, against a real Ghidra.
//
// The question these answer is not "does setName work" — it does — but whether
// a *resident* script can write at all. analyzeHeadless holds a transaction for
// as long as a script runs, and while it is open every save fails; a server
// that never returns has to hand that transaction back first (finding 31). None
// of that is visible without a JVM.

// startWritable is startIn with editing permitted, returning the pieces needed
// to stop the process and open the same project again.
func startWritable(t *testing.T) (*ghidra.Client, ghidra.Options) {
	t.Helper()
	in := install(t)

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	t.Cleanup(cancel)

	projectDir := t.TempDir()
	bin := fixture(t)
	logf := func(f string, a ...any) { t.Logf(f, a...) }
	if err := ghidra.Import(ctx, in, projectDir, "itest", bin, logf); err != nil {
		t.Fatalf("Import: %v", err)
	}
	opts := ghidra.Options{
		Install:     in,
		ProjectDir:  projectDir,
		ProjectName: "itest",
		Program:     filepath.Base(bin),
		Writable:    true,
		Timeout:     4 * time.Minute,
		Logf:        logf,
	}
	c, err := ghidra.Start(ctx, opts)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c, opts
}

// TestEditsReachTheDisk is the load-bearing test of the whole feature.
//
// It renames a function, stops Ghidra, and starts a *new* process on the same
// project. Only a save that actually happened survives that; an in-process
// read-back passes just as well when the change never left memory, which is
// exactly what it did before end(true) was added — every save answered "Unable
// to lock due to active transaction".
func TestEditsReachTheDisk(t *testing.T) {
	c, opts := startWritable(t)
	ctx := context.Background()

	fn, err := c.Decompile(ctx, "accumulate")
	if err != nil {
		t.Fatalf("Decompile: %v", err)
	}
	res, err := c.Rename(ctx, ghidra.Edit{
		Kind:     ghidra.EditFunction,
		Function: fn.Entry,
		Name:     fn.Name,
		Value:    "total_up",
	})
	if err != nil {
		t.Fatalf("Rename: %v", err)
	}
	if res.Function.Name != "total_up" {
		t.Errorf("renamed function is %q, want total_up", res.Function.Name)
	}
	if res.Was != "accumulate" {
		t.Errorf("was = %q, want accumulate — an undo has nothing to go back to", res.Was)
	}
	if !strings.Contains(res.Function.Text, "total_up") {
		t.Error("the re-decompiled text does not carry the new name")
	}

	// Stop it properly, so the project lock is released for the next open.
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	again, err := ghidra.Start(ctx, opts)
	if err != nil {
		t.Fatalf("second Start: %v", err)
	}
	defer again.Close()

	back, err := again.Decompile(ctx, "total_up")
	if err != nil {
		t.Fatalf("the rename did not survive: %v", err)
	}
	if back.Entry != fn.Entry {
		t.Errorf("total_up is at %s, want %s — a different function was renamed",
			back.Entry, fn.Entry)
	}
}

// TestRenameAndRetypeALocal covers the case the whole feature is for: a
// variable the decompiler invented a name and a type for.
func TestRenameAndRetypeALocal(t *testing.T) {
	c, _ := startWritable(t)
	ctx := context.Background()

	fn, err := c.Decompile(ctx, "accumulate")
	if err != nil {
		t.Fatalf("Decompile: %v", err)
	}
	if len(fn.Variables) == 0 {
		t.Fatal("no variables to rename")
	}
	v := fn.Variables[0]
	if v.ID == "" {
		t.Fatal("no symbol id; an edit has nothing precise to address")
	}

	res, err := c.Rename(ctx, ghidra.Edit{
		Kind:     ghidra.EditVariable,
		Function: fn.Entry,
		Symbol:   v.ID,
		Name:     v.Name,
		Value:    "running_total",
	})
	if err != nil {
		t.Fatalf("Rename: %v", err)
	}
	if !hasVar(res.Function.Variables, "running_total") {
		t.Fatalf("no running_total among %v", names(res.Function.Variables))
	}
	if res.Now != "running_total" {
		t.Errorf("now = %q, want running_total", res.Now)
	}

	// And the id it comes back with is the one a retype has to use: the edit
	// renumbered the symbols (finding 34), so the client's old id is stale.
	renamed := findVar(res.Function.Variables, "running_total")
	res2, err := c.Retype(ctx, ghidra.Edit{
		Kind:     ghidra.EditVariable,
		Function: fn.Entry,
		Symbol:   renamed.ID,
		Name:     renamed.Name,
		Value:    "unsigned long long",
	})
	if err != nil {
		t.Fatalf("Retype: %v", err)
	}
	got := findVar(res2.Function.Variables, "running_total")
	if got == nil {
		t.Fatalf("running_total vanished: %v", names(res2.Function.Variables))
	}
	if !strings.Contains(strings.ToLower(got.Type), "long") {
		t.Errorf("type = %q, want something long", got.Type)
	}
	if res2.Was != renamed.Type {
		t.Errorf("was = %q, want the previous type %q", res2.Was, renamed.Type)
	}
}

// TestRetypeAFunctionRenamesIt pins finding 36: applying a prototype in Ghidra
// carries the name with it, so "set the signature" is also a rename. A caller
// that reported only a type change would leave the stack showing the old name.
func TestRetypeAFunctionRenamesIt(t *testing.T) {
	c, _ := startWritable(t)
	ctx := context.Background()

	fn, err := c.Decompile(ctx, "accumulate")
	if err != nil {
		t.Fatalf("Decompile: %v", err)
	}
	res, err := c.Retype(ctx, ghidra.Edit{
		Kind:     ghidra.EditFunction,
		Function: fn.Entry,
		Name:     fn.Name,
		Value:    "long summarise(unsigned int count)",
	})
	if err != nil {
		t.Fatalf("Retype: %v", err)
	}
	if res.Function.Name != "summarise" {
		t.Errorf("name = %q, want summarise — a prototype carries a name",
			res.Function.Name)
	}
	if !strings.Contains(res.Function.Signature, "long") {
		t.Errorf("signature = %q, want the new return type", res.Function.Signature)
	}
	if !strings.Contains(res.Was, "accumulate") {
		t.Errorf("was = %q, want the old prototype", res.Was)
	}
}

// TestBadPrototypeIsAnError pins finding 36. Ghidra answers an unparseable
// prototype with null rather than an exception, so an unchecked implementation
// reports success and changes nothing — the one outcome worse than failing.
func TestBadPrototypeIsAnError(t *testing.T) {
	c, _ := startWritable(t)
	ctx := context.Background()

	fn, err := c.Decompile(ctx, "accumulate")
	if err != nil {
		t.Fatalf("Decompile: %v", err)
	}
	_, err = c.Retype(ctx, ghidra.Edit{
		Kind:     ghidra.EditFunction,
		Function: fn.Entry,
		Name:     fn.Name,
		Value:    "wibble *wobble(qux)",
	})
	if err == nil {
		t.Fatal("an unparseable prototype was accepted")
	}
	// The wording, not merely the failure. Leaving the null unchecked still
	// fails — the command refuses a null signature — but it fails with "the
	// prototype parsed but could not be applied", which is untrue and sends
	// the reader looking for the wrong problem.
	if !strings.Contains(err.Error(), "cannot read") {
		t.Errorf("error = %q; it should say the prototype could not be read, "+
			"not that it could not be applied", err)
	}
	// And nothing changed.
	after, err := c.Decompile(ctx, "accumulate")
	if err != nil {
		t.Fatalf("the function is gone after a refused edit: %v", err)
	}
	if after.Signature != fn.Signature {
		t.Errorf("signature changed to %q despite the refusal", after.Signature)
	}
}

// TestBadTypeIsAnError. Unlike a prototype, a bad type string throws, and
// Ghidra's own message is the useful one.
func TestBadTypeIsAnError(t *testing.T) {
	c, _ := startWritable(t)
	ctx := context.Background()

	fn, err := c.Decompile(ctx, "accumulate")
	if err != nil {
		t.Fatalf("Decompile: %v", err)
	}
	_, err = c.Retype(ctx, ghidra.Edit{
		Kind:     ghidra.EditVariable,
		Function: fn.Entry,
		Symbol:   fn.Variables[0].ID,
		Name:     fn.Variables[0].Name,
		Value:    "struct not_a_type_at_all",
	})
	if err == nil {
		t.Fatal("an unknown type was accepted")
	}
	if !strings.Contains(err.Error(), "not_a_type_at_all") {
		t.Errorf("error = %q, which does not name the type that was refused", err)
	}
}

// TestAStaleSymbolIsRefused. An edit renumbers the ids of the symbols it did
// not touch, so a client's id is routinely one edit out of date. Applying it to
// whatever now holds that id would rename the wrong variable, which is worse
// than refusing: "??" says nothing, a wrong name says something false.
func TestAStaleSymbolIsRefused(t *testing.T) {
	c, _ := startWritable(t)
	ctx := context.Background()

	fn, err := c.Decompile(ctx, "accumulate")
	if err != nil {
		t.Fatalf("Decompile: %v", err)
	}
	_, err = c.Rename(ctx, ghidra.Edit{
		Kind:     ghidra.EditVariable,
		Function: fn.Entry,
		Symbol:   "999999999",
		Name:     "no_such_variable",
		Value:    "whatever",
	})
	if err == nil {
		t.Fatal("a rename of a variable that is not there was accepted")
	}
	if !strings.Contains(err.Error(), "no_such_variable") {
		t.Errorf("error = %q, which does not say what could not be found", err)
	}
}

// TestReadOnlyClientRefusesEdits is the guard, at the far side of the socket.
//
// It matters that this is tested against a *real* Ghidra: -readOnly does not
// stop a script writing (finding 32), so nothing but this refusal stands
// between a user's own project and an edit.
func TestReadOnlyClientRefusesEdits(t *testing.T) {
	c := start(t) // Writable is false.
	ctx := context.Background()

	fn, err := c.Decompile(ctx, "accumulate")
	if err != nil {
		t.Fatalf("Decompile: %v", err)
	}
	_, err = c.Rename(ctx, ghidra.Edit{
		Kind:     ghidra.EditFunction,
		Function: fn.Entry,
		Name:     fn.Name,
		Value:    "should_not_happen",
	})
	if err == nil {
		t.Fatal("a read-only sidecar accepted an edit")
	}
	if !strings.Contains(err.Error(), "read-only") {
		t.Errorf("error = %q, which does not say why", err)
	}
	if _, err := c.Decompile(ctx, "should_not_happen"); err == nil {
		t.Fatal("the refused rename happened anyway")
	}
}

func hasVar(vars []ghidra.Var, name string) bool { return findVar(vars, name) != nil }

func findVar(vars []ghidra.Var, name string) *ghidra.Var {
	for i := range vars {
		if vars[i].Name == name {
			return &vars[i]
		}
	}
	return nil
}

func names(vars []ghidra.Var) []string {
	out := make([]string, 0, len(vars))
	for _, v := range vars {
		out = append(out, v.Name)
	}
	return out
}
