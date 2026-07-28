package mi_test

import (
	"context"
	"errors"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/retrocpugeek/gdb-wui/internal/gdbfake"
	"github.com/retrocpugeek/gdb-wui/internal/mi"
)

// collector accumulates the records the client hands to its Handler.
type collector struct {
	mu   sync.Mutex
	recs []mi.Record
	ch   chan mi.Record
}

func newCollector() *collector {
	return &collector{ch: make(chan mi.Record, 256)}
}

func (c *collector) handle(r mi.Record) {
	c.mu.Lock()
	c.recs = append(c.recs, r)
	c.mu.Unlock()
	select {
	case c.ch <- r:
	default:
	}
}

func (c *collector) all() []mi.Record {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]mi.Record(nil), c.recs...)
}

// waitFor blocks until a record satisfying pred arrives.
func (c *collector) waitFor(t *testing.T, what string, pred func(mi.Record) bool) mi.Record {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for {
		select {
		case r := <-c.ch:
			if pred(r) {
				return r
			}
		case <-deadline:
			t.Fatalf("timed out waiting for %s; saw %d records", what, len(c.all()))
		}
	}
}

// startFake wires a client to a scripted fake. The handshake is skipped so
// transcripts describe only the dialogue under test.
func startFake(t *testing.T, transcript string, opts ...gdbfake.Option) (*mi.Client, *gdbfake.Fake, *collector) {
	t.Helper()
	fake, err := gdbfake.StartTranscript(transcript, opts...)
	if err != nil {
		t.Fatalf("parse transcript: %v", err)
	}
	col := newCollector()
	client, err := mi.Start(t.Context(), mi.Options{
		Handshake: []string{}, // non-nil empty: no handshake
		Handler:   col.handle,
		Logf:      t.Logf,
		Stdin:     fake.ClientStdin,
		Stdout:    fake.ClientStdout,
	})
	if err != nil {
		t.Fatalf("start client: %v", err)
	}
	t.Cleanup(func() {
		_ = client.Close(context.Background())
		fake.Close()
		for _, f := range fake.Failures() {
			t.Errorf("transcript mismatch: %s", f)
		}
	})
	return client, fake, col
}

func TestSendReceivesReply(t *testing.T) {
	c, _, _ := startFake(t, `
> -gdb-show mi-async
< ^done,value="off"
`)
	rec, err := c.Send(t.Context(), "-gdb-show mi-async")
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if got := rec.Results.Str("value"); got != "off" {
		t.Errorf("value = %q, want off", got)
	}
	if !rec.HasToken || rec.Token != 1 {
		t.Errorf("token = %d (has=%v), want 1", rec.Token, rec.HasToken)
	}
}

func TestErrorReplyBecomesGoError(t *testing.T) {
	c, _, _ := startFake(t, `
> -exec-continue
< ^error,msg="Cannot execute this command while the selected thread is running."
`)
	rec, err := c.Send(t.Context(), "-exec-continue")
	if err == nil {
		t.Fatal("Send returned nil error for a ^error reply")
	}
	gerr, ok := mi.AsError(err)
	if !ok {
		t.Fatalf("error is %T, want *mi.Error", err)
	}
	if !strings.Contains(gerr.Msg, "Cannot execute this command") {
		t.Errorf("Msg = %q", gerr.Msg)
	}
	// The record is still available for callers that want the payload.
	if !rec.IsError() {
		t.Error("record should still be the ^error record")
	}
}

// TestStoppedBetweenWriteAndReply is the ordering hazard: the async record
// arrives before the reply to the command that caused it. The reply must still
// be correlated, and the *stopped must reach the handler.
func TestStoppedBetweenWriteAndReply(t *testing.T) {
	c, _, col := startFake(t, `
> -exec-continue
< *stopped,reason="breakpoint-hit",bkptno="1",thread-id="1"
< ^running
`)
	rec, err := c.Send(t.Context(), "-exec-continue")
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if rec.Class != mi.ClassRunning {
		t.Errorf("class = %q, want running", rec.Class)
	}
	stopped := col.waitFor(t, "*stopped", func(r mi.Record) bool {
		return r.Type == mi.RecExec && r.Class == mi.ClassStopped
	})
	if got := stopped.Results.Str("reason"); got != "breakpoint-hit" {
		t.Errorf("reason = %q", got)
	}
}

// TestOrphanTokenZeroResult covers a result caused by something the user typed
// at the console: it carries token 0, matches no pending command, and must be
// delivered as an event rather than dropped or treated as an error.
func TestOrphanTokenZeroResult(t *testing.T) {
	c, _, col := startFake(t, `
> -gdb-show confirm
< 0^done,value="from-console"
< ^done,value="off"
`)
	rec, err := c.Send(t.Context(), "-gdb-show confirm")
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if got := rec.Results.Str("value"); got != "off" {
		t.Errorf("correlated the wrong reply: value = %q, want off", got)
	}
	orphan := col.waitFor(t, "orphan result", func(r mi.Record) bool {
		return r.Type == mi.RecResult && r.HasToken && r.Token == 0
	})
	if got := orphan.Results.Str("value"); got != "from-console" {
		t.Errorf("orphan value = %q", got)
	}
}

