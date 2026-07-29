//go:build integration

package debugger_test

import (
	"strings"
	"testing"

	"github.com/retrocpugeek/gdb-wui/internal/wire"
)

// The M4 done-criteria, as tests: expand a nested struct, watch an expression,
// see <optimized out> rendered honestly, and confirm the varobj registry is
// empty after a re-run.

// structsBreakLine is the line in structs.c after cfg is fully populated.
const structsBreakLine = 42 // inspect(&cfg);

// stopInStructs gets a session stopped with a populated struct in scope.
func stopInStructs(t *testing.T) *harness {
	t.Helper()
	h := startReal(t, "structs")
	h.mustDo(wire.TypeExeLoad, wire.ExeLoadRequest{Path: "structs"})
	h.mustDo(wire.TypeBpSetSource, wire.BreakpointRequest{
		Path: "structs.c", Line: structsBreakLine,
	})
	h.mustDo(wire.TypeExecRun, wire.ExecRequest{})
	h.rec.wait(t, wire.EventStopped)
	return h
}

func nodeByName(nodes []wire.VarNode, name string) (wire.VarNode, bool) {
	for _, n := range nodes {
		if n.Name == name {
			return n, true
		}
	}
	return wire.VarNode{}, false
}

// TestLocalsCreateNoVarobjs is the design's central claim: listing locals is
// free, however large they are.
func TestLocalsCreateNoVarobjs(t *testing.T) {
	h := stopInStructs(t)

	out := h.mustDo(wire.TypeVarsLocals, wire.VarsLocalsRequest{}).(wire.VarsLocals)
	if len(out.Variables) == 0 {
		t.Fatal("no locals")
	}
	cfg, ok := nodeByName(out.Variables, "cfg")
	if !ok {
		t.Fatalf("no cfg in %+v", out.Variables)
	}
	if !cfg.Expandable {
		t.Error("cfg is a struct; --simple-values omits its value, so it must be expandable")
	}
	if cfg.Value != "" {
		t.Errorf("cfg has value %q; an aggregate should have none under --simple-values", cfg.Value)
	}
	if cfg.ID != "" {
		t.Errorf("cfg already has varobj %q; listing locals must create none", cfg.ID)
	}
	if n := h.sess.VarobjCount(); n != 0 {
		t.Errorf("%d varobjs exist after merely listing locals, want 0", n)
	}
}

