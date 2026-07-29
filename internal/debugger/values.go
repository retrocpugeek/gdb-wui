package debugger

import (
	"context"
	"fmt"
	"strings"

	"github.com/retrocpugeek/gdb-wui/internal/wire"
)

// Locals, watches and registers: the value-inspection request handlers.

func (s *Session) varsLocals(r *request) (any, *wire.Error) {
	req, werr := decode[wire.VarsLocalsRequest](r.req.Payload)
	if werr != nil {
		return nil, werr
	}
	if werr := s.checkStopSeq(req.StopSeq); werr != nil {
		return nil, werr
	}
	thread := s.thread(req.Thread)
	frame := req.Frame

	locals, werr := s.fetchLocals(r.ctx, thread, frame)
	if werr != nil {
		return nil, werr
	}
	if thread == s.st.selThread && frame == s.st.selFrame {
		s.st.locals = locals
	}
	return wire.VarsLocals{
		StopSeq:   s.st.stopSeq,
		ThreadID:  thread,
		Frame:     frame,
		Variables: localNodes(locals),
	}, nil
}

// localNodes lifts the flat locals into tree rows.
//
// They carry no varobj: one is created only if the user opens the row. That is
// what keeps `int huge[100000]` free until somebody asks.
func localNodes(locals []wire.Variable) []wire.VarNode {
	out := make([]wire.VarNode, 0, len(locals))
	for _, v := range locals {
		out = append(out, wire.VarNode{
			Path:         "local:" + v.Name,
			Name:         v.Name,
			Expr:         v.Name,
			Type:         v.Type,
			Value:        v.Value,
			Expandable:   v.Expandable,
			InScope:      true,
			Arg:          v.Arg,
			OptimizedOut: v.OptimizedOut,
		})
	}
	return out
}

func (s *Session) varsExpand(r *request) (any, *wire.Error) {
	req, werr := decode[wire.VarsExpandRequest](r.req.Payload)
	if werr != nil {
		return nil, werr
	}
	if req.Path == "" {
		return nil, wire.NewError(wire.CodeBadRequest, "path is required")
	}
	if werr := s.checkStopSeq(req.StopSeq); werr != nil {
		return nil, werr
	}
	thread := s.thread(req.Thread)
	frame := req.Frame

	parent, werr := s.varobjFor(r.ctx, req, thread, frame)
	if werr != nil {
		return nil, werr
	}

	children, hasMore, numChild, werr := s.listChildren(r.ctx, parent, req.From, req.To)
	if werr != nil {
		return nil, werr
	}
	return wire.VarsExpand{
		Path:     req.Path,
		ID:       parent.name,
		StopSeq:  s.st.stopSeq,
		Children: children,
		HasMore:  hasMore,
		NumChild: numChild,
	}, nil
}

// varobjFor finds or creates the varobj a request refers to.
//
// Three cases, in order of preference: one already exists for this path and is
// still usable; the client named an id we still know; or nothing exists and one
// has to be created from the expression. The last case is the common one — a
// flat local being opened for the first time.
func (s *Session) varobjFor(ctx context.Context, req wire.VarsExpandRequest, thread, frame int) (*varobj, *wire.Error) {
	if v, ok := s.vars.get(req.Path); ok && s.usable(v, thread, frame) {
		return v, nil
	}
	if req.ID != "" {
		if v, ok := s.vars.byVarName(req.ID); ok && s.usable(v, thread, frame) {
			return v, nil
		}
	}

	expr := req.Expr
	if expr == "" {
		expr = exprFromPath(req.Path)
	}
	if expr == "" {
		return nil, wire.NewError(wire.CodeBadRequest,
			"cannot expand "+req.Path+" without an expression")
	}
	// A path that already exists but is stale — wrong frame, or out of scope —
	// is replaced rather than reused.
	if old, ok := s.vars.get(req.Path); ok {
		if _, werr := s.send(ctx, "-var-delete "+old.root); werr != nil {
			s.logf("replacing stale varobj %s: %s", old.name, werr.Message)
		}
		s.vars.evictRoot(old.root)
	}
	return s.createRoot(ctx, req.Path, expr, thread, frame, false)
}

