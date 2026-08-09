package wire

// Locating a place by name.

const TypeGotoLocate = "goto.locate"

// GotoLocateRequest asks where something is.
//
// Target is whatever a user typed into the go-to box: a symbol, an address, a
// gdb expression, or a FILE:LINE. Resolving it here rather than in the browser
// is what keeps the four centre views agreeing about where "walk" is — and gdb
// is the only thing that knows a running program's load bias, so a client that
// resolved names against the symbol table would be wrong for every
// position-independent executable the moment it started.
type GotoLocateRequest struct {
	Target  string `json:"target"`
	Thread  int    `json:"thread,omitempty"`
	Frame   int    `json:"frame,omitempty"`
	StopSeq uint64 `json:"stopSeq,omitempty"`
}

// GotoLocation is the reply to goto.locate.
//
// Every field is optional except Target, because the answer is genuinely
// partial in ordinary cases: a stripped binary's symbol has an address and no
// source line, and a line in a header that generated no code has a file and no
// address. The view that asked decides whether what came back is enough for it.
type GotoLocation struct {
	// Target is echoed, so a late reply can be matched to what was typed.
	Target string `json:"target"`
	// Address is hex, empty when only a source position could be established.
	Address string `json:"address,omitempty"`
	Addr    uint64 `json:"addr,omitempty"`
	// Func is the function containing the address, when a symbol covers it.
	Func string `json:"func,omitempty"`
	// Source is where the address lives in the program's source, when it has
	// debug info. Nil is the ordinary answer for a stripped binary and for
	// data, not a failure.
	Source *SourceRef `json:"source,omitempty"`
}
