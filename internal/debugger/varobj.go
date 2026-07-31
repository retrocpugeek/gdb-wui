package debugger

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/retrocpugeek/gdb-wui/internal/mi"
	"github.com/retrocpugeek/gdb-wui/internal/wire"
)

// The varobj registry.
//
// gdb variable objects are a server-side resource with no garbage collector:
// every -var-create leaks until a matching -var-delete, and a debugger UI that
// creates one per visible row per stop will accumulate tens of thousands over
// an afternoon. So they are created as late as possible, kept as long as they
// are useful, and deleted deliberately.
//
// Three decisions shape everything here:
//
//   - The flat locals list uses no varobjs at all. -stack-list-variables
//     --simple-values omits "value" exactly for aggregates, which is both the
//     expandable signal the tree needs and the reason a 100k-element array
//     costs nothing until someone opens it.
//   - Varobjs exist only for expanded subtrees and watches, and they *persist
//     across stops*, refreshed with a single -var-update. Persisting is what
//     keeps ids stable so the tree stays open while you step — the single most
//     important property of that panel.
//   - Roots get names we choose (r17), so deleting one is deterministic;
//     children keep the names gdb assigns (r17.items), because deleting a root
//     deletes its children and we want that to be gdb's problem, not ours.

// maxRoots bounds the registry. Expanding a struct a few hundred times over a
// long session is ordinary; leaking a varobj for each is not.
const maxRoots = 512

// childPageSize caps one -var-list-children call. char buf[1<<20] is a real
// declaration, and fetching a million children to render twenty of them would
// be a 40 MB message.
const childPageSize = 200

// Children are listed with --simple-values, and it is worth recording why,
// because the obvious alternative looks better and is not.
//
// --simple-values omits the value for aggregates, which doubles as the
// expandable signal. The cost is that `char name[16]` shows no value, so a
// string reads as an openable array of chars rather than as "item-0".
//
// --all-values does not fix that. Checked against gdb 17.1: it renders a
// char[16] child as the literal "[16]", which is no more useful and looks like
// a value when it is not. A `char *` already shows its string under
// --simple-values, because a pointer is a simple type. So there is nothing to
// buy here, only per-child rendering cost to pay.

// varobj is one live gdb variable object.
type varobj struct {
	// name is the gdb-side name: "r17" for a root we created, "r17.items" for
	// a child gdb named.
	name string
	// path is the stable client-facing identity.
	path string
	// expr is the expression it was created from.
	expr string
	// root is the name of the root this belongs to, for LRU accounting and
	// deletion. A root's root is itself.
	root string
	// floating varobjs were created with @ and follow the current frame; bound
	// ones were created with * against a specific frame.
	floating bool
	// frame identifies what a bound varobj was created against. It is *not* the
	// frame address: that is the PC, which changes on every step inside one
	// frame, so caching on it would throw the tree away continuously.
	frame frameIdentity

	typ      string
	value    string
	numChild int
	hasMore  bool
	inScope  bool
	changed  bool
}

// frameIdentity is what makes two stops "the same frame" for cache purposes.
//
// Deliberately not the frame address. That is the program counter, which
// changes with every single step *within* a function, so an address-keyed cache
// would be invalidated constantly and the variables tree would collapse on
// every step.
type frameIdentity struct {
	thread     int
	level      int
	function   string
	stackDepth int
}

func (s *Session) currentFrameIdentity(thread, level int) frameIdentity {
	id := frameIdentity{thread: thread, level: level, stackDepth: len(s.st.frames)}
	for _, f := range s.st.frames {
		if f.Level == level {
			id.function = f.Func
			break
		}
	}
	return id
}

// varRegistry owns every live varobj.
type varRegistry struct {
	byPath map[string]*varobj
	byName map[string]*varobj
	// lruRoots is root names, least recently used first.
	lruRoots []string
	nextID   uint64
}

