package wire

// The memory viewer.

// Request types implemented in M7.
const (
	TypeMemRead    = "mem.read"
	TypeEvalExpr   = "eval.expr"
	TypeMemSymbols = "mem.symbols"
	TypeMemWrite   = "mem.write"
)

// MemReadRequest reads bytes.
//
// Address may be a plain address or any gdb expression — "&cfg", "$sp",
// "buf+16". Resolving expressions server-side is what lets the UI accept what a
// user would actually type instead of demanding a hex number.
type MemReadRequest struct {
	Address string `json:"address"`
	// Offset shifts the read, so a client paging through a region does not have
	// to re-evaluate the expression for every chunk.
	Offset int `json:"offset,omitempty"`
	// Count is how many bytes to read.
	Count   int    `json:"count"`
	StopSeq uint64 `json:"stopSeq,omitempty"`
}

// MemoryRange is one contiguous run of readable bytes.
type MemoryRange struct {
	Start string `json:"start"`
	// Addr is Start parsed, so the client need not.
	Addr uint64 `json:"addr"`
	// DataHex is the bytes, two lowercase hex digits each, no separators.
	DataHex string `json:"dataHex"`
}

// Memory is the reply to mem.read.
//
// Ranges rather than one buffer, because a region can be partly unmapped and
// the gap has to be visible: the viewer renders bytes it does not have as "??"
// instead of zeros, which would be a lie.
type Memory struct {
	StopSeq uint64 `json:"stopSeq"`
	// Requested echoes the expression, for the UI's address bar.
	Requested string `json:"requested,omitempty"`
	// Addr is where the read actually started, after resolving the expression
	// and applying the offset.
	Addr   uint64        `json:"addr"`
	Count  int           `json:"count"`
	Ranges []MemoryRange `json:"ranges"`
	// Unreadable is true when gdb refused the whole read. It is an ordinary
	// outcome — pointing a hex viewer at an unmapped page is how you find out
	// it is unmapped — not an error worth interrupting the user for.
	Unreadable bool `json:"unreadable,omitempty"`
}

// MemWriteRequest writes bytes.
//
// Address takes the same expressions mem.read does, so the viewer can write
// back to a row using the address it already has in hand.
type MemWriteRequest struct {
	Address string `json:"address"`
	Offset  int    `json:"offset,omitempty"`
	// DataHex is the bytes, two hex digits each, no separators — the same
	// encoding MemoryRange uses on the way out. Bytes rather than a value and a
	// width, because the hex view edits bytes and nothing else has to agree
	// about endianness.
	DataHex string `json:"dataHex"`
	StopSeq uint64 `json:"stopSeq,omitempty"`
}

// MemWrite is the reply to mem.write.
type MemWrite struct {
	StopSeq uint64 `json:"stopSeq"`
	Addr    uint64 `json:"addr"`
	Count   int    `json:"count"`
}

// MemSymbolsRequest asks which symbol each address falls in.
//
// A hex dump of raw addresses is hard to place; "cfg+0x10" says where you are.
// The client sends the addresses it is showing rather than a range, because
// the memory view is virtual: it renders a screenful out of a 4 KiB chunk and
// symbolising the whole chunk would be mostly work nobody sees.
type MemSymbolsRequest struct {
	// Addresses are hex strings. Capped server-side.
	Addresses []string `json:"addresses"`
	StopSeq   uint64   `json:"stopSeq,omitempty"`
}

// MemSymbol names one address.
type MemSymbol struct {
	Addr string `json:"addr"`
	// Name is gdb's rendering, "inspect+16" or bare "main". Empty when the
	// address is in no symbol at all, which is the ordinary case for the stack
	// and the heap.
	Name string `json:"name,omitempty"`
	// From is SymbolFromBinary — the empty string — for a name gdb produced,
	// and SymbolFromDecompiler for one only Ghidra has. A client must mark the
	// second: DAT_001a08de is plainly a guess, but a label somebody renamed is
	// not, and a column that showed the two alike would be presenting a
	// recovery as something the binary says.
	From string `json:"from,omitempty"`
}

// MemSymbols is the reply to mem.symbols. Addresses with no symbol are
// omitted, so an empty reply is a meaningful answer rather than a failure.
type MemSymbols struct {
	Symbols []MemSymbol `json:"symbols"`
}

// EvalExprRequest evaluates a gdb expression.
type EvalExprRequest struct {
	Expr    string `json:"expr"`
	Thread  int    `json:"thread,omitempty"`
	Frame   int    `json:"frame,omitempty"`
	StopSeq uint64 `json:"stopSeq,omitempty"`
}

// EvalExpr is the reply to eval.expr.
type EvalExpr struct {
	Expr  string `json:"expr"`
	Value string `json:"value"`
	// Addr is set when the value looks like an address, so the memory viewer
	// can jump straight there.
	Addr uint64 `json:"addr,omitempty"`
}
