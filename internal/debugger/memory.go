package debugger

import (
	"fmt"
	"strings"

	"github.com/retrocpugeek/gdb-wui/internal/wire"
)

// The memory viewer's server side.

// maxMemRead bounds one request. The viewer asks in chunks; a single huge read
// would be a message nobody renders and a pause nobody expects.
const maxMemRead = 64 << 10

func (s *Session) memRead(r *request) (any, *wire.Error) {
	req, werr := decode[wire.MemReadRequest](r.req.Payload)
	if werr != nil {
		return nil, werr
	}
	if werr := s.checkStopSeq(req.StopSeq); werr != nil {
		return nil, werr
	}
	if strings.TrimSpace(req.Address) == "" {
		return nil, wire.NewError(wire.CodeBadRequest, "address is required")
	}
	if req.Count <= 0 {
		return nil, wire.NewError(wire.CodeBadRequest, "count must be positive")
	}
	if req.Count > maxMemRead {
		return nil, wire.NewError(wire.CodeTooLarge,
			fmt.Sprintf("at most %d bytes per read", maxMemRead))
	}

	// The address may be any gdb expression — "&cfg", "$sp", "buf+16" — so the
	// offset cannot simply be added to a string. Resolve first, then offset.
	base, werr := s.resolveAddress(r, req.Address)
	if werr != nil {
		return nil, werr
	}
	addr := base
	if req.Offset != 0 {
		addr = uint64(int64(base) + int64(req.Offset))
	}

	out := wire.Memory{
		StopSeq:   s.st.stopSeq,
		Requested: req.Address,
		Addr:      addr,
		Count:     req.Count,
	}

	cmd := fmt.Sprintf("-data-read-memory-bytes 0x%x %d", addr, req.Count)
	rec, werr := s.send(r.ctx, cmd)
	if werr != nil {
		// gdb fails the whole read rather than returning what it could, so an
		// unmapped page anywhere in the range means no bytes at all. That is
		// reported as a readable outcome, not an error: pointing a hex viewer
		// at unmapped memory is how you discover it is unmapped, and the
		// viewer renders "??" rather than raising a dialog.
		if werr.Code == wire.CodeGDBError {
			out.Unreadable = true
			return out, nil
		}
		return nil, werr
	}

	list, ok := rec.Results.List("memory")
	if !ok {
		out.Unreadable = true
		return out, nil
	}
	for _, m := range list {
		begin := m.Results().Str("begin")
		start, err := parseAddress(begin)
		if err != nil {
			continue
		}
		out.Ranges = append(out.Ranges, wire.MemoryRange{
			Start:   begin,
			Addr:    start,
			DataHex: m.Results().Str("contents"),
		})
	}
	if len(out.Ranges) == 0 {
		out.Unreadable = true
	}
	return out, nil
}

// resolveAddress turns an expression into an address.
//
// A plain number is parsed here rather than asked about, which saves a
// round-trip for the common case of the viewer paging through a region it is
// already looking at.
func (s *Session) resolveAddress(r *request, expr string) (uint64, *wire.Error) {
	expr = strings.TrimSpace(expr)
	if n, err := parseAddress(expr); err == nil {
		return n, nil
	}
	rec, werr := s.send(r.ctx, "-data-evaluate-expression "+quote(expr))
	if werr != nil {
		// A symbol from the ELF table alone has an address but no type, and
		// gdb refuses to read its *value*: "'LogType' has unknown type; cast
		// it to its declared type". Its address is still perfectly well known,
		// and the address is all a caller of this function wanted, so ask for
		// that instead. This is the normal case for a binary built without -g,
		// not an edge case.
		if addr, ok := s.addressOf(r, expr); ok {
			return addr, nil
		}
		return 0, wire.NewError(wire.CodeGDBError, werr.Message)
	}
	value := rec.Results.Str("value")
	if n, ok := addressFromValue(value); ok {
		return n, nil
	}
	// The expression evaluated but not to something address-shaped — an int
	// with debug info, say. Its address is still a sensible destination.
	if addr, ok := s.addressOf(r, expr); ok {
		return addr, nil
	}
	return 0, wire.NewError(wire.CodeBadRequest,
		fmt.Sprintf("%q evaluates to %q, which is not an address", expr, value))
}

