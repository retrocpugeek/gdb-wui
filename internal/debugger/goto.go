package debugger

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/retrocpugeek/gdb-wui/internal/wire"
)

// Where is this?
//
// One resolver behind the go-to box, rather than one per view. The views need
// different facts about the same place — the source view needs a file and a
// line, the disassembly needs an address — and answering them separately would
// let "walk" mean two places at once.
//
// It also has to be gdb that answers. -symbol-info-* reports link-time
// addresses, so for a position-independent executable a client resolving names
// against the symbol table would be wrong about every one of them the moment
// the program started and was relocated. gdb knows the load bias; nothing else
// does.

func (s *Session) gotoLocate(r *request) (any, *wire.Error) {
	req, werr := decode[wire.GotoLocateRequest](r.req.Payload)
	if werr != nil {
		return nil, werr
	}
	target := strings.TrimSpace(req.Target)
	if target == "" {
		return nil, wire.NewError(wire.CodeBadRequest, "a target is required")
	}
	if werr := s.checkStopSeq(req.StopSeq); werr != nil {
		return nil, werr
	}

	out := wire.GotoLocation{Target: target}

	if file, line, ok := splitFileLine(target); ok {
		return s.locateLine(r.ctx, out, file, line)
	}

	// Anything else is an expression for gdb: a function name, a hex address,
	// "$pc", "&head". resolveAddress is the same path the memory viewer takes,
	// which is what makes the two boxes accept the same things.
	addr, werr := s.resolveAddress(r, target)
	if werr != nil {
		// gdb has never heard of FUN_0010e2dc or DAT_001a08de: they are the
		// decompiler's names for a program that has none of its own, and they
		// are the only names a stripped binary offers. Asked second, so that a
		// real symbol always wins and gdb stays in charge of what it knows.
		//
		// Resolved by name and not by reading the address out of the name. The
		// digits in FUN_0010e2dc are Ghidra's link-time address, and the
		// program is somewhere else entirely once a PIE has been relocated.
		if !plausibleDecompName(target) {
			return nil, werr
		}
		found, ok := s.decompAddressOf(r, target)
		if !ok {
			return nil, werr
		}
		out.Addr = found
		out.Address = fmt.Sprintf("0x%x", found)
		s.describeAddress(r.ctx, &out)
		if out.Func == "" {
			// The ordinary case: gdb has no symbol covering this address, so it
			// named nothing. The name that was typed is the best there is.
			out.Func = target
		}
		return out, nil
	}
	out.Addr = addr
	out.Address = fmt.Sprintf("0x%x", addr)
	s.describeAddress(r.ctx, &out)
	return out, nil
}

// locateLine answers a FILE:LINE target.
//
// -data-disassemble takes -f/-l, and although it starts at the containing
// function's entry rather than at the line asked for, the grouped output says
// which line each instruction belongs to. So the address is found by looking
// for the line rather than by trusting where gdb began. -n -1 asks for the
// whole function, which bounds the work by the function's size and guarantees
// the line is inside what came back.
//
// The alternative was `info line FILE:N`, whose reply is an English sentence.
// It is parseable here — gdb runs under LC_ALL=C on purpose — but it names only
// the file's basename, and resolving a source path into the project needs the
// full one that -data-disassemble reports. Finding 30.
func (s *Session) locateLine(ctx context.Context, out wire.GotoLocation, file string, line int) (any, *wire.Error) {
	dis, werr := s.disassemble(ctx,
		fmt.Sprintf("-data-disassemble -f %s -l %d -n -1 -- 5", quote(file), line))
	if werr != nil {
		// "Invalid filename" and "Invalid line number" both land here, and both
		// are the user's answer rather than a fault.
		return nil, wire.NewError(wire.CodeNotFound, werr.Message)
	}

	for _, in := range dis.Instructions {
		if in.Line != line {
			continue
		}
		out.Addr = in.Addr
		out.Address = in.Address
		out.Func = in.Func
		out.Source = in.Source
		return out, nil
	}

	// The line is real but generated no code of its own — a declaration, a
	// blank line, a brace. There is no address, and saying so beats handing
	// back the address of some neighbouring line as though it were this one.
	// The source view can still go there, which is the view a FILE:LINE was
	// most likely typed for.
	ref := s.resolveSourceFull("", file, line)
	out.Source = &ref
	return out, nil
}

// describeAddress fills in the function and source line covering an address.
//
// One instruction is enough: gdb reports it grouped under its source line when
// there is debug info, and flat when there is not. Both are ordinary — the flat
// answer is what a stripped binary gives, and what data gives.
func (s *Session) describeAddress(ctx context.Context, out *wire.GotoLocation) {
	dis, werr := s.disassemble(ctx,
		fmt.Sprintf("-data-disassemble -s 0x%x -e 0x%x -- 5", out.Addr, out.Addr+1))
	if werr != nil {
		// An address in no mapped section — a plain data pointer, or a typo
		// that still parsed. The address itself is still the answer.
		return
	}
	if len(dis.Instructions) == 0 {
		return
	}
	in := dis.Instructions[0]
	out.Func = in.Func
	if in.Source != nil && in.Source.Line > 0 {
		out.Source = in.Source
	}
}

// splitFileLine recognises gdb's own FILE:LINE spelling.
//
// The last colon, so that a C++ name keeps its "::" — "Foo::bar" splits into
// "Foo:" and "bar", which is not a number and therefore not a location. The
// line has to be a positive decimal, which is also what stops "0x40:1" and
// "localhost:1234" from being read as source positions by accident.
func splitFileLine(target string) (string, int, bool) {
	at := strings.LastIndexByte(target, ':')
	if at < 0 {
		return "", 0, false
	}
	line, err := strconv.Atoi(target[at+1:])
	if err != nil || line <= 0 {
		return "", 0, false
	}
	file := strings.TrimSpace(target[:at])
	if file == "" || strings.HasSuffix(file, ":") {
		return "", 0, false
	}
	return file, line, true
}
