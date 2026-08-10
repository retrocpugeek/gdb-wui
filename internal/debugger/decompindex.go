package debugger

import (
	"fmt"
	"strings"

	"github.com/retrocpugeek/gdb-wui/internal/wire"
)

// The decompiler's names, as something you can look up.
//
// A stripped binary's symbol pane is empty and its go-to box refuses
// everything, which is exactly backwards: that is the program with the most to
// find and the least to find it with. Ghidra has names for all of it —
// FUN_0010e2dc for every function it recovered, DAT_001a08de for every global
// something referenced — and until now they existed only inside decompiled
// text. You could read a name and have no way to ask where it was.
//
// So the index is read once and held. Filtering a search box against it is then
// the same in-process scan the gdb symbol table already gets, rather than a
// round trip to a JVM per keystroke, and resolving one name for a go-to or a
// breakpoint is a map lookup.
//
// Addresses are kept in Ghidra's coordinates and biased on the way out, for the
// same reason decomp.names does it: a position-independent executable relocates
// when it starts, so a runtime address cached before `run` is wrong after it.

// maxIndex bounds what is held for one program. Firmware with more labels than
// this exists, and the pane shows a page of a filtered list either way; the cap
// is against unbounded memory, not against large programs.
const maxIndex = 200_000

// indexPage is how many entries are asked for per round trip. The sidecar caps
// a request at 5000.
const indexPage = 5000

// decompEntry is one name the decompiler knows.
type decompEntry struct {
	Name string
	// Addr is a Ghidra address, before any bias.
	Addr uint64
	// Kind is wire.SymbolFunction or wire.SymbolVariable.
	Kind string
	// Type is the data type of a global, when one has been applied. Always
	// empty for a function: a prototype is worth showing but costs a
	// decompilation per row, which is the thing the index exists not to do.
	Type string
}

// decompNamed finds one entry by its exact name.
//
// Exact and case-sensitive, unlike the search filter. A go-to or a breakpoint
// is acting on a name, and matching "dat_1a08de" to DAT_001a08de would be
// guessing at which of several near-misses was meant.
func (s *Session) decompNamed(r *request, name string) (decompEntry, bool) {
	entries, byName := s.decompIndex(r)
	if byName == nil {
		return decompEntry{}, false
	}
	at, ok := byName[name]
	if !ok {
		return decompEntry{}, false
	}
	return entries[at], true
}

// decompIndex returns the index, building it if this is the first ask.
//
// A nil map means there is nothing to look in — no decompiler, or one that is
// still starting — and every caller treats that as "the decompiler does not
// know this name", which is also what it means when the index is present and
// the name is not in it.
func (s *Session) decompIndex(r *request) ([]decompEntry, map[string]int) {
	s.decomp.mu.Lock()
	client := s.decomp.client
	s.decomp.mu.Unlock()
	if client == nil {
		// Asking is what starts it, as everywhere else. This request gets
		// nothing; the next one, after analysis, gets the index.
		s.maybeStartDecomp()
		return nil, nil
	}
	if s.decomp.indexFor == client && s.decomp.indexBy != nil {
		return s.decomp.index, s.decomp.indexBy
	}

	entries := make([]decompEntry, 0, 1024)
	for offset := 0; offset < maxIndex; offset += indexPage {
		list, err := client.Functions(r.ctx, offset, indexPage, "")
		if err != nil {
			return nil, nil
		}
		for _, f := range list.Functions {
			// A thunk is a jump to the real thing under the same name, and
			// listing both makes every library call look like two functions.
			if f.Thunk {
				continue
			}
			addr, err := parseAddress(f.Entry)
			if err != nil {
				continue
			}
			entries = append(entries, decompEntry{
				Name: f.Name, Addr: addr, Kind: wire.SymbolFunction,
			})
		}
		if offset+len(list.Functions) >= list.Total || len(list.Functions) == 0 {
			break
		}
	}
	for offset := 0; offset < maxIndex; offset += indexPage {
		list, err := client.Data(r.ctx, offset, indexPage, "")
		if err != nil {
			// Functions alone is a worse index than both and a better one than
			// none. An older sidecar has no data op, and refusing to list
			// anything because of it would be the wrong half of the truth.
			break
		}
		for _, d := range list.Data {
			addr, err := parseAddress(d.Address)
			if err != nil {
				continue
			}
			entries = append(entries, decompEntry{
				Name: d.Name, Addr: addr, Kind: wire.SymbolVariable, Type: d.Type,
			})
		}
		if offset+len(list.Data) >= list.Total || len(list.Data) == 0 {
			break
		}
	}

	byName := make(map[string]int, len(entries))
	for i, e := range entries {
		// First wins. Ghidra permits two symbols with one name (finding 35),
		// and the alternative to picking one is refusing to resolve a name the
		// user can see on the screen.
		if _, seen := byName[e.Name]; !seen {
			byName[e.Name] = i
		}
	}
	s.decomp.index, s.decomp.indexBy, s.decomp.indexFor = entries, byName, client
	return entries, byName
}

