package debugger

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/retrocpugeek/gdb-wui/internal/mi"
	"github.com/retrocpugeek/gdb-wui/internal/wire"
)

// Disassembly.
//
// Two shapes come back from -data-disassemble and both have to be handled: a
// flat list of instructions, and instructions grouped under src_and_asm_line
// when gdb can attribute them to source. Which one arrives is not a mode the
// caller picks — mode 5 is requested always, and gdb gives the grouped form
// only if the code has debug info. A stripped binary yields the flat form, and
// that is the case M6 is judged on, so the flat path is not a fallback: it is
// the important one.

// maxInstructions bounds one reply. A whole function is usually a few hundred
// instructions; a request that would return a hundred thousand is a mistake
// somewhere, and truncating with a flag beats a 40 MB message.
const maxInstructions = 4000

// disasmWindow is how much to disassemble around the PC when the containing
// function cannot be determined — a stripped binary with no symbol covering
// the address. Backwards is a guess, because x86 instructions are
// variable-length and there is no way to know where the previous one started;
// gdb resyncs quickly in practice.
const (
	disasmWindowBefore = 64
	disasmWindowAfter  = 256
)

func (s *Session) disasmFunction(r *request) (any, *wire.Error) {
	req, werr := decode[wire.DisasmFunctionRequest](r.req.Payload)
	if werr != nil {
		return nil, werr
	}
	if werr := s.checkStopSeq(req.StopSeq); werr != nil {
		return nil, werr
	}

	addr := req.Address
	if addr == "" {
		frame := req.Frame
		if frame == 0 {
			frame = s.st.selFrame
		}
		f, ok := s.frameAt(frame)
		if !ok {
			return nil, wire.NewError(wire.CodeNotReady, "no frame to disassemble")
		}
		addr = f.Address
	}
	if addr == "" {
		return nil, wire.NewError(wire.CodeBadRequest, "no address")
	}

	// The -a option asks for "the function containing this address", which is
	// exactly what a disassembly panel wants and saves guessing a range. It is
	// capability-gated because it is not in every gdb: -list-features reports
	// data-disassemble-a-option when it is available.
	if s.client.HasFeature("data-disassemble-a-option") {
		out, werr := s.disassemble(r.ctx, fmt.Sprintf("-data-disassemble -a %s -- 5", addr))
		if werr == nil {
			out.PC = s.currentPC()
			return out, nil
		}
		// -a fails for an address with no function around it, which is normal
		// in stripped code. Fall through to a window rather than reporting an
		// error the user can do nothing about.
		s.logf("-data-disassemble -a %s: %s", addr, werr.Message)
	}

	n, err := parseAddress(addr)
	if err != nil {
		return nil, wire.NewError(wire.CodeBadRequest, "unparseable address "+addr)
	}
	start := uint64(0)
	if n > disasmWindowBefore {
		start = n - disasmWindowBefore
	}
	out, werr := s.disassemble(r.ctx,
		fmt.Sprintf("-data-disassemble -s 0x%x -e 0x%x -- 5", start, n+disasmWindowAfter))
	if werr != nil {
		return nil, werr
	}
	out.PC = s.currentPC()
	return out, nil
}

func (s *Session) disasmRange(r *request) (any, *wire.Error) {
	req, werr := decode[wire.DisasmRangeRequest](r.req.Payload)
	if werr != nil {
		return nil, werr
	}
	if werr := s.checkStopSeq(req.StopSeq); werr != nil {
		return nil, werr
	}
	start, err := parseAddress(req.Start)
	if err != nil {
		return nil, wire.NewError(wire.CodeBadRequest, "unparseable start address")
	}
	end, err := parseAddress(req.End)
	if err != nil {
		return nil, wire.NewError(wire.CodeBadRequest, "unparseable end address")
	}
	if end <= start {
		return nil, wire.NewError(wire.CodeBadRequest, "end must be after start")
	}
	if end-start > 1<<20 {
		return nil, wire.NewError(wire.CodeTooLarge, "range is larger than 1 MiB")
	}

	out, werr := s.disassemble(r.ctx,
		fmt.Sprintf("-data-disassemble -s 0x%x -e 0x%x -- 5", start, end))
	if werr != nil {
		return nil, werr
	}
	out.PC = s.currentPC()
	return out, nil
}

// disassemble runs one command and parses whichever shape comes back.
func (s *Session) disassemble(ctx context.Context, cmd string) (wire.Disassembly, *wire.Error) {
	rec, werr := s.send(ctx, cmd)
	if werr != nil {
		return wire.Disassembly{}, werr
	}
	list, ok := rec.Results.Get("asm_insns")
	if !ok {
		return wire.Disassembly{StopSeq: s.st.stopSeq}, nil
	}

	out := wire.Disassembly{StopSeq: s.st.stopSeq}

	// The grouped shape: src_and_asm_line entries, each holding a source line
	// and the instructions generated for it.
	if groups := list.All("src_and_asm_line"); len(groups) > 0 {
		out.HasSource = true
		for _, g := range groups {
			line, _ := g.Int("line")
			ref := s.resolveSourceFull(g.Results().Str("fullname"), g.Results().Str("file"), line)
			insns, ok := g.List("line_asm_insn")
			if !ok {
				continue
			}
			for _, in := range insns {
				inst := s.parseInstruction(in.Items)
				inst.Line = line
				src := ref
				inst.Source = &src
				out.Instructions = append(out.Instructions, inst)
				if len(out.Instructions) >= maxInstructions {
					out.Truncated = true
					break
				}
			}
			if out.Truncated {
				break
			}
		}
	} else {
		// The flat shape. This is what a stripped binary gives, and it is the
		// case instruction-level debugging exists for.
		for _, in := range list.Elements() {
			out.Instructions = append(out.Instructions, s.parseInstruction(in.Items))
			if len(out.Instructions) >= maxInstructions {
				out.Truncated = true
				break
			}
		}
	}

	if len(out.Instructions) > 0 {
		first, last := out.Instructions[0], out.Instructions[len(out.Instructions)-1]
		out.Start, out.End = first.Address, last.Address
		out.Func = first.Func
	}
	return out, nil
}

func (s *Session) parseInstruction(t mi.Results) wire.Instruction {
	offset, _ := t.Int("offset")
	inst := wire.Instruction{
		Address: t.Str("address"),
		Func:    t.Str("func-name"),
		Offset:  offset,
		Opcodes: t.Str("opcodes"),
		Text:    t.Str("inst"),
	}
	if n, err := parseAddress(inst.Address); err == nil {
		inst.Addr = n
	}
	return inst
}

// currentPC is the selected frame's address, so the UI can mark it.
func (s *Session) currentPC() string {
	if f, ok := s.frameAt(s.st.selFrame); ok {
		return f.Address
	}
	return ""
}

func parseAddress(s string) (uint64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("empty address")
	}
	// ParseUint with base 0 handles both "0x555..." and a bare decimal, which
	// is what gdb and a hand-typed address respectively look like.
	return strconv.ParseUint(s, 0, 64)
}

// --- instruction-level stepping ---------------------------------------------

func (s *Session) execStepI(r *request) (any, *wire.Error) {
	return s.execResume(r, "-exec-step-instruction")
}

func (s *Session) execNextI(r *request) (any, *wire.Error) {
	return s.execResume(r, "-exec-next-instruction")
}