func newVarRegistry() *varRegistry {
	return &varRegistry{byPath: map[string]*varobj{}, byName: map[string]*varobj{}}
}

func (r *varRegistry) get(path string) (*varobj, bool) {
	v, ok := r.byPath[path]
	if ok {
		r.touch(v.root)
	}
	return v, ok
}

func (r *varRegistry) byVarName(name string) (*varobj, bool) {
	v, ok := r.byName[name]
	return v, ok
}

func (r *varRegistry) add(v *varobj) {
	r.byPath[v.path] = v
	r.byName[v.name] = v
	if v.name == v.root {
		r.lruRoots = append(r.lruRoots, v.name)
	} else {
		r.touch(v.root)
	}
}

// touch moves a root to the most-recently-used end.
func (r *varRegistry) touch(root string) {
	for i, name := range r.lruRoots {
		if name == root {
			r.lruRoots = append(r.lruRoots[:i], r.lruRoots[i+1:]...)
			r.lruRoots = append(r.lruRoots, root)
			return
		}
	}
}

// evictRoot removes a root and everything gdb deleted along with it.
func (r *varRegistry) evictRoot(root string) {
	for path, v := range r.byPath {
		if v.root == root {
			delete(r.byPath, path)
			delete(r.byName, v.name)
		}
	}
	for i, name := range r.lruRoots {
		if name == root {
			r.lruRoots = append(r.lruRoots[:i], r.lruRoots[i+1:]...)
			break
		}
	}
}

func (r *varRegistry) roots() []string {
	return append([]string(nil), r.lruRoots...)
}

func (r *varRegistry) newName(prefix string) string {
	r.nextID++
	return prefix + strconv.FormatUint(r.nextID, 10)
}

// createRoot makes a new root varobj.
//
// Bound roots pass --thread/--frame; note the option order. gdb's own usage
// message is "NAME FRAME EXPRESSION", and the general --thread/--frame options
// have to come *before* those positional arguments — putting them after, as
// would seem natural, makes gdb read them as part of the expression and reply
// with a usage error.
func (s *Session) createRoot(ctx context.Context, path, expr string, thread, frame int, floating bool) (*varobj, *wire.Error) {
	s.evictIfFull(ctx)

	prefix := "r"
	frameSpec := "*"
	if floating {
		prefix, frameSpec = "w", "@"
	}
	name := s.vars.newName(prefix)

	cmd := fmt.Sprintf("-var-create --thread %d --frame %d %s %s %s",
		thread, frame, name, frameSpec, quote(expr))
	rec, werr := s.send(ctx, cmd)
	if werr != nil {
		return nil, werr
	}

	v := &varobj{
		name:     rec.Results.Str("name"),
		path:     path,
		expr:     expr,
		floating: floating,
		inScope:  true,
	}
	if v.name == "" {
		v.name = name
	}
	v.root = v.name
	v.typ = rec.Results.Str("type")
	v.value = rec.Results.Str("value")
	v.numChild, _ = rec.Results.Int("numchild")
	v.hasMore, _ = rec.Results.Bool("has_more")
	if !floating {
		v.frame = s.currentFrameIdentity(thread, frame)
	}
	s.vars.add(v)
	return v, nil
}

// evictIfFull deletes the least recently used root to stay under the cap.
func (s *Session) evictIfFull(ctx context.Context) {
	for len(s.vars.lruRoots) >= maxRoots {
		root := s.vars.lruRoots[0]
		// A real -var-delete, not just forgetting: dropping the reference
		// leaks the object inside gdb, which is the failure this cap exists to
		// prevent in the first place.
		if _, werr := s.send(ctx, "-var-delete "+root); werr != nil {
			s.logf("evicting varobj %s: %s", root, werr.Message)
		}
		s.vars.evictRoot(root)
	}
}

