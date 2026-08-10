package debugger

// Editing what the decompiler guessed.
//
// A stripped binary decompiles to FUN_0010d2b0, local_10 and undefined8, and
// correcting those is most of the work of reading one. The names go into the
// Ghidra project, not into anything gdb-wui invented, so they are there next
// time — and are the same names the call stack and the symbol list show.
//
// Two properties this file exists to keep:
//
//	Only gdb-wui's own project is written to. A project the user pointed at
//	with -ghidra-project holds their names, types and comments, and -readOnly
//	does not protect it: under that flag the sidecar can still rename a
//	function and save the file (finding 32). The refusal is here and in the
//	sidecar, on both sides of the socket.
//
//	An edit is answered with the function decompiled again. A rename renumbers
//	the ids of the symbols it did not touch (finding 34) and a retype reshapes
//	the body around it, so a client that patched its own copy would be holding
//	keys that address nothing.

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/retrocpugeek/gdb-wui/internal/ghidra"
	"github.com/retrocpugeek/gdb-wui/internal/wire"
)

// maxUndo bounds the journal. Deep enough to cover a naming session, shallow
// enough that it is never the reason a long-lived server grows.
const maxUndo = 200

// decompUndo is one edit, inverted, ready to be applied.
//
// It holds Ghidra coordinates rather than the runtime addresses the request
// arrived in. A position-independent executable relocates on every run, so a
// runtime address stored now is wrong after the next `run` — and the journal
// outlives any number of them.
type decompUndo struct {
	op   string
	edit ghidra.Edit
	// did describes what undoing it will do, for the status line.
	did string
	// run groups edits that were made together, so that forty annotations
	// written by an agent in one burst come back off in one step. author is
	// what does the grouping — a person's edit between two of an agent's
	// starts a new run, which is exactly where a user would want the boundary.
	run    string
	author string
	// at is when the edit was made, and only the gap between consecutive edits
	// by one author is read from it.
	at time.Time
}

// runGap ends a run. Long enough that an agent thinking between two tool calls
// stays in one run; short enough that a burst an hour later is a different one.
const runGap = 2 * time.Minute

// author narrows what a client may claim to the one value that means anything.
//
// Unrecognised is a person, not an error: the field exists so an agent can say
// it is one, and a client that sends nonsense gets the safer reading — its
// edits are recorded as stated rather than inferred, which understates the
// machine's involvement rather than overstating a person's.
func author(claimed string) string {
	if claimed == wire.DecompAuthorAgent {
		return ghidra.AuthorAgent
	}
	return ""
}

func (s *Session) decompRename(r *request) (any, *wire.Error) {
	return s.decompEdit(r, wire.TypeDecompRename)
}

func (s *Session) decompRetype(r *request) (any, *wire.Error) {
	return s.decompEdit(r, wire.TypeDecompRetype)
}

func (s *Session) decompComment(r *request) (any, *wire.Error) {
	return s.decompEdit(r, wire.TypeDecompComment)
}

func (s *Session) decompEdit(r *request, op string) (any, *wire.Error) {
	req, werr := decode[wire.DecompEditRequest](r.req.Payload)
	if werr != nil {
		return nil, werr
	}
	client, werr := s.writableDecomp()
	if werr != nil {
		return nil, werr
	}

	value := strings.TrimSpace(req.Value)
	// Empty is a comment being removed, and is the only edit with no value.
	if value == "" && op != wire.TypeDecompComment {
		what := "a name"
		if op == wire.TypeDecompRetype {
			what = "a type"
		}
		return nil, wire.NewError(wire.CodeBadRequest, what+" is required")
	}
	if op == wire.TypeDecompRename {
		if werr := checkName(value); werr != nil {
			return nil, werr
		}
	}

	bias, biasFrom := s.decompBias(r, client)
	edit := ghidra.Edit{
		Kind:   req.Kind,
		Symbol: req.Symbol,
		Name:   req.Name,
		Value:  value,
		Author: author(req.Author),
	}
	if edit.Function, werr = toGhidraAddr(req.Function, bias, "function"); werr != nil {
		return nil, werr
	}
	if req.Address != "" {
		if edit.Address, werr = toGhidraAddr(req.Address, bias, "address"); werr != nil {
			return nil, werr
		}
	}
	return s.applyDecompEdit(r, client, op, edit, bias, biasFrom, true)
}

