package ghidra

// The decompilation schema, as produced by scripts/DecompJson.java and
// documented in docs/decompilation.md. Changing either without the other is
// what TestSchemaMatchesJavaSource exists to catch.

// Schema is the version this package understands. A document announcing a
// different one is refused rather than partially decoded: a cached sidecar
// outlives the code that wrote it, and guessing at unknown fields renders the
// wrong thing instead of saying it cannot.
const Schema = 2

// Program identifies what was decompiled.
type Program struct {
	Name   string `json:"name"`
	Path   string `json:"path"`
	Format string `json:"format"`
	// SHA256 is the cache key and the mismatch guard. Keyed on a path, a cache
	// would serve a stale decompilation of a rebuilt binary; worse, it would
	// let someone read one build while debugging another.
	SHA256       string `json:"sha256"`
	LanguageID   string `json:"languageId"`
	CompilerSpec string `json:"compilerSpec"`
	PointerSize  int    `json:"pointerSize"`
	// ImageBase is Ghidra's link-time base. gdb's addresses are not these; the
	// bias is computed from a symbol present in both, not from this field. It
	// is here because a consumer cannot notice the problem without it.
	ImageBase string `json:"imageBase"`
}

// Function is one decompiled function.
type Function struct {
	Name      string `json:"name"`
	Entry     string `json:"entry"`
	BodyStart string `json:"bodyStart"`
	BodyEnd   string `json:"bodyEnd"`
	Signature string `json:"signature"`
	// Source is where the name came from, in Ghidra's vocabulary:
	// USER_DEFINED, ANALYSIS, IMPORTED or DEFAULT. See SourceUser and friends.
	Source    string `json:"source"`
	Frame     Frame  `json:"frame"`
	Variables []Var  `json:"variables"`
	// Globals are the module-scope symbols the function touches. A separate
	// map in Ghidra, and the readable half of the picture: a fixed address is
	// valid at every pc, unlike a register, and needs no frame, unlike a stack
	// slot.
	Globals   []Global `json:"globals"`
	LineCount int      `json:"lineCount"`
	Text      string   `json:"text"`
	Lines     []Line   `json:"lines"`
	// CommentLines are the lines of Text that are wholly comment, with the
	// address each annotates. Taken from the markup rather than from the text:
	// a decompiled `puts("/* x */")` defeats any prefix test, and a long
	// comment is wrapped across lines of which only the first would match one.
	CommentLines []CommentLine `json:"commentLines,omitempty"`
	// Comments are the comments stored against this function, as typed. Not
	// the same thing as CommentLines: those say where the decorated, wrapped
	// rendering ended up, and this is the text an editor has to be given.
	Comments []Comment `json:"comments,omitempty"`
}

// Comment kinds. Two, because the decompiler displays two.
const (
	// CommentPre is printed above the statement generated from its address.
	CommentPre = "pre"
	// CommentPlate is on the entry point and is printed as the function's
	// header comment.
	CommentPlate = "plate"
)

// CommentLine is one rendered line that is wholly comment.
type CommentLine struct {
	// N is 1-based into Text.
	N int `json:"n"`
	// Addr is the address the comment annotates, before any bias. Empty for a
	// decompiler warning, which belongs to no address.
	Addr string `json:"addr,omitempty"`
}

// Ghidra's source types, as the sidecar spells them. Ghidra's own analysers
// also produce ANALYSIS names — a demangler's, for one — so it means "inferred
// rather than stated" and not "written by an agent". A consumer that claimed
// the stronger reading would be crediting a guess to whoever last ran an agent.
const (
	SourceUser     = "USER_DEFINED"
	SourceAnalysis = "ANALYSIS"
	SourceImported = "IMPORTED"
	SourceDefault  = "DEFAULT"
)

// AuthorAgent marks an edit as something other than a person typing.
//
// It decides two things in the sidecar: a name is recorded as ANALYSIS rather
// than USER_DEFINED, and a comment is bookmarked so its author outlives the
// session. Nothing else about the edit changes — an agent goes through the same
// guards, the same transaction and the same journal.
const AuthorAgent = "agent"

