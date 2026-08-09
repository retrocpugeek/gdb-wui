//go:build integration

package debugger_test

import (
	"strconv"
	"strings"
	"testing"

	"github.com/retrocpugeek/gdb-wui/internal/wire"
)

// Writing back: variables, registers and memory, against a real gdb.
//
// The recurring question in this file is whether a write reached the *inferior*
// or only the debugger's idea of it. A varobj reporting a new value proves
// nothing on its own — it is gdb's cache — so the assertions here read the
// bytes back through a different command wherever they can.

// readInt reads a four-byte little-endian int through mem.read, which is a
// different path from the one every write below takes.
func readInt(t *testing.T, h *harness, expr string) int64 {
	t.Helper()
	out := h.mustDo(wire.TypeMemRead, wire.MemReadRequest{Address: expr, Count: 4}).(wire.Memory)
	if out.Unreadable || len(out.Ranges) == 0 {
		t.Fatalf("%s is not readable", expr)
	}
	hex := out.Ranges[0].DataHex
	if len(hex) != 8 {
		t.Fatalf("%s: got %d hex digits, want 8", expr, len(hex))
	}
	var n int64
	for i := 3; i >= 0; i-- {
		b, err := strconv.ParseUint(hex[i*2:i*2+2], 16, 8)
		if err != nil {
			t.Fatalf("%s: %q is not hex: %v", expr, hex, err)
		}
		n = n<<8 | int64(b)
	}
	return n
}

func stopSeqOf(t *testing.T, h *harness) uint64 {
	t.Helper()
	return h.sess.Snapshot().StopSeq
}

// TestAssignALocalReachesTheInferior is the load-bearing one.
//
// cfg.count is read back through mem.read rather than from the reply, because
// -var-assign answers from gdb's varobj and would report the new value even if
// nothing had been written to the process.
func TestAssignALocalReachesTheInferior(t *testing.T) {
	h := stopInStructs(t)

	locals := h.mustDo(wire.TypeVarsLocals, wire.VarsLocalsRequest{}).(wire.VarsLocals)
	cfg, ok := nodeByName(locals.Variables, "cfg")
	if !ok {
		t.Fatal("no cfg among the locals")
	}
	children := h.mustDo(wire.TypeVarsExpand, wire.VarsExpandRequest{
		Path: cfg.Path, Expr: cfg.Expr,
	}).(wire.VarsExpand)
	count, ok := nodeByName(children.Children, "count")
	if !ok {
		t.Fatal("no cfg.count")
	}
	if !count.Editable {
		t.Fatal("cfg.count is an int and must be editable")
	}
	if before := readInt(t, h, "&cfg.count"); before != 3 {
		t.Fatalf("cfg.count starts at %d, want 3 — the fixture changed", before)
	}

	out := h.mustDo(wire.TypeVarsAssign, wire.VarsAssignRequest{
		Path: count.Path, ID: count.ID, Expr: count.Expr, Value: "11",
	}).(wire.VarsAssign)
	if out.Value != "11" {
		t.Errorf("assign reported %q, want 11", out.Value)
	}
	if got := readInt(t, h, "&cfg.count"); got != 11 {
		t.Errorf("cfg.count reads back as %d; the write did not reach the process", got)
	}

	// An edit is a change, and the panel marks changes. Without this the number
	// moves with nothing on it, which reads as a repaint rather than as the
	// edit that was just made.
	again := h.mustDo(wire.TypeVarsExpand, wire.VarsExpandRequest{
		Path: cfg.Path, Expr: cfg.Expr,
	}).(wire.VarsExpand)
	after, _ := nodeByName(again.Children, "count")
	if !after.Changed {
		t.Error("cfg.count is not marked changed after being assigned to")
	}
}