// TestExpandNestedStruct is the headline M4 criterion.
//
// It also pins a gdb behaviour worth knowing: varobj children of a *pointer*
// are the pointee's fields, not an index. `struct item *items` expands straight
// to id/name/weight — gdb dereferences for you. Only a genuine array yields
// numeric children, which is what the [n] path form is for.
func TestExpandNestedStruct(t *testing.T) {
	h := stopInStructs(t)

	first := h.mustDo(wire.TypeVarsExpand, wire.VarsExpandRequest{
		Path: "local:cfg", Expr: "cfg",
	}).(wire.VarsExpand)
	if len(first.Children) == 0 {
		t.Fatal("cfg expanded to nothing")
	}
	items, ok := nodeByName(first.Children, "items")
	if !ok {
		t.Fatalf("no items child in %+v", first.Children)
	}
	if items.ID == "" {
		t.Error("an expanded child has no varobj id")
	}
	if items.Path != "local:cfg.items" {
		t.Errorf("path = %q, want local:cfg.items", items.Path)
	}

	// Through the pointer: gdb hands back the struct's fields directly.
	second := h.mustDo(wire.TypeVarsExpand, wire.VarsExpandRequest{
		Path: items.Path, ID: items.ID,
	}).(wire.VarsExpand)
	id, ok := nodeByName(second.Children, "id")
	if !ok {
		t.Fatalf("no id field under the pointer in %+v", second.Children)
	}
	if id.Value != "0" {
		t.Errorf("cfg.items->id = %q, want 0", id.Value)
	}
	if id.Path != "local:cfg.items.id" {
		t.Errorf("path = %q, want local:cfg.items.id", id.Path)
	}

	// A char array is an aggregate, so --simple-values gives it no value and it
	// arrives expandable. That is a real limitation of the trade: a string is
	// shown as an openable array of chars rather than as "item-0". The
	// alternative, --all-values, would eagerly stringify every array in scope,
	// which is exactly the 100k-element problem the design set out to avoid.
	name, ok := nodeByName(second.Children, "name")
	if !ok {
		t.Fatalf("no name field under the pointer in %+v", second.Children)
	}
	if !name.Expandable {
		t.Error("char[16] is an aggregate; it should be expandable")
	}
	if !strings.Contains(name.Type, "char") {
		t.Errorf("name type = %q, want a char array", name.Type)
	}
	chars := h.mustDo(wire.TypeVarsExpand, wire.VarsExpandRequest{
		Path: name.Path, ID: name.ID,
	}).(wire.VarsExpand)
	if len(chars.Children) == 0 {
		t.Fatal("char array expanded to nothing")
	}
	if got := chars.Children[0]; !strings.Contains(got.Value, "105") &&
		!strings.Contains(got.Value, "i") {
		t.Errorf("cfg.items->name[0] = %q, want the letter i", got.Value)
	}

	// A real array does yield indices, and those must read as [n].
	matrix, ok := nodeByName(first.Children, "matrix")
	if !ok {
		t.Fatalf("no matrix child in %+v", first.Children)
	}
	rows := h.mustDo(wire.TypeVarsExpand, wire.VarsExpandRequest{
		Path: matrix.Path, ID: matrix.ID,
	}).(wire.VarsExpand)
	if len(rows.Children) != 2 {
		t.Fatalf("matrix has %d rows, want 2", len(rows.Children))
	}
	if rows.Children[0].Path != "local:cfg.matrix[0]" {
		t.Errorf("array child path = %q, want local:cfg.matrix[0] — an index is not a field",
			rows.Children[0].Path)
	}
	cols := h.mustDo(wire.TypeVarsExpand, wire.VarsExpandRequest{
		Path: rows.Children[1].Path, ID: rows.Children[1].ID,
	}).(wire.VarsExpand)
	if len(cols.Children) != 3 {
		t.Fatalf("matrix[1] has %d columns, want 3", len(cols.Children))
	}
	if got := cols.Children[2]; got.Value != "7" {
		t.Errorf("matrix[1][2] = %q, want 7", got.Value)
	} else if got.Path != "local:cfg.matrix[1][2]" {
		t.Errorf("path = %q, want local:cfg.matrix[1][2]", got.Path)
	}

	// The same subtree re-expanded must reuse the varobj rather than pile up
	// new ones, or stepping through a loop leaks a tree per stop.
	before := h.sess.VarobjCount()
	again := h.mustDo(wire.TypeVarsExpand, wire.VarsExpandRequest{
		Path: "local:cfg", Expr: "cfg",
	}).(wire.VarsExpand)
	if again.ID != first.ID {
		t.Errorf("re-expanding created a new varobj: %q then %q", first.ID, again.ID)
	}
	if after := h.sess.VarobjCount(); after != before {
		t.Errorf("varobj count went %d -> %d on re-expansion", before, after)
	}
}

// TestScalarChildHasValue: a leaf must carry its value, or the tree shows
// nothing but names.
func TestScalarChildHasValue(t *testing.T) {
	h := stopInStructs(t)
	out := h.mustDo(wire.TypeVarsExpand, wire.VarsExpandRequest{
		Path: "local:cfg", Expr: "cfg",
	}).(wire.VarsExpand)

	count, ok := nodeByName(out.Children, "count")
	if !ok {
		t.Fatalf("no count child in %+v", out.Children)
	}
	if count.Value != "3" {
		t.Errorf("cfg.count = %q, want 3", count.Value)
	}
	if count.Expandable {
		t.Error("an int is not expandable")
	}
}

