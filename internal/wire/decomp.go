package wire

import "strings"

// Decompilation: recovered C shown beside a live session, for a binary with no
// source. gdb stays in charge — this is a view, not a second debugger.
//
// The producer is Ghidra, running as a separate process that gdb-wui
// supervises. Everything here is a projection of the sidecar schema documented
// in docs/decompilation.md.

// Request types.
const (
	TypeDecompStatus   = "decomp.status"
	TypeDecompFunction = "decomp.function"
	TypeDecompNames    = "decomp.names"
	TypeDecompRename   = "decomp.rename"
	TypeDecompRetype   = "decomp.retype"
	TypeDecompComment  = "decomp.comment"
	TypeDecompUndo     = "decomp.undo"
)

// Events.
const (
	// EventDecompLog carries one line about what the decompiler is doing, for
	// the log pane. Unlike the raw MI stream it is not behind a flag: the
	// volume is one line per human-paced operation, and the alternative is a
	// pane that shows "starting" for a minute with no way to tell whether
	// anything is happening.
	EventDecompLog = "decompLog"
	// EventDecompEdited reports that a name or a type in the decompiler's
	// database changed. Broadcast rather than left to the reply, because one
	// server serves however many browser tabs are open on it and they all show
	// the old name until told otherwise — and because an edit is not local to
	// what was edited: changing a function's prototype changes how every
	// caller decompiles.
	EventDecompEdited = "decompEdited"
	// EventDecompChanged reports that the decompiler's availability or the
	// program it holds has changed — it started, it died, or a different
	// executable was loaded. Clients refetch status rather than being sent it,
	// because most of them are not showing the pane and do not care.
	EventDecompChanged = "decompChanged"
)

// Decompiler states, reported by decomp.status.
const (
	// DecompOff means no Ghidra was configured or found. Not an error: the
	// feature is optional and the UI says how to enable it.
	DecompOff = "off"
	// DecompStarting means the process is coming up. Startup is seconds for a
	// project that already exists and minutes when a binary has to be
	// analysed first, so this is a state a client will genuinely observe.
	DecompStarting = "starting"
	// DecompReady means functions can be decompiled.
	DecompReady = "ready"
	// DecompFailed means startup or the process itself failed. Error says why.
	DecompFailed = "failed"
)

// DecompStatus is the reply to decomp.status and describes what the pane can
// offer right now.
type DecompStatus struct {
	// State is one of the Decomp* constants above.
	State string `json:"state"`
	// Error explains DecompFailed. Empty otherwise.
	Error string `json:"error,omitempty"`
	// GhidraVersion is part of the cache key as well as a display string: two
	// Ghidra releases decompile differently.
	GhidraVersion string `json:"ghidraVersion,omitempty"`
	// Program identifies what the decompiler has open.
	Program *DecompProgram `json:"program,omitempty"`
	// FunctionCount is how many functions it knows about.
	FunctionCount int `json:"functionCount,omitempty"`
	// Editable reports whether names and types can be changed. False for a
	// project the user named with -ghidra-project: that one holds their own
	// work and gdb-wui only reads it. A client shows the menu items either way
	// and disables them with the reason, because an item that is absent
	// teaches nobody that the feature exists.
	Editable bool `json:"editable,omitempty"`
	// Undo describes the run at the top of the journal, so a client can offer
	// "undo everything the agent just wrote" without asking for it first. Nil
	// when nothing has been edited.
	Undo *DecompRun `json:"undo,omitempty"`
	// Mismatch is set when the decompiler's program is not the binary gdb has
	// loaded. It is a warning rather than a refusal because the two builds are
	// often the same code — a stripped and an unstripped link of one program
	// share every address — but reading a decompilation of a different build
	// than the one being debugged is a confidently wrong answer, and the user
	// has to be told which they are looking at.
	Mismatch string `json:"mismatch,omitempty"`
}

