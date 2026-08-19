//go:build integration

package debugger_test

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/retrocpugeek/gdb-wui/internal/srcfs"
	"github.com/retrocpugeek/gdb-wui/internal/testutil"
	"github.com/retrocpugeek/gdb-wui/internal/wire"
)

// Debugging a program for another architecture, through an emulator's stub.
//
// This is the one path that cannot be checked against the host's own binaries,
// and it is the one where being wrong is quiet: gdb reads a stub's registers
// through whatever architecture it believes in, so the wrong one yields
// plausible nonsense rather than an error. The claims in
// docs/features/remote.md are the assertions here, so the page cannot drift
// from what the code does.
//
// Skipped unless the cross toolchain, the emulator and a multi-architecture gdb
// are all installed; CI installs all three.

// armProject builds hello.c for ARM and returns the project holding it.
//
// Statically linked so qemu needs no sysroot, and built from a copy inside the
// project for the reason every example in the documentation is: a binary
// records the path its compiler was given, and only a path under the project
// can be served or breakpointed by name.
func armProject(t *testing.T) *srcfs.FS {
	t.Helper()
	testutil.RequireInstalledTools(t, "arm-linux-gnueabihf-gcc", "qemu-arm", "gdb-multiarch")

	dir := t.TempDir()
	src := filepath.Join(testutil.RepoRoot(t), "testdata", "fixtures", "hello.c")
	body, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "hello.c")
	if err := os.WriteFile(dst, body, 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("arm-linux-gnueabihf-gcc", "-g", "-O0", "-static",
		"-o", filepath.Join(dir, "hello-arm"), dst)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("cross-compiling hello.c: %v\n%s", err, out)
	}

	f, err := srcfs.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	return f
}

// qemuStub runs the program under qemu, stopped, waiting for a debugger.
func qemuStub(t *testing.T, files *srcfs.FS) int {
	t.Helper()
	// A port the kernel just handed out, released again immediately. Racy in
	// principle and not in practice, and the alternative — a fixed port — fails
	// whenever two of these run at once.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()

	cmd := exec.Command("qemu-arm", "-g", fmt.Sprint(port),
		filepath.Join(files.Abs(), "hello-arm"))
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting qemu: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	return port
}

func TestArmProgramUnderQemu(t *testing.T) {
	files := armProject(t)
	port := qemuStub(t, files)
	h := startRealWithGDB(t, files, "gdb-multiarch")

	// The ELF first: only loading the program sets the architecture, and
	// connecting without it is the mistake the page's warning is about.
	h.mustDo(wire.TypeExeLoad, wire.ExeLoadRequest{Path: "hello-arm"})
	h.mustDo(wire.TypeConsoleExec, wire.ConsoleExecRequest{
		Line: fmt.Sprintf("target remote 127.0.0.1:%d", port),
	})

	if remote := h.sess.Snapshot().Remote; remote == nil || !remote.Connected {
		t.Fatalf("Remote = %+v after connecting to qemu", remote)
	}

	// qemu holds the program at its entry point until a debugger arrives, so
	// there is a stop to read without running anything.
	waitFor(t, h, func(snap wire.Hello) bool {
		return snap.RunState == wire.RunStateStopped && len(snap.Frames) > 0
	}, "a stop at the entry point")
	if got := h.sess.Snapshot().Frames[0].Func; got != "_start" {
		t.Errorf("stopped in %q, want _start", got)
	}

	// The architecture came out of the ELF rather than out of the stub, which
	// is the claim the page makes. gdb's own words, since there is no MI query.
	//
	// Waited for, not read: console records are events, and they are broadcast
	// from the actor after the request that produced them has been answered.
	// Reading the recorder the moment console.exec returns reads what has
	// arrived so far, which on a loaded runner was nothing at all.
	h.mustDo(wire.TypeConsoleExec, wire.ConsoleExecRequest{Line: "show architecture"})
	waitForConsole(t, h, "armv7", "gdb to report an ARM architecture")

	// Registers are ARM's, which is the same fact from the other side: a gdb
	// reading this stub as x86-64 would answer with rax and rip.
	names := h.mustDo(wire.TypeRegsNames, nil).(wire.RegsNames)
	if !hasName(names.Names, "r0") || !hasName(names.Names, "cpsr") {
		t.Errorf("register names are not ARM's: %v", firstFew(names.Names))
	}

	// And source-level debugging over the stub: a breakpoint by file and line,
	// resolved to the function, then continued to.
	bp := h.mustDo(wire.TypeBpSetSource, wire.BreakpointRequest{
		Path: "hello.c", Line: lineMainLoop,
	}).(wire.Breakpoint)
	if bp.Func != "main" {
		t.Errorf("hello.c:%d resolved to %q, want main", lineMainLoop, bp.Func)
	}

	// Forget the stop qemu was already holding, or the wait below matches it
	// and reports success without the program having moved.
	h.rec.reset()
	h.mustDo(wire.TypeExecContinue, wire.ExecRequest{})
	stopped := h.rec.wait(t, wire.EventStopped).(wire.Stopped)
	if len(stopped.Frames) == 0 || stopped.Frames[0].Source.Line != lineMainLoop {
		t.Fatalf("did not stop on hello.c:%d: %+v", lineMainLoop, stopped.Frames)
	}

	// Leaving is `disconnect`, and the indicator has to follow it.
	h.mustDo(wire.TypeConsoleExec, wire.ConsoleExecRequest{Line: "disconnect"})
	if remote := h.sess.Snapshot().Remote; remote != nil {
		t.Errorf("Remote = %+v after disconnecting", remote)
	}
}

// waitFor polls the snapshot, for the states that arrive as events rather than
// as replies.
func waitFor(t *testing.T, h *harness, ok func(wire.Hello) bool, what string) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		if ok(h.sess.Snapshot()) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// consoleText is everything gdb has written to the console this session.
func consoleText(h *harness) string {
	var out string
	for _, e := range h.rec.all() {
		if e.name != wire.EventConsole {
			continue
		}
		if m, ok := e.payload.(map[string]string); ok {
			out += m["text"]
		}
	}
	return out
}

// waitForConsole waits for gdb to have written want to the console.
func waitForConsole(t *testing.T, h *harness, want, what string) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		if strings.Contains(consoleText(h), want) {
			return
		}
		if time.Now().After(deadline) {
			t.Errorf("timed out waiting for %s; the console says:\n%s",
				what, consoleText(h))
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func hasName(names []string, want string) bool {
	for _, n := range names {
		if n == want {
			return true
		}
	}
	return false
}

func firstFew(names []string) []string {
	if len(names) > 8 {
		return names[:8]
	}
	return names
}
