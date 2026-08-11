package wire

// The symbol table: the static picture of a program, as opposed to the
// stack, registers and locals, which only exist while it runs.

// Requests.
const (
	TypeSymbolsList = "symbols.list"
	TypeSymbolsLoad = "symbols.load"
)

// Symbol-loading modes.
//
// These exist because loading a program and loading its symbols are different
// acts that only coincide when gdb starts the program itself. `exe.load` uses
// -file-exec-and-symbols, which declares both "these are the symbols" and
// "this is the program to run". Against a target that is already running the
// code — an emulator stub, a process someone else started — there is nothing
// to run locally, and saying otherwise leaves Run offering to start a second
// copy.
const (
	// SymbolsReplace discards the current symbol table and installs this one.
	SymbolsReplace = "replace"
	// SymbolsAdd adds a symbol table alongside the existing ones, optionally
	// biased by an offset.
	SymbolsAdd = "add"
)

// Events.
const (
	// EventSymbolsInvalidated tells clients the symbol table they are showing
	// belongs to a program that is no longer loaded. It fires when a new
	// executable is loaded and when a typed console command changes what gdb
	// has symbols for — `file`, `add-symbol-file`, `symbol-file` — because a
	// remote-target user reaches for exactly those, and a stale pane that
	// still lists the previous program's functions would be misleading.
	EventSymbolsInvalidated = "symbolsInvalidated"
)

// Symbol kinds.
const (
	SymbolFunction = "function"
	SymbolVariable = "variable"
)

// Where an entry in the list came from.
//
// The two are not the same kind of thing and a client must not present them
// alike. A binary symbol is a fact recorded by whoever built the program; a
// decompiler name is a guess — FUN_0010e2dc, DAT_001a08de — or somebody's
// correction of one, and it exists only in the Ghidra project beside the
// binary. Everything else in the pane behaves the same for both, which is the
// point: a name you can see is a name you can go to.
const (
	// SymbolFromBinary is the program's own symbol table or its debug info. The
	// empty string, so a client written before this field existed reads every
	// entry it understands as what it always was.
	SymbolFromBinary = ""
	// SymbolFromDecompiler is a name Ghidra recovered or that was written into
	// the Ghidra project. Present only for a session with a decompiler, and
	// only for names the binary does not already have.
	SymbolFromDecompiler = "decompiler"
)

// Symbol is one entry from the program's symbol tables.
//
// Two populations end up here and they are not interchangeable. A symbol with
// debug info knows its source file and line, so it can be jumped to in the
// source view. One from the ELF symbol table alone knows only an address, and
// the only destination available for it is the disassembly. Debug reports
// which kind it is, so the UI can say so rather than offering a jump that does
// nothing.
type Symbol struct {
	Name string `json:"name"`
	// Kind is SymbolFunction or SymbolVariable.
	Kind string `json:"kind"`
	// Type is the C declaration type, when debug info supplies one.
	Type string `json:"type,omitempty"`
	// File is the project-relative source path, set only when the symbol has
	// debug info *and* the file resolves inside the project.
	File string `json:"file,omitempty"`
	// GDBPath is the path as gdb reported it, kept even when File is empty so
	// the UI can show where the symbol claims to live and the user can add a
	// substitution.
	GDBPath string `json:"gdbPath,omitempty"`
	Line    int    `json:"line,omitempty"`
	// Address is set for symbols without debug info.
	Address string `json:"address,omitempty"`
	// Debug distinguishes the two populations described above.
	Debug bool `json:"debug,omitempty"`
	// From is SymbolFromBinary or SymbolFromDecompiler. A decompiler entry
	// never has Debug, a File or a Line: Ghidra recovered it from machine code,
	// which is the situation that produced it in the first place.
	From string `json:"from,omitempty"`
}

// SymbolsListRequest asks for symbols matching a filter.
//
// The filter is applied on the server, over a cached table, rather than the
// client fetching everything once and filtering locally. A stripped firmware
// image can carry tens of thousands of symbols, and shipping all of them to
// the browser to support a search box is a lot of bytes for a list nobody
// scrolls to the end of.
type SymbolsListRequest struct {
	// Filter is a case-insensitive substring match on the name. Empty matches
	// everything.
	Filter string `json:"filter,omitempty"`
	// Kind restricts to SymbolFunction or SymbolVariable. Empty means both.
	Kind string `json:"kind,omitempty"`
	// Limit bounds the reply. Zero means the server's default.
	Limit int `json:"limit,omitempty"`
}

// SymbolsLoadRequest loads symbols without touching the executable.
type SymbolsLoadRequest struct {
	// Path is root-relative, like every other path in the protocol.
	Path string `json:"path"`
	// Mode is SymbolsReplace or SymbolsAdd. Empty means replace.
	Mode string `json:"mode,omitempty"`
	// Offset biases every address in the file, for an image that does not run
	// where it was linked — the ordinary case for bare metal. A string because
	// a 64-bit address does not survive JSON's float64, and because "0x8000"
	// is how people write one. Only meaningful with SymbolsAdd.
	Offset string `json:"offset,omitempty"`
}

// SymbolsLoaded is the reply to symbols.load.
type SymbolsLoaded struct {
	Path string `json:"path"`
	Mode string `json:"mode"`
	// Available is the size of the symbol table afterwards, so the caller can
	// tell "loaded, and found nothing" from "loaded".
	Available int `json:"available"`
}

// SymbolsList is the reply to symbols.list.
type SymbolsList struct {
	Symbols []Symbol `json:"symbols"`
	// Matched is how many symbols the filter selected, before Limit was
	// applied. The UI shows "200 of 4096" from this, so a truncated list reads
	// as truncated rather than as the whole answer.
	Matched int `json:"matched"`
	// Available is the size of the whole table.
	Available int `json:"available"`
	// Truncated reports that Limit cut the reply short.
	Truncated bool `json:"truncated,omitempty"`
	// Analysing reports that the decompiler is still importing or analysing,
	// so more names are coming. It is the difference between "this program has
	// no symbols" and "not yet": a stripped binary answers the first one truly
	// and the second one usefully, and on firmware the wait is minutes.
	Analysing bool `json:"analysing,omitempty"`
}