// usable reports whether an existing varobj still describes what the client
// asked about.
func (s *Session) usable(v *varobj, thread, frame int) bool {
	if !v.inScope {
		return false
	}
	if v.floating {
		return true
	}
	return v.frame == s.currentFrameIdentity(thread, frame)
}

// exprFromPath recovers an expression from a stable path, for the case where a
// client re-expands a subtree whose varobj was evicted and does not resend the
// expression.
func exprFromPath(path string) string {
	if i := strings.IndexByte(path, ':'); i >= 0 {
		return path[i+1:]
	}
	return path
}

// --- watches ---------------------------------------------------------------

func (s *Session) watchAdd(r *request) (any, *wire.Error) {
	req, werr := decode[wire.WatchAddRequest](r.req.Payload)
	if werr != nil {
		return nil, werr
	}
	expr := strings.TrimSpace(req.Expr)
	if expr == "" {
		return nil, wire.NewError(wire.CodeBadRequest, "expr is required")
	}
	if len(s.st.watches) >= maxWatches {
		return nil, wire.NewError(wire.CodeTooLarge,
			fmt.Sprintf("at most %d watches", maxWatches))
	}

	s.st.watchSeq++
	path := fmt.Sprintf("watch:%d", s.st.watchSeq)

	// Floating (@), so the watch follows the current frame instead of being
	// pinned to the one that happened to be selected when it was typed. That is
	// what makes "watch i" behave the way a user expects while stepping.
	v, werr := s.createRoot(r.ctx, path, expr, s.thread(0), s.st.selFrame, true)
	if werr != nil {
		// The expression is the user's, so a gdb error here is a normal
		// outcome, not a fault: report it and add nothing.
		return nil, werr
	}
	s.st.watches = append(s.st.watches, watch{path: path, expr: expr})
	out := s.watchList()
	s.cfg.Events.Broadcast(wire.EventWatchesChanged, out)
	_ = v
	return out, nil
}

func (s *Session) watchRemove(r *request) (any, *wire.Error) {
	req, werr := decode[wire.WatchRemoveRequest](r.req.Payload)
	if werr != nil {
		return nil, werr
	}
	kept := s.st.watches[:0]
	var removed bool
	for _, w := range s.st.watches {
		if w.path == req.Path {
			removed = true
			if v, ok := s.vars.get(w.path); ok {
				if _, werr := s.send(r.ctx, "-var-delete "+v.root); werr != nil {
					s.logf("deleting watch %s: %s", v.name, werr.Message)
				}
				s.vars.evictRoot(v.root)
			}
			continue
		}
		kept = append(kept, w)
	}
	s.st.watches = kept
	if !removed {
		return nil, wire.NewError(wire.CodeNotFound, "no such watch")
	}
	out := s.watchList()
	s.cfg.Events.Broadcast(wire.EventWatchesChanged, out)
	return out, nil
}

func (s *Session) watchListRequest(r *request) (any, *wire.Error) {
	return s.watchList(), nil
}

// maxWatches is a sanity bound; the panel is a list a human reads.
const maxWatches = 200

// watch is a user's expression, kept independently of the varobj behind it.
//
// The expression outlives the varobj on purpose: a re-run deletes every varobj,
// but the user's watches should still be there afterwards, which means
// remembering what they asked for and recreating it.
type watch struct {
	path string
	expr string
}

func (s *Session) watchList() wire.WatchList {
	out := wire.WatchList{StopSeq: s.st.stopSeq, Watches: make([]wire.VarNode, 0, len(s.st.watches))}
	for _, w := range s.st.watches {
		if v, ok := s.vars.get(w.path); ok {
			node := nodeFor(v, v.numChild > 0)
			node.Name = w.expr
			node.Expr = w.expr
			out.Watches = append(out.Watches, node)
			continue
		}
		// No varobj: either it has not been created yet, or the program is not
		// running. Show the expression rather than dropping the row.
		out.Watches = append(out.Watches, wire.VarNode{
			Path: w.path, Name: w.expr, Expr: w.expr, InScope: false,
		})
	}
	return out
}

