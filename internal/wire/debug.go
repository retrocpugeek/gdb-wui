package wire

// The debugger half of the protocol. Split from wire.go only for readability;
// it is the same contract and the same rules — see docs/protocol.md, which a
// test keeps in sync with the declarations here.

// Request types implemented in M3.
const (
	TypeExeLoad = "exe.load"

	TypeExecRun      = "exec.run"
	TypeExecContinue = "exec.continue"
	TypeExecPause    = "exec.pause"
	TypeExecStep     = "exec.step"
	TypeExecNext     = "exec.next"
	TypeExecFinish   = "exec.finish"
	TypeExecKill     = "exec.kill"

	TypeBpSetSource  = "bp.setSource"
	TypeBpDelete     = "bp.delete"
	TypeBpSetEnabled = "bp.setEnabled"
	TypeBpList       = "bp.list"

	TypeStackList   = "stack.list"
	TypeFrameSelect = "frame.select"
)

// Events added in M3.
const (
	EventStopped            = "stopped"
	EventRunning            = "running"
	EventExited             = "exited"
	EventExeLoaded          = "exeLoaded"
	EventBreakpointsChanged = "breakpointsChanged"
	EventSelectionChanged   = "selectionChanged"
	EventGDBDead            = "gdbDead"
	EventMI                 = "mi"
)

// Payloads.

// ExeLoadRequest loads a program. Path is root-relative.
type ExeLoadRequest struct {
	Path string   `json:"path"`
	Args []string `json:"args,omitempty"`
}

// ExecRequest is the shared shape of the exec group. All fields are optional;
// an absent thread means "the selected one".
type ExecRequest struct {
	Thread int `json:"thread,omitempty"`
	Frame  int `json:"frame,omitempty"`
	// StopAtMain uses -exec-run --start. Only exec.run reads it.
	StopAtMain bool `json:"stopAtMain,omitempty"`
	// StopSeq is the stop the client believed it was acting on. A mismatch
	// means the user clicked twice, or clicked while a stop was in flight, and
	// the request is dropped rather than applied to the wrong state.
	StopSeq uint64 `json:"stopSeq,omitempty"`
}

// ExecAck is the reply to an exec command.
//
// It is an acknowledgement, not a completion: gdb returns ^running as soon as
// it accepts the command, and the stop arrives later as an event.
type ExecAck struct {
	RunState string `json:"runState"`
	StopSeq  uint64 `json:"stopSeq"`
}

// BreakpointRequest sets a breakpoint on a source line.
type BreakpointRequest struct {
	// Path is root-relative.
	Path string `json:"path"`
	Line int    `json:"line"`
	// Temporary deletes the breakpoint once it is hit.
	Temporary bool `json:"temporary,omitempty"`
	// Condition is an optional gdb expression.
	Condition string `json:"condition,omitempty"`
}

// BreakpointIDRequest names an existing breakpoint.
type BreakpointIDRequest struct {
	Number  int  `json:"number"`
	Enabled bool `json:"enabled,omitempty"`
}

// Breakpoint is one entry in the mirror.
type Breakpoint struct {
	Number  int    `json:"number"`
	Type    string `json:"type,omitempty"`
	Enabled bool   `json:"enabled"`
	// Pending is true before gdb has resolved a location to an address. The
	// address arrives later in a =breakpoint-modified, which is why breakpoint
	// state is event-driven rather than read once at creation.
	Pending bool   `json:"pending,omitempty"`
	Address string `json:"address,omitempty"`
	Func    string `json:"func,omitempty"`
	// Path is root-relative when the file could be located in the project.
	Path string `json:"path,omitempty"`
	// GDBPath is what gdb reported, kept when Path could not be resolved.
	GDBPath   string `json:"gdbPath,omitempty"`
	Line      int    `json:"line,omitempty"`
	Condition string `json:"condition,omitempty"`
	HitCount  int    `json:"hitCount,omitempty"`
	Temporary bool   `json:"temporary,omitempty"`
	// Original is gdb's original-location, shown when nothing better exists.
	Original string `json:"original,omitempty"`
}

// BreakpointList is the reply to bp.list and the payload of
// breakpointsChanged.
type BreakpointList struct {
	Breakpoints []Breakpoint `json:"breakpoints"`
}

// SourceRef locates a frame in the project's source.
//
// Every field is optional because a frame need not have any of them: a stripped
// binary reports func="??" with no file at all, so addr is the only guaranteed
// identity. Clients must handle Available being false.
type SourceRef struct {
	// Available reports whether Path names a file the browser can fetch.
	Available bool `json:"available"`
	// Path is root-relative, set only when Available.
	Path string `json:"path,omitempty"`
	// GDBPath is the path gdb reported, set when it could not be resolved
	// inside the project — a libc frame, or a build-time path that no longer
	// exists on this machine.
	GDBPath string `json:"gdbPath,omitempty"`
	Line    int    `json:"line,omitempty"`
}

