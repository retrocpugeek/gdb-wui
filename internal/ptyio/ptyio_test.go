package ptyio

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestOpenAndName(t *testing.T) {
	term, err := Open()
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer term.Close()

	if !strings.HasPrefix(term.Name(), "/dev/") {
		t.Errorf("Name = %q, want a device path", term.Name())
	}
	if _, err := os.Stat(term.Name()); err != nil {
		t.Errorf("the slave device does not exist: %v", err)
	}
}

// TestSlaveHeldOpenAcrossProcessExit is the reason Open keeps a slave fd.
//
// On Linux, reading a pty master fails with EIO once the last slave descriptor
// closes. If the only slave were the child's, then every time the debuggee
// exited the terminal would die with it, and the next run would have nowhere to
// write. Holding one keeps the pty alive across any number of runs.
func TestSlaveHeldOpenAcrossProcessExit(t *testing.T) {
	term, err := Open()
	if err != nil {
		t.Fatal(err)
	}
	defer term.Close()

	// Two children in turn, both writing to the same terminal.
	for i, word := range []string{"first", "second"} {
		cmd := exec.Command("/bin/echo", word)
		slave, err := os.OpenFile(term.Name(), os.O_RDWR, 0)
		if err != nil {
			t.Fatalf("run %d: opening the slave: %v", i, err)
		}
		cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
		if err := cmd.Run(); err != nil {
			slave.Close()
			t.Fatalf("run %d: %v", i, err)
		}
		slave.Close()

		got := readSome(t, term)
		if !strings.Contains(got, word) {
			t.Errorf("run %d: read %q, want it to contain %q", i, got, word)
		}
	}
}

// TestWriteReachesTheProgram is the half that makes the terminal interactive.
func TestWriteReachesTheProgram(t *testing.T) {
	term, err := Open()
	if err != nil {
		t.Fatal(err)
	}
	defer term.Close()

	slave, err := os.OpenFile(term.Name(), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer slave.Close()

	cmd := exec.Command("/bin/cat")
	cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	}()

	// \r, not \n: the line discipline maps CR to NL on input (ICRNL), and a
	// terminal sends CR when you press Enter. Sending \n works by accident on
	// some programs and not others.
	if _, err := term.Write([]byte("hello\r")); err != nil {
		t.Fatalf("Write: %v", err)
	}

	got := readUntil(t, term, "hello", 3*time.Second)
	if !strings.Contains(got, "hello") {
		t.Errorf("read %q, want it to contain the echoed input", got)
	}
}

// TestEchoIsOnByDefault: the terminal echoes typed characters, so the frontend
// gets that for free rather than having to render local echo itself.
func TestEchoIsOnByDefault(t *testing.T) {
	term, err := Open()
	if err != nil {
		t.Fatal(err)
	}
	defer term.Close()

	if _, err := term.Write([]byte("abc")); err != nil {
		t.Fatal(err)
	}
	got := readUntil(t, term, "abc", 2*time.Second)
	if !strings.Contains(got, "abc") {
		t.Errorf("read %q; ECHO appears to be off, so the UI would show nothing "+
			"as the user types", got)
	}
}

func TestResize(t *testing.T) {
	term, err := Open()
	if err != nil {
		t.Fatal(err)
	}
	defer term.Close()

	if err := term.Resize(24, 80); err != nil {
		t.Fatalf("Resize: %v", err)
	}
	// Zero is ignored rather than being an error: a hidden panel reports 0x0
	// and that is not worth failing a request over.
	if err := term.Resize(0, 0); err != nil {
		t.Errorf("Resize(0,0) = %v, want it ignored", err)
	}
}

func TestReadAfterCloseIsEOF(t *testing.T) {
	term, err := Open()
	if err != nil {
		t.Fatal(err)
	}
	if err := term.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	buf := make([]byte, 16)
	if _, err := term.Read(buf); !errors.Is(err, io.EOF) {
		t.Errorf("Read after Close = %v, want EOF", err)
	}
	if err := term.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
}

func TestIsClosedRecognisesEIO(t *testing.T) {
	// EIO from a master read is the normal end of session on Linux, not a
	// fault, and treating it as one would surface an error dialog every time a
	// program exits.
	if !isClosed(syscall.EIO) {
		t.Error("EIO is not recognised as a closed terminal")
	}
	if !isClosed(io.EOF) || !isClosed(os.ErrClosed) {
		t.Error("EOF or ErrClosed is not recognised as closed")
	}
	if isClosed(syscall.EINVAL) {
		t.Error("EINVAL was treated as a closed terminal; it is a real error")
	}
}

func readSome(t *testing.T, term *Terminal) string {
	t.Helper()
	return readUntil(t, term, "", time.Second)
}

// readUntil reads synchronously until the wanted text appears or the deadline
// passes. No helper goroutine: one would race on the buffer and, worse, outlive
// the call and consume bytes the next assertion is waiting for.
func readUntil(t *testing.T, term *Terminal, want string, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	if err := term.SetReadDeadline(deadline); err != nil {
		t.Fatalf("SetReadDeadline: %v", err)
	}
	defer func() { _ = term.SetReadDeadline(time.Time{}) }()

	var sb strings.Builder
	buf := make([]byte, 4096)
	for time.Now().Before(deadline) {
		n, err := term.Read(buf)
		if n > 0 {
			sb.Write(buf[:n])
			if want == "" || strings.Contains(sb.String(), want) {
				return sb.String()
			}
		}
		if err != nil {
			return sb.String()
		}
	}
	return sb.String()
}