// TestWatchedAggregatesAreNotEditable covers the varobj roots.
//
// A watch is the one place a struct reaches the tree as a *root* rather than
// as a flat local or a listed child, so it is the only thing that exercises
// the -var-show-attributes answer.
func TestWatchedAggregatesAreNotEditable(t *testing.T) {
	h := stopInStructs(t)

	watches := h.mustDo(wire.TypeWatchAdd, wire.WatchAddRequest{Expr: "cfg"}).(wire.WatchList)
	got, ok := nodeByName(watches.Watches, "cfg")
	if !ok {
		t.Fatal("the watch did not come back")
	}
	if got.Editable {
		t.Error("a watch on a struct is marked editable")
	}

	watches = h.mustDo(wire.TypeWatchAdd, wire.WatchAddRequest{Expr: "cfg.count"}).(wire.WatchList)
	scalar, ok := nodeByName(watches.Watches, "cfg.count")
	if !ok {
		t.Fatal("the second watch did not come back")
	}
	if !scalar.Editable {
		t.Error("a watch on an int is not marked editable")
	}
}

// TestAssignALocalWithNoVarobj: editing a flat local is often the *first* thing
// done to it, so the path where no varobj exists yet has to work.
func TestAssignALocalWithNoVarobj(t *testing.T) {
	h := stopInStructs(t)

	// i is a plain int local, never expanded, so nothing exists for it.
	out := h.mustDo(wire.TypeVarsAssign, wire.VarsAssignRequest{
		Path: "local:i", Expr: "i", Value: "42",
	}).(wire.VarsAssign)
	if out.Value != "42" {
		t.Errorf("value = %q, want 42", out.Value)
	}
	if out.ID == "" {
		t.Error("no varobj id came back; the client cannot address the row it just edited")
	}
	if got := readInt(t, h, "&i"); got != 42 {
		t.Errorf("i reads back as %d", got)
	}
}

// TestAssignReportsWhatLanded. gdb truncates to the target's width, so the
// reply has to be the read-back value and not the value that was sent.
func TestAssignReportsWhatLanded(t *testing.T) {
	h := stopInStructs(t)

	// cfg.items[0].name[0] is a char. 321 does not fit in one.
	out := h.mustDo(wire.TypeVarsAssign, wire.VarsAssignRequest{
		Path: "local:cfg.items[0].name[0]", Expr: "cfg.items[0].name[0]", Value: "321",
	}).(wire.VarsAssign)
	if strings.Contains(out.Value, "321") {
		t.Errorf("value = %q, want the truncated char gdb actually stored", out.Value)
	}
	// 321 & 0xff is 65, which gdb prints as `65 'A'`.
	if !strings.Contains(out.Value, "65") {
		t.Errorf("value = %q, want 65 — 321 truncated to a char", out.Value)
	}
}

// TestAssignAcceptsAnExpression: a value cell takes what a user would type, not
// only a literal. Anything less means switching to the console to say `x + 1`.
func TestAssignAcceptsAnExpression(t *testing.T) {
	h := stopInStructs(t)

	out := h.mustDo(wire.TypeVarsAssign, wire.VarsAssignRequest{
		Path: "local:i", Expr: "i", Value: "cfg.count * 10 + 4",
	}).(wire.VarsAssign)
	if out.Value != "34" {
		t.Errorf("value = %q, want 34 — cfg.count is 3", out.Value)
	}
}

// TestAggregatesAreNotEditable pins both halves: the flag the UI reads, and
// gdb's refusal behind it. Without the flag every struct and array row in the
// tree would offer an edit that cannot work.
func TestAggregatesAreNotEditable(t *testing.T) {
	h := stopInStructs(t)

	locals := h.mustDo(wire.TypeVarsLocals, wire.VarsLocalsRequest{}).(wire.VarsLocals)
	cfg, ok := nodeByName(locals.Variables, "cfg")
	if !ok {
		t.Fatal("no cfg")
	}
	if cfg.Editable {
		t.Error("cfg is a struct and must not be marked editable")
	}
	i, ok := nodeByName(locals.Variables, "i")
	if !ok {
		t.Fatal("no i")
	}
	if !i.Editable {
		t.Error("i is an int and must be marked editable")
	}

	children := h.mustDo(wire.TypeVarsExpand, wire.VarsExpandRequest{
		Path: cfg.Path, Expr: cfg.Expr,
	}).(wire.VarsExpand)
	matrix, ok := nodeByName(children.Children, "matrix")
	if !ok {
		t.Fatal("no cfg.matrix")
	}
	if matrix.Editable {
		t.Error("cfg.matrix is an array and must not be marked editable")
	}
	label, ok := nodeByName(children.Children, "label")
	if !ok {
		t.Fatal("no cfg.label")
	}
	if !label.Editable {
		t.Errorf("cfg.label (%s) is a pointer and must be editable — a pointer is "+
			"the most useful thing in this tree to redirect", label.Value)
	}

	if _, werr := h.do(wire.TypeVarsAssign, wire.VarsAssignRequest{
		Path: cfg.Path, Expr: cfg.Expr, Value: "0",
	}); werr == nil {
		t.Error("assigning to a struct was accepted")
	}
}