// TestCancelledCommandLeavesTombstone: the caller gives up, then the reply
// arrives. It must be dropped, not surfaced as a spurious event, and it must
// not be handed to whoever sends next.
func TestCancelledCommandLeavesTombstone(t *testing.T) {
	c, _, col := startFake(t, `
> -slow-command
! delay 150ms
< ^done,value="late"
> -fast-command
< ^done,value="fast"
`)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()
	if _, err := c.Send(ctx, "-slow-command"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want DeadlineExceeded", err)
	}

	rec, err := c.Send(t.Context(), "-fast-command")
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if got := rec.Results.Str("value"); got != "fast" {
		t.Fatalf("got the abandoned command's reply: value = %q", got)
	}
	// Give the late reply time to be (not) delivered.
	time.Sleep(50 * time.Millisecond)
	for _, r := range col.all() {
		if r.Type == mi.RecResult && r.Results.Str("value") == "late" {
			t.Error("the abandoned reply was delivered as an event")
		}
	}
}

// TestGdbDiesMidCommand: the outstanding Send must fail with ErrDead rather
// than hang, and Dead must close.
func TestGdbDiesMidCommand(t *testing.T) {
	c, _, _ := startFake(t, `
> -exec-run
! partial ^done,st
`)
	_, err := c.Send(t.Context(), "-exec-run")
	if !errors.Is(err, mi.ErrDead) {
		t.Fatalf("err = %v, want ErrDead", err)
	}
	select {
	case <-c.Dead():
	case <-time.After(time.Second):
		t.Fatal("Dead() never closed")
	}
	if c.DeadErr() == nil {
		t.Error("DeadErr is nil after death")
	}
}

// TestPartialLineIsSurfaced: the truncated line gdb managed to write before
// dying must reach the handler as garbage rather than vanish.
func TestPartialLineIsSurfaced(t *testing.T) {
	_, _, col := startFake(t, `
! partial ~"half a co
`)
	rec := col.waitFor(t, "the truncated line", func(r mi.Record) bool {
		return r.Type == mi.RecGarbage
	})
	if !strings.Contains(rec.Text, "half a co") {
		t.Errorf("Text = %q, want the partial line preserved", rec.Text)
	}
	if rec.Err == nil {
		t.Error("a truncated MI record should carry Err, unlike inferior output")
	}
}

