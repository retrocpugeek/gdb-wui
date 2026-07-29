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
)

// OptimizedOut is the value gdb reports for a variable the compiler erased. It
// is a value like any other on the wire; the UI renders it differently, and
// honestly, rather than pretending the variable is missing.
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
	// that has never been expanded has no varobj at all — that is the point of
	// --simple-values.
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