// decompUndoLast reverses the last edit.
//
// It goes through the same path as any other edit — same guard, same
// transaction, same save — rather than reaching for Ghidra's undo, which
// saving clears (finding 33). The undone edit is popped whether or not it
// succeeds: an inverse that fails will fail again, and leaving it on the
// journal makes undo a wall rather than a step back.
func (s *Session) decompUndoLast(r *request) (any, *wire.Error) {
	req, werr := decode[wire.DecompUndoRequest](r.req.Payload)
	if werr != nil {
		return nil, werr
	}
	client, werr := s.writableDecomp()
	if werr != nil {
		return nil, werr
	}
	if len(s.decomp.journal) == 0 {
		return nil, wire.NewError(wire.CodeBadRequest, "nothing to undo")
	}
	// A run is reversed newest-first, one edit at a time, through the same path
	// a single undo takes. Not because it is tidy: each inverse was computed
	// against the state the edit left behind, so applying them out of order
	// would put a name back that something later had already renamed.
	want := 1
	if req.Run != "" {
		top := s.topRun()
		if top == nil || top.ID != req.Run {
			return nil, wire.NewError(wire.CodeBadRequest,
				"that run is not at the top of the journal; only the most "+
					"recent one can be undone")
		}
		want = top.Count
	}

	bias, biasFrom := s.decompBias(r, client)
	var out wire.DecompEdit
	undone := 0
	for ; undone < want && len(s.decomp.journal) > 0; undone++ {
		last := s.decomp.journal[len(s.decomp.journal)-1]
		s.decomp.journal = s.decomp.journal[:len(s.decomp.journal)-1]

		res, werr := s.applyDecompEdit(r, client, last.op, last.edit, bias, biasFrom, false)
		if werr != nil {
			if undone == 0 {
				return nil, werr
			}
			// Part way through a run. What has been reversed stays reversed —
			// re-applying it would need an inverse of an inverse, and the
			// journal holds no such thing — so this reports how far it got
			// rather than pretending either outcome.
			out.Warning = fmt.Sprintf("undid %d of %d, then: %s",
				undone, want, werr.Message)
			break
		}
		out = res.(wire.DecompEdit)
		out.Did = last.did
	}
	if undone > 1 {
		out.Did = fmt.Sprintf("undid %d edits", undone)
	}
	out.CanUndo = len(s.decomp.journal) > 0
	out.Run = s.topRun()
	return out, nil
}

// applyDecompEdit is the one path an edit takes, undo included.
//
// record is false for an undo, which must not push its own inverse: it would
// make the journal a toggle between two states rather than a history.
func (s *Session) applyDecompEdit(r *request, client *ghidra.Client, op string,
	edit ghidra.Edit, bias int64, biasFrom string, record bool) (any, *wire.Error) {

	var res *ghidra.EditResult
	var err error
	switch op {
	case wire.TypeDecompRetype:
		res, err = client.Retype(r.ctx, edit)
	case wire.TypeDecompComment:
		res, err = client.Comment(r.ctx, edit)
	default:
		res, err = client.Rename(r.ctx, edit)
	}
	if err != nil {
		var gerr *ghidra.Error
		if asGhidraError(err, &gerr) {
			// Ghidra's own message is the informative one — which name is
			// duplicated, what is wrong with a type string — so it is passed
			// through rather than summarised.
			s.decompLog(wire.DecompLogWarn, "%s: %s", op, gerr.Msg)
			return nil, wire.NewError(wire.CodeBadRequest, gerr.Msg)
		}
		s.decompLog(wire.DecompLogError, "%s: %v", op, err)
		return nil, wire.NewError(wire.CodeInternal, err.Error())
	}

	did := describeEdit(op, edit, res)
	if record {
		s.pushUndo(op, edit, res)
	}
	// A rename changes a name the index is keyed on, so the index has to go.
	// Dropped for every edit rather than only for a rename: applying a
	// prototype renames the function too, and a type on a global can define
	// data where there was none and give a label a shape the pane shows.
	s.forgetDecompIndex()
	s.decompLog(wire.DecompLogInfo, "%s", did)

	out := wire.DecompEdit{
		Function: s.renderDecomp(res.Function, bias, biasFrom, s.currentPC()),
		Did:      did,
		Warning:  res.Warning,
		CanUndo:  len(s.decomp.journal) > 0,
		Run:      s.topRun(),
	}
	// Broadcast as well as reply. One server serves however many browser tabs
	// are open on it, and an edit changes more than the pane it was made in:
	// the call stack shows these names, so does the symbol list, and a new
	// prototype changes how every caller decompiles.
	s.emit(wire.EventDecompEdited, map[string]any{})
	return out, nil
}