// Decompiler log levels. The pane colours them; nothing else reads them.
const (
	DecompLogInfo  = "info"
	DecompLogWarn  = "warn"
	DecompLogError = "error"
)

// DecompLog is one line of decompiler activity.
type DecompLog struct {
	Text  string `json:"text"`
	Level string `json:"level,omitempty"`
	// Millis times an operation that finished. Zero for anything else. Kept
	// separate from Text so a client can render it consistently rather than
	// parsing it back out.
	Millis int64 `json:"millis,omitempty"`
}

// DecompProgram identifies the decompiled binary.
type DecompProgram struct {
	Name       string `json:"name"`
	SHA256     string `json:"sha256,omitempty"`
	LanguageID string `json:"languageId,omitempty"`
	// ImageBase is Ghidra's link-time base. It is not how the client computes
	// the runtime bias — that comes from a symbol present in both — but a
	// client cannot notice the problem without it.
	ImageBase   string `json:"imageBase,omitempty"`
	PointerSize int    `json:"pointerSize,omitempty"`
}

// DecompFunctionRequest asks for one decompiled function.
//
// Target is a function name or any address inside one, and the address form is
// the important one: a caller holding a program counter should not have to
// work out which function it is in. Empty means the frame gdb has selected,
// which is what the pane wants on every stop.
type DecompFunctionRequest struct {
	Target  string `json:"target,omitempty"`
	Thread  int    `json:"thread,omitempty"`
	Frame   int    `json:"frame,omitempty"`
	StopSeq uint64 `json:"stopSeq,omitempty"`
}

// DecompFunction is the reply: recovered C, and enough to relate it to what
// gdb is doing.
type DecompFunction struct {
	Name      string `json:"name"`
	Signature string `json:"signature,omitempty"`
	// Source is where the function's name came from: one of the DecompSource
	// constants. A stripped binary's functions are DecompSourceGhidra until
	// somebody or something names them.
	Source string `json:"source,omitempty"`
	// Entry, BodyStart and BodyEnd are Ghidra addresses, before Bias.
	Entry     string `json:"entry"`
	BodyStart string `json:"bodyStart,omitempty"`
	BodyEnd   string `json:"bodyEnd,omitempty"`
	// Text is the whole function. Line n of it is line n of Lines.
	Text  string       `json:"text"`
	Lines []DecompLine `json:"lines"`
	// CommentLines are the lines of Text that are wholly comment, so a client
	// can draw them as comment and let one be edited by pointing at it. The
	// decompiler's own markup says which, rather than the text: `puts("/* x
	// */")` defeats any prefix test, and a long comment is wrapped across lines
	// of which only the first would match.
	CommentLines []DecompCommentLine `json:"commentLines,omitempty"`
	// Comments are the comments stored against this function, as typed —
	// undecorated and unwrapped, which is what an editor has to be prefilled
	// with. Addresses are runtime, like everything else here.
	Comments []DecompComment `json:"comments,omitempty"`
	Vars     []DecompVar     `json:"vars,omitempty"`
	Frame    *DecompFrame    `json:"frame,omitempty"`
	// Bias is what to add to a Ghidra address to get the one gdb shows. Zero
	// for a non-PIE loaded where it was linked, which firmware usually is.
	// Computed server-side from a symbol both sides know, never from image
	// bases, because that is the arithmetic that goes wrong silently.
	Bias int64 `json:"bias"`
	// BiasFrom names the symbol the bias was established from. Empty means it
	// could not be — a stripped image has no name gdb and Ghidra share — and
	// then Bias is zero and the addresses are link-time. A client must say so
	// rather than implying they are runtime ones.
	BiasFrom string `json:"biasFrom,omitempty"`
	// PCLine is the line the current program counter is on, 0 if none. The
	// server resolves it so every client does not reimplement the lookup —
	// including the tie-break, since about one address in five is claimed by
	// two lines on optimised code.
	PCLine int `json:"pcLine,omitempty"`
	// PCLineAmbiguous reports that more than one line claimed the pc. Shown,
	// not hidden: it is the same imprecision as stepping -O2 code with DWARF.
	PCLineAmbiguous bool `json:"pcLineAmbiguous,omitempty"`
	// PCLineApprox reports that no line claimed the pc at all and the nearest
	// preceding one was used. Prologues, spills and epilogues belong to no
	// expression, and stepping lands on them constantly; a client shows this
	// differently rather than asserting the program is on that line.
	PCLineApprox bool `json:"pcLineApprox,omitempty"`
}

