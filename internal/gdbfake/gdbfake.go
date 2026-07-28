// Package gdbfake is a scripted gdb that speaks MI over in-process pipes.
//
// It exists because the interesting failures cannot be produced on demand by a
// real debugger: a *stopped that arrives between a command's write and its
// reply, gdb dying halfway through a line, a result carrying token 0, a
// hundred-thousand-element reply. Each is a one-line transcript entry here and
// a flaky afternoon with real gdb.
//
// A transcript is a sequence of lines:
//
//	> -exec-continue          expect this command (trailing * matches a prefix)
//	< ^running                send this record; a leading ^ gets the request's token
//	< *stopped,reason="…"     out-of-band records are sent verbatim
//	< 0^done                  an explicit token, for orphan-result tests
//	! prompt                  send "(gdb) "
//	! delay 50ms              pause
//	! partial ^done,a=        write an unterminated line, then EOF
//	! eof                     close the stream: gdb has died
//
// Records listed after a command are sent in transcript order, which is how the
// out-of-order cases are expressed: put the *stopped line before the ^running
// line and that is what the client sees.
package gdbfake

import (
	"bufio"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"time"
)

// StepKind discriminates transcript steps.
type StepKind int

const (
	StepExpect  StepKind = iota // wait for a command from the client
	StepSend                    // send a record
	StepPrompt                  // send "(gdb) "
	StepDelay                   // sleep
	StepPartial                 // send an unterminated line, then EOF
	StepEOF                     // close the output stream
)

// Step is one transcript entry.
type Step struct {
	Kind StepKind
	Text string
	Dur  time.Duration
}

// Expect builds a step that waits for a command. A trailing "*" matches any
// suffix.
func Expect(cmd string) Step { return Step{Kind: StepExpect, Text: cmd} }

// Send builds a step that emits a record. A leading '^' is prefixed with the
// token of the most recent command.
func Send(rec string) Step { return Step{Kind: StepSend, Text: rec} }

// Delay builds a pause.
func Delay(d time.Duration) Step { return Step{Kind: StepDelay, Dur: d} }

// EOF builds a step that closes the stream, simulating gdb exiting.
func EOF() Step { return Step{Kind: StepEOF} }

// Partial builds a step that writes an unterminated line and then closes the
// stream, simulating gdb dying mid-record.
func Partial(text string) Step { return Step{Kind: StepPartial, Text: text} }

// Fake is a running scripted gdb.
type Fake struct {
	// ClientStdin is what the client writes commands to.
	ClientStdin io.WriteCloser
	// ClientStdout is what the client reads records from.
	ClientStdout io.Reader

	inR  *io.PipeReader
	inW  *io.PipeWriter
	outR *io.PipeReader
	outW *io.PipeWriter

	mu       sync.Mutex
	received []string
	failures []string
	done     chan struct{}

	// DefaultDone answers any command the script does not expect with ^done,
	// so a transcript only has to describe the part of the dialogue it is
	// about. Off by default: silent acceptance of unexpected commands is how a
	// test passes while testing nothing.
	DefaultDone bool

	// StrictSerialisation records a failure if a second command is already
	// waiting in the pipe when one is read. That is the only observable
	// signature of two commands being in flight at once, which GDB/MI does not
	// support — and it is exactly what SendUnlocked does on purpose, so it is
	// opt-in.
	StrictSerialisation bool
}

// Option configures a Fake.
type Option func(*Fake)

// WithDefaultDone answers unscripted commands with ^done instead of recording a
// failure. Use it for the startup handshake, not for the behaviour under test.
func WithDefaultDone() Option {
	return func(f *Fake) { f.DefaultDone = true }
}

// WithStrictSerialisation fails the transcript if two commands are ever in
// flight at once. See Fake.StrictSerialisation.
func WithStrictSerialisation() Option {
	return func(f *Fake) { f.StrictSerialisation = true }
}

// Start runs a transcript. Close must be called to release its goroutine.
func Start(steps []Step, opts ...Option) *Fake {
	f := &Fake{done: make(chan struct{})}
	for _, o := range opts {
		o(f)
	}
	f.inR, f.inW = io.Pipe()
	f.outR, f.outW = io.Pipe()
	f.ClientStdin, f.ClientStdout = f.inW, f.outR

	go f.run(steps)
	return f
}

// StartTranscript parses a transcript and runs it.
func StartTranscript(transcript string, opts ...Option) (*Fake, error) {
	steps, err := Parse(transcript)
	if err != nil {
		return nil, err
	}
	return Start(steps, opts...), nil
}

// Parse turns transcript text into steps.
func Parse(transcript string) ([]Step, error) {
	var steps []Step
	for i, raw := range strings.Split(transcript, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		lineno := i + 1
		verb, rest, _ := strings.Cut(line, " ")
		rest = strings.TrimSpace(rest)
		switch verb {
		case ">":
			steps = append(steps, Expect(rest))
		case "<":
			steps = append(steps, Send(rest))
		case "!":
			directive, arg, _ := strings.Cut(rest, " ")
			arg = strings.TrimSpace(arg)
			switch directive {
			case "prompt":
				steps = append(steps, Step{Kind: StepPrompt})
			case "eof":
				steps = append(steps, EOF())
			case "partial":
				steps = append(steps, Partial(arg))
			case "delay":
				d, err := time.ParseDuration(arg)
				if err != nil {
					return nil, fmt.Errorf("line %d: bad duration %q: %w", lineno, arg, err)
				}
				steps = append(steps, Delay(d))
			default:
				return nil, fmt.Errorf("line %d: unknown directive %q", lineno, directive)
			}
		default:
			return nil, fmt.Errorf("line %d: expected '>', '<' or '!', got %q", lineno, line)
		}
	}
	return steps, nil
}

