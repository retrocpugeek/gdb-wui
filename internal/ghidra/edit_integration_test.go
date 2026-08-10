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

// TestCommentAppearsInTheDecompiledText is the whole claim of the feature: a
// comment written into the listing is printed in the C, which is not something
// a caller can assume. The decompiler displays PRE comments and the entry
// point's PLATE comment with its default options, and nothing else (finding
// 39) — a comment stored anywhere else would be an edit that appears to do
// nothing at all.
func TestCommentAppearsInTheDecompiledText(t *testing.T) {
	c, _ := startWritable(t)
	ctx := context.Background()

	fn, err := c.Decompile(ctx, "accumulate")
	if err != nil {
		t.Fatalf("Decompile: %v", err)
	}
	line := firstMappedLine(t, fn)

	res, err := c.Comment(ctx, ghidra.Edit{
		Kind:     ghidra.EditLine,
		Function: fn.Entry,
		Address:  line.Addrs[0],
		Value:    "the running total lives here",
	})
	if err != nil {
		t.Fatalf("Comment: %v", err)
	}
	if !strings.Contains(res.Function.Text, "the running total lives here") {
		t.Fatalf("the comment is not in the decompiled text:\n%s", res.Function.Text)
	}
	if res.Was != "" {
		t.Errorf("was = %q, want empty — there was no comment before", res.Was)
	}

	// It is reported both as stored text and as a rendered line, and the two
	// are different things: an editor needs the first, a pane needs the second.
	stored := findComment(res.Function.Comments, line.Addrs[0])
	if stored == nil {
		t.Fatalf("no stored comment at %s: %+v", line.Addrs[0], res.Function.Comments)
	}
	if stored.Text != "the running total lives here" || stored.Kind != ghidra.CommentPre {
		t.Errorf("stored comment = %+v, want the text back as a pre comment", stored)
	}
	marked := findCommentLine(res.Function.CommentLines, line.Addrs[0])
	if marked == nil {
		t.Fatalf("no comment line for %s: %+v", line.Addrs[0], res.Function.CommentLines)
	}
	if !strings.Contains(textLine(res.Function.Text, marked.N), "the running total lives here") {
		t.Errorf("line %d is %q, which is not the comment",
			marked.N, textLine(res.Function.Text, marked.N))
	}
	// And it claims no addresses, so the program counter can never land on it.
	for _, l := range res.Function.Lines {
		if l.N == marked.N {
			t.Errorf("comment line %d also appears in the address map as %v", l.N, l.Addrs)
		}
	}

	// Editing it reports what was there, which is what an undo needs.
	res2, err := c.Comment(ctx, ghidra.Edit{
		Kind:     ghidra.EditLine,
		Function: fn.Entry,
		Address:  line.Addrs[0],
		Value:    "second thoughts",
	})
	if err != nil {
		t.Fatalf("second Comment: %v", err)
	}
	if res2.Was != "the running total lives here" {
		t.Errorf("was = %q, want the previous comment", res2.Was)
	}
	if strings.Contains(res2.Function.Text, "the running total lives here") {
		t.Error("the old comment is still in the text")
	}

	// And empty removes it rather than leaving a bare /* */ on the page.
	res3, err := c.Comment(ctx, ghidra.Edit{
		Kind:     ghidra.EditLine,
		Function: fn.Entry,
		Address:  line.Addrs[0],
		Value:    "",
	})
	if err != nil {
		t.Fatalf("removing Comment: %v", err)
	}
	if strings.Contains(res3.Function.Text, "second thoughts") {
		t.Error("the comment survived being removed")
	}
	if len(res3.Function.Comments) != 0 {
		t.Errorf("comments = %+v after removing the only one", res3.Function.Comments)
	}
}

// TestFunctionCommentIsTheHeader: the PLATE comment on the entry point is what
// the decompiler prints above the prototype. Any other placement stores fine
// and shows nothing.
func TestFunctionCommentIsTheHeader(t *testing.T) {
	c, _ := startWritable(t)
	ctx := context.Background()

	fn, err := c.Decompile(ctx, "accumulate")
	if err != nil {
		t.Fatalf("Decompile: %v", err)
	}
	res, err := c.Comment(ctx, ghidra.Edit{
		Kind:     ghidra.EditFunction,
		Function: fn.Entry,
		Value:    "sums the list; callers rely on the wrap",
	})
	if err != nil {
		t.Fatalf("Comment: %v", err)
	}
	if !strings.Contains(res.Function.Text, "sums the list") {
		t.Fatalf("the header comment is not in the text:\n%s", res.Function.Text)
	}
	stored := findComment(res.Function.Comments, fn.Entry)
	if stored == nil || stored.Kind != ghidra.CommentPlate {
		t.Errorf("comments = %+v, want a plate comment at the entry point",
			res.Function.Comments)
	}
	// Above the prototype: a header comment that printed below the signature
	// would still contain the text and be the wrong thing.
	body := strings.Index(res.Function.Text, "accumulate")
	if at := strings.Index(res.Function.Text, "sums the list"); at > body {
		t.Errorf("the comment is at %d, after the function's own name at %d", at, body)
	}
}