// DecompNamesRequest asks which function each address falls in.
//
// The call stack of a stripped binary is a column of "?? ()": gdb has no
// symbol for any of it, and the decompiler is the only thing here that knows
// otherwise. The request takes the addresses a client is showing rather than a
// range, for the same reason mem.symbols does — a stack is a handful of
// addresses and asking about the whole program to name six frames would be
// work nobody sees.
type DecompNamesRequest struct {
	// Addresses are runtime addresses, hex. Capped server-side.
	Addresses []string `json:"addresses"`
	StopSeq   uint64   `json:"stopSeq,omitempty"`
}

// DecompName is one address and the function the decompiler puts it in.
type DecompName struct {
	// Addr echoes the address asked about.
	Addr string `json:"addr"`
	// Name is the decompiler's name for the function: FUN_0010d2b0 for one it
	// recovered, or whatever it has been renamed to in Ghidra. It is *not* a
	// symbol — a client must not present it as one.
	Name string `json:"name"`
	// Signature is the recovered prototype, "undefined8 FUN_0010d2b0(long *)".
	// Types in it are the decompiler's guesses.
	Signature string `json:"signature,omitempty"`
	// Entry is the function's first address, translated back to runtime.
	Entry string `json:"entry,omitempty"`
	// Offset is Addr - Entry, so a client can render "FUN_0010d2b0+0x1c"
	// rather than a name that is equally true of a hundred instructions.
	Offset int  `json:"offset,omitempty"`
	Thunk  bool `json:"thunk,omitempty"`
}

// DecompNames is the reply to decomp.names.
//
// An empty list is an ordinary answer, not a failure: the decompiler may be
// off, still analysing, or simply not have the code — a libc frame is not in
// the program it was given. The client leaves gdb's "??" alone in every one of
// those cases, so it needs no distinction between them here.
type DecompNames struct {
	Names []DecompName `json:"names"`
	// State is the decompiler's state, so a client that has not asked for the
	// status can still say why an empty answer is empty.
	State string `json:"state,omitempty"`
}

// Edit kinds. What the user pointed at decides which of Ghidra's APIs can
// change it, and the three are not interchangeable.
const (
	// DecompEditFunction is the function itself. For decomp.rename the value is
	// a name; for decomp.retype it is a whole C prototype, which in Ghidra also
	// renames the function.
	DecompEditFunction = "function"
	// DecompEditVariable is a local or a parameter.
	DecompEditVariable = "variable"
	// DecompEditGlobal is a module-scope symbol, addressed by Address. A global
	// has no HighSymbol behind it, so decomp.retype applies the type as data in
	// the listing at that address rather than through the decompiler's own
	// symbol table; the effect on the recovered C is the same.
	DecompEditGlobal = "global"
	// DecompEditLine is one line of the recovered C, addressed by the address
	// it was generated from. Only decomp.comment takes it: a line has no name
	// and no type, and a line that came from no address — a brace, a blank —
	// cannot be commented at all, because there is nothing to hang the comment
	// on. Those are the same lines that cannot hold a breakpoint.
	DecompEditLine = "line"
)