// Comment is one note stored in the program's listing.
type Comment struct {
	// Addr is Ghidra's link-time address, before any bias.
	Addr string `json:"addr"`
	Kind string `json:"kind"`
	Text string `json:"text"`
	// Author is AuthorAgent when a bookmark marks this comment as an agent's,
	// and empty when a person wrote it. A comment carries no source type of its
	// own, so this is the only record there is.
	Author string `json:"author"`
}

// Line maps one line of Text to the addresses its tokens carry.
//
// Addrs is a set, not a range, and that is the whole design: a decompiled
// line's addresses are routinely disjoint and consecutive lines interleave, so
// a min/max range would claim instructions belonging to a different line.
type Line struct {
	// N is 1-based into Text.
	N     int      `json:"n"`
	Addrs []string `json:"addrs"`
}

// Frame is what Ghidra believes about the stack layout.
//
// Needed because a stack variable's offset is relative to Ghidra's frame base
// — the stack pointer at function entry — and not to any register gdb knows.
// Recovering that base is per-ABI: $rbp+8 on x86-64 with a frame pointer,
// $sp+Size on MIPS64, $sp-SPDepth on ARM. See docs/decompilation.md.
type Frame struct {
	Size                int  `json:"size"`
	LocalSize           int  `json:"localSize"`
	ParamOffset         int  `json:"paramOffset"`
	ReturnAddressOffset int  `json:"returnAddressOffset"`
	GrowsNegative       bool `json:"growsNegative"`
	// SPDepth is where the stack pointer sits relative to the frame base over
	// the body of the function, negative on a stack that grows down. It is the
	// prologue's whole effect, which Size is not: Size is derived from the
	// variables Ghidra found, so it understates a frame whose lowest slot is
	// never touched.
	//
	// A pointer because absent and zero are different answers. Nil is "Ghidra
	// could not settle on one", which happens when the stack pointer moves
	// through the body; zero is a function that never moves it at all.
	SPDepth *int `json:"spDepth,omitempty"`
}

// Storage kinds. They are not equally useful and the difference has to be
// shown rather than hidden.
const (
	// StorageStack is a real location, once the frame base is reconciled.
	StorageStack = "stack"
	// StorageRegister is a real location, but only near Var.PC: in optimised
	// code the decompiler packs many variables into one register.
	StorageRegister = "register"
	// StorageUnique is a decompiler temporary. It exists nowhere in the
	// machine and can never be shown a value.
	StorageUnique = "unique"
	// StorageOther is anything else, in Ghidra's own spelling.
	StorageOther = "other"
)

// Var is one local or parameter.
type Var struct {
	Name string `json:"name"`
	// ID addresses this variable for an edit. A string rather than a number
	// because a decompiler-only symbol's id is around 4.6e18, which does not
	// survive a round trip through a JavaScript number; nothing does arithmetic
	// on it. Empty from a sidecar written before edits existed, which is why
	// the name is also a key. See Edit.
	ID string `json:"id"`
	// Source is where the name came from. Empty means there is no database
	// symbol at all — the decompiler invented this one for this decompilation,
	// which is not the same as a name nobody has touched.
	Source string `json:"source"`
	Type   string `json:"type"`
	Size   int    `json:"size"`
	Param  bool   `json:"param"`
	// PC bounds a register variable's validity. Empty for stack storage.
	PC      string  `json:"pc"`
	Storage Storage `json:"storage"`
}

// Storage is where a variable lives.
type Storage struct {
	Kind string `json:"kind"`
	// Offset is set for StorageStack, relative to Ghidra's frame base.
	Offset int `json:"offset"`
	// Register is set for StorageRegister, in Ghidra's spelling (upper case).
	Register string `json:"register"`
	// Text is Ghidra's own rendering, for StorageOther.
	Text string `json:"text"`
}

// Global is a module-scope symbol with a fixed address.
type Global struct {
	Name string `json:"name"`
	Type string `json:"type"`
	Size int    `json:"size"`
	// Address is Ghidra's link-time address, before any bias.
	Address string `json:"address"`
}

// FunctionRef is one entry in the browsable index: enough to list and to ask
// for, without paying to decompile.
type FunctionRef struct {
	Name  string `json:"name"`
	Entry string `json:"entry"`
	Thunk bool   `json:"thunk"`
}