// TestAssignEmptyValueIsRefused: committing an empty cell must not be read as
// assigning zero.
func TestAssignEmptyValueIsRefused(t *testing.T) {
	h := stopInStructs(t)
	if _, werr := h.do(wire.TypeVarsAssign, wire.VarsAssignRequest{
		Path: "local:i", Expr: "i", Value: "   ",
	}); werr == nil {
		t.Fatal("an empty value was accepted")
	} else if werr.Code != wire.CodeBadRequest {
		t.Errorf("code = %q, want badRequest", werr.Code)
	}
}

// TestAssignAgainstAStaleStopIsRefused. The frame a value was read in is the
// frame it must be written in; a superseded stopSeq means the user is looking
// at numbers from somewhere else.
func TestAssignAgainstAStaleStopIsRefused(t *testing.T) {
	h := stopInStructs(t)
	stale := stopSeqOf(t, h)

	h.mustDo(wire.TypeExecNext, wire.ExecRequest{StopSeq: stale})
	h.rec.wait(t, wire.EventStopped)

	if _, werr := h.do(wire.TypeVarsAssign, wire.VarsAssignRequest{
		Path: "local:i", Expr: "i", Value: "1", StopSeq: stale,
	}); werr == nil {
		t.Fatal("a write naming a superseded stop was accepted")
	} else if werr.Code != wire.CodeBusy {
		t.Errorf("code = %q, want busy", werr.Code)
	}
}

// --- registers -------------------------------------------------------------

// writableRegister finds a general-purpose register on whatever this is.
//
// By name rather than by number: number 0 is a scratch register on x86 and
// AArch64 but hardwired zero on RISC-V, so a test written against the number
// would pass everywhere it was run and be wrong in principle.
func writableRegister(t *testing.T, h *harness) wire.Register {
	t.Helper()
	values := h.mustDo(wire.TypeRegsValues, wire.RegsValuesRequest{}).(wire.RegsValues)
	want := map[string]bool{
		"rax": true, "eax": true, // x86-64, x86
		"x0": true, "r0": true, // AArch64, ARM
		"a0": true, "v0": true, // RISC-V, MIPS
	}
	for _, reg := range values.Registers {
		if want[reg.Name] {
			return reg
		}
	}
	t.Skipf("no general-purpose register recognised among %d; this test is host-specific",
		len(values.Registers))
	return wire.Register{}
}

func TestWriteARegister(t *testing.T) {
	h := stopInStructs(t)
	reg := writableRegister(t, h)

	out := h.mustDo(wire.TypeRegsWrite, wire.RegsWriteRequest{
		Number: reg.Number, Value: "0x2a",
	}).(wire.RegsWrite)

	if out.Register.Number != reg.Number {
		t.Errorf("wrote %d, got %d back", reg.Number, out.Register.Number)
	}
	if out.Register.Value != "0x2a" {
		t.Errorf("$%s = %q after writing 0x2a", reg.Name, out.Register.Value)
	}
	if !out.Register.Changed {
		t.Error("the written register is not marked changed, so the edit lands with no mark on it")
	}

	// Read back through the whole-file command, which is the one the panel uses
	// on every stop: if the write only updated the reply we built, this fails.
	values := h.mustDo(wire.TypeRegsValues, wire.RegsValuesRequest{}).(wire.RegsValues)
	for _, r := range values.Registers {
		if r.Number == reg.Number && r.Value != "0x2a" {
			t.Errorf("$%s reads back as %q from the full register list", reg.Name, r.Value)
		}
	}
}