// DecompEditRequest is the payload of decomp.rename, decomp.retype and
// decomp.comment.
//
// The decompiler's names are guesses — FUN_0010d2b0, local_10, undefined8 —
// and correcting them is most of what makes a stripped binary readable. They go
// into the Ghidra project gdb-wui imported, so they outlive the session; a
// project the user named with -ghidra-project is refused, and the refusal says
// so.
type DecompEditRequest struct {
	// Kind is one of the DecompEdit* constants.
	Kind string `json:"kind"`
	// Function is the entry address of the function on screen, as the client
	// was given it — a runtime address, translated back here. Required for
	// every kind, a global included: it is the function decompiled again for
	// the reply.
	Function string `json:"function"`
	// Symbol is DecompVar.ID, opaque and optional. An edit renumbers the ids of
	// the symbols it did not touch, so this is routinely one edit stale — and
	// stale here does not mean it stops resolving, it means it resolves to a
	// neighbour. It is therefore consulted only when there is no Name to check
	// it against.
	Symbol string `json:"symbol,omitempty"`
	// Name is the symbol's current name, and it is the key: it is what the
	// caller pointed at, and unlike an id it does not silently become somebody
	// else. A name that is not in the function any more is refused rather than
	// applied to whatever the id now addresses, and the refusal lists what the
	// function does have, so a client working from a stale view can recover
	// without decompiling again to find out. A comment hangs on an address
	// rather than on a symbol, so for decomp.comment this carries the
	// function's name and is used only to describe the edit.
	Name string `json:"name,omitempty"`
	// Address locates a DecompEditGlobal or a DecompEditLine, runtime.
	Address string `json:"address,omitempty"`
	// Value is the new name, the new type, or the comment text. Empty is
	// refused for a name and a type, and for a comment means remove it: a
	// stored empty comment prints as a bare `/* */`, which is a mark on the
	// page that says nothing.
	Value string `json:"value"`
	// Author is DecompAuthorAgent when something other than the person at the
	// keyboard made this edit. The browser never sets it; the MCP bridge always
	// does. It changes how the edit is recorded — a name becomes an inferred
	// name rather than a stated one, and a comment is marked with its author —
	// and nothing else: an agent passes the same guards as anyone.
	//
	// It is a claim by the client, not a fact the server can check. Anything
	// that can reach this protocol could lie about it, and anything that can
	// reach this protocol can already rename whatever it likes.
	Author  string `json:"author,omitempty"`
	StopSeq uint64 `json:"stopSeq,omitempty"`
}

// DecompAuthorAgent marks an edit as an agent's rather than a person's.
const DecompAuthorAgent = "agent"

// Where a decompiler name came from. Reported so a client can say "you named
// this" rather than implying it about everything.
const (
	// DecompSourceUser is a name somebody typed.
	DecompSourceUser = "user"
	// DecompSourceInferred is a name something worked out: an agent, or one of
	// Ghidra's own analysers. The two are not distinguished, because Ghidra
	// does not distinguish them — and a client that said "an agent named this"
	// on the strength of it would be crediting a demangler's work to a model.
	DecompSourceInferred = "inferred"
	// DecompSourceSymbol is a name that came out of the binary: a symbol table,
	// or DWARF.
	DecompSourceSymbol = "symbol"
	// DecompSourceGhidra is Ghidra's own generated name — local_10, FUN_00401154.
	DecompSourceGhidra = "ghidra"
	// DecompSourceNone, the empty string, means there is no symbol behind this
	// name at all: the decompiler invented it for this decompilation and it
	// exists nowhere in the program's database. Not the same as a name nobody
	// has touched, and not shown alike.
	DecompSourceNone = ""
)

// DecompUndoRequest reverses the last edit of this session, or a whole run of
// them.
//
// gdb-wui keeps its own journal of inverse edits rather than using Ghidra's
// undo, because saving clears Ghidra's undo stack (finding 33) and every edit
// is saved: an unsaved rename lives only inside the sidecar process, and losing
// an afternoon of naming to a crash is not a trade worth making.
type DecompUndoRequest struct {
	// Run reverses every edit in one run rather than the single last edit. An
	// agent writes forty annotations in a burst and forty undos is not an undo;
	// the server groups consecutive edits by the same author into a run and
	// reports the topmost one in DecompStatus.
	Run     string `json:"run,omitempty"`
	StopSeq uint64 `json:"stopSeq,omitempty"`
}