// TestWatchFollowsFrame: a watch is floating, so it tracks the frame the user
// is looking at rather than the one that happened to be selected when typed.
func TestWatchFollowsFrame(t *testing.T) {
	h := startReal(t, "hello")
	h.mustDo(wire.TypeExeLoad, wire.ExeLoadRequest{Path: "hello"})
	h.mustDo(wire.TypeBpSetSource, wire.BreakpointRequest{Path: "hello.c", Line: lineAddSum})
	h.mustDo(wire.TypeExecRun, wire.ExecRequest{})
	stopped := h.rec.wait(t, wire.EventStopped).(wire.Stopped)

	// In add(), "a" is an argument.
	out := h.mustDo(wire.TypeWatchAdd, wire.WatchAddRequest{Expr: "a"}).(wire.WatchList)
	if len(out.Watches) != 1 {
		t.Fatalf("watches = %+v", out.Watches)
	}
	w := out.Watches[0]
	if w.Name != "a" {
		t.Errorf("watch name = %q, want the expression", w.Name)
	}
	if w.Value == "" {
		t.Errorf("watch has no value: %+v", w)
	}

	// Continue to the next call; the same watch must report the new value.
	h.mustDo(wire.TypeExecContinue, wire.ExecRequest{StopSeq: stopped.StopSeq})
	h.rec.wait(t, wire.EventStopped)

	after := h.mustDo(wire.TypeWatchList, nil).(wire.WatchList)
	if len(after.Watches) != 1 {
		t.Fatalf("watch disappeared across a stop: %+v", after.Watches)
	}
	if after.Watches[0].Value == "" {
		t.Error("watch lost its value after stepping")
	}
	if after.Watches[0].Path != w.Path {
		t.Errorf("watch path changed: %q -> %q", w.Path, after.Watches[0].Path)
	}
}

func TestWatchRejectsBadExpression(t *testing.T) {
	h := stopInStructs(t)
	_, werr := h.do(wire.TypeWatchAdd, wire.WatchAddRequest{Expr: "no_such_symbol_xyz"})
	if werr == nil {
		t.Fatal("a nonsense expression was accepted as a watch")
	}
	if werr.Code != wire.CodeGDBError {
		t.Errorf("code = %q, want gdb_error", werr.Code)
	}
	// And it must not have been recorded.
	out := h.mustDo(wire.TypeWatchList, nil).(wire.WatchList)
	if len(out.Watches) != 0 {
		t.Errorf("a failed watch was still added: %+v", out.Watches)
	}
}

func TestWatchRemove(t *testing.T) {
	h := stopInStructs(t)
	added := h.mustDo(wire.TypeWatchAdd, wire.WatchAddRequest{Expr: "cfg.count"}).(wire.WatchList)
	path := added.Watches[0].Path

	out := h.mustDo(wire.TypeWatchRemove, wire.WatchRemoveRequest{Path: path}).(wire.WatchList)
	if len(out.Watches) != 0 {
		t.Errorf("watches = %+v, want empty", out.Watches)
	}
	if n := h.sess.VarobjCount(); n != 0 {
		t.Errorf("%d varobjs remain after removing the only watch", n)
	}
}

// TestOptimizedOutIsHonest is the -O2 criterion: the value gdb gives is shown,
// not hidden or faked.
func TestOptimizedOutIsHonest(t *testing.T) {
	h := startReal(t, "opt")
	h.mustDo(wire.TypeExeLoad, wire.ExeLoadRequest{Path: "opt"})
	h.mustDo(wire.TypeBpSetSource, wire.BreakpointRequest{Path: "opt.c", Line: 17})
	h.mustDo(wire.TypeExecRun, wire.ExecRequest{})
	h.rec.wait(t, wire.EventStopped)

	out := h.mustDo(wire.TypeVarsLocals, wire.VarsLocalsRequest{}).(wire.VarsLocals)
	var sawOptimizedOut bool
	for _, v := range out.Variables {
		if v.Value == wire.OptimizedOut {
			sawOptimizedOut = true
			if !v.OptimizedOut {
				t.Errorf("%s has the optimized-out value but the flag is unset", v.Name)
			}
			if v.Expandable {
				t.Errorf("%s is optimized out; it must not offer a twisty", v.Name)
			}
		}
	}
	if !sawOptimizedOut {
		t.Skip("this compiler kept every local at -O2; nothing to assert")
	}
}

