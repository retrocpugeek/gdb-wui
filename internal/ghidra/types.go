package ghidra

// The decompilation schema, as produced by scripts/DecompJson.java and
// documented in docs/decompilation.md. Changing either without the other is
// what TestSchemaMatchesJavaSource exists to catch.

// Schema is the version this package understands. A document announcing a
// different one is refused rather than partially decoded: a cached sidecar
// outlives the code that wrote it, and guessing at unknown fields renders the
// wrong thing instead of saying it cannot.
const Schema = 1

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
// $sp+Size on MIPS64. See docs/decompilation.md.
type Frame struct {
	Size                int  `json:"size"`
	LocalSize           int  `json:"localSize"`
	ParamOffset         int  `json:"paramOffset"`
	ReturnAddressOffset int  `json:"returnAddressOffset"`
	GrowsNegative       bool `json:"growsNegative"`
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
	Name  string `json:"name"`
	Type  string `json:"type"`
	Size  int    `json:"size"`
	Param bool   `json:"param"`
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

// FunctionList is the reply to Functions.
type FunctionList struct {
	Total     int           `json:"total"`
	Offset    int           `json:"offset"`
	Functions []FunctionRef `json:"functions"`
}
