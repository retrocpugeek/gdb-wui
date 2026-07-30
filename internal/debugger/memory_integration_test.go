//go:build integration

package debugger_test

import (
	"strconv"
	"strings"
	"testing"

	"github.com/retrocpugeek/gdb-wui/internal/wire"
)

// M7: the memory viewer. An expression to a window, read-only, holes visible.

func TestMemReadByExpression(t *testing.T) {
	h := stopInStructs(t)

	// The point of resolving expressions server-side: a user types what they
	// have in their head, not a hex number they had to look up first.
	out := h.mustDo(wire.TypeMemRead, wire.MemReadRequest{
		Address: "&cfg", Count: 32,
	}).(wire.Memory)

	if out.Unreadable {
		t.Fatal("&cfg is on the stack and must be readable")
	}
	if out.Addr == 0 {
		t.Error("no address resolved")
	}
	if len(out.Ranges) == 0 {
		t.Fatal("no ranges returned")
	}
	total := 0
	for _, r := range out.Ranges {
		if r.DataHex == "" {
			t.Error("a range with no bytes")
		}
		if len(r.DataHex)%2 != 0 {
			t.Errorf("hex of odd length: %q", r.DataHex)
		}
		total += len(r.DataHex) / 2
	}
	if total != 32 {
		t.Errorf("got %d bytes, asked for 32", total)
	}
	if out.Requested != "&cfg" {
		t.Errorf("requested = %q, want the expression echoed back", out.Requested)
	}
}

func TestMemReadByAddress(t *testing.T) {
	h := stopInStructs(t)

	// Resolve once, then read at the address — the path the viewer takes when
	// paging through a region it is already looking at.
	first := h.mustDo(wire.TypeMemRead, wire.MemReadRequest{
		Address: "&cfg", Count: 16,
	}).(wire.Memory)

	byAddr := h.mustDo(wire.TypeMemRead, wire.MemReadRequest{
		Address: "0x" + strconv.FormatUint(first.Addr, 16), Count: 16,
	}).(wire.Memory)

	if byAddr.Addr != first.Addr {
		t.Errorf("addr = %#x, want %#x", byAddr.Addr, first.Addr)
	}
	if len(byAddr.Ranges) == 0 || byAddr.Ranges[0].DataHex != first.Ranges[0].DataHex {
		t.Error("reading the same address twice gave different bytes")
	}
}

func TestMemReadOffset(t *testing.T) {
	h := stopInStructs(t)

	whole := h.mustDo(wire.TypeMemRead, wire.MemReadRequest{
		Address: "&cfg", Count: 32,
	}).(wire.Memory)
	shifted := h.mustDo(wire.TypeMemRead, wire.MemReadRequest{
		Address: "&cfg", Offset: 16, Count: 16,
	}).(wire.Memory)

	if shifted.Addr != whole.Addr+16 {
		t.Errorf("offset read starts at %#x, want %#x", shifted.Addr, whole.Addr+16)
	}
	// The second half of the whole read must equal the offset read.
	if len(whole.Ranges) > 0 && len(shifted.Ranges) > 0 {
		wantTail := whole.Ranges[0].DataHex[32:]
		if shifted.Ranges[0].DataHex != wantTail {
			t.Errorf("offset bytes %q do not match the tail %q",
				shifted.Ranges[0].DataHex, wantTail)
		}
	}
}

// TestMemReadUnreadable is the case the "??" rendering exists for. Pointing a
// hex viewer at an unmapped page is how you discover it is unmapped, so it must
// be an ordinary answer rather than an error.
func TestMemReadUnreadable(t *testing.T) {
	h := stopInStructs(t)

	out, werr := h.do(wire.TypeMemRead, wire.MemReadRequest{Address: "0x0", Count: 16})
	if werr != nil {
		t.Fatalf("reading address zero failed the request: %s: %s", werr.Code, werr.Message)
	}
	mem := out.(wire.Memory)
	if !mem.Unreadable {
		t.Error("address zero was reported as readable")
	}
	if len(mem.Ranges) != 0 {
		t.Errorf("unreadable memory returned %d ranges", len(mem.Ranges))
	}
}

func TestMemReadValidation(t *testing.T) {
	h := stopInStructs(t)
	for _, tc := range []struct {
		name string
		req  wire.MemReadRequest
		code string
	}{
		{"no address", wire.MemReadRequest{Count: 16}, wire.CodeBadRequest},
		{"zero count", wire.MemReadRequest{Address: "&cfg"}, wire.CodeBadRequest},
		{"absurd count", wire.MemReadRequest{Address: "&cfg", Count: 1 << 24}, wire.CodeTooLarge},
		{"nonsense expression", wire.MemReadRequest{Address: "no_such_thing_xyz", Count: 16},
			wire.CodeGDBError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, werr := h.do(wire.TypeMemRead, tc.req)
			if werr == nil {
				t.Fatal("accepted an invalid request")
			}
			if werr.Code != tc.code {
				t.Errorf("code = %q, want %q", werr.Code, tc.code)
			}
		})
	}
}

// TestMemReadReflectsMemory: the viewer must show what is actually there, so a
// known value has to appear in the bytes.
func TestMemReadReflectsMemory(t *testing.T) {
	h := stopInStructs(t)

	// cfg.count is 3 at this point, and cfg.count is the struct's first member.
	out := h.mustDo(wire.TypeMemRead, wire.MemReadRequest{
		Address: "&cfg", Count: 8,
	}).(wire.Memory)
	if len(out.Ranges) == 0 {
		t.Fatal("no bytes")
	}
	// Little-endian: the int 3 is "03000000".
	if !strings.HasPrefix(out.Ranges[0].DataHex, "03000000") {
		t.Errorf("bytes at &cfg are %q, want them to start with the int 3",
			out.Ranges[0].DataHex)
	}
}

func TestEvalExpr(t *testing.T) {
	h := stopInStructs(t)

	out := h.mustDo(wire.TypeEvalExpr, wire.EvalExprRequest{Expr: "cfg.count"}).(wire.EvalExpr)
	if out.Value != "3" {
		t.Errorf("cfg.count = %q, want 3", out.Value)
	}

	// A pointer's value carries a decoration gdb adds; the address must still
	// be extracted, because that is what the memory viewer jumps to.
	ptr := h.mustDo(wire.TypeEvalExpr, wire.EvalExprRequest{Expr: "cfg.label"}).(wire.EvalExpr)
	if ptr.Addr == 0 {
		t.Errorf("cfg.label = %q but no address was extracted", ptr.Value)
	}
	if !strings.Contains(ptr.Value, "demo") {
		t.Errorf("cfg.label = %q, want it to show the string", ptr.Value)
	}
}

func TestEvalExprRejectsNonsense(t *testing.T) {
	h := stopInStructs(t)
	if _, werr := h.do(wire.TypeEvalExpr, wire.EvalExprRequest{Expr: "no_such_symbol_xyz"}); werr == nil {
		t.Error("a nonsense expression was accepted")
	}
}

func TestMemReadGatedWhileRunning(t *testing.T) {
	h := startReal(t, "threads")
	h.mustDo(wire.TypeExeLoad, wire.ExeLoadRequest{Path: "threads"})
	h.mustDo(wire.TypeExecRun, wire.ExecRequest{})
	h.rec.wait(t, wire.EventRunning)

	if _, werr := h.do(wire.TypeMemRead, wire.MemReadRequest{Address: "0x1000", Count: 16}); werr == nil {
		t.Error("mem.read was accepted while running")
	} else if werr.Code != wire.CodeBusy {
		t.Errorf("code = %q, want busy", werr.Code)
	}
}