// forgetDecompIndex drops the index after something that can change a name.
//
// An edit renames exactly one thing, so rebuilding the whole index for it is
// more work than the edit was. It is still the right trade: the index is read
// on a keystroke and written by a human action, the rebuild is one round trip
// to a warm sidecar, and an index that lies about a name is worse than one that
// takes a moment to come back.
func (s *Session) forgetDecompIndex() {
	s.decomp.index, s.decomp.indexBy, s.decomp.indexFor = nil, nil, nil
}

// decompSymbols is the decompiler's contribution to the symbol pane.
//
// have reports which names the binary's own table already carries: a program
// with symbols has no use for a second entry saying the same thing, and a name
// that appears in both is the binary's, because that one is a fact rather than
// a recovery. Everything left is what the binary could not tell you.
func (s *Session) decompSymbols(r *request, filter, kind string, have map[string]bool) []wire.Symbol {
	entries, _ := s.decompIndex(r)
	if len(entries) == 0 {
		return nil
	}
	bias, ok := s.decompRuntimeBias(r)
	if !ok {
		// Rather than a list of link-time addresses labelled as runtime ones.
		// symbols.list is answerable while the inferior runs — the table is a
		// property of the file — but the *bias* is not: establishing it needs a
		// gdb round trip, and those are refused while the program is running.
		// A pane that goes quiet for the duration is a smaller lie than one
		// full of addresses that are off by the load offset.
		return nil
	}

	needle := strings.ToLower(strings.TrimSpace(filter))
	out := make([]wire.Symbol, 0, 64)
	for _, e := range entries {
		if kind != "" && e.Kind != kind {
			continue
		}
		if needle != "" && !strings.Contains(strings.ToLower(e.Name), needle) {
			continue
		}
		if have[e.Name] {
			continue
		}
		out = append(out, wire.Symbol{
			Name:    e.Name,
			Kind:    e.Kind,
			Type:    e.Type,
			Address: fmt.Sprintf("0x%x", uint64(int64(e.Addr)+bias)),
			From:    wire.SymbolFromDecompiler,
		})
	}
	return out
}

// decompIndexSize is how many names the index holds, without building it. The
// count goes into "200 of 4096" beside the search box, and a pane that has not
// been opened is not a reason to start a JVM.
func (s *Session) decompIndexSize() int { return len(s.decomp.index) }

// decompAnalysing reports that names are still on their way.
//
// It is what stops an empty pane from saying "this program has no symbols" to
// somebody whose firmware is four minutes into an import. True is a promise the
// pane will fill; false is not a promise that it will not, since the decompiler
// may be off entirely, and the pane says nothing in that case either way.
func (s *Session) decompAnalysing() bool {
	s.decomp.mu.Lock()
	defer s.decomp.mu.Unlock()
	return s.decomp.state == wire.DecompStarting || s.decomp.starting
}

// decompAddressOf resolves a decompiler name to the address gdb would use.
//
// The bias is re-established rather than cached with the entry, because a
// position-independent executable moves when it starts running: the same name
// answers 0x10e2dc before `run` and 0x5555555612dc after it, and both are
// right at the time.
func (s *Session) decompAddressOf(r *request, name string) (uint64, bool) {
	entry, ok := s.decompNamed(r, name)
	if !ok {
		return 0, false
	}
	bias, ok := s.decompRuntimeBias(r)
	if !ok {
		return 0, false
	}
	return uint64(int64(entry.Addr) + bias), true
}

// decompRuntimeBias is decompBias with the case it cannot answer separated out.
//
// A zero bias means two different things and they must not be confused. Before
// the program starts, gdb reports link-time addresses and zero is the right
// answer. Once there is an inferior, zero-with-no-anchor means the bias could
// not be worked out at all — and translating an address with it would hand back
// somewhere unmapped while reporting success.
func (s *Session) decompRuntimeBias(r *request) (int64, bool) {
	s.decomp.mu.Lock()
	client := s.decomp.client
	s.decomp.mu.Unlock()
	if client == nil {
		return 0, false
	}
	bias, biasFrom := s.decompBias(r, client)
	if bias != 0 || biasFrom != "" {
		return bias, true
	}
	started := s.st.runState == wire.RunStateStopped ||
		s.st.runState == wire.RunStateRunning
	return 0, !started
}

// plausibleDecompName is a cheap filter on what is worth asking the decompiler
// about at all.
//
// It exists to keep the index out of the way of gdb's own expression syntax: a
// go-to box takes "$pc", "&head" and "0x401136" as well as names, and none of
// those should reach a lookup. A Ghidra label is an identifier — letters,
// digits and underscores, not starting with a digit — so anything else is
// somebody's expression and not a name this could know.
func plausibleDecompName(s string) bool {
	if s == "" {
		return false
	}
	for i, c := range s {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c == '_':
		case c >= '0' && c <= '9':
			if i == 0 {
				return false
			}
		default:
			return false
		}
	}
	return true
}