// DecompRun describes the group of edits at the top of the undo journal, so a
// client can offer to reverse the lot rather than one at a time.
type DecompRun struct {
	// ID identifies the run in a DecompUndoRequest.
	ID string `json:"id"`
	// Author is DecompAuthorAgent for an agent's run, empty for a person's.
	Author string `json:"author,omitempty"`
	// Count is how many edits are in it.
	Count int `json:"count"`
}

// DecompEdit is the reply to decomp.rename, decomp.retype, decomp.comment and
// decomp.undo.
type DecompEdit struct {
	// Function is the whole function decompiled again. Not an acknowledgement:
	// a rename renumbers every other symbol's id and a retype reshapes the body
	// around it, so a client that patched its own copy would hold keys that no
	// longer address anything.
	Function DecompFunction `json:"function"`
	// Did describes the change in the past tense, for the status line —
	// "renamed local_10 to count".
	Did string `json:"did"`
	// Warning is set when the edit succeeded and the user still has to be told
	// something: a name that is now ambiguous, or an edit that could not be
	// saved. Ghidra accepts two functions with one name without a word.
	Warning string `json:"warning,omitempty"`
	// CanUndo reports whether anything is left on the journal, so a client can
	// disable the item rather than offer an undo that will fail.
	CanUndo bool `json:"canUndo,omitempty"`
	// Run is the group this edit was recorded in, and what to send back to undo
	// the whole of it.
	Run *DecompRun `json:"run,omitempty"`
}

// ExecStepLineRequest steps until the program counter leaves a set of
// addresses — the addresses of the line currently showing.
//
// It exists because gdb's own `next` and `step` are useless without a line
// table. With none, gdb's step range is the whole function, so "step over" in
// a binary with no debug info runs to the function's exit: measured on a
// symbols-but-no-DWARF build, `break main` then `next` lands at 0x7ffff7c2a601,
// inside libc, having returned out of main entirely.
//
// The rule is "step until the pc belongs to a different line", and it has to
// be that rather than anything simpler. A line's address set is *sparse* — the
// addresses its tokens carry, not every instruction between them — so stepping
// until the pc leaves the set ends at the first unlisted instruction, which is
// usually the second one. And a line's span is no good either: a loop header's
// addresses are genuinely disjoint, wrapped around the body, so its span covers
// the whole loop and stepping out of it would step out of the loop.
//
// Resolving "which line" uses the same rule the pc marker does, fallback
// included, so an instruction between a line's tokens maps back to that line
// and the walk continues.
//
// The client sends the map because it already holds it, which saves the server
// decompiling the function again on every step. A few kilobytes per step at
// human speeds.
type ExecStepLineRequest struct {
	// Lines is the function's map. Empty degrades to a single instruction step,
	// because with nothing to step over, one instruction is all that can be
	// claimed.
	Lines []DecompLine `json:"lines,omitempty"`
	// BodyStart and BodyEnd bound the function, so a walk that returns into a
	// caller does not keep matching by coincidence.
	BodyStart string `json:"bodyStart,omitempty"`
	BodyEnd   string `json:"bodyEnd,omitempty"`
	// Over steps over calls rather than into them.
	Over    bool   `json:"over,omitempty"`
	Thread  int    `json:"thread,omitempty"`
	StopSeq uint64 `json:"stopSeq,omitempty"`
}

// LineCount is how many lines Text has, which is not len(Lines): only lines
// carrying addresses appear there.
func (f DecompFunction) LineCount() int {
	if f.Text == "" {
		return 0
	}
	return strings.Count(strings.TrimSuffix(f.Text, "\n"), "\n") + 1
}

