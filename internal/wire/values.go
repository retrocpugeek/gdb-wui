package wire

// The value-inspection half of the protocol: locals, watches and registers.

// Request types implemented in M4.
const (
	TypeVarsLocals = "vars.locals"
	TypeVarsExpand = "vars.expand"

	TypeWatchAdd    = "watch.add"
	TypeWatchRemove = "watch.remove"
	TypeWatchList   = "watch.list"

	TypeRegsNames  = "regs.names"
	TypeRegsValues = "regs.values"

	// The writes. Separate types rather than one "set value", because the three
	// go to gdb by three different commands and fail in three different ways.
	TypeVarsAssign = "vars.assign"
	TypeRegsWrite  = "regs.write"
)

// Events added in M4.
const (
	// EventVarsInvalidated tells clients to drop every variable node they hold.
	// It is emitted when the varobjs behind them are deleted wholesale — on a
	// re-run or a new program — so a client cannot keep rendering ids that no
	// longer exist.
	EventVarsInvalidated = "varsInvalidated"
	// EventWatchesChanged is emitted when the watch list changes.
	EventWatchesChanged = "watchesChanged"
	// EventValueWritten is emitted after a successful write to a variable, a
	// register or memory. It is broadcast rather than returned, because a write
	// invalidates what *every* connected client is showing: assigning a local
	// changes the memory or the register it lives in, and a second browser
	// looking at that address would otherwise show the old bytes until the next
	// stop.
	EventValueWritten = "valueWritten"
)

// ValueWritten is the payload of valueWritten.
//
// It carries no address and no new value on purpose. Assigning a local changes
// the register or the stack slot behind it, and writing one byte can change a
// variable a client is watching, so there is no useful subset to invalidate:
// every value on screen is suspect and the panels re-read what they show. What
// the event does carry is enough for a status line, so a second browser can say
// why its numbers moved.
type ValueWritten struct {
	StopSeq uint64 `json:"stopSeq"`
	// What is "variable", "register" or "memory".
	What string `json:"what"`
	// Detail names the target, "counter" or "$rax" or "0x404060".
	Detail string `json:"detail"`
	// Value is what the target holds afterwards, as gdb renders it. Empty for a
	// memory write, where the bytes sent are the whole answer.
	Value string `json:"value,omitempty"`
}

// OptimizedOut is the value gdb reports for a variable the compiler erased. It
// is a value like any other on the wire. The UI renders it differently rather
// than hiding the variable.
const OptimizedOut = "<optimized out>"

// VarNode is one row in the variables tree.
//
// The client keys on Path, not ID. Path is a stable expression path —
// "local:cfg.items[0].name" — that survives the varobj behind it being deleted
// and recreated, which happens on every re-run and on LRU eviction. Keying on
// ID would mean the user's expansion state evaporates exactly when they are
// stepping and watching a value change, which is when they care most.
type VarNode struct {
	// Path is the stable identity. Roots are "local:<name>" or "watch:<n>".
	Path string `json:"path"`
	// ID is the gdb varobj name, empty until one has been created. A flat local
	// that has never been expanded has no varobj at all, which is what
	// --simple-values buys.
	ID string `json:"id,omitempty"`
	// Name is what to display: the field name, or the array index.
	Name string `json:"name"`
	// Expr is the full expression, used to create a varobj on expansion.
	Expr string `json:"expr,omitempty"`
	Type string `json:"type,omitempty"`
	// Value is absent for aggregates under --simple-values.
	Value string `json:"value,omitempty"`
	// NumChild is gdb's child count, which for a pointer counts the pointee.
	NumChild int `json:"numChild,omitempty"`
	// Expandable is the UI's signal to draw a twisty. It comes from the
	// *absence* of a value under --simple-values, not from a type guess.
	Expandable bool `json:"expandable"`
	// HasMore reports that gdb has more children than were returned.
	HasMore bool `json:"hasMore,omitempty"`
	// InScope is false when the frame a varobj was bound to is gone.
	InScope bool `json:"inScope"`
	// Changed marks a value that differs from the previous stop.
	Changed bool `json:"changed,omitempty"`
	// Arg marks a function argument rather than a local.
	Arg bool `json:"arg,omitempty"`
	// OptimizedOut is a convenience for the UI, derived from Value.
	OptimizedOut bool `json:"optimizedOut,omitempty"`
	// Editable reports that gdb will accept an assignment to this node, which
	// is true of scalars and pointers and false of arrays, structs and unions.
	//
	// The server answers it because the UI must decide whether to offer an edit
	// *before* one is attempted, and the only alternative — offering it on
	// everything and reporting gdb's refusal afterwards — puts a dead
	// affordance on every aggregate row in the tree.
	Editable bool `json:"editable,omitempty"`
}

// VarsLocalsRequest asks for a frame's locals.
type VarsLocalsRequest struct {
	Thread  int    `json:"thread,omitempty"`
	Frame   int    `json:"frame,omitempty"`
	StopSeq uint64 `json:"stopSeq,omitempty"`
}

// VarsLocals is the reply to vars.locals.
type VarsLocals struct {
	StopSeq   uint64    `json:"stopSeq"`
	ThreadID  int       `json:"threadId"`
	Frame     int       `json:"frame"`
	Variables []VarNode `json:"variables"`
}

