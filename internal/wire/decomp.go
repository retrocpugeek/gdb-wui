package wire

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
)

// Events.
const (
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
	// Mismatch is set when the decompiler's program is not the binary gdb has
	// loaded. It is a warning rather than a refusal because the two builds are
	// often the same code — a stripped and an unstripped link of one program
	// share every address — but reading a decompilation of a different build
	// than the one being debugged is a confidently wrong answer, and the user
	// has to be told which they are looking at.
	Mismatch string `json:"mismatch,omitempty"`
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
	// Entry, BodyStart and BodyEnd are Ghidra addresses, before Bias.
	Entry     string `json:"entry"`
	BodyStart string `json:"bodyStart,omitempty"`
	BodyEnd   string `json:"bodyEnd,omitempty"`
	// Text is the whole function. Line n of it is line n of Lines.
	Text  string       `json:"text"`
	Lines []DecompLine `json:"lines"`
	Vars  []DecompVar  `json:"vars,omitempty"`
	Frame *DecompFrame `json:"frame,omitempty"`
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
	// Lines is the function's map. Empty degrades to a single instruction
	// step, which is honest: with nothing to step over, one instruction is the
	// most that can be claimed.
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
	Name  string `json:"name"`
	Type  string `json:"type,omitempty"`
	Param bool   `json:"param,omitempty"`
	// Storage is one of the DecompStorage* constants.
	Storage string `json:"storage"`
	// Expr is a gdb expression for the variable's value, when one can be
	// formed. Empty when it cannot — which is most of the time for register
	// storage away from PC, and always for a temporary.
	Expr string `json:"expr,omitempty"`
	// PC bounds a register variable's validity, with Bias applied.
	PC string `json:"pc,omitempty"`
}

// DecompFrame is Ghidra's view of the stack layout, passed through so a client
// can explain an expression it was given.
type DecompFrame struct {
	Size          int  `json:"size"`
	GrowsNegative bool `json:"growsNegative,omitempty"`
}
