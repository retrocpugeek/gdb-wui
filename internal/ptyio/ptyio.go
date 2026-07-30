// Package ptyio gives the debuggee its own terminal.
//
// Without one, the inferior's stdout goes wherever gdb's does — which is the
// pipe carrying MI — and the program's output arrives interleaved with protocol
// records as unparseable lines. That is survivable (the parser hands them back
// as garbage) but it is not a terminal: there is no way to type into the
// program, and libc switches to block buffering when stdout is not a tty, so a
// prompt printed without a newline never appears at all.
//
// A pty fixes all three at once. gdb is told about it with -inferior-tty-set
// before the program starts.
package ptyio

import (
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"
)

// Terminal is an allocated pty pair.
type Terminal struct {
	// master is our end: what the program writes appears here, and what we
	// write here appears on its stdin.
	master *os.File
	// slave is the program's end. We keep it open for the whole session even
	// though gdb opens it by name — see Close.
	slave *os.File

	mu     sync.Mutex
	closed bool
}

// Open allocates a pty pair.
func Open() (*Terminal, error) {
	master, slave, err := pty.Open()
	if err != nil {
		return nil, fmt.Errorf("ptyio: allocating a pty: %w", err)
	}
	return &Terminal{master: master, slave: slave}, nil
}

// Name is the slave device path, for -inferior-tty-set.
func (t *Terminal) Name() string { return t.slave.Name() }

// Read returns bytes the program wrote.
//
// The returned error is io.EOF once the terminal is finished. EIO is the
// interesting case: on Linux a master read fails with EIO rather than returning
// zero bytes when the last slave descriptor closes, so it means "the other end
// is gone", not "something broke". Holding our own slave fd open (see Open)
// keeps that from happening every time the inferior exits, which would
// otherwise tear down the terminal after the first run.
func (t *Terminal) Read(p []byte) (int, error) {
	n, err := t.master.Read(p)
	if err != nil && isClosed(err) {
		return n, io.EOF
	}
	return n, err
}

// Write sends bytes to the program's stdin.
func (t *Terminal) Write(p []byte) (int, error) {
	n, err := t.master.Write(p)
	if err != nil && isClosed(err) {
		return n, io.EOF
	}
	return n, err
}

// SetReadDeadline bounds a blocking Read.
//
// The session reads in a dedicated goroutine until EOF and needs no deadline;
// this exists so tests can read synchronously. Reading from a helper goroutine
// instead invites two bugs at once — a race on the buffer, and a goroutine that
// outlives its test and steals the next one's bytes.
func (t *Terminal) SetReadDeadline(deadline time.Time) error {
	return t.master.SetReadDeadline(deadline)
}

// Resize sets the window size, so the program's own idea of the terminal
// matches the one on screen. Programs that draw columns read this.
func (t *Terminal) Resize(rows, cols uint16) error {
	if rows == 0 || cols == 0 {
		return nil
	}
	return pty.Setsize(t.master, &pty.Winsize{Rows: rows, Cols: cols})
}

// Close releases both ends.
func (t *Terminal) Close() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return nil
	}
	t.closed = true
	// The slave first: it is the one we have been holding open on purpose, and
	// closing it is what finally lets a blocked master read return.
	err := t.slave.Close()
	if merr := t.master.Close(); err == nil {
		err = merr
	}
	return err
}

// isClosed reports whether an error means the far end has gone away rather
// than something being wrong.
func isClosed(err error) bool {
	if errors.Is(err, io.EOF) || errors.Is(err, os.ErrClosed) {
		return true
	}
	// EIO from a pty master is the normal end-of-session signal on Linux.
	// EAGAIN can surface on a non-blocking fd with no data and no writer.
	return errors.Is(err, syscall.EIO) || errors.Is(err, syscall.EAGAIN)
}