// pushUndo records the inverse of an edit that has just been made.
//
// Keyed on the name the symbol answers to *now* and with no symbol id at all,
// because the edit has renumbered the ids of everything around it (finding 34)
// and the name is the key that survived.
func (s *Session) pushUndo(op string, edit ghidra.Edit, res *ghidra.EditResult) {
	var inverse ghidra.Edit
	var did string
	if op == wire.TypeDecompComment {
		// A comment is addressed by where it hangs, and nothing about that
		// moved, so the inverse is this edit with the old text — including no
		// text at all, which removes a comment that was just added. That case
		// is the common one and is exactly the one the guard below would drop.
		inverse = edit
		inverse.Value = res.Was
		did = "put the previous comment back"
		if res.Was == "" {
			did = "removed the comment again"
		}
	} else {
		if res.Was == "" {
			// Nothing to go back to. A rename of something the sidecar could
			// not describe is better left un-undoable than undone to an empty
			// name.
			return
		}
		inverse = edit
		inverse.Symbol = ""
		inverse.Name = res.Now
		inverse.Value = res.Was

		back := res.Was
		if op == wire.TypeDecompRetype && edit.Kind == ghidra.EditFunction {
			back = "its previous prototype"
		}
		did = fmt.Sprintf("put %s back to %s", nameOf(res.Now, edit), back)
	}
	s.decomp.journal = append(s.decomp.journal, decompUndo{
		op:     op,
		edit:   inverse,
		did:    did,
		run:    s.runFor(edit.Author),
		author: edit.Author,
		at:     time.Now(),
	})
	if len(s.decomp.journal) > maxUndo {
		s.decomp.journal = s.decomp.journal[len(s.decomp.journal)-maxUndo:]
	}
}

// runFor puts this edit in the run above it, or starts a new one.
//
// Same author and close enough in time is the same run. The alternative — the
// client naming its own run — was rejected because a client cannot see the
// boundary either: an agent does not know when it has finished thinking, and
// a person never says.
func (s *Session) runFor(author string) string {
	if n := len(s.decomp.journal); n > 0 {
		last := s.decomp.journal[n-1]
		if last.author == author && time.Since(last.at) < runGap {
			return last.run
		}
	}
	s.decomp.runSeq++
	return "r" + strconv.FormatUint(s.decomp.runSeq, 10)
}

// topRun describes the run at the head of the journal, which is the only one a
// client is offered: undoing out of order would apply an inverse to a state it
// was not computed against.
func (s *Session) topRun() *wire.DecompRun {
	n := len(s.decomp.journal)
	if n == 0 {
		return nil
	}
	run := s.decomp.journal[n-1].run
	count := 0
	for i := n - 1; i >= 0 && s.decomp.journal[i].run == run; i-- {
		count++
	}
	return &wire.DecompRun{
		ID:     run,
		Author: authorOut(s.decomp.journal[n-1].author),
		Count:  count,
	}
}

// authorOut is the wire's spelling of an author, so that the sidecar's word for
// it is not the one on the protocol.
func authorOut(a string) string {
	if a == ghidra.AuthorAgent {
		return wire.DecompAuthorAgent
	}
	return ""
}