func (f *Fake) run(steps []Step) {
	defer close(f.done)
	br := bufio.NewReaderSize(f.inR, 64*1024)
	token := ""

	for _, st := range steps {
		switch st.Kind {
		case StepExpect:
			line, err := br.ReadString('\n')
			if err != nil {
				f.fail("expected %q but the client closed its stdin: %v", st.Text, err)
				_ = f.outW.Close()
				return
			}
			line = strings.TrimRight(line, "\r\n")
			// Checked before the reply is sent: at this instant a
			// correctly-serialised client cannot have written anything more.
			overlapped := f.StrictSerialisation && br.Buffered() > 0
			tok, cmd := splitToken(line)
			token = tok
			f.mu.Lock()
			f.received = append(f.received, cmd)
			f.mu.Unlock()
			if !matches(st.Text, cmd) {
				f.fail("expected command %q, got %q", st.Text, cmd)
			}
			if overlapped {
				f.fail("a second command was in flight while %q was unanswered", cmd)
			}

		case StepSend:
			text := st.Text
			if strings.HasPrefix(text, "^") {
				text = token + text
			}
			if err := f.writeLine(text); err != nil {
				return
			}

		case StepPrompt:
			if err := f.writeLine("(gdb) "); err != nil {
				return
			}

		case StepDelay:
			time.Sleep(st.Dur)

		case StepPartial:
			text := st.Text
			if strings.HasPrefix(text, "^") {
				text = token + text
			}
			if _, err := io.WriteString(f.outW, text); err != nil {
				return
			}
			_ = f.outW.Close()
			return

		case StepEOF:
			_ = f.outW.Close()
			return
		}
	}

	// The script ran out. Keep answering so the client can shut down cleanly:
	// its Close sends -exec-interrupt, kill and -gdb-exit, and a fake that
	// stops responding would turn every test teardown into a timeout.
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			_ = f.outW.Close()
			return
		}
		tok, cmd := splitToken(strings.TrimRight(line, "\r\n"))
		f.mu.Lock()
		f.received = append(f.received, cmd)
		unexpected := !f.DefaultDone && !isShutdown(cmd)
		f.mu.Unlock()
		if unexpected {
			f.fail("unscripted command after end of transcript: %q", cmd)
		}
		if cmd == "-gdb-exit" {
			_ = f.writeLine(tok + "^exit")
			_ = f.outW.Close()
			return
		}
		if err := f.writeLine(tok + "^done"); err != nil {
			return
		}
	}
}

func (f *Fake) writeLine(s string) error {
	_, err := io.WriteString(f.outW, s+"\n")
	return err
}

func (f *Fake) fail(format string, args ...any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failures = append(f.failures, fmt.Sprintf(format, args...))
}

// Failures returns transcript mismatches observed so far.
func (f *Fake) Failures() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.failures...)
}

// Received returns the commands the fake saw, tokens stripped.
func (f *Fake) Received() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.received...)
}

// Done is closed when the transcript has finished running.
func (f *Fake) Done() <-chan struct{} { return f.done }

// Close tears the fake down.
func (f *Fake) Close() {
	_ = f.outW.Close()
	_ = f.inR.Close()
	_ = f.inW.Close()
	_ = f.outR.Close()
}

// splitToken separates the leading decimal token from the command.
func splitToken(line string) (token, cmd string) {
	i := 0
	for i < len(line) && line[i] >= '0' && line[i] <= '9' {
		i++
	}
	return line[:i], line[i:]
}

func matches(pattern, cmd string) bool {
	if strings.HasSuffix(pattern, "*") {
		return strings.HasPrefix(cmd, strings.TrimSuffix(pattern, "*"))
	}
	return pattern == cmd
}

// isShutdown reports whether a command is part of the client's Close sequence,
// which every transcript would otherwise have to spell out.
func isShutdown(cmd string) bool {
	switch cmd {
	case "-gdb-exit", "-exec-interrupt", `-interpreter-exec console "kill"`:
		return true
	}
	return false
}

// BigList builds a reply with n elements, for the large-payload test.
func BigList(name string, n int) string {
	var sb strings.Builder
	sb.WriteString("^done,")
	sb.WriteString(name)
	sb.WriteString("=[")
	for i := range n {
		if i > 0 {
			sb.WriteByte(',')
		}
		sb.WriteString(`{index="`)
		sb.WriteString(strconv.Itoa(i))
		sb.WriteString(`",value="0x`)
		sb.WriteString(strconv.FormatInt(int64(i), 16))
		sb.WriteString(`"}`)
	}
	sb.WriteString("]")
	return sb.String()
}
