package debugger

import (
	"bytes"
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/retrocpugeek/gdb-wui/internal/mi"
	"github.com/retrocpugeek/gdb-wui/internal/wire"
)

// Symbols.
//
// The symbol table is static within one program: it changes when a file is
// loaded, not when the inferior runs or stops. So it is read once and cached,
// and the search box filters the cache. The alternative — passing the user's
// text to gdb as a --name regexp on every keystroke — puts a gdb round trip in
// the way of each character, and gdb's regexp is not what a user typing into a
// box marked "filter" expects anyway.
//
// Reading it is two commands, not one. gdb splits functions and variables into
// separate MI commands, and each reports its own nondebug section: the ELF
// symbols it classifies as code, and those it classifies as data. That split
// is where the function-vs-variable distinction comes from for stripped
// binaries, which have no debug info to ask.

const (
	// symbolsDefaultLimit is how many rows go back when the client does not
	// say. It is a screenful and a wide margin, not a guess at the table size.
	symbolsDefaultLimit = 500
	// symbolsMaxLimit bounds what a client can ask for in one reply.
	symbolsMaxLimit = 5000
)

func (s *Session) symbolsList(r *request) (any, *wire.Error) {
	req, werr := decode[wire.SymbolsListRequest](r.req.Payload)
	if werr != nil {
		return nil, werr
	}
	if werr := s.ensureSymbols(r.ctx); werr != nil {
		return nil, werr
	}

	limit := req.Limit
	if limit <= 0 {
		limit = symbolsDefaultLimit
	}
	if limit > symbolsMaxLimit {
		limit = symbolsMaxLimit
	}

	needle := strings.ToLower(strings.TrimSpace(req.Filter))
	out := make([]wire.Symbol, 0, limit)
	matched := 0
	for _, sym := range s.st.symbols {
		if req.Kind != "" && sym.Kind != req.Kind {
			continue
		}
		if needle != "" && !strings.Contains(strings.ToLower(sym.Name), needle) {
			continue
		}
		matched++
		if len(out) < limit {
			out = append(out, sym)
		}
	}
	// Rank within the page rather than over the whole table: the user is
	// looking for something they can half-remember the name of, and an exact
	// or leading match is nearly always it. Sorting the page keeps this O(page).
	rankSymbols(out, needle)

	return wire.SymbolsList{
		Symbols:   out,
		Matched:   matched,
		Available: len(s.st.symbols),
		Truncated: matched > len(out),
	}, nil
}

// rankSymbols puts the likeliest hit first: exact name, then prefix, then
// anything else, alphabetical within each band.
func rankSymbols(syms []wire.Symbol, needle string) {
	band := func(name string) int {
		if needle == "" {
			return 2
		}
		lower := strings.ToLower(name)
		switch {
		case lower == needle:
			return 0
		case strings.HasPrefix(lower, needle):
			return 1
		default:
			return 2
		}
	}
	sort.SliceStable(syms, func(i, j int) bool {
		bi, bj := band(syms[i].Name), band(syms[j].Name)
		if bi != bj {
			return bi < bj
		}
		return syms[i].Name < syms[j].Name
	})
}

// ensureSymbols fills the cache if it is cold.
//
// A program with genuinely no symbols — a stripped binary with no dynamic
// table — must not be re-read on every keystroke, so emptiness is recorded
// separately from coldness.
func (s *Session) ensureSymbols(ctx context.Context) *wire.Error {
	if s.st.symbolsRead {
		return nil
	}
	syms, werr := s.fetchSymbols(ctx)
	if werr != nil {
		return werr
	}
	s.st.symbols = syms
	s.st.symbolsRead = true
	return nil
}

// invalidateSymbols drops the cache and tells clients to refetch.
func (s *Session) invalidateSymbols() {
	if !s.st.symbolsRead && s.st.symbols == nil {
		return
	}
	s.st.symbols = nil
	s.st.symbolsRead = false
	s.cfg.Events.Broadcast(wire.EventSymbolsInvalidated, map[string]any{})
}

func (s *Session) fetchSymbols(ctx context.Context) ([]wire.Symbol, *wire.Error) {
	var out []wire.Symbol
	for _, q := range []struct {
		cmd  string
		kind string
	}{
		{"-symbol-info-functions --include-nondebug", wire.SymbolFunction},
		{"-symbol-info-variables --include-nondebug", wire.SymbolVariable},
	} {
		rec, werr := s.send(ctx, q.cmd)
		if werr != nil {
			// One half failing should not lose the other. A gdb too old for
			// these commands errors on both and the caller gets an empty
			// table, which is the truth for it.
			continue
		}
		out = append(out, s.parseSymbols(rec, q.kind)...)
	}
	return out, nil
}

