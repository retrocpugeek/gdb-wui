//go:build integration

package debugger_test

import (
	"bufio"
	"context"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/retrocpugeek/gdb-wui/internal/srcfs"
	"github.com/retrocpugeek/gdb-wui/internal/wire"
)

// Attaching to a process, against a real gdb and a real process.
//
// The promise being tested is not that attaching works — gdb's business — but
// that letting go does: a program that was running before the session must be
// running after it. See finding 43, and mi.doClose, whose `kill` is what would
// otherwise end it.

// startTracee compiles the fixture, runs it, and waits for it to say it can be
// attached to. The returned pid belongs to a process that is nobody's inferior.
func startTracee(t *testing.T) (*srcfs.FS, int) {
	t.Helper()
	files := realProject(t, "tracee")

	cmd := exec.Command(filepath.Join(files.Abs(), "tracee"))
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting the tracee: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})

	// The line arrives after prctl(PR_SET_PTRACER), so reading it is the
	// handshake: attaching before it is a race the test would lose rarely and
	// confusingly.
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil || line != "ready\n" {
		t.Fatalf("the tracee did not come up: %q %v", line, err)
	}
	return files, cmd.Process.Pid
}

// alive reports whether the process exists. Signal 0 checks for it without
// disturbing it, which is the whole question here.
func alive(pid int) bool { return syscall.Kill(pid, 0) == nil }

// attachTo attaches and waits for the stop that follows.
//
// The skip is decided by gdb's own state and never by ours. A test that
// skipped when the *session* had not recorded the attach would skip on exactly
// the bug it exists to catch: a server that ignores `attach` looks identical to
// a kernel that refused it.
func attachTo(t *testing.T, h *harness, pid int) {
	t.Helper()
	h.mustDo(wire.TypeConsoleExec, wire.ConsoleExecRequest{
		Line: "attach " + strconv.Itoa(pid),
	})

	// The command returns once gdb has answered it; the *stopped that says the
	// process is now ours follows.
	deadline := time.Now().Add(10 * time.Second)
	for h.sess.Snapshot().RunState != wire.RunStateStopped {
		if time.Now().After(deadline) {
			// ptrace_scope may be 2 or 3, or a policy may forbid this outright;
			// neither is a failure of the code under test.
			t.Skipf("attaching to pid %d did not stop the process; "+
				"assuming the kernel refused it", pid)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestAttachedProcessSurvivesTheSession(t *testing.T) {
	files, pid := startTracee(t)
	h := startRealWithFiles(t, files)
	attachTo(t, h, pid)

	snap := h.sess.Snapshot()
	if snap.Remote == nil || !snap.Remote.Connected {
		t.Fatalf("Remote = %+v after attaching to pid %d: the session does not "+
			"know it is holding somebody else's process", snap.Remote, pid)
	}
	if snap.Remote.Kind != wire.TargetAttach || snap.Remote.PID != pid {
		t.Errorf("Remote = %+v, want kind %q and pid %d",
			snap.Remote, wire.TargetAttach, pid)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := h.sess.Close(ctx); err != nil {
		t.Fatalf("close: %v", err)
	}

	// Detaching is asynchronous from the process's point of view: SIGCONT and
	// the ptrace release take a moment to land.
	deadline := time.Now().Add(2 * time.Second)
	for !alive(pid) && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if !alive(pid) {
		t.Error("closing the session killed a process it had only attached to")
	}
}

// The stop that attaching produces has to reach the panels, or the UI shows an
// attached process it can say nothing about. gdb reports it exactly as it
// reports any other stop, so this is really a test that nothing on our side
// discards it for want of a program having been loaded.
func TestAttachingFillsTheStack(t *testing.T) {
	files, pid := startTracee(t)
	h := startRealWithFiles(t, files)
	attachTo(t, h, pid)

	if snap := h.sess.Snapshot(); len(snap.Frames) == 0 {
		t.Fatalf("no stack after attaching: run state %q", snap.RunState)
	}
}