// TestRegistryEmptyAfterRerun is the leak check the plan asks for by name.
func TestRegistryEmptyAfterRerun(t *testing.T) {
	h := stopInStructs(t)

	h.mustDo(wire.TypeVarsExpand, wire.VarsExpandRequest{Path: "local:cfg", Expr: "cfg"})
	h.mustDo(wire.TypeWatchAdd, wire.WatchAddRequest{Expr: "cfg.count"})
	if n := h.sess.VarobjCount(); n == 0 {
		t.Fatal("expanding and watching created no varobjs; the test proves nothing")
	}

	h.rec.reset()
	h.mustDo(wire.TypeExecRun, wire.ExecRequest{})

	// The invalidation must reach clients, or they keep rendering dead ids.
	h.rec.wait(t, wire.EventVarsInvalidated)
	h.rec.wait(t, wire.EventStopped)

	// After the re-run the registry holds only what the new stop recreated:
	// the watches. No expanded subtree survives, because the frames they were
	// bound to are gone.
	watches := h.mustDo(wire.TypeWatchList, nil).(wire.WatchList)
	if len(watches.Watches) != 1 {
		t.Errorf("watches did not survive the re-run: %+v", watches.Watches)
	}
	if n := h.sess.VarobjCount(); n > 1 {
		t.Errorf("%d varobjs after a re-run, want at most the one recreated watch", n)
	}
}

// TestRegisters covers the numbering rule and change highlighting.
func TestRegisters(t *testing.T) {
	h := stopInStructs(t)

	names := h.mustDo(wire.TypeRegsNames, nil).(wire.RegsNames)
	if len(names.Names) == 0 {
		t.Fatal("no register names")
	}
	if names.Names[0] != "rax" {
		t.Errorf("register 0 = %q, want rax", names.Names[0])
	}
	var empties int
	for _, n := range names.Names {
		if n == "" {
			empties++
		}
	}
	if empties == 0 {
		t.Error("no empty register names survived; every index after a gap would be mislabelled")
	}

	values := h.mustDo(wire.TypeRegsValues, wire.RegsValuesRequest{}).(wire.RegsValues)
	if len(values.Registers) == 0 {
		t.Fatal("no register values")
	}
	if values.Format != "x" {
		t.Errorf("format = %q, want the x default", values.Format)
	}
	byNumber := map[int]wire.Register{}
	for _, r := range values.Registers {
		byNumber[r.Number] = r
	}
	if rax, ok := byNumber[0]; !ok || rax.Name != "rax" || rax.Value == "" {
		t.Errorf("register 0 = %+v", rax)
	}

	// Step, then look again: something must have changed, and gdb is the one
	// that says so.
	stopSeq := h.sess.Snapshot().StopSeq
	h.mustDo(wire.TypeExecNext, wire.ExecRequest{StopSeq: stopSeq})
	h.rec.wait(t, wire.EventStopped)

	after := h.mustDo(wire.TypeRegsValues, wire.RegsValuesRequest{}).(wire.RegsValues)
	var changed int
	for _, r := range after.Registers {
		if r.Changed {
			changed++
		}
	}
	if changed == 0 {
		t.Error("no register changed across a step; at minimum the PC did")
	}
}

func TestRegsValuesRejectsBadFormat(t *testing.T) {
	h := stopInStructs(t)
	if _, werr := h.do(wire.TypeRegsValues, wire.RegsValuesRequest{Format: "qq"}); werr == nil {
		t.Error("an invalid register format was accepted")
	}
}

// TestValueRequestsAreGatedWhileRunning: the value panels must not be able to
// ask gdb questions it will refuse.
func TestValueRequestsAreGatedWhileRunning(t *testing.T) {
	h := startReal(t, "threads")
	h.mustDo(wire.TypeExeLoad, wire.ExeLoadRequest{Path: "threads"})
	h.mustDo(wire.TypeExecRun, wire.ExecRequest{})
	h.rec.wait(t, wire.EventRunning)

	for _, typ := range []string{wire.TypeVarsLocals, wire.TypeVarsExpand, wire.TypeRegsValues} {
		_, werr := h.do(typ, map[string]any{"path": "local:x"})
		if werr == nil {
			t.Errorf("%s was accepted while running", typ)
			continue
		}
		if werr.Code != wire.CodeBusy {
			t.Errorf("%s: code = %q, want busy", typ, werr.Code)
		}
	}
	// Listing watches is bookkeeping and must still work.
	if _, werr := h.do(wire.TypeWatchList, nil); werr != nil {
		t.Errorf("watch.list while running: %s", werr.Message)
	}
}

