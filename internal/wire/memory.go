package wire

// The memory viewer.

// Request types implemented in M7.
const (
	TypeMemRead  = "mem.read"
	TypeEvalExpr = "eval.expr"
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