// parseSymbols turns one -symbol-info-* reply into wire symbols.
//
// Shape:
//
//	symbols={debug=[{filename=…,fullname=…,symbols=[{line=…,name=…,type=…}]}],
//	         nondebug=[{address=…,name=…}]}
func (s *Session) parseSymbols(rec mi.Record, kind string) []wire.Symbol {
	table, ok := rec.Results.Tuple("symbols")
	if !ok {
		return nil
	}
	var out []wire.Symbol

	if files, ok := table.List("debug"); ok {
		for _, f := range files {
			gdbPath := f.Results().Str("fullname")
			if gdbPath == "" {
				gdbPath = f.Results().Str("filename")
			}
			rel := s.relPathFor(gdbPath)
			entries, _ := f.List("symbols")
			for _, e := range entries {
				name := e.Results().Str("name")
				if name == "" {
					continue
				}
				line, _ := e.Int("line")
				out = append(out, wire.Symbol{
					Name:    name,
					Kind:    kind,
					Type:    e.Results().Str("type"),
					File:    rel,
					GDBPath: gdbPath,
					Line:    line,
					Debug:   true,
				})
			}
		}
	}

	if plain, ok := table.List("nondebug"); ok {
		for _, e := range plain {
			name := e.Results().Str("name")
			if name == "" {
				continue
			}
			out = append(out, wire.Symbol{
				Name:    name,
				Kind:    kind,
				Address: normaliseSymbolAddr(e.Results().Str("address")),
			})
		}
	}
	return out
}

// relPathFor maps a path gdb reported onto one inside the project, or returns
// "" when it does not live there. The index is consulted as well as the plain
// containment check, because a program built elsewhere reports build-time
// paths that only match by trailing components.
func (s *Session) relPathFor(gdbPath string) string {
	if gdbPath == "" || s.files == nil {
		return ""
	}
	if rel, ok := s.files.RelPath(gdbPath); ok {
		return rel
	}
	if rel, ok := s.files.Index().Locate(gdbPath); ok {
		return rel
	}
	return ""
}

// normaliseSymbolAddr trims the zero padding gdb applies to nondebug
// addresses. "0x0000000000001040" is the same address as "0x1040" and matches
// what every other panel shows.
func normaliseSymbolAddr(addr string) string {
	if !strings.HasPrefix(addr, "0x") {
		return addr
	}
	n, err := strconv.ParseUint(addr[2:], 16, 64)
	if err != nil {
		return addr
	}
	return "0x" + strconv.FormatUint(n, 16)
}

// symbolsLoad installs a symbol table without declaring a program to run.
//
// This is what the two remote cases need. Against a stub that cannot load an
// ELF, and against a process someone else started, the code is already in the
// target's memory: the only thing missing is what the addresses mean.
// `exe.load` cannot express that — -file-exec-and-symbols sets both — and the
// difference is not cosmetic, because an exec file leaves the UI offering to
// Run a second, local copy of a program that is already running elsewhere.
func (s *Session) symbolsLoad(r *request) (any, *wire.Error) {
	req, werr := decode[wire.SymbolsLoadRequest](r.req.Payload)
	if werr != nil {
		return nil, werr
	}
	if req.Path == "" {
		return nil, wire.NewError(wire.CodeBadRequest, "path is required")
	}
	if s.files == nil {
		return nil, wire.NewError(wire.CodeInternal, "no project is configured")
	}

	mode := req.Mode
	if mode == "" {
		mode = wire.SymbolsReplace
	}
	if mode != wire.SymbolsReplace && mode != wire.SymbolsAdd {
		return nil, wire.NewError(wire.CodeBadRequest,
			fmt.Sprintf("unknown mode %q; want %q or %q",
				mode, wire.SymbolsReplace, wire.SymbolsAdd))
	}

	// Same containment and same ELF check as exe.load. A symbol file is an
	// object file; anything else makes gdb complain in a way that reads like a
	// gdb-wui bug.
	head, err := s.files.Head(req.Path, 4)
	if err != nil {
		return nil, fsError(err)
	}
	if !bytes.Equal(head, elfMagic) {
		return nil, wire.NewError(wire.CodeBadRequest,
			fmt.Sprintf("%s is not an ELF object file", req.Path))
	}
	abs, err := s.files.AbsPath(req.Path)
	if err != nil {
		return nil, fsError(err)
	}

	var cmd string
	switch mode {
	case wire.SymbolsReplace:
		cmd = "-file-symbol-file " + quote(abs)
	case wire.SymbolsAdd:
		// add-symbol-file has no MI form, so it goes through the console — the
		// same route `starti` takes.
		line := "add-symbol-file " + gdbQuote(abs)
		if off := strings.TrimSpace(req.Offset); off != "" {
			n, err := parseAddress(off)
			if err != nil {
				return nil, wire.NewError(wire.CodeBadRequest,
					fmt.Sprintf("unparseable offset %q", off))
			}
			line += fmt.Sprintf(" -o 0x%x", n)
		}
		cmd = "-interpreter-exec console " + quote(line)
	}

	if _, werr := s.send(r.ctx, cmd); werr != nil {
		return nil, werr
	}

	// Whatever was cached describes the previous table.
	s.invalidateSymbols()
	if werr := s.ensureSymbols(r.ctx); werr != nil {
		return nil, werr
	}
	return wire.SymbolsLoaded{
		Path:      req.Path,
		Mode:      mode,
		Available: len(s.st.symbols),
	}, nil
}

// gdbQuote wraps a path for gdb's *console* parser, which splits on spaces.
// MI quoting is a separate layer applied on top by quote().
func gdbQuote(p string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`)
	return `"` + r.Replace(p) + `"`
}
