package debugger

import (
	"fmt"
	"strings"

	"github.com/retrocpugeek/gdb-wui/internal/wire"
)

// Writing back: variables, registers and memory.
//
// Everything else in this package reads. These three are collected here because
// they share the properties that make a write different from a read, and each
// one of those properties was a decision:
//
//   - A write needs a stopped inferior, for the same reason a read does, and it
//     needs the client's stopSeq to match. Writing a value computed from a
//     stop that has since been superseded would land in a frame that is not the
//     one the user was looking at.
//   - Every write reads the target back and returns what gdb says it now holds.
//     Assigning 321 to a char gives 65; echoing the input would hide that until
//     the next stop — finding 28.
//   - Every write broadcasts valueWritten, because it invalidates what other
//     clients are showing as well as this one.
//
// The value a user types is passed to gdb as an expression rather than parsed
// here. "0x10", "counter + 1" and "&head" are all things someone reasonably
// types into a value cell, and gdb already knows how to evaluate them in the
// right frame.

func (s *Session) varsAssign(r *request) (any, *wire.Error) {
	req, werr := decode[wire.VarsAssignRequest](r.req.Payload)
	if werr != nil {
		return nil, werr
	}
	if req.Path == "" {
		return nil, wire.NewError(wire.CodeBadRequest, "path is required")
	}
	value := strings.TrimSpace(req.Value)
	if value == "" {
		return nil, wire.NewError(wire.CodeBadRequest, "a value is required")
	}
	if werr := s.checkStopSeq(req.StopSeq); werr != nil {
		return nil, werr
	}
	thread := s.thread(req.Thread)

	// The same find-or-create the tree uses to expand a node. A local that has
	// never been opened has no varobj, and editing it is a perfectly ordinary
	// first thing to do to it.
	v, werr := s.varobjFor(r.ctx, req.Path, req.ID, req.Expr, thread, req.Frame)
	if werr != nil {
		return nil, werr
	}

	// -var-assign returns the new value, so no separate read-back is needed.
	rec, werr := s.send(r.ctx, fmt.Sprintf("-var-assign %s %s", v.name, quote(value)))
	if werr != nil {
		return nil, werr
	}
	// Assigning to one node moves others: a union's siblings, anything reached
	// through a pointer that was just redirected. gdb only re-reads a varobj
	// when asked, so without this the rest of the tree keeps its old numbers.
	s.updateVarobjs(r.ctx)

	v.value = rec.Results.Str("value")
	// The write is a change like any other, and the panel highlights changes.
	// After updateVarobjs, because that sets marks of its own and this node's
	// is not in doubt.
	v.changed = true

	out := wire.VarsAssign{
		Path:    req.Path,
		ID:      v.name,
		Value:   v.value,
		StopSeq: s.st.stopSeq,
	}
	s.emit(wire.EventValueWritten, wire.ValueWritten{
		StopSeq: s.st.stopSeq,
		What:    "variable",
		Detail:  displayName(req.Path),
		Value:   v.value,
	})
	return out, nil
}

func (s *Session) regsWrite(r *request) (any, *wire.Error) {
	req, werr := decode[wire.RegsWriteRequest](r.req.Payload)
	if werr != nil {
		return nil, werr
	}
	value := strings.TrimSpace(req.Value)
	if value == "" {
		return nil, wire.NewError(wire.CodeBadRequest, "a value is required")
	}
	if werr := s.checkStopSeq(req.StopSeq); werr != nil {
		return nil, werr
	}
	format, werr := registerFormat(req.Format)
	if werr != nil {
		return nil, werr
	}
	thread := s.thread(req.Thread)

	if len(s.st.registerNames) == 0 {
		if werr := s.loadRegisterNames(r.ctx); werr != nil {
			return nil, werr
		}
	}
	if req.Number < 0 || req.Number >= len(s.st.registerNames) {
		return nil, wire.NewError(wire.CodeBadRequest,
			fmt.Sprintf("there is no register %d", req.Number))
	}
	name := s.st.registerNames[req.Number]
	if name == "" {
		// A gap in gdb's numbering. It can be read by number and displayed, but
		// there is no $name to put on the left of an assignment.
		return nil, wire.NewError(wire.CodeUnsupported,
			fmt.Sprintf("register %d has no name, so it cannot be written", req.Number))
	}

	// -gdb-set var, because MI has no register-write command — finding 25. The
	// --thread option is accepted here even though -gdb-set is documented as a
	// pass-through to the CLI `set`.
	cmd := fmt.Sprintf("-gdb-set --thread %d var $%s = %s", thread, name, value)
	if _, werr := s.send(r.ctx, cmd); werr != nil {
		return nil, werr
	}

	// A local held in the register that was just written is now a different
	// number, and the varobj behind it does not know.
	s.updateVarobjs(r.ctx)

	reg, werr := s.readRegister(r, thread, req.Number, format)
	if werr != nil {
		return nil, werr
	}
	s.emit(wire.EventValueWritten, wire.ValueWritten{
		StopSeq: s.st.stopSeq,
		What:    "register",
		Detail:  "$" + name,
		Value:   reg.Value,
	})
	return wire.RegsWrite{
		StopSeq:  s.st.stopSeq,
		ThreadID: thread,
		Format:   format,
		Register: reg,
	}, nil
}