// recreateWatches rebuilds watch varobjs after the registry was cleared.
func (s *Session) recreateWatches(ctx context.Context) {
	if len(s.st.watches) == 0 || s.st.runState != wire.RunStateStopped {
		return
	}
	for _, w := range s.st.watches {
		if _, ok := s.vars.get(w.path); ok {
			continue
		}
		if _, werr := s.createRoot(ctx, w.path, w.expr, s.thread(0), s.st.selFrame, true); werr != nil {
			// An expression that no longer resolves in this program is the
			// user's to fix; the row stays, marked out of scope.
			s.logf("re-creating watch %q: %s", w.expr, werr.Message)
		}
	}
}

// --- registers -------------------------------------------------------------

func (s *Session) regsNames(r *request) (any, *wire.Error) {
	if len(s.st.registerNames) == 0 {
		if werr := s.loadRegisterNames(r.ctx); werr != nil {
			return nil, werr
		}
	}
	return wire.RegsNames{Names: s.st.registerNames}, nil
}

// loadRegisterNames caches the name list.
//
// The list contains empty strings at stable indices — gdb has gaps in its
// register numbering — so it is stored verbatim. Filtering the blanks would
// shift every later index and silently mislabel half the register file.
func (s *Session) loadRegisterNames(ctx context.Context) *wire.Error {
	rec, werr := s.send(ctx, "-data-list-register-names")
	if werr != nil {
		return werr
	}
	list, ok := rec.Results.List("register-names")
	if !ok {
		return nil
	}
	names := make([]string, 0, len(list))
	for _, n := range list {
		names = append(names, n.Str)
	}
	s.st.registerNames = names
	return nil
}

func (s *Session) regsValues(r *request) (any, *wire.Error) {
	req, werr := decode[wire.RegsValuesRequest](r.req.Payload)
	if werr != nil {
		return nil, werr
	}
	if werr := s.checkStopSeq(req.StopSeq); werr != nil {
		return nil, werr
	}
	format := req.Format
	if format == "" {
		format = "x"
	}
	if len(format) != 1 || !strings.Contains("xdotNrz", format) {
		return nil, wire.NewError(wire.CodeBadRequest, "format must be one of x d o t N r z")
	}
	thread := s.thread(req.Thread)

	if len(s.st.registerNames) == 0 {
		if werr := s.loadRegisterNames(r.ctx); werr != nil {
			return nil, werr
		}
	}

	// gdb tracks which registers changed since the last stop, so change
	// highlighting costs one command rather than a diff of the whole file.
	changed := map[int]bool{}
	if rec, werr := s.send(r.ctx, "-data-list-changed-registers"); werr == nil {
		if list, ok := rec.Results.List("changed-registers"); ok {
			for _, n := range list {
				changed[atoiSafe(n.Str)] = true
			}
		}
	} else {
		s.logf("-data-list-changed-registers: %s", werr.Message)
	}

	rec, werr := s.send(r.ctx, fmt.Sprintf("-data-list-register-values --thread %d %s", thread, format))
	if werr != nil {
		return nil, werr
	}
	list, ok := rec.Results.List("register-values")
	if !ok {
		return nil, nil
	}

	out := wire.RegsValues{
		StopSeq:   s.st.stopSeq,
		ThreadID:  thread,
		Format:    format,
		Registers: make([]wire.Register, 0, len(list)),
	}
	for _, v := range list {
		num, _ := v.Int("number")
		reg := wire.Register{
			Number:  num,
			Value:   v.Results().Str("value"),
			Changed: changed[num],
		}
		// Registers are identified by number; the name is decoration, and may
		// legitimately be empty.
		if num >= 0 && num < len(s.st.registerNames) {
			reg.Name = s.st.registerNames[num]
		}
		out.Registers = append(out.Registers, reg)
	}
	return out, nil
}
