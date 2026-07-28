//go:build integration

package mi_test

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/retrocpugeek/gdb-wui/internal/mi"
	"github.com/retrocpugeek/gdb-wui/internal/testutil"
)

// startGDB brings up a real gdb with the full handshake.
func startGDB(t *testing.T, args ...string) (*mi.Client, *collector) {
	t.Helper()
	testutil.RequireGDB(t, 10)

	col := newCollector()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	t.Cleanup(cancel)

	client, err := mi.Start(ctx, mi.Options{
		Args:    args,
		Handler: col.handle,
		Logf:    t.Logf,
		// HOME is redirected even though --nx is always passed: belt and
		// braces, since anything gdb writes should land in the temp dir.
		ExtraEnv: []string{"HOME=" + t.TempDir()},
	})
	if err != nil {
		t.Fatalf("start gdb: %v", err)
	}
	t.Cleanup(func() {
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := client.Close(shutdown); err != nil {
			t.Errorf("close: %v", err)
		}
	})
	return client, col
}

// TestHandshakeAgainstRealGDB is the load-bearing check on the startup
// sequence. mi-async in particular defaults to off, and if the handshake stops
// turning it on, Pause silently stops working in a way no unit test can see.
func TestHandshakeAgainstRealGDB(t *testing.T) {
	c, _ := startGDB(t)

	for _, tc := range []struct{ setting, want string }{
		{"mi-async", "on"},
		{"non-stop", "off"},
		{"confirm", "off"},
		{"pagination", "off"},
	} {
		rec, err := c.Send(t.Context(), "-gdb-show "+tc.setting)
		if err != nil {
			t.Errorf("-gdb-show %s: %v", tc.setting, err)
			continue
		}
		if got := rec.Results.Str("value"); got != tc.want {
			t.Errorf("%s = %q, want %q", tc.setting, got, tc.want)
		}
	}

	// Every feature the design depends on, checked once, here.
	for _, f := range []string{
		"thread-info",
		"data-read-memory-bytes",
		"breakpoint-notifications",
		"exec-run-start-option",
		"undefined-command-error-code",
	} {
		if !c.HasFeature(f) {
			t.Errorf("gdb does not advertise the %q feature", f)
		}
	}
}

// TestBreakpointRunStopAgainstRealGDB is the M1 vertical slice: load a program,
// break, run, and receive a *stopped carrying a frame.
func TestBreakpointRunStopAgainstRealGDB(t *testing.T) {
	bin := testutil.DebugFixture(t, "hello")
	c, col := startGDB(t, bin)

	rec, err := c.Send(t.Context(), "-break-insert main")
	if err != nil {
		t.Fatalf("-break-insert: %v", err)
	}
	bkpt, ok := rec.Results.Tuple("bkpt")
	if !ok {
		t.Fatalf("no bkpt in reply: %s", rec.Raw)
	}
	if got := bkpt.Str("func"); got != "main" {
		t.Errorf("breakpoint func = %q, want main", got)
	}

	if _, err := c.Send(t.Context(), "-exec-run"); err != nil {
		t.Fatalf("-exec-run: %v", err)
	}

	stopped := col.waitFor(t, "*stopped at the breakpoint", func(r mi.Record) bool {
		return r.Type == mi.RecExec && r.Class == mi.ClassStopped &&
			r.Results.Str("reason") == "breakpoint-hit"
	})
	frame, ok := stopped.Results.Tuple("frame")
	if !ok {
		t.Fatalf("*stopped carried no frame: %s", stopped.Raw)
	}
	if got := frame.Str("func"); got != "main" {
		t.Errorf("stopped in %q, want main", got)
	}
	if !strings.HasSuffix(frame.Str("fullname"), "hello.c") {
		t.Errorf("fullname = %q, want it to end in hello.c", frame.Str("fullname"))
	}

	// The stack must survive parsing with its repeated "frame" keys intact.
	rec, err = c.Send(t.Context(), "-stack-list-frames")
	if err != nil {
		t.Fatalf("-stack-list-frames: %v", err)
	}
	stack, ok := rec.Results.Get("stack")
	if !ok {
		t.Fatalf("no stack: %s", rec.Raw)
	}
	if n := len(stack.All("frame")); n < 1 {
		t.Errorf("stack has %d frames, want at least 1", n)
	}
}