// TestInferiorGarbageReachesHandler is finding 3 end to end.
func TestInferiorGarbageReachesHandler(t *testing.T) {
	c, _, col := startFake(t, `
> -exec-continue
< ^running
< total=3 argc=1
< *stopped,reason="exited-normally"
`)
	if _, err := c.Send(t.Context(), "-exec-continue"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	rec := col.waitFor(t, "inferior output", func(r mi.Record) bool {
		return r.Type == mi.RecGarbage
	})
	if rec.Text != "total=3 argc=1" {
		t.Errorf("Text = %q", rec.Text)
	}
	if rec.Err != nil {
		t.Errorf("Err = %v; inferior output is not a parse failure", rec.Err)
	}
}

func TestPromptIsDeliveredNotConfused(t *testing.T) {
	c, _, col := startFake(t, `
> -gdb-show confirm
! prompt
< ^done,value="off"
`)
	rec, err := c.Send(t.Context(), "-gdb-show confirm")
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if rec.Results.Str("value") != "off" {
		t.Errorf("value = %q", rec.Results.Str("value"))
	}
	col.waitFor(t, "prompt record", func(r mi.Record) bool { return r.Type == mi.RecPrompt })
}

// TestLargeReply is finding 9 through the whole stack: a 100k-element reply
// must arrive intact, which it does not if the reader uses bufio.Scanner.
func TestLargeReply(t *testing.T) {
	const n = 100_000
	fake := gdbfake.Start([]gdbfake.Step{
		gdbfake.Expect("-data-list-register-values x"),
		gdbfake.Send(gdbfake.BigList("register-values", n)),
	})
	t.Cleanup(fake.Close)

	col := newCollector()
	c, err := mi.Start(t.Context(), mi.Options{
		Handshake: []string{},
		Handler:   col.handle,
		Logf:      t.Logf,
		Stdin:     fake.ClientStdin,
		Stdout:    fake.ClientStdout,
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { _ = c.Close(context.Background()) })

	rec, err := c.Send(t.Context(), "-data-list-register-values x")
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	vals, ok := rec.Results.List("register-values")
	if !ok {
		t.Fatal("no register-values list")
	}
	if len(vals) != n {
		t.Fatalf("got %d elements, want %d", len(vals), n)
	}
	if got, _ := vals[n-1].StrOK("index"); got != "99999" {
		t.Errorf("last element index = %q", got)
	}
}

// TestSendSerialised checks the cap-1 semaphore: concurrent Sends must not
// interleave, because MI is not concurrency-safe.
func TestSendSerialised(t *testing.T) {
	const n = 20
	steps := []gdbfake.Step{}
	for range n {
		steps = append(steps,
			gdbfake.Expect("-probe"),
			gdbfake.Delay(time.Millisecond),
			gdbfake.Send(`^done`),
		)
	}
	fake := gdbfake.Start(steps, gdbfake.WithStrictSerialisation())
	t.Cleanup(fake.Close)

	col := newCollector()
	c, err := mi.Start(t.Context(), mi.Options{
		Handshake: []string{},
		Handler:   col.handle,
		Logf:      t.Logf,
		Stdin:     fake.ClientStdin,
		Stdout:    fake.ClientStdout,
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { _ = c.Close(context.Background()) })

	var wg sync.WaitGroup
	errs := make(chan error, n)
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := c.Send(t.Context(), "-probe"); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent Send: %v", err)
	}
	for _, f := range fake.Failures() {
		t.Errorf("transcript mismatch: %s", f)
	}
}

// TestSendUnlockedBypassesSemaphore is why SendUnlocked exists: Pause must work
// while a long-running command holds the command semaphore.
func TestSendUnlockedBypassesSemaphore(t *testing.T) {
	fake := gdbfake.Start([]gdbfake.Step{
		gdbfake.Expect(`-interpreter-exec console "shell sleep 60"`),
		gdbfake.Expect("-exec-interrupt"),
		gdbfake.Send("^done"), // reply to the interrupt: token is the interrupt's
		gdbfake.Send(`*stopped,reason="signal-received",signal-name="SIGINT"`),
	})
	t.Cleanup(fake.Close)

	col := newCollector()
	c, err := mi.Start(t.Context(), mi.Options{
		Handshake: []string{},
		Handler:   col.handle,
		Logf:      t.Logf,
		Stdin:     fake.ClientStdin,
		Stdout:    fake.ClientStdout,
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { _ = c.Close(context.Background()) })

	slowDone := make(chan struct{})
	go func() {
		defer close(slowDone)
		ctx, cancel := context.WithTimeout(t.Context(), 600*time.Millisecond)
		defer cancel()
		// Never answered by the transcript: it holds the semaphore.
		_, _ = c.Send(ctx, `-interpreter-exec console "shell sleep 60"`)
	}()

	// Let the slow command take the semaphore.
	time.Sleep(50 * time.Millisecond)

	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if _, err := c.SendUnlocked(ctx, "-exec-interrupt"); err != nil {
		t.Fatalf("SendUnlocked while a command is outstanding: %v", err)
	}
	col.waitFor(t, "*stopped from the interrupt", func(r mi.Record) bool {
		return r.Type == mi.RecExec && r.Class == mi.ClassStopped
	})
	<-slowDone
}

func TestSendRejectsNewlines(t *testing.T) {
	c, _, _ := startFake(t, ``, gdbfake.WithDefaultDone())
	if _, err := c.Send(t.Context(), "-break-insert foo\n-exec-run"); err == nil {
		t.Error("a command containing a newline was accepted; it would inject a second command")
	}
}

func TestSendAfterDeathFails(t *testing.T) {
	c, _, _ := startFake(t, `
! eof
`)
	select {
	case <-c.Dead():
	case <-time.After(time.Second):
		t.Fatal("Dead() never closed after EOF")
	}
	if _, err := c.Send(t.Context(), "-anything"); !errors.Is(err, mi.ErrDead) {
		t.Errorf("err = %v, want ErrDead", err)
	}
}

// TestNoGoroutineLeak polls the goroutine count after Close rather than pulling
// in a dependency for it.
func TestNoGoroutineLeak(t *testing.T) {
	before := runtime.NumGoroutine()

	for range 20 {
		fake := gdbfake.Start([]gdbfake.Step{
			gdbfake.Expect("-probe"),
			gdbfake.Send("^done"),
		})
		col := newCollector()
		c, err := mi.Start(t.Context(), mi.Options{
			Handshake: []string{},
			Handler:   col.handle,
			Stdin:     fake.ClientStdin,
			Stdout:    fake.ClientStdout,
		})
		if err != nil {
			t.Fatalf("start: %v", err)
		}
		if _, err := c.Send(t.Context(), "-probe"); err != nil {
			t.Fatalf("send: %v", err)
		}
		if err := c.Close(context.Background()); err != nil {
			t.Fatalf("close: %v", err)
		}
		fake.Close()
	}

	// Goroutines die asynchronously; poll rather than assert immediately.
	deadline := time.Now().Add(3 * time.Second)
	for {
		got := runtime.NumGoroutine()
		if got <= before+2 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("goroutines: %d before, %d after 20 client lifecycles", before, got)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestCloseIsIdempotent(t *testing.T) {
	c, _, _ := startFake(t, ``, gdbfake.WithDefaultDone())
	if err := c.Close(context.Background()); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := c.Close(context.Background()); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}
