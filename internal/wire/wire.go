// Package wire holds the browser-facing protocol types.
//
// It imports nothing from the rest of the program, and nothing in it does any
// work. That is deliberate: the protocol is the contract between the Go server
// and a frontend that cannot be refactored by the compiler, so it lives in one
// place that is cheap to read in full and impossible to accidentally couple to
// the debugger's internals.
//
// See docs/protocol.md, which a test keeps in sync with this file.
package wire

import "encoding/json"

// Request is a browser-to-server message.
//
//	{"id":17,"type":"exec.step","payload":{"thread":1}}
type Request struct {
	ID      uint64          `json:"id"`
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

// Response is the reply to exactly one Request, correlated by ID.
//
// Exec responses are acknowledgements, not completions: -exec-continue returns
// as soon as gdb accepts it, and the stop arrives later as an Event. The
// frontend is therefore event-driven by construction rather than by discipline.
type Response struct {
	ID      uint64          `json:"id"`
	OK      bool            `json:"ok"`
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload,omitempty"`
	Error   *Error          `json:"error,omitempty"`
}

// Event is an unsolicited server-to-browser message.
//
// Seq is server-monotonic across every event on a connection, so the frontend
// can detect a gap and tests can assert ordering.
type Event struct {
	Event   string          `json:"event"`
	Seq     uint64          `json:"seq"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

// Error is a failure reply. Code is drawn from the closed set below so the
// frontend can branch on it; Message is for humans and is never parsed.
type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

// NewError builds an Error.
func NewError(code, message string) *Error { return &Error{Code: code, Message: message} }

// The closed set of error codes. Adding one is a protocol change: it must be
// documented in docs/protocol.md and handled by the frontend's default branch
// either way.
const (
	// CodeBadRequest means the payload was malformed or a field was invalid.
	CodeBadRequest = "bad_request"
	// CodeUnsupported means the request type is not known to this server.
	// Unknown types get this, never a closed connection.
	CodeUnsupported = "unsupported"
	// CodeNotReady means no program is loaded yet.
	CodeNotReady = "not_ready"
	// CodeBusy means the inferior is running and the request needs it stopped.
	// This is the run-state gate turning gdb's cryptic "Selected thread is
	// running." into a documented contract.
	CodeBusy = "busy"
	// CodeGDBError wraps a ^error reply from gdb.
	CodeGDBError = "gdb_error"
	// CodeGDBDead means the gdb process is gone.
	CodeGDBDead = "gdb_dead"
	// CodeTimeout means gdb did not answer in time.
	CodeTimeout = "timeout"
	// CodePathDenied means a path escaped the project root.
	CodePathDenied = "path_denied"
	// CodeNotFound means the named thing does not exist.
	CodeNotFound = "not_found"
	// CodeTooLarge means the result exceeded a hard cap.
	CodeTooLarge = "too_large"
	// CodeInternal means a bug on the server side.
	CodeInternal = "internal"
)

// ErrorCodes is every code, for the test that checks the docs list them all.
var ErrorCodes = []string{
	CodeBadRequest, CodeUnsupported, CodeNotReady, CodeBusy, CodeGDBError,
	CodeGDBDead, CodeTimeout, CodePathDenied, CodeNotFound, CodeTooLarge,
	CodeInternal,
}

// Request types. M2 implements the session group; the rest arrive with the
// milestones that need them.
const (
	TypeSessionHello   = "session.hello"
	TypeSessionPing    = "session.ping"
	TypeSessionInfo    = "session.info"
	TypeSessionRestart = "session.restart"
)

// RequestTypes is every type the server answers today.
//
// Two tests read it: one asserts each appears in docs/protocol.md, and one
// asserts the hub answers each rather than returning "unsupported". Together
// they mean the docs cannot drift from the dispatch table in either direction.
var RequestTypes = []string{
	TypeSessionHello,
	TypeSessionInfo,
	TypeSessionPing,
	TypeSessionRestart,

	TypeExeLoad,

	TypeExecRun,
	TypeExecContinue,
	TypeExecPause,
	TypeExecStep,
	TypeExecNext,
	TypeExecFinish,
	TypeExecKill,

	TypeBpSetSource,
	TypeBpSetAddress,
	TypeBpDelete,
	TypeBpSetEnabled,
	TypeBpList,

	TypeStackList,
	TypeFrameSelect,

	TypeVarsLocals,
	TypeVarsExpand,

	TypeWatchAdd,
	TypeWatchRemove,
	TypeWatchList,

	TypeRegsNames,
	TypeRegsValues,

	TypeConsoleExec,
	TypeConsoleComplete,

	TypeInferiorStdin,
	TypeInferiorSignal,
	TypeInferiorResize,

	TypeThreadsList,
	TypeThreadSelect,

	TypeDisasmFunction,
	TypeDisasmRange,

	TypeExecStepI,
	TypeExecStepLine,
	TypeExecNextI,

	TypeMemRead,
	TypeEvalExpr,
	TypeMemSymbols,

	TypePathSubstitute,
	TypePathAddDir,
	TypePathList,

	TypeSymbolsList,
	TypeSymbolsLoad,

	TypeDecompStatus,
	TypeDecompFunction,
}

// SessionRequestTypes is the subset the hub answers with no debugger attached.
// The rest require a Session and return not_ready without one.
var SessionRequestTypes = []string{
	TypeSessionHello,
	TypeSessionInfo,
	TypeSessionPing,
}

// Event names.
const (
	EventHello        = "hello"
	EventConsole      = "console"
	EventError        = "error"
	EventShuttingDown = "shuttingDown"
)

// EventNames is every event the server emits today.
var EventNames = []string{
	EventHello,
	EventConsole,
	EventError,
	EventShuttingDown,

	EventStopped,
	EventRunning,
	EventExited,
	EventExeLoaded,
	EventBreakpointsChanged,
	EventSelectionChanged,
	EventGDBDead,
	EventMI,

	EventVarsInvalidated,
	EventWatchesChanged,

	EventInferiorOutput,
	EventThreadsChanged,

	EventSymbolsInvalidated,
	EventRemoteChanged,
	EventDecompChanged,
	EventDecompLog,
}

// Hello is pushed to every connection the moment it opens, before anything is
// requested.
//
// It carries a full snapshot rather than a greeting because that single
// decision is what makes page reload, reconnect and a second browser tab work
// for free. Retrofitting it later means rewriting every panel's initialisation,
// which is why it exists in M2 with almost nothing to report yet.
type Hello struct {
	// Protocol is bumped when an incompatible change lands, so a stale cached
	// frontend can say so instead of misbehaving.
	Protocol int `json:"protocol"`
	// Server is the build version.
	Server string `json:"server"`
	// ProjectRoot is the absolute path being browsed, for display only. Every
	// path in the protocol is root-relative.
	ProjectRoot string `json:"projectRoot"`
	// GDBVersion is empty until a debugger session exists (M3).
	GDBVersion string `json:"gdbVersion,omitempty"`
	// Features is gdb's -list-features, empty until M3.
	Features []string `json:"features,omitempty"`
	// RunState is one of the RunState constants.
	RunState string `json:"runState"`
	// StopSeq increments on every stop; requests carry it and stale responses
	// are dropped.
	StopSeq uint64 `json:"stopSeq"`

	// The fields below are what let a client repaint entirely from this
	// snapshot, so a page reload behaves the same as a first load.

	// ExePath is the loaded program, root-relative, empty if none.
	ExePath string `json:"exePath,omitempty"`
	// Breakpoints is the full mirror.
	Breakpoints []Breakpoint `json:"breakpoints,omitempty"`
	// Threads is empty unless the inferior exists.
	Threads []Thread `json:"threads,omitempty"`
	// Frames is the selected thread's stack, present only when stopped.
	Frames []Frame `json:"frames,omitempty"`
	// Locals belong to the selected frame, present only when stopped.
	Locals []Variable `json:"locals,omitempty"`
	// Selection is the current thread and frame, present only when stopped.
	Selection *Selection `json:"selection,omitempty"`
	// LastStopReason is why the inferior last stopped, for the status bar.
	LastStopReason string `json:"lastStopReason,omitempty"`
	// Remote describes a connection to a target this server did not start.
	// Absent means there is none.
	Remote *RemoteTarget `json:"remote,omitempty"`
}

// RemoteTarget describes a connection to a debug target this server did not
// start — a gdbserver, an emulator's stub.
//
// It is reported by the server rather than inferred by the client from the
// commands it sent, because the connection can also be made or dropped by a
// command typed at the console, and two browser tabs must agree about it.
type RemoteTarget struct {
	Connected bool `json:"connected"`
	// Address is what was passed to `target remote`, when it was recorded.
	Address string `json:"address,omitempty"`
}

// Protocol is the current protocol version.
const Protocol = 1

// Run states.
const (
	// RunStateNoProgram means no executable is loaded.
	RunStateNoProgram = "noProgram"
	// RunStateStopped means the inferior exists and is stopped.
	RunStateStopped = "stopped"
	// RunStateRunning means the inferior is running; most queries are gated.
	RunStateRunning = "running"
	// RunStateExited means the inferior ran to completion.
	RunStateExited = "exited"
)

// TreeEntry is one directory entry from GET /api/tree.
type TreeEntry struct {
	// Name is the base name.
	Name string `json:"name"`
	// Path is the root-relative slash path, which is what every other request
	// takes.
	Path string `json:"path"`
	// Dir reports whether the entry is a directory.
	Dir bool `json:"dir"`
	// Size is the file size in bytes; zero for directories.
	Size int64 `json:"size,omitempty"`
	// Symlink reports whether the entry itself is a symbolic link. It is shown
	// rather than followed: a link out of the root is refused on open, and the
	// UI should say why rather than appear to lose the file.
	Symlink bool `json:"symlink,omitempty"`
	// Kind is "elf" for a candidate debuggee, "exec" for something executable
	// that is not ELF, and absent otherwise.
	//
	// The server decides this because the client cannot: a compiled program
	// usually has no extension, and guessing from the filename is wrong in both
	// directions. It is what makes "clicking this loads a program" and
	// "clicking this opens text" visibly different actions rather than a
	// surprise.
	Kind string `json:"kind,omitempty"`
}

// Tree is the response body of GET /api/tree.
type Tree struct {
	// Path is the directory that was listed, root-relative ("" is the root).
	Path string `json:"path"`
	// Entries are the immediate children, directories first then by name.
	Entries []TreeEntry `json:"entries"`
	// Truncated reports that the listing hit the entry cap. It is surfaced
	// rather than dropped, so that a directory listing 5000 of its 9000 files
	// says so.
	Truncated bool `json:"truncated"`
}

// ErrorBody is the JSON body of a failed HTTP request. It reuses the WebSocket
// error codes so the frontend has one error vocabulary, not two.
type ErrorBody struct {
	Error Error `json:"error"`
}
