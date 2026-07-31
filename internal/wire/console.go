package wire

// The console, inferior terminal and thread surface.

// Request types implemented in M5.
const (
	TypeConsoleExec     = "console.exec"
	TypeConsoleComplete = "console.complete"

	TypeInferiorStdin  = "inferior.stdin"
	TypeInferiorSignal = "inferior.signal"
	TypeInferiorResize = "inferior.resize"

	TypeThreadsList  = "threads.list"
	TypeThreadSelect = "thread.select"
)

// Events added in M5.
const (
	// EventInferiorOutput carries bytes the debuggee wrote to its terminal.
	EventInferiorOutput = "inferiorOutput"
	// EventThreadsChanged is emitted when threads appear or disappear.
	EventThreadsChanged = "threadsChanged"
	// EventRemoteChanged is emitted when a remote target is connected or
	// disconnected, however that happened — a typed console command or the
	// connect button.
	EventRemoteChanged = "remoteChanged"
)

// ConsoleExecRequest runs a command as if typed at gdb's prompt.
type ConsoleExecRequest struct {
	Line string `json:"line"`
}

// ConsoleExecResult reports what the command did to the session.
//
// The command is the user's, so it can change anything: `b main.c:12` adds a
// breakpoint, `next` moves the program, `thread 2` changes the selection. The
// server therefore resyncs afterwards and says what it re-read, rather than
// pretending a console command is inert.
type ConsoleExecResult struct {
	// Resynced lists what was re-read after the command.
	Resynced []string `json:"resynced,omitempty"`
	RunState string   `json:"runState"`
	StopSeq  uint64   `json:"stopSeq"`
}

// ConsoleCompleteRequest asks gdb to complete a partial command.
type ConsoleCompleteRequest struct {
	Prefix string `json:"prefix"`
}

// ConsoleComplete is the reply. gdb does the completion, so the frontend needs
// no table of commands and cannot drift from the gdb it is driving.
type ConsoleComplete struct {
	// Completion is gdb's single best expansion, if it has one.
	Completion string   `json:"completion,omitempty"`
	Matches    []string `json:"matches,omitempty"`
	// Truncated reports that gdb stopped listing matches.
	Truncated bool `json:"truncated,omitempty"`
}

// InferiorStdinRequest sends bytes to the debuggee's terminal.
//
// Base64, because the bytes are arbitrary: a user may be typing into a program
// that expects raw control characters, and JSON strings cannot carry them
// losslessly.
type InferiorStdinRequest struct {
	DataB64 string `json:"dataB64"`
}

// InferiorSignalRequest sends a signal to the debuggee's process group.
type InferiorSignalRequest struct {
	// Signal is a name such as INT or TERM.
	Signal string `json:"signal"`
}

// InferiorResizeRequest tells the debuggee's terminal how big it is.
type InferiorResizeRequest struct {
	Rows int `json:"rows"`
	Cols int `json:"cols"`
}

// InferiorOutput carries terminal bytes to the browser.
type InferiorOutput struct {
	DataB64 string `json:"dataB64"`
}

// ThreadsListRequest asks for the thread list.
type ThreadsListRequest struct {
	StopSeq uint64 `json:"stopSeq,omitempty"`
}

// ThreadsList is the reply to threads.list and the payload of threadsChanged.
type ThreadsList struct {
	StopSeq  uint64   `json:"stopSeq"`
	Threads  []Thread `json:"threads"`
	Selected int      `json:"selected"`
}

// ThreadSelectRequest changes the selected thread.
type ThreadSelectRequest struct {
	Thread  int    `json:"thread"`
	StopSeq uint64 `json:"stopSeq,omitempty"`
}
