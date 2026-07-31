package debugger

import (
	"context"
	"fmt"
	"strings"

	"github.com/retrocpugeek/gdb-wui/internal/wire"
)

// Source-path resolution, for real.
//
// M3 handled the easy case: a program built here reports absolute paths inside
// the project, and those resolve directly. Everything else — a binary built in a
// container, a libc frame, a moved checkout — reported an unresolvable path and
// the UI said so. This is the rest.
//
// The order is: resolve directly; else match the project's basename index by
// longest trailing path-component count and, on a clear winner, *teach gdb the
// prefix* with substitute-path so every later frame is right at the source
// rather than fixed up here; else offer the candidates and let the user choose.

// resolveSourceFull is the resolution path used from M8 onward.
func (s *Session) resolveSourceFull(fullname, file string, line int) wire.SourceRef {
	ref := wire.SourceRef{Line: line}
	gdbPath := fullname
	if gdbPath == "" {
		gdbPath = file
	}
	ref.GDBPath = gdbPath
	if gdbPath == "" || s.files == nil {
		return ref
	}
	if cached, ok := s.srcCache[gdbPath]; ok {
		cached.Line = line
		return cached
	}

	resolved := ref
	switch {
	case func() bool {
		rel, ok := s.files.RelPath(gdbPath)
		if ok {
			resolved.Available, resolved.Path, resolved.GDBPath = true, rel, gdbPath
		}
		return ok
	}():
		// Already inside the project: nothing to teach gdb.

	default:
		// Not here under that name. Try the index.
		if rel, ok := s.files.Index().Locate(gdbPath); ok {
			resolved.Available, resolved.Path = true, rel
			s.teachSubstitution(gdbPath, rel)
		} else {
			// Ambiguous or absent. Offer what is here so the user can pick,
			// rather than leaving them with a path that means nothing.
			resolved.Candidates = s.files.Index().Candidates(gdbPath)
		}
	}

	if resolved.Available {
		resolved.Stale = s.sourceIsStale(resolved.Path)
	}
	if s.srcCache == nil {
		s.srcCache = map[string]wire.SourceRef{}
	}
	s.srcCache[gdbPath] = resolved
	resolved.Line = line
	return resolved
}

// teachSubstitution tells gdb about a prefix mapping, once.
//
// Once per mapping, not per file: substitute-path accumulates, and adding the
// same rule repeatedly leaves gdb walking a list that grows with every frame.
func (s *Session) teachSubstitution(gdbPath, rel string) {
	from, to, ok := s.files.SubstitutionFor(gdbPath, rel)
	if !ok {
		return
	}
	for _, sub := range s.st.substitutions {
		if sub.From == from {
			return
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), s.cfg.CommandTimeout)
	defer cancel()

	cmd := fmt.Sprintf("-gdb-set substitute-path %s %s", quote(from), quote(to))
	if _, werr := s.send(ctx, cmd); werr != nil {
		s.logf("substitute-path %s -> %s: %s", from, to, werr.Message)
		return
	}
	s.st.substitutions = append(s.st.substitutions, wire.Substitution{From: from, To: to})
	// Every cached miss might resolve now, so the cache has to go. It is small
	// and rebuilt on the next stop.
	s.srcCache = nil
	s.logf("taught gdb substitute-path %s -> %s", from, to)
}

// sourceIsStale reports whether a source file is newer than the program.
//
// If it is, the line numbers in the debug info describe code that is no longer
// what is on screen. Saying so beats letting someone chase a discrepancy
// between the source they are reading and the instructions actually running.
func (s *Session) sourceIsStale(rel string) bool {
	if s.st.exePath == "" {
		return false
	}
	src, err := s.files.Stat(rel)
	if err != nil {
		return false
	}
	exe, err := s.files.Stat(s.st.exePath)
	if err != nil {
		return false
	}
	return src.ModTime().After(exe.ModTime())
}

