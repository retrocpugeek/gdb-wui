package mi

import (
	"encoding/json"
	"strings"
)

// Type is the MI record kind, determined by the character after the optional
// token.
type Type uint8

const (
	// RecGarbage is a line that is not MI at all. It is the zero value on
	// purpose: it is what an uninitialised or unparseable record is.
	//
	// This is not defensive padding. gdb's stdout carries the *inferior's*
	// stdout interleaved with MI records, so a debuggee that prints
	// "total=3 argc=1" puts exactly that line between two MI records. The
	// parser must hand it back rather than erroring, and the caller must
	// surface it rather than dropping it.
	RecGarbage Type = iota
	RecResult       // ^done, ^running, ^error, ^exit, ^connected
	RecExec         // *stopped, *running
	RecNotify       // =breakpoint-modified, =thread-created
	RecStatus       // +download-progress
	RecConsole      // ~"text" — gdb's own console output
	RecTarget       // @"text" — remote target output
	RecLog          // &"text" — gdb's log/echo of what it is doing
	RecPrompt       // (gdb)
)

func (t Type) String() string {
	switch t {
	case RecResult:
		return "result"
	case RecExec:
		return "exec"
	case RecNotify:
		return "notify"
	case RecStatus:
		return "status"
	case RecConsole:
		return "console"
	case RecTarget:
		return "target"
	case RecLog:
		return "log"
	case RecPrompt:
		return "prompt"
	}
	return "garbage"
}

// Result classes carried by RecResult.
const (
	ClassDone      = "done"
	ClassRunning   = "running"
	ClassConnected = "connected"
	ClassError     = "error"
	ClassExit      = "exit"
)

// Async classes worth naming; others pass through as strings.
const (
	ClassStopped = "stopped"
)

// Record is one parsed line of gdb's MI output.
type Record struct {
	Type Type

	// Token is the correlation token the client prefixed to the command that
	// produced this record. HasToken is false for asynchronous records and for
	// results caused by console commands the user typed.
	Token    uint64
	HasToken bool

	// Class is the result class ("done", "error") or async class ("stopped").
	// Empty for stream records, prompts and garbage.
	Class string

	// Results is the record's payload.
	Results Results

	// Text is the unescaped payload of a stream record, or the raw line for
	// garbage.
	Text string

	// Raw is the line as received, minus the line terminator.
	Raw string

	// Err is set when the line looked like MI — it had a recognised classifier
	// character — but failed to parse. Genuine inferior output has Type
	// RecGarbage and a nil Err, which is how a caller tells "the debuggee
	// printed something" from "gdb died halfway through a line".
	Err error
}

// IsStream reports whether the record carries console/target/log text.
func (r Record) IsStream() bool {
	return r.Type == RecConsole || r.Type == RecTarget || r.Type == RecLog
}

// IsAsync reports whether the record is an out-of-band notification.
func (r Record) IsAsync() bool {
	return r.Type == RecExec || r.Type == RecNotify || r.Type == RecStatus
}

// IsError reports whether the record is a ^error result.
func (r Record) IsError() bool {
	return r.Type == RecResult && r.Class == ClassError
}

// ErrorMessage returns the msg field of a ^error record, with a fallback so a
// caller never has to render an empty string.
func (r Record) ErrorMessage() string {
	if msg := r.Results.Str("msg"); msg != "" {
		return msg
	}
	if r.IsError() {
		return "gdb reported an error with no message"
	}
	return ""
}

// ErrorCode returns the machine-readable code some errors carry, e.g.
// "undefined-command" from the undefined-command-error-code feature.
func (r Record) ErrorCode() string { return r.Results.Str("code") }

// MarshalJSON renders the record in the canonical JSON form the rest of the
// program passes around. Keeping it here rather than in each caller is what
// makes raw-MI passthrough to the browser a no-op.
func (r Record) MarshalJSON() ([]byte, error) {
	out := struct {
		Type    string  `json:"type"`
		Token   *uint64 `json:"token,omitempty"`
		Class   string  `json:"class,omitempty"`
		Results Results `json:"results,omitempty"`
		Text    string  `json:"text,omitempty"`
		Err     string  `json:"err,omitempty"`
	}{
		Type:    r.Type.String(),
		Class:   r.Class,
		Results: r.Results,
		Text:    r.Text,
	}
	if r.HasToken {
		tok := r.Token
		out.Token = &tok
	}
	if r.Err != nil {
		out.Err = r.Err.Error()
	}
	return json.Marshal(out)
}

// MI re-renders the record in MI syntax; the inverse of ParseRecord for every
// record that round-trips (garbage and prompts render as themselves).
func (r Record) MI() string {
	var sb strings.Builder
	switch r.Type {
	case RecGarbage:
		return r.Text
	case RecPrompt:
		return "(gdb)"
	}
	if r.HasToken {
		sb.WriteString(u64s(r.Token))
	}
	switch r.Type {
	case RecResult:
		sb.WriteByte('^')
	case RecExec:
		sb.WriteByte('*')
	case RecNotify:
		sb.WriteByte('=')
	case RecStatus:
		sb.WriteByte('+')
	case RecConsole:
		sb.WriteByte('~')
	case RecTarget:
		sb.WriteByte('@')
	case RecLog:
		sb.WriteByte('&')
	}
	if r.IsStream() {
		sb.WriteByte('"')
		writeEscaped(&sb, r.Text)
		sb.WriteByte('"')
		return sb.String()
	}
	sb.WriteString(r.Class)
	for _, res := range r.Results {
		sb.WriteByte(',')
		if res.Name != "" {
			sb.WriteString(res.Name)
			sb.WriteByte('=')
		}
		res.Value.appendMI(&sb)
	}
	return sb.String()
}
