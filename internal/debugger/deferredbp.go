package debugger

import (
	"context"
	"debug/elf"
	"fmt"

	"github.com/retrocpugeek/gdb-wui/internal/wire"
)

// Breakpoints on decompiler names, placed before the program has started.
//
// A decompiler name is turned into an address here, since gdb has never heard
// of it, and gdb takes an address breakpoint at face value: when a
// position-independent executable is loaded and relocated, gdb re-resolves its
// breakpoints on names and leaves those on addresses where they were. Placed
// before `run`, a breakpoint on FUN_0010ce00 would go in at the link-time
// 0xce00 and fail to insert at 0x5555555..., with the program left running
// past where the user asked to stop.
//
// So before the program starts, such a breakpoint is given to gdb as the name:
// a pending breakpoint, listed as such, which gdb cannot resolve but keeps. A
// run with any of these starts at the first instruction instead (starti),
// where the program has been mapped and the decompiler's addresses can be
// translated; each is replaced by a breakpoint on its address there, and the
// run carries on to wherever it was going. The stop in between is not
// reported.

// deferredStart is a run that stopped at the first instruction only to resolve
// deferred breakpoints, and what it was asked to do.
type deferredStart struct {
	// resume carries on once they are placed: false for Run→entry, which
	// wanted the stop anyway.
	resume bool
	// stopAtMain is Run→main, which then needs its breakpoint on main.
	stopAtMain bool
}

// started reports whether there is a process whose addresses are final.
func (s *Session) started() bool {
	return s.st.runState == wire.RunStateStopped || s.st.runState == wire.RunStateRunning
}

// exeIsPIE reports whether the loaded program is position-independent, which
// is what makes its addresses before `run` differ from those after.
func (s *Session) exeIsPIE() bool {
	if s.st.exePath == "" || s.files == nil {
		return false
	}
	abs, err := s.files.AbsPath(s.st.exePath)
	if err != nil {
		return false
	}
	f, err := elf.Open(abs)
	if err != nil {
		return false
	}
	defer f.Close()
	return f.Type == elf.ET_DYN
}

// deferDecompName reports whether a breakpoint on loc must wait for the
// program to start: a decompiler name, before the program has started, in a
// program that will move when it does.
func (s *Session) deferDecompName(r *request, loc string) bool {
	if s.started() || !plausibleDecompName(loc) || s.gdbKnowsSymbol(r, loc) {
		return false
	}
	if _, ok := s.decompNamed(r, loc); !ok {
		return false
	}
	return s.exeIsPIE()
}

// deferredBreakpoints are the pending breakpoints on decompiler names.
func (s *Session) deferredBreakpoints(r *request) []wire.Breakpoint {
	var out []wire.Breakpoint
	for _, bp := range s.st.breakpoints {
		if !bp.Pending || !plausibleDecompName(bp.Original) || s.gdbKnowsSymbol(r, bp.Original) {
			continue
		}
		if _, ok := s.decompNamed(r, bp.Original); ok {
			out = append(out, bp)
		}
	}
	return out
}

// resolveDeferred replaces each deferred breakpoint with one on its address,
// now that the program has been loaded: same condition, temporariness and
// enabled state. One the decompiler cannot place stays pending, and says why.
func (s *Session) resolveDeferred(ctx context.Context) {
	r := &request{ctx: ctx}
	for _, bp := range s.deferredBreakpoints(r) {
		addr, ok := s.decompAddressOf(r, bp.Original)
		if !ok {
			s.logf("breakpoint %d on %s: the decompiler could not place it, so it stays pending",
				bp.Number, bp.Original)
			continue
		}
		placed, werr := s.insertBreakpoint(r, breakpointSpec{
			location:  fmt.Sprintf("*0x%x", addr),
			temporary: bp.Temporary,
			condition: bp.Condition,
		})
		if werr != nil {
			s.logf("breakpoint %d on %s at 0x%x: %s", bp.Number, bp.Original, addr, werr.Message)
			continue
		}
		if !bp.Enabled {
			if _, werr := s.send(ctx, fmt.Sprintf("-break-disable %d", placed.Number)); werr == nil {
				placed.Enabled = false
				s.st.breakpoints[placed.Number] = placed
			}
		}
		s.dropBreakpoint(r, bp.Number)
		s.logf("breakpoint on %s placed at 0x%x, now that the program is loaded", bp.Original, addr)
	}
}

// startCommand is the command that starts a run, and the deferred start to
// record with it: starti, when deferred breakpoints are waiting for the
// program to be loaded.
func (s *Session) startCommand(r *request, cmd string, stopAtEntry, stopAtMain bool) (string, *deferredStart) {
	if len(s.deferredBreakpoints(r)) == 0 {
		return cmd, nil
	}
	return `-interpreter-exec console "starti"`, &deferredStart{resume: !stopAtEntry, stopAtMain: stopAtMain}
}

// finishDeferredStart handles the stop a deferred start asked for: place the
// breakpoints, and carry on unless the stop was wanted. Reports whether the
// stop has been dealt with and is not to be shown.
func (s *Session) finishDeferredStart(ctx context.Context) bool {
	ds := s.st.deferredStart
	if ds == nil {
		return false
	}
	s.st.deferredStart = nil
	s.resolveDeferred(ctx)
	if !ds.resume {
		return false
	}
	if ds.stopAtMain {
		// What -exec-run --start would have placed. A program with no main
		// symbol runs on, as --start would have let it.
		if _, werr := s.send(ctx, "-break-insert -t main"); werr != nil {
			s.logf("Run to main: %s", werr.Message)
		}
	}
	if _, werr := s.send(ctx, "-exec-continue"); werr != nil {
		s.logf("continuing after placing deferred breakpoints: %s", werr.Message)
		return false
	}
	s.st.runState = wire.RunStateRunning
	return true
}