// readRegister reads one register back after writing it.
//
// -data-list-register-values takes a list of numbers, so this asks about the
// one that changed rather than re-reading the whole file. Changed is set here
// rather than asked for: gdb's -data-list-changed-registers compares against
// the last stop, and a register written and then written back to its old value
// would report unchanged when the panel needs to show that an edit landed.
func (s *Session) readRegister(r *request, thread, number int, format string) (wire.Register, *wire.Error) {
	rec, werr := s.send(r.ctx, fmt.Sprintf(
		"-data-list-register-values --thread %d %s %d", thread, format, number))
	if werr != nil {
		return wire.Register{}, werr
	}
	reg := wire.Register{Number: number, Changed: true}
	if number < len(s.st.registerNames) {
		reg.Name = s.st.registerNames[number]
	}
	list, ok := rec.Results.List("register-values")
	if !ok || len(list) == 0 {
		return wire.Register{}, wire.NewError(wire.CodeGDBError,
			fmt.Sprintf("gdb did not report register %d back", number))
	}
	reg.Value = list[0].Results().Str("value")
	return reg, nil
}

// registerFormat validates a display format. Shared with regs.values so the
// two cannot drift into accepting different sets.
func registerFormat(format string) (string, *wire.Error) {
	if format == "" {
		return "x", nil
	}
	if len(format) != 1 || !strings.Contains("xdotNrz", format) {
		return "", wire.NewError(wire.CodeBadRequest, "format must be one of x d o t N r z")
	}
	return format, nil
}

// maxMemWrite bounds one write. The hex view edits a byte at a time; the bound
// is here so that a client cannot turn one message into an arbitrarily long
// gdb command line.
const maxMemWrite = 4 << 10

func (s *Session) memWrite(r *request) (any, *wire.Error) {
	req, werr := decode[wire.MemWriteRequest](r.req.Payload)
	if werr != nil {
		return nil, werr
	}
	if werr := s.checkStopSeq(req.StopSeq); werr != nil {
		return nil, werr
	}
	if strings.TrimSpace(req.Address) == "" {
		return nil, wire.NewError(wire.CodeBadRequest, "address is required")
	}
	data, werr := cleanHex(req.DataHex)
	if werr != nil {
		return nil, werr
	}

	base, werr := s.resolveAddress(r, req.Address)
	if werr != nil {
		return nil, werr
	}
	addr := base
	if req.Offset != 0 {
		addr = uint64(int64(base) + int64(req.Offset))
	}

	// Quoted although gdb takes it bare, because cleanHex is the only thing
	// standing between a client's string and a gdb command line and one guard
	// on that path is not enough.
	cmd := fmt.Sprintf("-data-write-memory-bytes 0x%x %s", addr, quote(data))
	if _, werr := s.send(r.ctx, cmd); werr != nil {
		return nil, werr
	}

	// Bytes underlie everything: a struct field the tree has open may be made
	// of the bytes just written, and gdb answers -var-list-children from its
	// cache until told otherwise.
	s.updateVarobjs(r.ctx)

	count := len(data) / 2
	s.emit(wire.EventValueWritten, wire.ValueWritten{
		StopSeq: s.st.stopSeq,
		What:    "memory",
		Detail:  fmt.Sprintf("0x%x", addr),
	})
	return wire.MemWrite{StopSeq: s.st.stopSeq, Addr: addr, Count: count}, nil
}

// cleanHex validates the bytes of a memory write.
//
// gdb checks some of this itself, and each case below says what it does with
// the input instead — finding 26:
//
//   - Empty is the one that matters. gdb answers ^done and writes nothing, so
//     committing an empty cell would report success and change nothing.
//   - "0xff" is refused with "Invalid argument", which tells a user who typed
//     the prefix they read in the address bar precisely nothing. Stripping it
//     is the friendlier answer and cannot be ambiguous: this is hex already.
//   - "zz" is also "Invalid argument". Naming the input beats that.
//   - Odd length gdb refuses clearly on its own; this only gets in first with
//     a message that says what to do about it.
func cleanHex(in string) (string, *wire.Error) {
	out := strings.TrimSpace(in)
	out = strings.TrimPrefix(out, "0x")
	if out == "" {
		return "", wire.NewError(wire.CodeBadRequest, "no bytes to write")
	}
	if len(out)%2 != 0 {
		return "", wire.NewError(wire.CodeBadRequest,
			fmt.Sprintf("%q is %d hex digits; bytes take two each", in, len(out)))
	}
	if len(out)/2 > maxMemWrite {
		return "", wire.NewError(wire.CodeTooLarge,
			fmt.Sprintf("at most %d bytes per write", maxMemWrite))
	}
	for _, c := range out {
		if !strings.ContainsRune("0123456789abcdefABCDEF", c) {
			return "", wire.NewError(wire.CodeBadRequest,
				fmt.Sprintf("%q is not hex", in))
		}
	}
	return strings.ToLower(out), nil
}