// DecompLine maps one line of Text to the addresses its tokens carry.
//
// Addrs is a set, not a range. A decompiled line's addresses are routinely
// disjoint and consecutive lines interleave, so a range would claim
// instructions belonging to a different line. Addresses here have Bias already
// applied, so they are directly comparable with everything else on the wire.
type DecompLine struct {
	N     int      `json:"n"`
	Addrs []string `json:"addrs"`
}

// Comment kinds, matching what the decompiler displays.
const (
	// DecompCommentPre is printed above the statement its address generated.
	DecompCommentPre = "pre"
	// DecompCommentPlate is on the entry point and is printed as the
	// function's header comment.
	DecompCommentPlate = "plate"
)

// DecompCommentLine is one rendered line that is wholly comment.
//
// Addr is what makes a comment editable by pointing at it: a comment is
// printed on its own line, which claims no addresses of its own — a comment
// line is deliberately absent from Lines, so that the program counter is never
// put on one — and without this there would be no way back from the text to
// the address the comment hangs on.
type DecompCommentLine struct {
	// N is 1-based into Text.
	N int `json:"n"`
	// Addr is the address the comment annotates, runtime. Empty for a
	// decompiler warning, which belongs to no address and is not editable.
	Addr string `json:"addr,omitempty"`
}

// DecompComment is one note stored in the Ghidra program, as typed.
//
// Sent so that editing an existing comment starts from what is there. It cannot
// be recovered from Text: the rendering is wrapped to the decompiler's print
// width and decorated with /* */, so reconstructing the original from it is
// guesswork.
type DecompComment struct {
	// Addr is the address the comment hangs on, runtime.
	Addr string `json:"addr"`
	// Kind is DecompCommentPre or DecompCommentPlate.
	Kind string `json:"kind"`
	Text string `json:"text"`
	// Author is DecompAuthorAgent when an agent wrote this note, empty when a
	// person did. A comment has no source type of its own, so the sidecar keeps
	// this beside it as a Ghidra bookmark.
	Author string `json:"author,omitempty"`
}

// Storage kinds for a decompiled variable. They are not equally useful and the
// difference has to be visible.
const (
	// DecompStorageStack is readable anywhere in the frame.
	DecompStorageStack = "stack"
	// DecompStorageRegister is readable only near PC: in optimised code the
	// decompiler packs many variables into one register.
	DecompStorageRegister = "register"
	// DecompStorageGlobal is a fixed address: valid at every pc, needing no
	// frame, and therefore the most readable kind there is.
	DecompStorageGlobal = "global"
	// DecompStorageNone covers a decompiler temporary and anything else
	// without a machine location. It can never show a value, and a UI that
	// omits these rather than blanking them is lying by omission.
	DecompStorageNone = "none"
)

// DecompVar is one local or parameter.
type DecompVar struct {
	Name string `json:"name"`
	// ID addresses this variable for an edit. Opaque, and a string rather than
	// a number because a decompiler-only symbol's id is around 4.6e18, which a
	// JavaScript number cannot hold exactly. Empty for a global, which is
	// addressed by Addr instead — nothing renumbers an address.
	ID    string `json:"id,omitempty"`
	Type  string `json:"type,omitempty"`
	Param bool   `json:"param,omitempty"`
	// Source is where the name came from: one of the DecompSource constants.
	Source string `json:"source,omitempty"`
	// Storage is one of the DecompStorage* constants.
	Storage string `json:"storage"`
	// Expr is a gdb expression for the variable's value, when one can be
	// formed. Empty when it cannot — which is most of the time for register
	// storage away from PC, and always for a temporary.
	Expr string `json:"expr,omitempty"`
	// PC bounds a register variable's validity, with Bias applied.
	PC string `json:"pc,omitempty"`
	// Addr is where a global lives, with Bias applied. Empty for anything else.
	Addr string `json:"addr,omitempty"`
}

// DecompFrame is Ghidra's view of the stack layout, passed through so a client
// can explain an expression it was given.
type DecompFrame struct {
	Size          int  `json:"size"`
	GrowsNegative bool `json:"growsNegative,omitempty"`
}