// TestWriteARegisterInTheAskedForFormat. The panel renders one format at a
// time; a value read back in another would appear to jump on commit.
//
// The value is written in decimal and read back in hex on purpose: with both
// in the same base an implementation that echoed the input rather than reading
// the register would pass.
func TestWriteARegisterInTheAskedForFormat(t *testing.T) {
	h := stopInStructs(t)
	reg := writableRegister(t, h)

	out := h.mustDo(wire.TypeRegsWrite, wire.RegsWriteRequest{
		Number: reg.Number, Value: "42", Format: "x",
	}).(wire.RegsWrite)
	if out.Register.Value != "0x2a" {
		t.Errorf("value = %q, want 0x2a — 42 rendered in the panel's format", out.Register.Value)
	}
	if out.Format != "x" {
		t.Errorf("format = %q, want x", out.Format)
	}

	dec := h.mustDo(wire.TypeRegsWrite, wire.RegsWriteRequest{
		Number: reg.Number, Value: "0x2a", Format: "d",
	}).(wire.RegsWrite)
	if dec.Register.Value != "42" {
		t.Errorf("value = %q, want the decimal 42", dec.Register.Value)
	}
}

func TestWriteAnUnknownRegisterIsRefused(t *testing.T) {
	h := stopInStructs(t)
	if _, werr := h.do(wire.TypeRegsWrite, wire.RegsWriteRequest{
		Number: 99999, Value: "1",
	}); werr == nil {
		t.Fatal("a register that does not exist was written")
	}
}

// --- memory ----------------------------------------------------------------

func TestWriteMemory(t *testing.T) {
	h := stopInStructs(t)

	if before := readInt(t, h, "&cfg.count"); before != 3 {
		t.Fatalf("cfg.count starts at %d, want 3", before)
	}
	out := h.mustDo(wire.TypeMemWrite, wire.MemWriteRequest{
		Address: "&cfg.count", DataHex: "07000000",
	}).(wire.MemWrite)
	if out.Count != 4 {
		t.Errorf("count = %d, want 4", out.Count)
	}
	if out.Addr == 0 {
		t.Error("no address reported")
	}
	if got := readInt(t, h, "&cfg.count"); got != 7 {
		t.Errorf("cfg.count = %d after writing 07000000", got)
	}
}

// TestWriteMemoryUpdatesTheTree.
//
// gdb answers -var-list-children from its own cached values, so an expanded
// struct goes on reporting the old number after the bytes underneath it change
// — until something asks for a -var-update. Nothing else does that between
// stops, and a write does not advance the stop, so the write has to.
func TestWriteMemoryUpdatesTheTree(t *testing.T) {
	h := stopInStructs(t)

	locals := h.mustDo(wire.TypeVarsLocals, wire.VarsLocalsRequest{}).(wire.VarsLocals)
	cfg, _ := nodeByName(locals.Variables, "cfg")
	before := h.mustDo(wire.TypeVarsExpand, wire.VarsExpandRequest{
		Path: cfg.Path, Expr: cfg.Expr,
	}).(wire.VarsExpand)
	if count, _ := nodeByName(before.Children, "count"); count.Value != "3" {
		t.Fatalf("cfg.count starts at %q, want 3", count.Value)
	}

	h.mustDo(wire.TypeMemWrite, wire.MemWriteRequest{
		Address: "&cfg.count", DataHex: "0c000000",
	})

	after := h.mustDo(wire.TypeVarsExpand, wire.VarsExpandRequest{
		Path: cfg.Path, Expr: cfg.Expr,
	}).(wire.VarsExpand)
	count, ok := nodeByName(after.Children, "count")
	if !ok {
		t.Fatal("cfg.count vanished")
	}
	if count.Value != "12" {
		t.Errorf("cfg.count = %q after writing 0c000000 to its bytes, want 12", count.Value)
	}
}