// --- the path request group -------------------------------------------------

func (s *Session) pathSubstitute(r *request) (any, *wire.Error) {
	req, werr := decode[wire.PathSubstituteRequest](r.req.Payload)
	if werr != nil {
		return nil, werr
	}

	from, to := strings.TrimSpace(req.From), strings.TrimSpace(req.To)
	if from == "" || to == "" {
		// The "locate this file" affordance knows two files, not two prefixes,
		// so it sends the pair and the prefixes are derived here.
		if req.GDBPath == "" || req.Path == "" {
			return nil, wire.NewError(wire.CodeBadRequest,
				"give from and to, or gdbPath and path")
		}
		if _, err := s.files.Stat(req.Path); err != nil {
			return nil, fsError(err)
		}
		var ok bool
		from, to, ok = s.files.SubstitutionFor(req.GDBPath, req.Path)
		if !ok {
			// Either the paths share no trailing components, or they already
			// agree. The second is not a failure worth alarming anyone about —
			// it means resolution had already worked.
			if _, already := s.files.RelPath(req.GDBPath); already {
				return s.pathListPayload(), nil
			}
			return nil, wire.NewError(wire.CodeBadRequest,
				fmt.Sprintf("%q and %q share no trailing path components, so there is "+
					"no prefix mapping that would relate them", req.GDBPath, req.Path))
		}
	}

	cmd := fmt.Sprintf("-gdb-set substitute-path %s %s", quote(from), quote(to))
	if _, werr := s.send(r.ctx, cmd); werr != nil {
		return nil, werr
	}
	s.st.substitutions = append(s.st.substitutions, wire.Substitution{From: from, To: to})
	s.srcCache = nil

	// Re-resolve the current stop so the source pane updates immediately rather
	// than at the next stop.
	s.refreshFramesAfterPathChange(r.ctx)
	return s.pathListPayload(), nil
}

func (s *Session) pathAddDir(r *request) (any, *wire.Error) {
	req, werr := decode[wire.PathAddDirRequest](r.req.Payload)
	if werr != nil {
		return nil, werr
	}
	if strings.TrimSpace(req.Dir) == "" {
		return nil, wire.NewError(wire.CodeBadRequest, "dir is required")
	}
	abs, err := s.files.AbsPath(req.Dir)
	if err != nil {
		return nil, fsError(err)
	}
	if _, werr := s.send(r.ctx, "-environment-directory "+quote(abs)); werr != nil {
		return nil, werr
	}
	s.st.sourceDirs = append(s.st.sourceDirs, req.Dir)
	s.srcCache = nil
	s.refreshFramesAfterPathChange(r.ctx)
	return s.pathListPayload(), nil
}

func (s *Session) pathList(r *request) (any, *wire.Error) {
	return s.pathListPayload(), nil
}

func (s *Session) pathListPayload() wire.PathList {
	out := wire.PathList{
		Substitutions: append([]wire.Substitution(nil), s.st.substitutions...),
		Directories:   append([]string(nil), s.st.sourceDirs...),
	}
	if s.files != nil {
		out.Indexed, out.IndexTruncated = s.files.Index().Stats()
	}
	return out
}

// refreshFramesAfterPathChange re-resolves the current stack and tells clients.
func (s *Session) refreshFramesAfterPathChange(ctx context.Context) {
	if s.st.runState != wire.RunStateStopped {
		return
	}
	frames, werr := s.fetchFrames(ctx, s.st.selThread, 0, maxFrames)
	if werr != nil {
		return
	}
	s.st.frames = frames
	sel := wire.Selection{
		ThreadID: s.st.selThread,
		Frame:    s.st.selFrame,
		StopSeq:  s.st.stopSeq,
		Frames:   frames,
	}
	if f, ok := s.frameAt(s.st.selFrame); ok {
		src := f.Source
		sel.Source = &src
	}
	s.emit(wire.EventSelectionChanged, sel)
}