func describeEdit(op string, edit ghidra.Edit, res *ghidra.EditResult) string {
	subject := nameOf(res.Was, edit)
	if op == wire.TypeDecompComment {
		// Where, not which address: the addresses in this layer are Ghidra's,
		// and quoting one at a user reading a running program would name a
		// place they cannot find.
		//
		// The name comes from the re-decompiled function rather than from the
		// request, because a comment is addressed by where it hangs and a
		// caller has no reason to send a name at all — an agent that did not
		// left every line of the log saying "a line of it".
		of := edit.Name
		if res.Function != nil && res.Function.Name != "" {
			of = res.Function.Name
		}
		where := "a line of " + nameOf(of, ghidra.Edit{})
		if edit.Kind == ghidra.EditFunction {
			where = "the function " + nameOf(of, ghidra.Edit{})
		}
		if edit.Value == "" {
			return "removed the comment on " + where
		}
		if res.Was != "" {
			return "changed the comment on " + where
		}
		return "commented " + where
	}
	if op == wire.TypeDecompRetype {
		if edit.Kind == ghidra.EditFunction {
			return fmt.Sprintf("gave %s the prototype %s", res.Now, edit.Value)
		}
		// Now, not Was: for a retype the sidecar reports the previous *type* in
		// Was, so using it as the subject described the edit as "made
		// undefined8 a int" — the old type where the name belongs.
		return fmt.Sprintf("made %s a %s", nameOf(res.Now, edit), edit.Value)
	}
	return fmt.Sprintf("renamed %s to %s", subject, edit.Value)
}

// nameOf prefers what the sidecar reported over what the client claimed: when
// an id matched and the name did not, the sidecar's is the true one.
func nameOf(reported string, edit ghidra.Edit) string {
	if reported != "" {
		return reported
	}
	if edit.Name != "" {
		return edit.Name
	}
	return "it"
}

// writableDecomp answers with the decompiler, or with the reason there is
// nothing to edit.
func (s *Session) writableDecomp() (*ghidra.Client, *wire.Error) {
	s.decomp.mu.Lock()
	client, writable, state, errText := s.decomp.client, s.decomp.writable,
		s.decomp.state, s.decomp.err
	s.decomp.mu.Unlock()

	if client == nil {
		msg := "the decompiler is not ready"
		switch state {
		case wire.DecompStarting:
			msg = "the decompiler is still starting"
		case wire.DecompFailed:
			msg = "the decompiler failed: " + errText
		case "", wire.DecompOff:
			msg = "no decompiler is configured"
		}
		return nil, wire.NewError(wire.CodeNotReady, msg)
	}
	if !writable {
		return nil, wire.NewError(wire.CodeBadRequest,
			"this Ghidra project is yours, and gdb-wui does not write to it. "+
				"Names and types can only be changed in the project it imported "+
				"itself, which is the one it uses when -ghidra-project is not given.")
	}
	return client, nil
}

// toGhidraAddr turns a runtime address into the link-time one the decompiler
// works in. The same translation decomp.function does, and for the same reason:
// on a position-independent executable the two differ by however far the loader
// moved the program, and an edit addressed in the wrong coordinates edits a
// different function.
func toGhidraAddr(addr string, bias int64, what string) (string, *wire.Error) {
	n, err := parseAddress(strings.TrimSpace(addr))
	if err != nil {
		return "", wire.NewError(wire.CodeBadRequest,
			fmt.Sprintf("%s %q is not an address", what, addr))
	}
	return fmt.Sprintf("0x%x", uint64(int64(n)-bias)), nil
}

// checkName rejects what Ghidra would take and a reader would regret.
//
// Ghidra accepts almost anything as a symbol name, spaces and punctuation
// included, and the result decompiles into text nobody can copy into gdb. This
// is not a C identifier check — Ghidra's own names are full of them — but it
// does insist on something that could be one.
func checkName(name string) *wire.Error {
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '_', r == '$', r == '.', r == ':', r == '@':
		default:
			return wire.NewError(wire.CodeBadRequest, fmt.Sprintf(
				"%q is not usable as a name: %q is not allowed in one", name, r))
		}
	}
	if name[0] >= '0' && name[0] <= '9' {
		return wire.NewError(wire.CodeBadRequest,
			fmt.Sprintf("%q starts with a digit, which no name may", name))
	}
	return nil
}