// deleteAllVarobjs clears the registry, in gdb and here.
//
// Called whenever the frames every bound varobj refers to cease to exist: a
// re-run, or a new program. Keeping them would mean serving values from a
// process that no longer exists.
func (s *Session) deleteAllVarobjs(ctx context.Context) {
	roots := s.vars.roots()
	for _, root := range roots {
		if _, werr := s.send(ctx, "-var-delete "+root); werr != nil {
			// gdb may already have discarded it, which is not an error worth
			// surfacing — the goal is an empty registry either way.
			s.logf("deleting varobj %s: %s", root, werr.Message)
		}
	}
	s.vars = newVarRegistry()
	if len(roots) > 0 {
		s.emit(wire.EventVarsInvalidated, map[string]any{})
	}
}

// refreshVarobjs updates every live varobj after a stop.
//
// One -var-update for the lot: gdb returns only the ones whose value actually
// changed, which is what makes per-stop refresh affordable and gives change
// highlighting for free.
func (s *Session) refreshVarobjs(ctx context.Context) {
	if len(s.vars.byName) == 0 {
		return
	}
	// Clear last stop's marks first, or a value that changed once stays
	// highlighted forever.
	for _, v := range s.vars.byName {
		v.changed = false
	}

	rec, werr := s.send(ctx, "-var-update --all-values *")
	if werr != nil {
		s.logf("-var-update: %s", werr.Message)
		return
	}
	changes, ok := rec.Results.Get("changelist")
	if !ok {
		return
	}
	// The changelist is a list of tuples, sometimes named "varobj" and
	// sometimes anonymous depending on the gdb version; take both.
	entries := changes.Elements()
	entries = append(entries, changes.All("varobj")...)

	for _, e := range entries {
		name := e.Results().Str("name")
		v, ok := s.vars.byVarName(name)
		if !ok {
			continue
		}
		if inScope, ok := e.Results().StrOK("in_scope"); ok {
			// "invalid" means the frame is gone for good; "false" means it is
			// merely not current. Either way the value must not be shown as if
			// it were live.
			v.inScope = inScope == "true"
		}
		if changed, ok := e.Results().Bool("type_changed"); ok && changed {
			// The expression now names something of a different type, so every
			// child id below it is meaningless. Drop the subtree and let the
			// client re-expand from its stable paths.
			v.typ = e.Results().Str("new_type")
			s.dropChildren(v)
		}
		if value, ok := e.Results().StrOK("value"); ok {
			v.value = value
			v.changed = true
		}
		if hasMore, ok := e.Results().Bool("has_more"); ok {
			v.hasMore = hasMore
		}
	}
}

// dropChildren forgets a node's descendants without deleting them in gdb —
// gdb has already replaced them.
func (s *Session) dropChildren(parent *varobj) {
	prefix := parent.name + "."
	for path, v := range s.vars.byPath {
		if strings.HasPrefix(v.name, prefix) {
			delete(s.vars.byPath, path)
			delete(s.vars.byName, v.name)
		}
	}
}

