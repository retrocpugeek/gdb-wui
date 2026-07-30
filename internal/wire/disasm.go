package wire

// Disassembly and instruction-level stepping.

// Request types implemented in M6.
const (
	TypeDisasmFunction = "disasm.function"
	TypeDisasmRange    = "disasm.range"

	TypeExecStepI = "exec.stepi"
	TypeExecNextI = "exec.nexti"
)

// Instruction is one disassembled instruction.
type Instruction struct {
	// Address is the instruction's address, as gdb printed it.
	Address string `json:"address"`
	// Addr is the same value parsed, so a client can compare without parsing
	// hex strings itself.
	Addr uint64 `json:"addr"`
	// Func and Offset locate the instruction within its function. Both are
	// absent for code with no symbol covering it.
	Func   string `json:"func,omitempty"`
	Offset int    `json:"offset,omitempty"`
	// Opcodes are the raw bytes, space-separated as gdb formats them.
	Opcodes string `json:"opcodes,omitempty"`
	// Text is the instruction itself, in the flavour gdb is configured for.
	Text string `json:"text"`
	// Line and Source are set only when gdb could attribute the instruction to
	// a source line — that is, with debug info. Both absent is the ordinary
	// case for a stripped binary, and the UI must render the instruction
	// anyway.
	Line   int        `json:"line,omitempty"`
	Source *SourceRef `json:"source,omitempty"`
}

// DisasmFunctionRequest disassembles the function containing an address.
type DisasmFunctionRequest struct {
	// Address is where to look; empty means the selected frame's PC.
	Address string `json:"address,omitempty"`
	Thread  int    `json:"thread,omitempty"`
	Frame   int    `json:"frame,omitempty"`
	StopSeq uint64 `json:"stopSeq,omitempty"`
}

// DisasmRangeRequest disassembles an explicit address range.
type DisasmRangeRequest struct {
	// Start and End are addresses, hex with or without 0x.
	Start   string `json:"start"`
	End     string `json:"end"`
	StopSeq uint64 `json:"stopSeq,omitempty"`
}

// Disassembly is the reply to both disassembly requests.
type Disassembly struct {
	StopSeq uint64 `json:"stopSeq"`
	// Func is the function disassembled, when there was one.
	Func string `json:"func,omitempty"`
	// Start and End bound what was returned, so a client extending the window
	// knows where to ask next.
	Start string `json:"start,omitempty"`
	End   string `json:"end,omitempty"`
	// PC is the current program counter, when it falls inside this window.
	PC string `json:"pc,omitempty"`
	// HasSource reports whether any instruction carried a source line. False
	// means a stripped binary, and the UI should not offer to jump to source.
	HasSource    bool          `json:"hasSource"`
	Instructions []Instruction `json:"instructions"`
	// Truncated reports that the request hit the instruction cap.
	Truncated bool `json:"truncated,omitempty"`
}