// TestWriteMemoryAtAnOffset is the shape the hex view uses: one row address
// plus the index of the byte double-clicked.
func TestWriteMemoryAtAnOffset(t *testing.T) {
	h := stopInStructs(t)

	h.mustDo(wire.TypeMemWrite, wire.MemWriteRequest{
		Address: "&cfg.count", Offset: 1, DataHex: "ff",
	})
	if got := readInt(t, h, "&cfg.count"); got != 3|0xff00 {
		t.Errorf("cfg.count = %#x, want the second byte set and the rest untouched", got)
	}
}

// TestWriteNothingIsRefused is the case gdb does not catch: it answers ^done to
// an empty hex string and writes nothing, so committing an empty cell would
// report success and change nothing.
func TestWriteNothingIsRefused(t *testing.T) {
	h := stopInStructs(t)
	for _, bad := range []string{"", "  ", "0x"} {
		if _, werr := h.do(wire.TypeMemWrite, wire.MemWriteRequest{
			Address: "&cfg.count", DataHex: bad,
		}); werr == nil {
			t.Errorf("%q was accepted as bytes and silently wrote nothing", bad)
		}
	}
}

// TestWriteMemoryTakesAPrefixedByte. gdb refuses "0xff" outright, and the hex
// view shows addresses with the prefix, so a user typing one into a byte cell
// is following the screen rather than making a mistake.
func TestWriteMemoryTakesAPrefixedByte(t *testing.T) {
	h := stopInStructs(t)
	h.mustDo(wire.TypeMemWrite, wire.MemWriteRequest{
		Address: "&cfg.count", DataHex: "0x07000000",
	})
	if got := readInt(t, h, "&cfg.count"); got != 7 {
		t.Errorf("cfg.count = %d after writing 0x07000000", got)
	}
}

// TestWriteMemoryRejectsBadHex. Both of these gdb refuses too, with "Invalid
// argument", which says nothing about which of the two things is wrong.
func TestWriteMemoryRejectsBadHex(t *testing.T) {
	h := stopInStructs(t)
	for _, bad := range []string{"f", "abc", "zz", "gg"} {
		werr := func() *wire.Error {
			_, werr := h.do(wire.TypeMemWrite, wire.MemWriteRequest{
				Address: "&cfg.count", DataHex: bad,
			})
			return werr
		}()
		if werr == nil {
			t.Errorf("%q was accepted as bytes", bad)
			continue
		}
		if !strings.Contains(werr.Message, bad) {
			t.Errorf("%q: message %q does not name what was rejected", bad, werr.Message)
		}
	}
}

func TestWriteUnmappedMemoryIsAnError(t *testing.T) {
	h := stopInStructs(t)
	// Unlike a read, which reports unreadable and lets the viewer show "??",
	// a write that did not happen has to say so: there is nothing on screen to
	// carry the bad news otherwise.
	if _, werr := h.do(wire.TypeMemWrite, wire.MemWriteRequest{
		Address: "0x10", DataHex: "ff",
	}); werr == nil {
		t.Fatal("writing to the first page was reported as success")
	}
}

// --- the broadcast ---------------------------------------------------------

// TestWritesAreBroadcast. A write invalidates what every connected browser is
// showing, not only the one that made it.
func TestWritesAreBroadcast(t *testing.T) {
	h := stopInStructs(t)

	for _, tc := range []struct {
		name string
		typ  string
		req  any
		what string
	}{
		{"variable", wire.TypeVarsAssign,
			wire.VarsAssignRequest{Path: "local:i", Expr: "i", Value: "5"}, "variable"},
		{"memory", wire.TypeMemWrite,
			wire.MemWriteRequest{Address: "&cfg.count", DataHex: "09000000"}, "memory"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h.rec.reset()
			h.mustDo(tc.typ, tc.req)
			ev := h.rec.wait(t, wire.EventValueWritten).(wire.ValueWritten)
			if ev.What != tc.what {
				t.Errorf("what = %q, want %q", ev.What, tc.what)
			}
			if ev.Detail == "" {
				t.Error("the event names no target, so a second browser cannot say what moved")
			}
		})
	}
}
