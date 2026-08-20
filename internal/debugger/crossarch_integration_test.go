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
// Skipped unless the cross toolchains, the emulators and a multi-architecture
// gdb are all installed; CI installs all of them.

// The architectures under test: three families, both widths of each, and both
// endiannesses of PowerPC. They are different answers to every question here —
// gdb reads a 32-bit ARM stub's registers as r0 and a 64-bit one's as x0, and
// a wrong belief about which is a plausible number rather than an error.
//
// This table is also what the decompiler's cross-architecture tests walk, so
// there is one list of the architectures gdb-wui claims to handle.
var crossArches = []struct {
	name string
	gcc  string
	qemu string
	exe  string
	// arch is what `show architecture` says, and regs are register names only
	// this architecture has.
	arch string
	regs []string
	// entry is what gdb calls the function the stub stops in. Not always
	// `_start`: under PowerPC's ELFv1 the symbol of that name is a function
	// *descriptor* — a triple of addresses in .opd — and the code is a second
	// symbol with a dot in front of it.
	entry string
	// lang and pointerSize are what Ghidra reports for this architecture, for
	// the tests that build a decompiler's-eye view of a frame without one.
	// The language IDs are the ones Ghidra chose for these very binaries —
	// e500 and A2ALT are its reading of the ELF headers, not a preference of
	// ours — and only their family prefix decides which rule applies.
	lang        string
	pointerSize int
}{
	{
		name: "arm", gcc: "arm-linux-gnueabihf-gcc", qemu: "qemu-arm",
		exe: "hello-arm", arch: "armv7", regs: []string{"r0", "cpsr"},
		entry: "_start", lang: "ARM:LE:32:v8", pointerSize: 4,
	},
	{
		name: "aarch64", gcc: "aarch64-linux-gnu-gcc", qemu: "qemu-aarch64",
		exe: "hello-aarch64", arch: "aarch64", regs: []string{"x0", "sp"},
		entry: "_start", lang: "AARCH64:LE:64:v8A", pointerSize: 8,
	},
	// Big-endian, and the 64-bit one is ELFv1 — every function symbol is a
	// descriptor in .opd rather than the code itself. Both are exactly the
	// sort of target this project is for, and neither resembles the host.
	//
	// The two widths share their register names, so `show architecture` is
	// what separates them: powerpc:common against powerpc:common64.
	{
		name: "ppc", gcc: "powerpc-linux-gnu-gcc", qemu: "qemu-ppc",
		exe: "hello-ppc", arch: "powerpc:common", regs: []string{"r0", "lr"},
		entry: "_start", lang: "PowerPC:BE:32:e500", pointerSize: 4,
	},
	{
		name: "ppc64", gcc: "powerpc64-linux-gnu-gcc", qemu: "qemu-ppc64",
		exe: "hello-ppc64", arch: "powerpc:common64", regs: []string{"r0", "lr"},
		entry: "._start", lang: "PowerPC:BE:64:A2ALT", pointerSize: 8,
	},
	// The same processor little-endian, which is ELFv2 and so has no
	// descriptors: the entry symbol loses its dot. Worth both, because the two
	// ABIs are a real fork in what a PowerPC binary looks like.
	{
		name: "ppc64le", gcc: "powerpc64le-linux-gnu-gcc", qemu: "qemu-ppc64le",
		exe: "hello-ppc64le", arch: "powerpc:common64", regs: []string{"r0", "lr"},
		entry: "_start", lang: "PowerPC:LE:64:A2ALT", pointerSize: 8,
	},
	// MIPS, whose rule this repository got wrong for a while: it was
	// established on a firmware image where frame.size and the stack depth
	// happen to be the same number, and gcc's ordinary output is where they
	// are not. glibc calls the entry point __start here.
	{
		name: "mips", gcc: "mips-linux-gnu-gcc", qemu: "qemu-mips",
		exe: "hello-mips", arch: "mips:isa32r2", regs: []string{"zero", "ra"},
		entry: "__start", lang: "MIPS:BE:32:default", pointerSize: 4,
	},
	{
		name: "mips64", gcc: "mips64-linux-gnuabi64-gcc", qemu: "qemu-mips64",
		exe: "hello-mips64", arch: "mips:isa64r2", regs: []string{"zero", "ra"},
		entry: "__start", lang: "MIPS:BE:64:default", pointerSize: 8,
	},
}

// crossProject builds hello.c for another architecture and returns the project
// holding it.
//
// Statically linked so qemu needs no sysroot, and built from a copy inside the
// project for the reason every example in the documentation is: a binary
// records the path its compiler was given, and only a path under the project
// can be served or breakpointed by name.
func crossProject(t *testing.T, gcc, qemu, exe string) *srcfs.FS {
	t.Helper()
	testutil.RequireInstalledTools(t, gcc, qemu, "gdb-multiarch")

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
	cmd := exec.Command(gcc, "-g", "-O0", "-static",
		"-o", filepath.Join(dir, exe), dst)
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

// qemuStub runs the named program under the named emulator, stopped, waiting
// for a debugger.
func qemuStub(t *testing.T, emulator, dir, exe string) int {
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

	cmd := exec.Command(emulator, "-g", fmt.Sprint(port), filepath.Join(dir, exe))
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting qemu: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	return port
}

func TestCrossArchProgramsUnderQemu(t *testing.T) {
	for _, arch := range crossArches {
		t.Run(arch.name, func(t *testing.T) {
			crossArchUnderQemu(t, arch.gcc, arch.qemu, arch.exe, arch.arch, arch.entry, arch.regs)
		})
	}
}

func crossArchUnderQemu(t *testing.T, gcc, qemu, exe, arch, entry string, regs []string) {
	files := crossProject(t, gcc, qemu, exe)
	port := qemuStub(t, qemu, files.Abs(), exe)
	h := startRealWithGDB(t, files, "gdb-multiarch")

	// The ELF first: only loading the program sets the architecture, and
	// connecting without it is the mistake the page's warning is about.
	h.mustDo(wire.TypeExeLoad, wire.ExeLoadRequest{Path: exe})
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
	if got := h.sess.Snapshot().Frames[0].Func; got != entry {
		t.Errorf("stopped in %q, want %s", got, entry)
	}

	// The architecture came out of the ELF rather than out of the stub, which
	// is the claim the page makes. gdb's own words, since there is no MI query.
	//
	// Waited for, not read: console records are events, and they are broadcast
	// from the actor after the request that produced them has been answered.
	// Reading the recorder the moment console.exec returns reads what has
	// arrived so far, which on a loaded runner was nothing at all.
	h.mustDo(wire.TypeConsoleExec, wire.ConsoleExecRequest{Line: "show architecture"})
	waitForConsole(t, h, arch, "gdb to report the "+arch+" architecture")

	// The registers are that architecture's, which is the same fact from the
	// other side: a gdb reading this stub as x86-64 would answer with rax and
	// rip.
	names := h.mustDo(wire.TypeRegsNames, nil).(wire.RegsNames)
	for _, want := range regs {
		if !hasName(names.Names, want) {
			t.Errorf("register names do not include %s, so they are not %s's: %v",
				want, arch, firstFew(names.Names))
		}
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