// TestCommentSurvivesARestart. The same proof as TestEditsReachTheDisk and for
// the same reason: an in-process read-back passes even when nothing was saved.
func TestCommentSurvivesARestart(t *testing.T) {
	c, opts := startWritable(t)
	ctx := context.Background()

	fn, err := c.Decompile(ctx, "accumulate")
	if err != nil {
		t.Fatalf("Decompile: %v", err)
	}
	if _, err := c.Comment(ctx, ghidra.Edit{
		Kind:     ghidra.EditFunction,
		Function: fn.Entry,
		Value:    "written before the restart",
	}); err != nil {
		t.Fatalf("Comment: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	again, err := ghidra.Start(ctx, opts)
	if err != nil {
		t.Fatalf("second Start: %v", err)
	}
	defer again.Close()

	back, err := again.Decompile(ctx, "accumulate")
	if err != nil {
		t.Fatalf("Decompile after restart: %v", err)
	}
	if !strings.Contains(back.Text, "written before the restart") {
		t.Errorf("the comment did not survive:\n%s", back.Text)
	}
}

// TestACommentIsFreeText. Names and types are constrained; a comment is the
// first thing a user can type that reaches Ghidra unfiltered, and it crosses
// the socket through a hand-rolled scanner on the far side. A comment that
// spells out a field name of the request must not be read as one.
func TestACommentIsFreeText(t *testing.T) {
	c, _ := startWritable(t)
	ctx := context.Background()

	fn, err := c.Decompile(ctx, "accumulate")
	if err != nil {
		t.Fatalf("Decompile: %v", err)
	}
	nasty := `"kind":"function", a \ backslash, a "quote" and a	tab`
	res, err := c.Comment(ctx, ghidra.Edit{
		Kind:     ghidra.EditFunction,
		Function: fn.Entry,
		Value:    nasty,
	})
	if err != nil {
		t.Fatalf("Comment: %v", err)
	}
	stored := findComment(res.Function.Comments, fn.Entry)
	if stored == nil {
		t.Fatalf("no comment stored: %+v", res.Function.Comments)
	}
	if stored.Text != nasty {
		t.Errorf("stored %q, want %q — it did not survive the round trip",
			stored.Text, nasty)
	}
	// The function is still a function: a request read as `kind: function` in
	// the wrong place would have renamed or retyped something.
	if res.Function.Name != fn.Name || res.Function.Signature != fn.Signature {
		t.Errorf("the function became %q %q; the comment was acted on",
			res.Function.Name, res.Function.Signature)
	}
}

// TestCommentOutsideTheFunctionIsRefused. A comment on an address the user is
// not looking at is one they will never see again, so it is refused rather
// than written somewhere else in the program.
func TestCommentOutsideTheFunctionIsRefused(t *testing.T) {
	c, _ := startWritable(t)
	ctx := context.Background()

	fn, err := c.Decompile(ctx, "accumulate")
	if err != nil {
		t.Fatalf("Decompile: %v", err)
	}
	_, err = c.Comment(ctx, ghidra.Edit{
		Kind:     ghidra.EditLine,
		Function: fn.Entry,
		Address:  "0x0",
		Value:    "nowhere near",
	})
	if err == nil {
		t.Fatal("a comment outside the function was accepted")
	}
	if !strings.Contains(err.Error(), "accumulate") {
		t.Errorf("error = %q, which does not say which function it is not in", err)
	}
}

// TestReadOnlyClientRefusesComments. The same guard as for a rename: -readOnly
// protects nothing (finding 32), so the refusal is the only thing between a
// user's own project and a write.
func TestReadOnlyClientRefusesComments(t *testing.T) {
	c := start(t) // Writable is false.
	ctx := context.Background()

	fn, err := c.Decompile(ctx, "accumulate")
	if err != nil {
		t.Fatalf("Decompile: %v", err)
	}
	_, err = c.Comment(ctx, ghidra.Edit{
		Kind:     ghidra.EditFunction,
		Function: fn.Entry,
		Value:    "should not be written",
	})
	if err == nil {
		t.Fatal("a read-only sidecar accepted a comment")
	}
	if !strings.Contains(err.Error(), "read-only") {
		t.Errorf("error = %q, which does not say why", err)
	}
	after, err := c.Decompile(ctx, "accumulate")
	if err != nil {
		t.Fatalf("Decompile: %v", err)
	}
	if strings.Contains(after.Text, "should not be written") {
		t.Error("the refused comment was written anyway")
	}
}

// TestAnAgentsNameIsRecordedAsInferred pins finding 40's first half. A name a
// person typed and a name something guessed must not come back alike, and
// Ghidra's own source types are the record: ANALYSIS for the guess,
// USER_DEFINED for the person.
func TestAnAgentsNameIsRecordedAsInferred(t *testing.T) {
	c, opts := startWritable(t)
	ctx := context.Background()

	fn, err := c.Decompile(ctx, "accumulate")
	if err != nil {
		t.Fatalf("Decompile: %v", err)
	}
	agent, err := c.Rename(ctx, ghidra.Edit{
		Kind:     ghidra.EditFunction,
		Function: fn.Entry,
		Name:     fn.Name,
		Value:    "guessed_name",
		Author:   ghidra.AuthorAgent,
	})
	if err != nil {
		t.Fatalf("Rename: %v", err)
	}
	if agent.Function.Source != ghidra.SourceAnalysis {
		t.Errorf("source = %q, want %s — an agent's name is inferred, not stated",
			agent.Function.Source, ghidra.SourceAnalysis)
	}

	// The same edit from a person is recorded differently, which is the whole
	// point: one field, two claims.
	person, err := c.Rename(ctx, ghidra.Edit{
		Kind:     ghidra.EditFunction,
		Function: fn.Entry,
		Name:     "guessed_name",
		Value:    "chosen_name",
	})
	if err != nil {
		t.Fatalf("second Rename: %v", err)
	}
	if person.Function.Source != ghidra.SourceUser {
		t.Errorf("source = %q, want %s", person.Function.Source, ghidra.SourceUser)
	}

	// And a local, which goes through a different Ghidra API — the one that
	// creates the database variable a decompiler local does not have.
	var local *ghidra.Var
	for i := range person.Function.Variables {
		if person.Function.Variables[i].ID != "" {
			local = &person.Function.Variables[i]
			break
		}
	}
	if local == nil {
		t.Fatal("no variable carried a symbol id")
	}
	withLocal, err := c.Rename(ctx, ghidra.Edit{
		Kind:     ghidra.EditVariable,
		Function: fn.Entry,
		Symbol:   local.ID,
		Name:     local.Name,
		Value:    "guessed_local",
		Author:   ghidra.AuthorAgent,
	})
	if err != nil {
		t.Fatalf("renaming a local: %v", err)
	}
	got := findVar(withLocal.Function.Variables, "guessed_local")
	if got == nil {
		t.Fatalf("guessed_local is not there: %v", names(withLocal.Function.Variables))
	}
	if got.Source != ghidra.SourceAnalysis {
		t.Errorf("local source = %q, want %s", got.Source, ghidra.SourceAnalysis)
	}

	// All of it has to survive the project being closed and opened again,
	// because that is when a user next sees it — in gdb-wui or in Ghidra.
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	again, err := ghidra.Start(ctx, opts)
	if err != nil {
		t.Fatalf("second Start: %v", err)
	}
	defer again.Close()
	back, err := again.Decompile(ctx, "chosen_name")
	if err != nil {
		t.Fatalf("Decompile after restart: %v", err)
	}
	if back.Source != ghidra.SourceUser {
		t.Errorf("after a restart the function's source is %q, want %s",
			back.Source, ghidra.SourceUser)
	}
	if v := findVar(back.Variables, "guessed_local"); v == nil ||
		v.Source != ghidra.SourceAnalysis {
		t.Errorf("after a restart the local is %+v, want one marked %s",
			v, ghidra.SourceAnalysis)
	}
}

// TestAnAgentsCommentIsMarked pins the other half. A comment has no source type
// — the listing stores text and nothing else — so authorship rides beside it as
// a bookmark, and the interesting cases are the transitions.
func TestAnAgentsCommentIsMarked(t *testing.T) {
	c, opts := startWritable(t)
	ctx := context.Background()

	fn, err := c.Decompile(ctx, "accumulate")
	if err != nil {
		t.Fatalf("Decompile: %v", err)
	}
	line := firstMappedLine(t, fn)
	at := line.Addrs[0]

	res, err := c.Comment(ctx, ghidra.Edit{
		Kind:     ghidra.EditLine,
		Function: fn.Entry,
		Address:  at,
		Value:    "the agent thinks this is a length",
		Author:   ghidra.AuthorAgent,
	})
	if err != nil {
		t.Fatalf("Comment: %v", err)
	}
	if got := findComment(res.Function.Comments, at); got == nil ||
		got.Author != ghidra.AuthorAgent {
		t.Fatalf("comment = %+v, want one marked as the agent's", got)
	}

	// A person editing an agent's note takes it over. What is on the page
	// afterwards is theirs, and leaving the mark would credit the agent with a
	// sentence it did not write.
	res, err = c.Comment(ctx, ghidra.Edit{
		Kind:     ghidra.EditLine,
		Function: fn.Entry,
		Address:  at,
		Value:    "no: it is a count",
	})
	if err != nil {
		t.Fatalf("second Comment: %v", err)
	}
	if got := findComment(res.Function.Comments, at); got == nil || got.Author != "" {
		t.Fatalf("after a person rewrote it the comment is %+v, want no author", got)
	}

	// And the mark survives a restart when it is the agent's.
	if _, err := c.Comment(ctx, ghidra.Edit{
		Kind:     ghidra.EditFunction,
		Function: fn.Entry,
		Value:    "written by the agent, before the restart",
		Author:   ghidra.AuthorAgent,
	}); err != nil {
		t.Fatalf("commenting the function: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	again, err := ghidra.Start(ctx, opts)
	if err != nil {
		t.Fatalf("second Start: %v", err)
	}
	defer again.Close()
	back, err := again.Decompile(ctx, "accumulate")
	if err != nil {
		t.Fatalf("Decompile after restart: %v", err)
	}
	if got := findComment(back.Comments, back.Entry); got == nil ||
		got.Author != ghidra.AuthorAgent {
		t.Errorf("after a restart the function comment is %+v, want the agent's mark", got)
	}
}

// TestRemovingACommentRemovesItsMark. A bookmark left behind would put the
// agent's name on whatever a person writes there next.
func TestRemovingACommentRemovesItsMark(t *testing.T) {
	c, _ := startWritable(t)
	ctx := context.Background()

	fn, err := c.Decompile(ctx, "accumulate")
	if err != nil {
		t.Fatalf("Decompile: %v", err)
	}
	line := firstMappedLine(t, fn)
	at := line.Addrs[0]

	for _, e := range []ghidra.Edit{
		{Kind: ghidra.EditLine, Function: fn.Entry, Address: at,
			Value: "the agent's", Author: ghidra.AuthorAgent},
		{Kind: ghidra.EditLine, Function: fn.Entry, Address: at, Value: ""},
	} {
		if _, err := c.Comment(ctx, e); err != nil {
			t.Fatalf("Comment %+v: %v", e, err)
		}
	}
	res, err := c.Comment(ctx, ghidra.Edit{
		Kind:     ghidra.EditLine,
		Function: fn.Entry,
		Address:  at,
		Value:    "mine now",
	})
	if err != nil {
		t.Fatalf("Comment: %v", err)
	}
	if got := findComment(res.Function.Comments, at); got == nil || got.Author != "" {
		t.Errorf("comment = %+v; the agent's mark outlived the comment it was on", got)
	}
}

// firstMappedLine is a line that came from an address, which is the only kind
// that can hold a comment.
func firstMappedLine(t *testing.T, fn *ghidra.Function) ghidra.Line {
	t.Helper()
	for _, l := range fn.Lines {
		if len(l.Addrs) > 0 {
			return l
		}
	}
	t.Fatalf("%s has no line with an address", fn.Name)
	return ghidra.Line{}
}

func findComment(comments []ghidra.Comment, addr string) *ghidra.Comment {
	for i := range comments {
		if comments[i].Addr == addr {
			return &comments[i]
		}
	}
	return nil
}

func findCommentLine(lines []ghidra.CommentLine, addr string) *ghidra.CommentLine {
	for i := range lines {
		if lines[i].Addr == addr {
			return &lines[i]
		}
	}
	return nil
}

// textLine is line n of a function's text, 1-based like everything else here.
func textLine(text string, n int) string {
	lines := strings.Split(text, "\n")
	if n < 1 || n > len(lines) {
		return ""
	}
	return lines[n-1]
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