// Frame is one stack frame.
type Frame struct {
	Level   int       `json:"level"`
	Address string    `json:"address"`
	Func    string    `json:"func,omitempty"`
	Args    []Arg     `json:"args,omitempty"`
	Source  SourceRef `json:"source"`
}

// Arg is one function argument as reported with the frame.
type Arg struct {
	Name  string `json:"name"`
	Value string `json:"value,omitempty"`
}

// Variable is one local, as reported by -stack-list-variables
// --simple-values.
//
// A missing Value is not an error: with --simple-values gdb omits it exactly
// for aggregates, so absence *is* the "expandable" signal the variables tree
// keys on in M4.
type Variable struct {
	Name       string `json:"name"`
	Type       string `json:"type,omitempty"`
	Value      string `json:"value,omitempty"`
	Expandable bool   `json:"expandable"`
	// Arg marks a function argument. gdb reports it as arg="1" and the
	// variables panel groups on it.
	Arg bool `json:"arg,omitempty"`
	// OptimizedOut is derived from Value, so the UI can render an erased
	// variable honestly instead of as a puzzling literal string.
	OptimizedOut bool `json:"optimizedOut,omitempty"`
}

// Thread is one thread from -thread-info.
type Thread struct {
	ID       int    `json:"id"`
	TargetID string `json:"targetId,omitempty"`
	Name     string `json:"name,omitempty"`
	// State is "running" or "stopped".
	State string `json:"state"`
	Core  string `json:"core,omitempty"`
	Frame *Frame `json:"frame,omitempty"`
}

// Stopped is the fat stop event.
//
// It carries everything the UI needs to repaint after a stop — threads, the
// selected thread's stack, and frame-0 locals — in one message. Fetching those
// separately costs four or five round-trips per single-step, which is the
// difference between stepping feeling instant and feeling laggy. Registers,
// disassembly and memory are deliberately not here: those panels pull lazily
// and pass stopSeq.
type Stopped struct {
	StopSeq uint64 `json:"stopSeq"`
	// Reason is gdb's stop reason, passed through unknown values and all.
	Reason string `json:"reason"`
	// Signal is set for signal-received stops.
	Signal        string `json:"signal,omitempty"`
	SignalMeaning string `json:"signalMeaning,omitempty"`
	// BreakpointNumber is set for breakpoint-hit stops.
	BreakpointNumber int `json:"breakpointNumber,omitempty"`
	// ReturnValue is set for function-finished stops.
	ReturnValue string `json:"returnValue,omitempty"`

	ThreadID int      `json:"threadId"`
	Threads  []Thread `json:"threads"`
	Frames   []Frame  `json:"frames"`
	// Locals are frame 0's, eagerly fetched.
	Locals []Variable `json:"locals"`

	RunState string `json:"runState"`
}

// Running is emitted when the inferior resumes.
type Running struct {
	// ThreadID is 0 for "all threads", which is what all-stop mode reports.
	ThreadID int    `json:"threadId,omitempty"`
	RunState string `json:"runState"`
}

// Exited is emitted when the inferior finishes.
type Exited struct {
	// ExitCode is nil when gdb did not report one.
	ExitCode *int   `json:"exitCode,omitempty"`
	Signal   string `json:"signal,omitempty"`
	RunState string `json:"runState"`
}

// ExeLoaded is emitted after a successful exe.load.
type ExeLoaded struct {
	Path     string `json:"path"`
	RunState string `json:"runState"`
}

// StackListRequest asks for a thread's frames.
type StackListRequest struct {
	Thread  int    `json:"thread,omitempty"`
	Low     int    `json:"low,omitempty"`
	High    int    `json:"high,omitempty"`
	StopSeq uint64 `json:"stopSeq,omitempty"`
}

// StackList is the reply to stack.list.
type StackList struct {
	StopSeq  uint64  `json:"stopSeq"`
	ThreadID int     `json:"threadId"`
	Frames   []Frame `json:"frames"`
}

// FrameSelectRequest changes the selected frame.
type FrameSelectRequest struct {
	Thread  int    `json:"thread,omitempty"`
	Frame   int    `json:"frame"`
	StopSeq uint64 `json:"stopSeq,omitempty"`
}

// Selection is the current thread and frame.
type Selection struct {
	ThreadID int        `json:"threadId"`
	Frame    int        `json:"frame"`
	StopSeq  uint64     `json:"stopSeq"`
	Locals   []Variable `json:"locals,omitempty"`
	Source   *SourceRef `json:"source,omitempty"`
}

// MILogEntry is one line of raw MI traffic, for the developer log pane.
type MILogEntry struct {
	// Direction is "out" for commands and "in" for records.
	Direction string `json:"direction"`
	Text      string `json:"text"`
}

// GDBDead is emitted when the gdb process exits unexpectedly.
type GDBDead struct {
	Reason string   `json:"reason"`
	Stderr []string `json:"stderr,omitempty"`
}