// VarsExpandRequest asks for one node's children.
//
// The client sends ID when it has one and Expr when it does not — a flat local
// being expanded for the first time has no varobj yet, and creating it lazily
// is what keeps a 100k-element array from being materialised by merely existing.
type VarsExpandRequest struct {
	Path    string `json:"path"`
	ID      string `json:"id,omitempty"`
	Expr    string `json:"expr,omitempty"`
	Thread  int    `json:"thread,omitempty"`
	Frame   int    `json:"frame,omitempty"`
	StopSeq uint64 `json:"stopSeq,omitempty"`
	// From and To page through children. Zero means "the first page".
	From int `json:"from,omitempty"`
	To   int `json:"to,omitempty"`
}

// VarsExpand is the reply to vars.expand.
type VarsExpand struct {
	Path     string    `json:"path"`
	ID       string    `json:"id,omitempty"`
	StopSeq  uint64    `json:"stopSeq"`
	Children []VarNode `json:"children"`
	HasMore  bool      `json:"hasMore,omitempty"`
	// NumChild is the total gdb reports, so the UI can say "200 of 4096".
	NumChild int `json:"numChild,omitempty"`
}

// VarsAssignRequest writes a new value to one node of the variables tree.
//
// It names the node the same three ways vars.expand does — path, id,
// expression — because the row being edited may never have been expanded and
// so may have no varobj yet. The server finds or creates one, exactly as it
// does for an expansion.
type VarsAssignRequest struct {
	Path string `json:"path"`
	ID   string `json:"id,omitempty"`
	Expr string `json:"expr,omitempty"`
	// Value is a gdb expression, not a literal: "42", "0x10", "x + 1" and
	// "&counter" are all things a user may reasonably type into the cell, and
	// gdb evaluates them in the frame the node belongs to.
	Value   string `json:"value"`
	Thread  int    `json:"thread,omitempty"`
	Frame   int    `json:"frame,omitempty"`
	StopSeq uint64 `json:"stopSeq,omitempty"`
}

// VarsAssign is the reply to vars.assign.
//
// Value is what gdb reports the node holds *after* the write, which is not
// always what was typed: assigning 321 to a char gives 65, and showing the
// echoed input instead would hide the truncation until the next stop.
type VarsAssign struct {
	Path    string `json:"path"`
	ID      string `json:"id,omitempty"`
	Value   string `json:"value"`
	StopSeq uint64 `json:"stopSeq"`
}

// WatchAddRequest adds a watch expression.
type WatchAddRequest struct {
	Expr string `json:"expr"`
}

// WatchRemoveRequest removes one by its path.
type WatchRemoveRequest struct {
	Path string `json:"path"`
}

// WatchList is the reply to the watch group and the payload of
// watchesChanged.
type WatchList struct {
	StopSeq uint64    `json:"stopSeq"`
	Watches []VarNode `json:"watches"`
}

// Register is one machine register.
//
// Registers are identified by *number*, never by name: gdb's name list
// contains empty strings at stable indices, so the position in the list is the
// only reliable identity.
type Register struct {
	Number  int    `json:"number"`
	Name    string `json:"name,omitempty"`
	Value   string `json:"value"`
	Changed bool   `json:"changed,omitempty"`
}

// RegsNames is the reply to regs.names. Empty entries are preserved, because
// they are what keeps every later index correct.
type RegsNames struct {
	Names []string `json:"names"`
}

// RegsValuesRequest asks for register values.
type RegsValuesRequest struct {
	Thread int `json:"thread,omitempty"`
	// Format is one of x, d, o, t, N (natural). Defaults to x.
	Format  string `json:"format,omitempty"`
	StopSeq uint64 `json:"stopSeq,omitempty"`
}

// RegsValues is the reply to regs.values.
type RegsValues struct {
	StopSeq   uint64     `json:"stopSeq"`
	ThreadID  int        `json:"threadId"`
	Format    string     `json:"format"`
	Registers []Register `json:"registers"`
}

// RegsWriteRequest writes one register.
//
// Number, not name, for the same reason the rest of the register protocol uses
// it: gdb's name list has empty entries at stable indices, so position is the
// only reliable identity. The server turns the number back into the $name gdb
// needs in an expression, and refuses a register it cannot name.
type RegsWriteRequest struct {
	Number int `json:"number"`
	// Value is a gdb expression. A user editing $pc types "main" or "0x401136"
	// as readily as a decimal, and gdb resolves all three.
	Value  string `json:"value"`
	Thread int    `json:"thread,omitempty"`
	// Format is the panel's display format, so the value read back is rendered
	// the way the row that asked for it will show it.
	Format  string `json:"format,omitempty"`
	StopSeq uint64 `json:"stopSeq,omitempty"`
}

// RegsWrite is the reply to regs.write. It carries the register read back
// after the write rather than the value that was sent, because a register
// narrower than the value silently keeps the low bits.
type RegsWrite struct {
	StopSeq  uint64   `json:"stopSeq"`
	ThreadID int      `json:"threadId"`
	Format   string   `json:"format"`
	Register Register `json:"register"`
}