// DataList is the reply to Data.
type DataList struct {
	Total  int       `json:"total"`
	Offset int       `json:"offset"`
	Data   []DataRef `json:"data"`
}

// DataRef is one module-scope label: a name, where it is, and what shape it has
// been given. Address is in Ghidra's coordinates like every other address the
// sidecar reports.
type DataRef struct {
	Name    string `json:"name"`
	Address string `json:"address"`
	// Type is the data type defined at the address, empty when the bytes are
	// still undefined — which is the ordinary state of a global nobody has
	// looked at yet, and not a failure.
	Type string `json:"type,omitempty"`
	// Length is how far the label runs, in bytes, and is zero when the bytes
	// are undefined. Zero means "this address, and nothing known about what
	// follows it" rather than one byte: Ghidra represents an unanalysed byte as
	// one undefined item, so a 1954-byte table nobody has typed yet would
	// otherwise claim to be a single byte.
	Length int `json:"length,omitempty"`
}

// FunctionName is one address and the function Ghidra says it falls in.
//
// Addr echoes what was asked, so a caller can match answers to questions
// without arithmetic: an address in no function is omitted, so the reply is
// shorter than the request whenever a stack has frames outside the program.
type FunctionName struct {
	Addr string `json:"addr"`
	Name string `json:"name"`
	// Entry is the function's first address, so a caller can render the offset
	// into it rather than a bare name that is true of a hundred instructions.
	Entry     string `json:"entry"`
	Signature string `json:"signature,omitempty"`
	Thunk     bool   `json:"thunk,omitempty"`
}

// NameList is the reply to Names.
type NameList struct {
	Names []FunctionName `json:"names"`
}

// Edit kinds. What a user points at decides which Ghidra API can change it,
// and the three are not interchangeable.
const (
	// EditFunction is the function itself: its name, or its whole prototype.
	EditFunction = "function"
	// EditVariable is a local or a parameter, addressed by Var.ID.
	EditVariable = "variable"
	// EditGlobal is a module-scope symbol, addressed by its address — which,
	// unlike a symbol id, nothing renumbers.
	EditGlobal = "global"
	// EditLine is one line of the recovered C, addressed by the address it was
	// generated from. Only Comment takes it: a line has no name and no type.
	EditLine = "line"
)

// Edit is one change to the decompiler's own database.
//
// Function is always set, even for a global: it is the function to decompile
// again for the reply, which is what the caller is looking at.
type Edit struct {
	// Kind is one of the Edit* constants.
	Kind string
	// Function is the entry address of the function on screen, in Ghidra's
	// coordinates.
	Function string
	// Symbol is Var.ID. Optional, and only consulted when Name is empty: an
	// edit renumbers the ids of the symbols it did not touch, so a caller's id
	// is routinely one edit stale, and a stale id resolves to a neighbour
	// rather than to nothing.
	Symbol string
	// Name is the symbol's current name, and the key an edit is resolved by. A
	// name the function no longer has is a stale view, and is reported as one
	// rather than guessed at.
	Name string
	// Address locates an EditGlobal, in Ghidra's coordinates.
	Address string
	// Value is the new name for a rename, the new type — a whole C prototype
	// for EditFunction — for a retype, or the text for a comment. Empty is a
	// valid comment and means remove it; it is rejected for the other two.
	Value string
	// Author is AuthorAgent for an edit made by something other than the person
	// at the keyboard, and empty otherwise.
	Author string
}

// EditResult is what one edit produced.
type EditResult struct {
	// Function is the whole function decompiled again, not an acknowledgement.
	Function *Function
	// Warning is set when the edit succeeded and the user still has to be told
	// something — a name that is now ambiguous, an edit that could not be
	// saved. Neither is a reason to report failure.
	Warning string
	// Was is the value before the edit: the old name, or the old type, or the
	// old prototype. Now is the name the symbol answers to afterwards, which a
	// retype of a function changes. Between them, the inverse edit.
	Was string
	Now string
}

// FunctionList is the reply to Functions.
type FunctionList struct {
	Total     int           `json:"total"`
	Offset    int           `json:"offset"`
	Functions []FunctionRef `json:"functions"`
}