// TestChangeFlagSurvivesReExpansion is a regression test for a bug that made
// the variables panel useless in the one case it exists for.
//
// -var-update marks changed varobjs at each stop. The client then re-expands
// its open subtrees, which calls -var-list-children. Building fresh objects
// from that reply — the obvious implementation — throws away the marks that
// were just set, so nothing is ever highlighted and a value silently changes
// under the user's eyes.
func TestChangeFlagSurvivesReExpansion(t *testing.T) {
	h := startReal(t, "structs")
	h.mustDo(wire.TypeExeLoad, wire.ExeLoadRequest{Path: "structs"})
	// Line 31 is the first of three consecutive assignments to cfg's own
	// members (count, label, items). Breaking inside the loop below them looks
	// equally good and is not: by then those members are already set, and the
	// loop only writes through the pointer, so none of cfg's *direct* children
	// change and the test would pass or fail for the wrong reason.
	h.mustDo(wire.TypeBpSetSource, wire.BreakpointRequest{Path: "structs.c", Line: 31})
	h.mustDo(wire.TypeExecRun, wire.ExecRequest{})
	h.rec.wait(t, wire.EventStopped)

	h.mustDo(wire.TypeVarsExpand, wire.VarsExpandRequest{Path: "local:cfg", Expr: "cfg"})

	// Step until something under cfg changes. A couple of lines is enough; the
	// loop assigns id, name and weight in turn.
	var sawChange bool
	for range 6 {
		seq := h.sess.Snapshot().StopSeq
		h.mustDo(wire.TypeExecNext, wire.ExecRequest{StopSeq: seq})
		h.rec.wait(t, wire.EventStopped)

		out := h.mustDo(wire.TypeVarsExpand, wire.VarsExpandRequest{
			Path: "local:cfg", Expr: "cfg",
		}).(wire.VarsExpand)
		for _, c := range out.Children {
			if c.Changed {
				sawChange = true
			}
		}
		if sawChange {
			break
		}
	}
	if !sawChange {
		t.Error("no child was ever marked changed while stepping through code that " +
			"assigns to them; change highlighting is dead")
	}
}

// TestChangeFlagClearsOnNextStop: a value that changed once must not stay
// highlighted forever, or the highlight means nothing.
func TestChangeFlagClearsOnNextStop(t *testing.T) {
	h := startReal(t, "hello")
	h.mustDo(wire.TypeExeLoad, wire.ExeLoadRequest{Path: "hello"})
	h.mustDo(wire.TypeBpSetSource, wire.BreakpointRequest{Path: "hello.c", Line: lineMainInit})
	h.mustDo(wire.TypeExecRun, wire.ExecRequest{})
	h.rec.wait(t, wire.EventStopped)

	h.mustDo(wire.TypeWatchAdd, wire.WatchAddRequest{Expr: "total"})

	// One step assigns total; the next does not touch it.
	changedAt := -1
	for i := range 4 {
		seq := h.sess.Snapshot().StopSeq
		h.mustDo(wire.TypeExecNext, wire.ExecRequest{StopSeq: seq})
		h.rec.wait(t, wire.EventStopped)

		out := h.mustDo(wire.TypeWatchList, nil).(wire.WatchList)
		if len(out.Watches) == 0 {
			t.Fatal("watch vanished")
		}
		if out.Watches[0].Changed {
			changedAt = i
		} else if changedAt >= 0 {
			// It changed earlier and is no longer marked: exactly right.
			return
		}
	}
	if changedAt < 0 {
		t.Skip("total never changed in the steps taken; nothing to assert about clearing")
	}
	t.Error("the watch stayed marked as changed on every subsequent stop")
}