// listChildren fetches one page of a varobj's children.
func (s *Session) listChildren(ctx context.Context, parent *varobj, from, to int) ([]wire.VarNode, bool, int, *wire.Error) {
	if to <= from {
		from, to = 0, childPageSize
	}
	cmd := fmt.Sprintf("-var-list-children --simple-values %s %d %d", parent.name, from, to)
	rec, werr := s.send(ctx, cmd)
	if werr != nil {
		return nil, false, 0, werr
	}

	numChild, _ := rec.Results.Int("numchild")
	hasMore, _ := rec.Results.Bool("has_more")

	list, ok := rec.Results.Get("children")
	if !ok {
		return nil, hasMore, numChild, nil
	}
	raw := list.All("child")
	if len(raw) == 0 {
		raw = list.Elements()
	}

	out := make([]wire.VarNode, 0, len(raw))
	for _, c := range raw {
		res := c.Results()
		exp := res.Str("exp")
		child := &varobj{
			name:     res.Str("name"),
			path:     childPath(parent.path, exp),
			expr:     childExpr(parent.expr, exp),
			root:     parent.root,
			floating: parent.floating,
			frame:    parent.frame,
			typ:      res.Str("type"),
			numChild: 0,
			inScope:  parent.inScope,
		}
		child.numChild, _ = res.Int("numchild")
		value, hasValue := res.StrOK("value")
		child.value = value
		child.hasMore, _ = res.Bool("has_more")

		// If gdb already gave us this child, keep the *registered* object
		// rather than the one just built. -var-update sets `changed` on the
		// registered object at each stop, and re-expansion happens after that;
		// returning the fresh struct would silently drop every change
		// highlight, which is the one thing the panel exists to show.
		if existing, ok := s.vars.byVarName(child.name); ok {
			existing.value = child.value
			existing.typ = child.typ
			existing.numChild = child.numChild
			existing.hasMore = child.hasMore
			existing.inScope = child.inScope
			child = existing
		} else {
			s.vars.add(child)
		}

		out = append(out, nodeFor(child, !hasValue))
	}
	return out, hasMore, numChild, nil
}

// nodeFor renders a varobj for the wire.
func nodeFor(v *varobj, expandable bool) wire.VarNode {
	node := wire.VarNode{
		Path:         v.path,
		ID:           v.name,
		Name:         displayName(v.path),
		Expr:         v.expr,
		Type:         v.typ,
		Value:        v.value,
		NumChild:     v.numChild,
		Expandable:   expandable || v.numChild > 0,
		HasMore:      v.hasMore,
		InScope:      v.inScope,
		Changed:      v.changed,
		OptimizedOut: v.value == wire.OptimizedOut,
	}
	return node
}

// childPath builds a stable path for a child.
//
// gdb reports an array child's exp as a bare index, so "items" plus "0" has to
// become "items[0]" rather than "items.0" — otherwise the path is ambiguous
// with a struct field literally named 0, and, more practically, it would not
// read like the expression a user recognises.
func childPath(parentPath, exp string) string {
	if isIndex(exp) {
		return parentPath + "[" + exp + "]"
	}
	return parentPath + "." + exp
}

func childExpr(parentExpr, exp string) string {
	if isIndex(exp) {
		return parentExpr + "[" + exp + "]"
	}
	// Not parentExpr + "." + exp: the parent may be a pointer, and gdb's own
	// child expressions handle that. This is only used when a varobj has to be
	// recreated from scratch, where the path is the better guide anyway.
	return parentExpr + "." + exp
}

func isIndex(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// displayName is the last component of a path.
func displayName(path string) string {
	if i := strings.LastIndexAny(path, ".["); i >= 0 {
		name := path[i:]
		name = strings.TrimPrefix(name, ".")
		return name
	}
	if i := strings.IndexByte(path, ':'); i >= 0 {
		return path[i+1:]
	}
	return path
}

// VarobjCount reports how many variable objects are live.
//
// Exported for tests: "the registry is empty after a re-run" is a leak check
// that cannot be made from outside the package any other way, and a leak here
// is invisible until gdb slows to a crawl hours later.
func (s *Session) VarobjCount() int { return len(s.vars.byName) }

// parseSimpleValues turns a -stack-list-variables reply into the flat list.
func parseVariables(list []mi.Value) []wire.Variable {
	out := make([]wire.Variable, 0, len(list))
	for _, v := range list {
		res := v.Results()
		value, hasValue := res.StrOK("value")
		isArg, _ := res.Bool("arg")
		out = append(out, wire.Variable{
			Name:  res.Str("name"),
			Type:  res.Str("type"),
			Value: value,
			// Absence of a value under --simple-values is the expandable
			// signal; it is not an error and not a missing field.
			Expandable:   !hasValue,
			Arg:          isArg,
			OptimizedOut: value == wire.OptimizedOut,
		})
	}
	return out
}