// addressOf asks gdb where an expression lives rather than what it holds.
//
// Wrapped in parentheses because the caller's expression may be anything —
// `buf+16` taken address-of is `&(buf+16)`, and `&buf+16` is a different
// place entirely.
func (s *Session) addressOf(r *request, expr string) (uint64, bool) {
	rec, werr := s.send(r.ctx, "-data-evaluate-expression "+quote("&("+expr+")"))
	if werr != nil {
		return 0, false
	}
	return addressFromValue(rec.Results.Str("value"))
}

// addressFromValue digs an address out of whatever gdb printed.
//
// gdb decorates pointer values: `0x55555555601a "demo"` for a char*,
// `0x555555555167 <main>` for a function. The address is the first token, and
// taking it means the viewer accepts "cfg.label" as readily as a hex literal.
func addressFromValue(value string) (uint64, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, false
	}
	if i := strings.IndexAny(value, " \t"); i > 0 {
		value = value[:i]
	}
	// A signed decimal is a legitimate answer too, from something like
	// `(int)&x` or a plain integer variable being used as an address.
	n, err := parseAddress(value)
	if err != nil {
		return 0, false
	}
	return n, true
}

func (s *Session) evalExpr(r *request) (any, *wire.Error) {
	req, werr := decode[wire.EvalExprRequest](r.req.Payload)
	if werr != nil {
		return nil, werr
	}
	if werr := s.checkStopSeq(req.StopSeq); werr != nil {
		return nil, werr
	}
	if strings.TrimSpace(req.Expr) == "" {
		return nil, wire.NewError(wire.CodeBadRequest, "expr is required")
	}
	thread := s.thread(req.Thread)
	frame := req.Frame
	if frame == 0 {
		frame = s.st.selFrame
	}

	cmd := fmt.Sprintf("-data-evaluate-expression --thread %d --frame %d %s",
		thread, frame, quote(req.Expr))
	rec, werr := s.send(r.ctx, cmd)
	if werr != nil {
		return nil, werr
	}
	out := wire.EvalExpr{Expr: req.Expr, Value: rec.Results.Str("value")}
	if addr, ok := addressFromValue(out.Value); ok {
		out.Addr = addr
	}
	return out, nil
}

// maxSymbolAddresses bounds one mem.symbols request. A screenful of hex is
// forty-odd rows; anything much larger is a client asking for a whole chunk it
// will not show.
const maxSymbolAddresses = 128

// memSymbols names the symbol each address falls in.
//
// gdb annotates a pointer with its symbol when it prints one — evaluating
// `(void*)0x5555555551f9` yields `0x5555555551f9 <inspect+16>` — so this needs
// no console command and no symbol table of our own. It is also the only
// answer that stays right across relocation and across shared libraries, each
// of which has its own load bias: gdb knows all of them and a table built from
// `-symbol-info-*` link-time addresses would not.
func (s *Session) memSymbols(r *request) (any, *wire.Error) {
	req, werr := decode[wire.MemSymbolsRequest](r.req.Payload)
	if werr != nil {
		return nil, werr
	}
	if werr := s.checkStopSeq(req.StopSeq); werr != nil {
		return nil, werr
	}

	out := wire.MemSymbols{}
	for i, a := range req.Addresses {
		if i >= maxSymbolAddresses {
			break
		}
		n, err := parseAddress(a)
		if err != nil {
			continue
		}
		rec, werr := s.send(r.ctx, fmt.Sprintf(
			"-data-evaluate-expression %s", quote(fmt.Sprintf("(void*)0x%x", n))))
		if werr != nil {
			// One unreadable address must not lose the rest of the screenful.
			continue
		}
		if name := symbolFromValue(rec.Results.Str("value")); name != "" {
			out.Symbols = append(out.Symbols, wire.MemSymbol{Addr: a, Name: name})
		}
	}
	return out, nil
}

// symbolFromValue pulls the name out of gdb's decoration.
//
// `0x5555555551f9 <inspect+16>` yields "inspect+16"; a bare `0x7fffffffd658`
// yields "", which is the truthful answer for a stack address.
func symbolFromValue(value string) string {
	open := strings.IndexByte(value, '<')
	if open < 0 {
		return ""
	}
	close := strings.LastIndexByte(value, '>')
	if close <= open {
		return ""
	}
	return strings.TrimSpace(value[open+1 : close])
}