// TestRunStateErrorsAgainstRealGDB pins the exact error strings the run-state
// gate turns into a documented "busy" response. If a future gdb rewords them,
// this test is where that is discovered — not in the field.
func TestRunStateErrorsAgainstRealGDB(t *testing.T) {
	testutil.RequireTools(t, "sleep")
	c, _ := startGDB(t, "/usr/bin/sleep")

	if _, err := c.Send(t.Context(), "-exec-arguments 30"); err != nil {
		t.Fatalf("-exec-arguments: %v", err)
	}
	if _, err := c.Send(t.Context(), "-exec-run"); err != nil {
		t.Fatalf("-exec-run: %v", err)
	}

	// -thread-info is the one query that works while running, and it is what
	// the run-state gate uses to know the inferior is up.
	deadline := time.Now().Add(5 * time.Second)
	var running bool
	for time.Now().Before(deadline) {
		rec, err := c.Send(t.Context(), "-thread-info")
		if err != nil {
			t.Fatalf("-thread-info while running: %v", err)
		}
		threads, _ := rec.Results.List("threads")
		if len(threads) > 0 && threads[0].Results().Str("state") == "running" {
			running = true
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !running {
		t.Fatal("inferior never reported state=running")
	}

	for _, tc := range []struct{ cmd, want string }{
		{"-exec-continue", "Cannot execute this command while the selected thread is running."},
		{"-stack-list-frames", "Selected thread is running."},
		{"-data-list-register-values x", "Selected thread is running."},
	} {
		_, err := c.Send(t.Context(), tc.cmd)
		gerr, ok := mi.AsError(err)
		if !ok {
			t.Errorf("%s: err = %v, want a gdb error", tc.cmd, err)
			continue
		}
		if gerr.Msg != tc.want {
			t.Errorf("%s:\n  got  %q\n  want %q\n(the run-state gate keys on this text)",
				tc.cmd, gerr.Msg, tc.want)
		}
	}

	// -exec-interrupt must work while running — the whole reason mi-async is on.
	if _, err := c.SendUnlocked(t.Context(), "-exec-interrupt"); err != nil {
		t.Fatalf("-exec-interrupt: %v", err)
	}
}

// TestInferiorOutputInterleaves is finding 3 against a real debugger: the
// program's stdout really does land in the MI stream when gdb is on pipes,
// which is why RecGarbage and the inferior pty both exist.
func TestInferiorOutputInterleaves(t *testing.T) {
	bin := testutil.DebugFixture(t, "hello")
	c, col := startGDB(t, bin)

	if _, err := c.Send(t.Context(), "-exec-run"); err != nil {
		t.Fatalf("-exec-run: %v", err)
	}
	col.waitFor(t, "the program's exit", func(r mi.Record) bool {
		return r.Type == mi.RecExec && r.Class == mi.ClassStopped &&
			strings.HasPrefix(r.Results.Str("reason"), "exited")
	})

	var sawGarbage bool
	for _, r := range col.all() {
		if r.Type == mi.RecGarbage && strings.Contains(r.Text, "total=") {
			sawGarbage = true
			if r.Err != nil {
				t.Errorf("inferior output carried a parse error: %v", r.Err)
			}
		}
	}
	if !sawGarbage {
		t.Skip("this gdb did not interleave inferior stdout into the MI stream; " +
			"the pty in M5 makes this moot either way")
	}
}

// TestNoDebugInfoDegrades is finding 5: a stripped binary must produce clean
// errors and empty results, not surprises.
func TestNoDebugInfoDegrades(t *testing.T) {
	bin := testutil.Fixture(t, "nodebug", "-O0")
	if out, err := runCmd("strip", bin); err != nil {
		t.Fatalf("strip: %v\n%s", err, out)
	}
	c, _ := startGDB(t, bin)

	if _, err := c.Send(t.Context(), "-break-insert main"); err == nil {
		t.Error("-break-insert main succeeded on a stripped binary; expected an error")
	}
	rec, err := c.Send(t.Context(), "-file-list-exec-source-files")
	if err != nil {
		t.Fatalf("-file-list-exec-source-files: %v", err)
	}
	files, ok := rec.Results.List("files")
	if !ok {
		t.Fatalf("no files list: %s", rec.Raw)
	}
	if len(files) != 0 {
		t.Errorf("got %d source files for a stripped binary, want 0", len(files))
	}
}

// TestGDBDeathIsReported kills gdb out from under the client.
func TestGDBDeathIsReported(t *testing.T) {
	c, _ := startGDB(t)

	// `kill -9` on gdb itself, from inside gdb, via its own shell escape.
	_, _ = c.Send(t.Context(), `-interpreter-exec console "shell kill -9 $PPID"`)

	select {
	case <-c.Dead():
	case <-time.After(5 * time.Second):
		t.Fatal("client did not notice gdb dying")
	}
	if c.DeadErr() == nil {
		t.Error("DeadErr is nil after gdb died")
	}
	if _, err := c.Send(t.Context(), "-gdb-show confirm"); err == nil {
		t.Error("Send succeeded after gdb died")
	}
}

func runCmd(name string, args ...string) (string, error) {
	out, err := exec.Command(name, args...).CombinedOutput()
	return string(out), err
}
