package debugger

import (
	"context"
	"crypto/sha256"
	"debug/elf"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/retrocpugeek/gdb-wui/internal/ghidra"
	"github.com/retrocpugeek/gdb-wui/internal/wire"
)

// Decompilation.
//
// The decompiler is a separate resource with its own lifetime, and is not part
// of the session's state machine. It lives behind its own mutex rather than in
// s.st, which keeps a cold start — seconds for an existing project, minutes
// when a binary has to be analysed — off the actor
// goroutine. Blocking the actor for a minute would freeze stepping,
// breakpoints and the console, which is a poor trade for a view.
//
// Once running, a decompile is 100-200ms and happens on the actor like any
// other round trip. That is consistent with disasm.function, which blocks the
// actor on gdb for comparable time.

// DecompConfig describes where Ghidra is and what it should open.
type DecompConfig struct {
	// Install is the located Ghidra. Nil disables the feature entirely, which
	// is an ordinary state and not an error.
	Install *ghidra.Install
	// ProjectDir and ProjectName address an existing Ghidra project. Program
	// names one program inside it — required, because a real project holds
	// several programs and, in the Debugger workflow, a pile of traces too.
	ProjectDir  string
	ProjectName string
	Program     string
	// CacheRoot is where gdb-wui creates its own Ghidra projects when no
	// existing one is configured. Keyed by the executable's sha256 inside, so
	// a restart reuses the analysis rather than paying for it again — that is
	// 71 seconds on a 2 MB firmware image.
	CacheRoot string
	// Analysis is how much of the binary Ghidra should analyse at import. The
	// zero value is ghidra.AnalysisAuto, which measures it and decides. Only
	// consulted for a project gdb-wui imports itself: one the user named has
	// already been analysed however they chose.
	Analysis ghidra.Analysis
	// Symbols names a file of `address [type] name` lines to import with the
	// binary, for an image whose own symbols are gone. Same restriction as
	// Analysis: a project the user named is theirs.
	Symbols string
	// Binary is the file to reverse, when it is not the one gdb loaded. An
	// emulated kernel is the case: gdb takes an ELF or nothing, and a raw
	// Image carved out of firmware is neither, so without this the decompiler
	// never starts at all. It also covers the ordinary mismatch, where gdb has
	// the stripped build and the full one is on disk beside it.
	Binary string
	// Processor is the Ghidra language ID for Binary — `ARM:LE:32:v7`.
	// Required with Base.
	Processor string
	// Base is where Binary is loaded, for a raw image with no format to say so
	// itself. It is also the entire mapping between gdb's addresses and
	// Ghidra's: a raw image has no symbols and no entry point to anchor a bias
	// on, so what is given here has to be the address the code actually runs
	// at. For a kernel that is the link address, and only once the MMU is on.
	Base string
}

// projectSuffix keeps the kinds of project apart in the cache.
//
// Empty for a fully analysed one with no symbols added, so that every project
// imported before any of this existed is still found where it was left. The
// rest carry what made them: an unanalysed project and an analysed one are
// different artefacts, and handing back the wrong one would leave a flag doing
// nothing at all, with no diff and no message to say why.
func projectSuffix(mode ghidra.Analysis, symbols, processor, base string) string {
	suffix := ""
	switch mode {
	case ghidra.AnalysisNone:
		suffix = "-noanalysis"
	case ghidra.AnalysisLean:
		suffix = "-lean"
	}
	if base != "" {
		// The same bytes at a different address are a different program:
		// every function in it is named after where it sits, and handing back
		// the one built at the old base would answer every lookup wrongly.
		suffix += "-at" + strings.TrimPrefix(strings.ToLower(base), "0x")
	}
	if processor != "" {
		// A language is a reading of the bytes, so it keys the project too.
		// Punctuation out, because this becomes a directory name.
		suffix += "-" + strings.Map(func(r rune) rune {
			if r == ':' || r == '/' || r == filepath.Separator {
				return '-'
			}
			return r
		}, strings.ToLower(processor))
	}
	if symbols != "" {
		// Keyed on the contents rather than the path: a symbol file that has
		// been regenerated is a different symbol file.
		if sum := hashFile(symbols); sum != "" {
			suffix += "-sym" + sum[:8]
		}
	}
	return suffix
}

// hashFile is the sha256 of a file, or "" if it cannot be read. Used for cache
// keys, where not knowing has to mean "do not claim a match".
func hashFile(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return ""
	}
	return hex.EncodeToString(h.Sum(nil))
}

// decompStartTimeout has to cover importing and analysing a binary, which is
// 71 seconds for a 2 MB firmware image and longer for anything bigger.
const decompStartTimeout = 20 * time.Minute

// decompLog puts one line about the decompiler in front of the user.
//
// emitOffActor because almost every caller is the background start goroutine
// or the process watcher, neither of which may touch session state. The two
// callers that do run on the actor are broadcasting the same shape, and the
// snapshot has nothing decompiler-shaped in it to publish, so the off-actor
// form is right for both.
func (s *Session) decompLog(level, format string, args ...any) {
	s.decompLogTimed(level, 0, format, args...)
}

func (s *Session) decompLogTimed(level string, ms int64, format string, args ...any) {
	text := fmt.Sprintf(format, args...)
	// The server's own log gets it too. A browser that was not connected when
	// something failed is the commonest way to lose the one line that explains
	// a failure.
	s.logf("decomp: %s", text)
	s.emitOffActor(wire.EventDecompLog, wire.DecompLog{
		Text: text, Level: level, Millis: ms,
	})
}

// decomp holds the decompiler and its state, under its own lock.
type decomp struct {
	mu    sync.Mutex
	state string
	err   string
	// client is non-nil only in DecompReady.
	client *ghidra.Client
	// starting guards against a second start while one is in flight.
	starting bool
	// writable is true only for a project gdb-wui imported itself, and it is
	// what permits decomp.rename and decomp.retype. A project the user pointed
	// at with -ghidra-project holds their own names, types and comments, and is
	// never written to.
	//
	// Read on the actor and written by the start goroutine, so it lives under
	// the same lock as the client it describes.
	writable bool
	// journal is the inverse of every edit made this session, most recent last,
	// which is how undo works. Ghidra's own undo cannot be used: saving clears
	// it (finding 33) and every edit is saved.
	//
	// Actor-only. Unlike the fields above, nothing off the actor touches it.
	journal []decompUndo
	// runSeq names the groups the journal is undone in. Actor-only as well.
	runSeq uint64
	// closed means the session is shutting down. A start already in flight
	// will finish and hand back a live process afterwards; without this it
	// would assign it to a client nobody is left to close, orphaning a JVM.
	closed bool
	// biasFrom names the symbol the bias is established from, and biasAddr is
	// that symbol's Ghidra address. The numeric bias is not cached, because a
	// position-independent executable relocates when it starts
	// running, so a bias computed before `run` is wrong after it — measured,
	// by watching a decompiled entry stay at its link-time 0x11e9 while gdb
	// had moved the program to 0x5555555551e9. Only the choice of symbol is
	// stable; its address has to be asked for again.
	biasFrom string
	biasAddr uint64
	// index is every name the decompiler knows — its functions and its
	// module-scope labels — in Ghidra's coordinates, with indexBy mapping a
	// name to its position, indexAt mapping a label's address to it, and
	// indexOrder listing the labels that have a known extent, in address order,
	// for the searches that ask what an address falls inside. indexFor is the
	// client it was read from,
	// which is what makes a restarted decompiler rebuild rather than answer
	// from a previous program's names. See decompindex.go.
	//
	// Actor-only, like the journal: built and read on the request path.
	index      []decompEntry
	indexBy    map[string]int
	indexAt    map[uint64]int
	indexOrder []int
	indexFor   *ghidra.Client
}

// decompStatus answers what the pane can offer right now, and starts the
// decompiler if this is the first time anyone asked.
//
// Starting on first ask rather than at session start is deliberate: most
// sessions never open the pane, and a 2 GB JVM that nobody wanted is a rude
// thing to spawn.
func (s *Session) decompStatus(r *request) (any, *wire.Error) {
	cfg := s.cfg.Decomp
	if cfg.Install == nil {
		return wire.DecompStatus{
			State: wire.DecompOff,
			Error: "no Ghidra installation configured; pass -ghidra or set " +
				ghidra.EnvInstall,
		}, nil
	}

	s.maybeStartDecomp()

	s.decomp.mu.Lock()
	state, errText, client := s.decomp.state, s.decomp.err, s.decomp.client
	writable := s.decomp.writable
	s.decomp.mu.Unlock()

	if state == "" {
		state = wire.DecompOff
	}
	out := wire.DecompStatus{
		State:         state,
		Error:         errText,
		GhidraVersion: cfg.Install.Version,
		Editable:      writable,
		// What one undo would reverse. Sent unasked because the client has to
		// offer it before anything goes wrong: an agent that has just written
		// forty annotations is exactly when "undo all of that" is wanted, and
		// asking for the journal's shape first would be a round trip nobody
		// makes until they already regret something.
		Undo: s.topRun(),
	}
	if client == nil {
		return out, nil
	}
	ready := client.Ready()
	out.FunctionCount = ready.FunctionCount
	out.Program = &wire.DecompProgram{
		Name:        ready.Program.Name,
		SHA256:      ready.Program.SHA256,
		LanguageID:  ready.Program.LanguageID,
		ImageBase:   ready.Program.ImageBase,
		PointerSize: ready.Program.PointerSize,
	}
	// The mismatch guard. Two builds of one program share every address, so
	// this is a warning rather than a refusal — but reading a decompilation of
	// a different build than the one being debugged is a confidently wrong
	// answer, and the user has to be told which they are looking at.
	// Not when the binary was named: the two files being different is then the
	// point of the flag, and warning about it would fire on every start.
	if want := s.st.exeSHA256; s.cfg.Decomp.Binary == "" &&
		want != "" && ready.Program.SHA256 != "" &&
		!strings.EqualFold(want, ready.Program.SHA256) {
		out.Mismatch = fmt.Sprintf(
			"the decompiler has %s (sha256 %s), but gdb has loaded a different build (%s)",
			ready.Program.Name, short(ready.Program.SHA256), short(want))
	}
	return out, nil
}

// maybeStartDecomp kicks off a start in the background, at most once.
//
// Called from the actor, so it may read session state; the goroutine it
// launches may not, and everything it needs is captured first.
func (s *Session) maybeStartDecomp() {
	cfg := s.cfg.Decomp
	if cfg.Install == nil {
		return
	}
	// Once one is running, starting or has failed there is nothing to do, and
	// that is worth knowing before the work below rather than only after it:
	// naming the project hashes the binary, which is tens of milliseconds on a
	// kernel image, and decomp.names asks on every stop. Only the actor calls
	// this, so the locked check further down is the same answer taken again
	// where it decides something.
	s.decomp.mu.Lock()
	busy := s.decomp.closed || s.decomp.starting ||
		s.decomp.state == wire.DecompReady || s.decomp.state == wire.DecompFailed
	s.decomp.mu.Unlock()
	if busy {
		return
	}
	// With no configured project, decompile whatever gdb has loaded. Resolved
	// here rather than at startup because the executable is usually chosen
	// after the session begins.
	//
	// This branch is also the only one that may be edited: the project below is
	// one gdb-wui imported and owns, keyed on the binary's hash. A project the
	// user named is theirs.
	var importPath string
	// mode and why are set with importPath, and only mean anything with it.
	mode, why := ghidra.AnalysisFull, ""
	writable := cfg.ProjectDir == ""
	if cfg.ProjectDir == "" {
		// Normally the binary is whatever gdb loaded. A configured one wins,
		// and is the only route in when gdb has no file at all: attached to an
		// emulator running a raw kernel Image, there is nothing gdb will take.
		abs, key := cfg.Binary, ""
		if abs != "" {
			key = hashFile(abs)
			if key == "" {
				// Checked at startup too, so this is the file having gone
				// since. Failed rather than a bare return, which would ask
				// again — and log again — on every stop.
				err := fmt.Sprintf("cannot read %s", abs)
				s.decomp.mu.Lock()
				s.decomp.state = wire.DecompFailed
				s.decomp.err = err
				s.decomp.mu.Unlock()
				s.decompLog(wire.DecompLogError, "%s", err)
				return
			}
		} else {
			if s.st.exePath == "" || s.st.exeSHA256 == "" || s.files == nil {
				return
			}
			var err error
			abs, err = s.files.AbsPath(s.st.exePath)
			if err != nil {
				return
			}
			key = s.st.exeSHA256
		}
		root := cfg.CacheRoot
		if root == "" {
			return
		}
		// Checked here as well as at startup: a caller of the library could
		// hand us anything, and Ghidra's own refusal names neither the path
		// nor the reason and arrives a JVM start later.
		if err := ghidra.CheckProjectPath(root); err != nil {
			s.decomp.mu.Lock()
			s.decomp.state = wire.DecompFailed
			s.decomp.err = err.Error()
			s.decomp.mu.Unlock()
			s.decompLog(wire.DecompLogError, "%v", err)
			return
		}
		// Resolved before the project is named, because the name has to
		// carry it: an unanalysed project and an analysed one are different
		// artefacts, and reusing the first when the second was asked for
		// would leave -ghidra-analysis=full doing nothing at all, with no
		// diff and no message to say why.
		mode, why = cfg.Analysis.ResolveFor(abs, cfg.Base != "")
		if cfg.Symbols != "" && mode == ghidra.AnalysisLean {
			// Nothing to discover: the symbol file says where the functions
			// are, which is the whole reason the lean analysis was going to be
			// run. Falling back to no analysis at all makes the import
			// seconds instead of a minute and a half.
			mode, why = ghidra.AnalysisNone, why+", but the symbol file names them"
		}
		// Keyed on the hash, not the path: a rebuilt binary must not be served
		// a stale analysis of its predecessor.
		cfg.ProjectDir = filepath.Join(root,
			key[:16]+projectSuffix(mode, cfg.Symbols, cfg.Processor, cfg.Base))
		cfg.ProjectName = "gdb-wui"
		cfg.Program = filepath.Base(abs)
		if _, err := os.Stat(filepath.Join(cfg.ProjectDir, cfg.ProjectName+".gpr")); err != nil {
			// No project yet, so this run pays for the analysis.
			if err := os.MkdirAll(cfg.ProjectDir, 0o755); err != nil {
				s.logf("decompilation: %v", err)
				return
			}
			importPath = abs
		}
	}
	s.decomp.mu.Lock()
	if s.decomp.closed || s.decomp.starting || s.decomp.state == wire.DecompReady ||
		s.decomp.state == wire.DecompFailed {
		s.decomp.mu.Unlock()
		return
	}
	s.decomp.starting = true
	s.decomp.state = wire.DecompStarting
	s.decomp.mu.Unlock()

	go func() {
		// Said on every start rather than only on the one that imports, and
		// before anything slow: which kind of project is open decides what the
		// tab can show, and the answer should not depend on having watched the
		// log the day it was built.
		if mode != ghidra.AnalysisFull {
			if why != "" {
				s.decompLog(wire.DecompLogInfo, "%s", why)
			}
			verb := "opening"
			if importPath != "" {
				verb = "importing"
			}
			switch mode {
			case ghidra.AnalysisNone:
				s.decompLog(wire.DecompLogInfo,
					"%s %s without analysis: each function is disassembled as it is opened, "+
						"and there are no cross-references or recovered parameter types",
					verb, cfg.Program)
			case ghidra.AnalysisLean:
				s.decompLog(wire.DecompLogInfo,
					"%s %s with the analyzers that find functions and not the ones that "+
						"cost the memory: expect most of the functions, named after their "+
						"addresses", verb, cfg.Program)
			}
		}
		if cfg.Binary != "" {
			// Which file is on screen is not deducible from anything else once
			// it is no longer the one gdb loaded, and for a raw image the base
			// decides whether every address lines up or none of them do.
			if cfg.Base != "" {
				s.decompLog(wire.DecompLogInfo, "reversing %s as raw %s loaded at %s",
					filepath.Base(cfg.Binary), cfg.Processor, cfg.Base)
			} else {
				s.decompLog(wire.DecompLogInfo, "reversing %s, which is not what gdb loaded",
					filepath.Base(cfg.Binary))
			}
		}
		if cfg.Symbols != "" {
			s.decompLog(wire.DecompLogInfo, "naming functions from %s",
				filepath.Base(cfg.Symbols))
		}
		// Import first, when there is no project yet. It has to be its own
		// invocation: analyzeHeadless commits an imported program only after
		// the postScript returns, and the resident server never returns, so
		// importing and serving together leaves an empty project behind.
		if importPath != "" {
			if mode == ghidra.AnalysisFull {
				s.decompLog(wire.DecompLogInfo,
					"importing %s — analysis is seconds for a small binary and minutes for firmware",
					filepath.Base(importPath))
			}
			started := time.Now()
			if err := ghidra.Import(context.Background(), cfg.Install,
				cfg.ProjectDir, cfg.ProjectName, importPath,
				ghidra.ImportOptions{
					Analysis:  mode,
					Symbols:   cfg.Symbols,
					Processor: cfg.Processor,
					Base:      cfg.Base,
				},
				s.ghidraProcessLog); err != nil {
				s.decompLog(wire.DecompLogError, "import failed: %v", err)
				s.decomp.mu.Lock()
				s.decomp.starting = false
				s.decomp.state = wire.DecompFailed
				s.decomp.err = err.Error()
				s.decomp.mu.Unlock()
				s.emitOffActor(wire.EventDecompChanged, map[string]any{})
				return
			}
			s.decompLogTimed(wire.DecompLogInfo, time.Since(started).Milliseconds(),
				"imported %s", filepath.Base(importPath))
		} else {
			s.decompLog(wire.DecompLogInfo, "opening %s (%s) read-only",
				cfg.Program, cfg.ProjectName)
		}
		startedAt := time.Now()
		client, err := ghidra.Start(context.Background(), ghidra.Options{
			Install:     cfg.Install,
			ProjectDir:  cfg.ProjectDir,
			ProjectName: cfg.ProjectName,
			Program:     cfg.Program,
			Writable:    writable,
			Timeout:     decompStartTimeout,
			Logf:        s.ghidraProcessLog,
		})
		s.decomp.mu.Lock()
		s.decomp.starting = false
		closed := s.decomp.closed
		switch {
		case err != nil:
			s.decomp.state = wire.DecompFailed
			s.decomp.err = err.Error()
		case closed:
			// The session shut down while this was starting. Nobody is left to
			// close it, so close it here rather than leave a 2 GB JVM behind.
			s.decomp.state = wire.DecompOff
		default:
			s.decomp.state = wire.DecompReady
			s.decomp.err = ""
			s.decomp.client = client
			s.decomp.writable = writable
		}
		s.decomp.mu.Unlock()

		switch {
		case err != nil:
			s.decompLog(wire.DecompLogError, "failed to start: %v", err)
		case closed:
			s.decompLog(wire.DecompLogInfo, "discarded: the session closed while it was starting")
		default:
			r := client.Ready()
			s.decompLogTimed(wire.DecompLogInfo, time.Since(startedAt).Milliseconds(),
				"ready — %s, %s, %d functions", r.Program.Name,
				r.Program.LanguageID, r.FunctionCount)
		}

		if closed && client != nil {
			_ = client.Close()
			return
		}

		// Off-actor: this goroutine must not touch session state, and the
		// snapshot has nothing decompiler-shaped in it to publish.
		s.emitOffActor(wire.EventDecompChanged, map[string]any{})

		if err == nil {
			// Notice the process dying so the pane stops claiming to work.
			go s.watchDecomp(client)
		}
	}()
}

func (s *Session) watchDecomp(client *ghidra.Client) {
	dead, reason := client.Dead()
	<-dead
	s.decomp.mu.Lock()
	if s.decomp.client == client {
		s.decomp.client = nil
		s.decomp.state = wire.DecompFailed
		s.decomp.err = reason().Error()
		s.decomp.biasFrom, s.decomp.biasAddr = "", 0
		s.decomp.mu.Unlock()
		s.decompLog(wire.DecompLogError, "died: %v", reason())
		s.emitOffActor(wire.EventDecompChanged, map[string]any{})
		return
	}
	s.decomp.mu.Unlock()
	s.emitOffActor(wire.EventDecompChanged, map[string]any{})
}

// closeDecomp stops the decompiler. A 2 GB JVM outliving the session is not
// acceptable.
func (s *Session) closeDecomp() {
	s.decomp.mu.Lock()
	client := s.decomp.client
	s.decomp.client = nil
	s.decomp.closed = true
	s.decomp.state = wire.DecompOff
	s.decomp.mu.Unlock()
	if client != nil {
		_ = client.Close()
	}
}

func (s *Session) decompFunction(r *request) (any, *wire.Error) {
	req, werr := decode[wire.DecompFunctionRequest](r.req.Payload)
	if werr != nil {
		return nil, werr
	}

	s.decomp.mu.Lock()
	client, state, errText := s.decomp.client, s.decomp.state, s.decomp.err
	s.decomp.mu.Unlock()

	if client == nil {
		s.maybeStartDecomp()
		msg := "the decompiler is not ready"
		switch state {
		case wire.DecompStarting:
			msg = "the decompiler is still starting"
		case wire.DecompFailed:
			msg = "the decompiler failed: " + errText
		case "", wire.DecompOff:
			msg = "no decompiler is configured"
		}
		return nil, wire.NewError(wire.CodeNotReady, msg)
	}

	target := strings.TrimSpace(req.Target)
	if target == "" {
		// Follow the selected frame. This is what the pane wants on a stop,
		// and it is the reason Decompile accepts an interior address: a
		// program counter is rarely a function entry.
		frame := req.Frame
		if frame == 0 {
			frame = s.st.selFrame
		}
		f, ok := s.frameAt(frame)
		if !ok || f.Address == "" {
			return nil, wire.NewError(wire.CodeNotReady, "no frame to decompile")
		}
		target = f.Address
	}

	bias, biasFrom := s.decompBias(r, client)

	// A runtime address has to go back into Ghidra's coordinates before the
	// decompiler will recognise it. Names pass through untouched.
	lookup := target
	if n, err := parseAddress(target); err == nil {
		ghidraAddr := uint64(int64(n) - bias)
		// An address in a different module is the commonest reason a lookup
		// fails, and Ghidra's answer for it — "no function 0x111b900" — names
		// a translated address the user never saw and does not explain that
		// the decompiler simply does not have that code. Stopping in the
		// dynamic loader is enough to produce it.
		if lo, hi, ok := s.exeImageRange(client); ok && (ghidraAddr < lo || ghidraAddr > hi) {
			name := client.Ready().Program.Name
			return nil, wire.NewError(wire.CodeBadRequest, fmt.Sprintf(
				"%s is not inside %s, which is the only program the decompiler has. "+
					"It is in a shared library or the dynamic loader.", target, name))
		}
		lookup = fmt.Sprintf("0x%x", ghidraAddr)
	}

	started := time.Now()
	fn, err := client.Decompile(r.ctx, lookup)
	if err != nil {
		var gerr *ghidra.Error
		if ok := asGhidraError(err, &gerr); ok {
			s.decompLog(wire.DecompLogWarn, "decompile %s: %s", target, gerr.Msg)
			return nil, wire.NewError(wire.CodeBadRequest, gerr.Msg)
		}
		s.decompLog(wire.DecompLogError, "decompile %s: %v", target, err)
		return nil, wire.NewError(wire.CodeInternal, err.Error())
	}

	out := s.renderDecomp(fn, bias, biasFrom, s.currentPC())
	mapped := 0
	for _, l := range out.Lines {
		mapped += len(l.Addrs)
	}
	s.decompLogTimed(wire.DecompLogInfo, time.Since(started).Milliseconds(),
		"decompiled %s — %d lines, %d mapped addresses, %d variables",
		out.Name, out.LineCount(), mapped, len(out.Vars))
	return out, nil
}

// renderDecomp projects a Ghidra function onto the wire, applying the bias so
// every address a client sees is one gdb would print.
func (s *Session) renderDecomp(fn *ghidra.Function, bias int64, biasFrom, pc string) wire.DecompFunction {
	out := wire.DecompFunction{
		Name:      fn.Name,
		Signature: fn.Signature,
		Source:    decompSource(fn.Source),
		Entry:     shiftAddr(fn.Entry, bias),
		BodyStart: shiftAddr(fn.BodyStart, bias),
		BodyEnd:   shiftAddr(fn.BodyEnd, bias),
		Text:      fn.Text,
		Bias:      bias,
		BiasFrom:  biasFrom,
		Frame:     &wire.DecompFrame{Size: fn.Frame.Size, GrowsNegative: fn.Frame.GrowsNegative},
	}
	for _, l := range fn.Lines {
		shifted := make([]string, 0, len(l.Addrs))
		for _, a := range l.Addrs {
			shifted = append(shifted, shiftAddr(a, bias))
		}
		out.Lines = append(out.Lines, wire.DecompLine{N: l.N, Addrs: shifted})
	}
	for _, c := range fn.CommentLines {
		out.CommentLines = append(out.CommentLines, wire.DecompCommentLine{
			N: c.N, Addr: shiftAddr(c.Addr, bias),
		})
	}
	for _, c := range fn.Comments {
		out.Comments = append(out.Comments, wire.DecompComment{
			Addr:   shiftAddr(c.Addr, bias),
			Kind:   c.Kind,
			Text:   c.Text,
			Author: c.Author,
		})
	}
	out.PCLine, out.PCLineAmbiguous, out.PCLineApprox =
		pcLine(out.Lines, pc, out.BodyStart, out.BodyEnd)

	for _, v := range fn.Variables {
		out.Vars = append(out.Vars, wire.DecompVar{
			Name:    v.Name,
			ID:      v.ID,
			Type:    v.Type,
			Source:  decompSource(v.Source),
			Param:   v.Param,
			Storage: storageKind(v.Storage.Kind),
			Expr:    varExpr(v, fn.Frame, s.decompLanguage()),
			PC:      shiftAddr(v.PC, bias),
		})
	}
	// Globals last, and they are the readable ones: a fixed address is valid at
	// every pc and needs no frame. Addressed by number rather than by name,
	// because in a stripped image Ghidra's name for one is DAT_<address>,
	// which gdb has never heard of.
	for _, g := range fn.Globals {
		addr := shiftAddr(g.Address, bias)
		out.Vars = append(out.Vars, wire.DecompVar{
			Name:    g.Name,
			Type:    g.Type,
			Storage: wire.DecompStorageGlobal,
			Expr:    fmt.Sprintf("*(%s *)%s", gdbCType(g.Type, g.Size), addr),
			PC:      "",
			Addr:    addr,
		})
	}
	return out
}

// decompSource turns Ghidra's vocabulary into the protocol's.
//
// Not one-to-one, and deliberately: Ghidra's ANALYSIS covers an agent's guess
// and its own demangler alike, so it becomes "inferred" rather than "an agent
// named this". Claiming the stronger reading would credit one guess to whoever
// last ran the other.
func decompSource(source string) string {
	switch source {
	case ghidra.SourceUser:
		return wire.DecompSourceUser
	case ghidra.SourceAnalysis:
		return wire.DecompSourceInferred
	case ghidra.SourceImported:
		return wire.DecompSourceSymbol
	case ghidra.SourceDefault:
		return wire.DecompSourceGhidra
	}
	// No symbol behind the name at all: the decompiler invented it.
	return wire.DecompSourceNone
}

// pcLine finds the line the program counter is on.
//
// Resolved here rather than in every client, because neither of the two rules
// is obvious.
//
// The tie-break: on optimised code about one address in five is claimed by two
// lines, and the answer is the lowest line number that claims it. The
// ambiguity is reported rather than hidden — it is the same imprecision as
// stepping through -O2 code with DWARF.
//
// The fallback: plenty of addresses are claimed by no line at all. A prologue,
// a register spill and an epilogue belong to no expression, and stepping lands
// on them constantly — measured on a hello-world, stepping off the last
// statement of a function lands on 0x1248 when the nearest mapped addresses
// are 0x1243 and 0x124d. Reporting "no line" there is accurate and useless: it
// makes the highlight blink out mid-step. So the nearest preceding line is
// used instead, flagged approximate, which a client shows differently.
//
// The prologue is the case that made this visible. `break process_packet` on
// the vwfw firmware lands at 0x120007ef8 — gdb skips the prologue for a named
// function — while the lowest address any decompiled line claims is
// 0x120007f28, seventy-two bytes further in. A "nearest preceding" rule finds
// nothing there and the marker vanishes at exactly the moment a breakpoint is
// hit, which is the moment a user is most certainly looking. Below everything
// mapped, the first mapped line is the answer.
//
// body bounds both fallbacks. Without it, asking for a function other than the
// one the program is stopped in would mark its first line, asserting the
// program is somewhere it is not.
func pcLine(lines []wire.DecompLine, pc, bodyStart, bodyEnd string) (n int, ambiguous, approx bool) {
	if pc == "" {
		return 0, false, false
	}
	want, err := parseAddress(pc)
	if err != nil {
		return 0, false, false
	}
	best, count := 0, 0
	// nearest tracks the greatest mapped address at or below the pc; first
	// tracks the lowest mapped address anywhere, for the prologue case.
	nearestLine, nearestAddr, haveNearest := 0, uint64(0), false
	firstLine, firstAddr, haveFirst := 0, uint64(0), false
	for _, l := range lines {
		var claims bool
		for _, a := range l.Addrs {
			v, err := parseAddress(a)
			if err != nil {
				continue
			}
			if v == want {
				claims = true
			}
			if v < want && (!haveNearest || v > nearestAddr ||
				(v == nearestAddr && l.N < nearestLine)) {
				nearestLine, nearestAddr, haveNearest = l.N, v, true
			}
			if !haveFirst || v < firstAddr || (v == firstAddr && l.N < firstLine) {
				firstLine, firstAddr, haveFirst = l.N, v, true
			}
		}
		if claims {
			count++
			if best == 0 || l.N < best {
				best = l.N
			}
		}
	}
	if best != 0 {
		return best, count > 1, false
	}
	// Only guess for a pc inside this function.
	if !within(want, bodyStart, bodyEnd) {
		return 0, false, false
	}
	if haveNearest {
		return nearestLine, false, true
	}
	if haveFirst {
		// Still in the prologue: below every mapped address.
		return firstLine, false, true
	}
	return 0, false, false
}

// within reports whether an address falls inside a function body. An
// unparseable bound is treated as no bound, because refusing to mark anything
// is a worse failure than marking approximately.
func within(addr uint64, start, end string) bool {
	lo, err1 := parseAddress(start)
	hi, err2 := parseAddress(end)
	if err1 != nil || err2 != nil {
		return true
	}
	return addr >= lo && addr <= hi
}

// storageKind collapses Ghidra's kinds onto what a client can act on. A
// decompiler temporary and an unrecognised storage are the same to a UI: there
// is no value to show, and saying so is better than omitting the row.
func storageKind(k string) string {
	switch k {
	case ghidra.StorageStack:
		return wire.DecompStorageStack
	case ghidra.StorageRegister:
		return wire.DecompStorageRegister
	default:
		return wire.DecompStorageNone
	}
}

// varExpr turns Ghidra's storage into a gdb expression.
//
// Ghidra's frame base is the stack pointer at function entry, so a stack
// variable is always at entry_sp + offset. Only recovering entry_sp is
// per-ABI, and each rule below was established by measurement — see
// docs/decompilation.md. An architecture with no established rule gets no
// expression rather than a guess, because a wrong address reads as a value.
func varExpr(v ghidra.Var, frame ghidra.Frame, lang string) string {
	switch v.Storage.Kind {
	case ghidra.StorageRegister:
		if v.Storage.Register == "" {
			return ""
		}
		// Valid only near v.PC — the decompiler packs many variables into one
		// register. The caveat travels with the row as Storage and PC rather
		// than being enforced here, so a client can show it instead of
		// silently having nothing to render.
		return "$" + strings.ToLower(v.Storage.Register)
	case ghidra.StorageStack:
		ctype := gdbCType(v.Type, v.Size)
		var base string
		var delta int
		switch {
		case strings.HasPrefix(lang, "x86"), strings.HasPrefix(lang, "ARM"),
			strings.HasPrefix(lang, "AARCH64"), strings.HasPrefix(lang, "PowerPC"),
			strings.HasPrefix(lang, "MIPS"):
			// One rule, on every architecture measured: the address is
			// entry_sp + offset, and entry_sp is wherever the prologue left
			// the stack pointer, so entry_sp = $sp - spDepth.
			//
			// The prologues share nothing. 32-bit ARM pushes and then
			// subtracts a constant; AArch64 writes `stp x29, x30, [sp, #-16]!`
			// and then subtracts a register; PowerPC does the whole thing in
			// one `stwu r1,-48(r1)`, or two `stdu`s for a large frame; MIPS in
			// one `addiu sp,sp,-16`; x86 pushes a return address before the
			// function is even entered. The depth accounts for all of them,
			// because it is measured from the same place the offsets are.
			//
			// The two rules this replaced were both right about the binary
			// they were measured on and wrong in general. `$rbp + pointerSize`
			// on x86 holds only where there is a frame pointer, and `-O2`
			// omits one: measured on gcc 15's `-O2` output, the addresses came
			// out 192 bytes into the *caller's* frame. `frame.Size` on MIPS
			// holds only where the frame's lowest slot is one something
			// touches, because the size is derived from the variables Ghidra
			// found rather than from the prologue; on gcc's output it was out
			// by four bytes on 32-bit and twelve on 64-bit. Neither failure
			// looks like a failure — both read as a value.
			//
			// A positive offset is ordinary rather than a mistake: the
			// PowerPC, MIPS and i386 ABIs pass or spill arguments in the
			// *caller's* frame, so a parameter sits at or above entry_sp while
			// the locals are below it. A negative delta is ordinary too —
			// x86-64's red zone puts a leaf function's locals below its own
			// stack pointer. The arithmetic is the same for both.
			//
			// No depth means Ghidra could not settle on one, and a frame whose
			// stack pointer moves under it has no static expression to give.
			if frame.SPDepth == nil {
				return ""
			}
			base, delta = "$sp", v.Storage.Offset-*frame.SPDepth
		default:
			return ""
		}
		sign := "+"
		if delta < 0 {
			sign, delta = "-", -delta
		}
		return fmt.Sprintf("*(%s *)(%s %s 0x%x)", ctype, base, sign, delta)
	default:
		return ""
	}
}

// ghidraPrimitives maps the decompiler's spelling of a base type onto one gdb
// will parse. Ghidra's names are its own: `undefined4`, `uint`, `qword` and
// friends mean nothing to a C expression parser, and neither does the name of
// a struct Ghidra invented.
var ghidraPrimitives = map[string]string{
	"char": "char", "uchar": "unsigned char", "sbyte": "signed char",
	"byte": "unsigned char", "bool": "unsigned char",
	"short": "short", "ushort": "unsigned short",
	"word": "unsigned short", "sword": "short",
	"int": "int", "uint": "unsigned int",
	"dword": "unsigned int", "sdword": "int",
	"long": "long", "ulong": "unsigned long",
	"qword": "unsigned long", "sqword": "long",
	"longlong": "long long", "ulonglong": "unsigned long long",
	"float": "float", "double": "double", "void": "void",
	"undefined": "unsigned char", "undefined1": "unsigned char",
	"undefined2": "unsigned short", "undefined4": "unsigned int",
	"undefined8": "unsigned long", "code": "void",
	"size_t": "unsigned long", "ssize_t": "long",
}

// gdbCType turns a Ghidra type into one gdb can parse, or gives up gracefully.
//
// Measured against gdb 17.1: `*(config * *)(...)` and `*(undefined1 * *)(...)`
// both fail with "No symbol in current context", which is the whole of the
// decompiler's vocabulary for anything but the handful of names below. An
// expression that cannot be evaluated shows the user nothing, so this degrades
// instead:
//
//	a pointer to something unnameable becomes void *, which prints an address;
//	anything else unnameable becomes an unsigned integer of the right size,
//	which prints the bytes that are actually there.
//
// Both lose the type and neither loses the value, which is the right way round
// — the pane already says the types are a decompiler's guess.
func gdbCType(ghidraType string, size int) string {
	t := strings.TrimSpace(ghidraType)
	if t == "" {
		return sizedInt(size)
	}
	// Peel a trailing array suffix, then pointer stars.
	array := ""
	if i := strings.Index(t, "["); i >= 0 && strings.HasSuffix(t, "]") {
		array, t = t[i:], strings.TrimSpace(t[:i])
	}
	stars := 0
	for strings.HasSuffix(t, "*") {
		stars++
		t = strings.TrimSpace(strings.TrimSuffix(t, "*"))
	}

	base, ok := ghidraPrimitives[strings.ToLower(t)]
	if !ok {
		// A struct, union or enum Ghidra named. gdb has never heard of it.
		if stars > 0 {
			base = "void"
		} else {
			return sizedInt(size)
		}
	}
	return base + strings.Repeat(" *", stars) + array
}

// sizedInt names an unsigned integer of a given width, for a value whose type
// cannot be expressed. Zero or an odd width falls back to a pointer-sized one,
// which is the commonest slot.
func sizedInt(size int) string {
	switch size {
	case 1:
		return "unsigned char"
	case 2:
		return "unsigned short"
	case 4:
		return "unsigned int"
	default:
		return "unsigned long"
	}
}

// decompBias establishes what to add to a Ghidra address to get gdb's.
//
// Not computed from image bases: that arithmetic is right for a non-PIE and
// silently wrong for everything else. Instead a symbol both sides know is
// resolved through gdb and subtracted, which is the same reasoning the symbols
// pane uses when it jumps by name rather than by address.
//
// A stripped image has no such symbol — Ghidra's names are FUN_<address>,
// which gdb has never heard of — and there the answer is zero with biasFrom
// empty, so a client can say the addresses are link-time rather than implying
// they are runtime ones.
func (s *Session) decompBias(r *request, client *ghidra.Client) (int64, string) {
	s.decomp.mu.Lock()
	from, ghidraAddr := s.decomp.biasFrom, s.decomp.biasAddr
	s.decomp.mu.Unlock()

	// Re-resolve the known symbol: one gdb round trip, and correct across the
	// relocation that happens when a PIE starts running.
	if from == entryAnchor {
		// Re-read rather than cache the number: the entry point moves when a
		// position-independent executable is loaded, exactly as a symbol does.
		if runtimeEntry, ok := s.runtimeEntryPoint(r); ok {
			return int64(runtimeEntry) - int64(ghidraAddr), from
		}
		s.decomp.mu.Lock()
		s.decomp.biasFrom, s.decomp.biasAddr = "", 0
		s.decomp.mu.Unlock()
	} else if from != "" {
		if gdbAddr, werr := s.addressOfSymbol(r, from); werr == nil {
			return int64(gdbAddr) - int64(ghidraAddr), from
		}
		// It stopped resolving — a new program was loaded, say. Pick again.
		s.decomp.mu.Lock()
		s.decomp.biasFrom, s.decomp.biasAddr = "", 0
		s.decomp.mu.Unlock()
	}

	// No cached anchor. Try a shared symbol first, then the entry point.
	list, err := client.Functions(r.ctx, 0, 200, "")
	if err != nil {
		return 0, ""
	}
	tried := 0
	for _, f := range list.Functions {
		if f.Thunk || !plausibleSymbol(f.Name) {
			continue
		}
		if tried >= 8 {
			break
		}
		tried++
		addr, err := parseAddress(f.Entry)
		if err != nil {
			continue
		}
		gdbAddr, werr := s.addressOfSymbol(r, f.Name)
		if werr != nil {
			continue
		}
		s.decomp.mu.Lock()
		s.decomp.biasFrom, s.decomp.biasAddr = f.Name, addr
		s.decomp.mu.Unlock()
		return int64(gdbAddr) - int64(addr), f.Name
	}
	return s.biasFromEntryPoint(r, client)
}

// biasFromEntryPoint locates a program with no symbols at all.
//
// A stripped position-independent executable is the case the whole decompiled
// view exists for, and it defeats the symbol anchor completely: measured on a
// buildroot busybox, all 372 of its function symbols are undefined imports and
// not one is defined, so there is no name gdb and Ghidra share. Without an
// anchor the bias stayed zero and every lookup by address missed, which is
// what "no function 0x7f2fd396dc80" meant.
//
// The entry point is the anchor that always exists. Its link-time value is in
// the ELF header, which gdb-wui can read itself; its runtime value is what gdb
// prints in `info files`. Neither needs a symbol table.
func (s *Session) biasFromEntryPoint(r *request, client *ghidra.Client) (int64, string) {
	if s.files == nil || s.st.exePath == "" {
		return 0, ""
	}
	if s.cfg.Decomp.Base != "" {
		// A raw image is placed where the configuration said, and Ghidra
		// reports an image base of zero for it whatever the block start is.
		// Both halves of the sum below would be wrong, and the mapping is
		// already settled: what was given as the base is the runtime address.
		return 0, ""
	}
	abs, err := s.files.AbsPath(s.st.exePath)
	if err != nil {
		return 0, ""
	}
	f, err := elf.Open(abs)
	if err != nil {
		return 0, ""
	}
	defer f.Close()

	// Ghidra places the image at its own base, so a file address maps to a
	// Ghidra address by the difference between that base and the lowest
	// address the ELF asks to be loaded at. Zero for a non-relocatable image
	// Ghidra loaded where it was linked.
	var minVaddr uint64 = ^uint64(0)
	for _, p := range f.Progs {
		if p.Type == elf.PT_LOAD && p.Vaddr < minVaddr {
			minVaddr = p.Vaddr
		}
	}
	if minVaddr == ^uint64(0) {
		return 0, ""
	}
	base, err := parseAddress(client.Ready().Program.ImageBase)
	if err != nil {
		return 0, ""
	}
	ghidraEntry := f.Entry + (base - minVaddr)

	runtimeEntry, ok := s.runtimeEntryPoint(r)
	if !ok {
		return 0, ""
	}
	bias := int64(runtimeEntry) - int64(ghidraEntry)
	s.decomp.mu.Lock()
	s.decomp.biasFrom, s.decomp.biasAddr = entryAnchor, ghidraEntry
	s.decomp.mu.Unlock()
	return bias, entryAnchor
}

// exeImageRange is the span of the loaded executable, in Ghidra's addresses.
//
// Read from the ELF rather than asked of Ghidra, which reports where the image
// starts but not how far it goes.
func (s *Session) exeImageRange(client *ghidra.Client) (lo, hi uint64, ok bool) {
	if s.files == nil || s.st.exePath == "" || s.cfg.Decomp.Base != "" {
		// With a raw image the executable gdb loaded, if there is one at all,
		// describes a different file. Declining to answer costs the "not
		// inside this program" message and leaves Ghidra's own "no function"
		// in its place, which is worse but not wrong.
		return 0, 0, false
	}
	abs, err := s.files.AbsPath(s.st.exePath)
	if err != nil {
		return 0, 0, false
	}
	f, err := elf.Open(abs)
	if err != nil {
		return 0, 0, false
	}
	defer f.Close()

	minV, maxV := ^uint64(0), uint64(0)
	for _, p := range f.Progs {
		if p.Type != elf.PT_LOAD {
			continue
		}
		if p.Vaddr < minV {
			minV = p.Vaddr
		}
		if end := p.Vaddr + p.Memsz; end > maxV {
			maxV = end
		}
	}
	if minV == ^uint64(0) || maxV <= minV {
		return 0, 0, false
	}
	base, err := parseAddress(client.Ready().Program.ImageBase)
	if err != nil {
		return 0, 0, false
	}
	return base, base + (maxV - minV), true
}

// entryAnchor names the entry-point anchor in BiasFrom. It is not a symbol
// name, and is not one a program could have.
const entryAnchor = "<entry point>"

// runtimeEntryPoint reads the relocated entry point out of `info files`.
//
// Prose, because gdb offers it no other way: -file-list-shared-libraries omits
// the main executable, and no MI command reports a section address.
func (s *Session) runtimeEntryPoint(r *request) (uint64, bool) {
	out, werr := s.runConsole(r.ctx, "info files")
	if werr != nil {
		return 0, false
	}
	for _, line := range strings.Split(out, "\n") {
		_, after, found := strings.Cut(line, "Entry point:")
		if !found {
			continue
		}
		n, err := parseAddress(strings.TrimSpace(after))
		if err != nil {
			continue
		}
		return n, true
	}
	return 0, false
}

// addressOfSymbol asks gdb where a named function lives.
func (s *Session) addressOfSymbol(r *request, name string) (uint64, *wire.Error) {
	rec, werr := s.send(r.ctx, "-data-evaluate-expression "+quote("&"+name))
	if werr != nil {
		return 0, wire.NewError(wire.CodeGDBError, werr.Message)
	}
	n, ok := addressFromValue(rec.Results.Str("value"))
	if !ok {
		return 0, wire.NewError(wire.CodeGDBError, "not an address")
	}
	return n, nil
}

// plausibleSymbol rejects the names Ghidra invents from an address. They are
// exactly the ones gdb cannot resolve, and asking about them is a wasted round
// trip per candidate.
func plausibleSymbol(name string) bool {
	if name == "" {
		return false
	}
	for _, prefix := range []string{"FUN_", "LAB_", "DAT_", "SUB_", "thunk_"} {
		if strings.HasPrefix(name, prefix) {
			return false
		}
	}
	// A leading underscore is usually CRT glue, present in both but not worth
	// preferring over a real name.
	return true
}

// shiftAddr moves one address by the bias, preserving the 0x form.
func shiftAddr(addr string, bias int64) string {
	if addr == "" {
		return ""
	}
	n, err := parseAddress(addr)
	if err != nil {
		return addr
	}
	return "0x" + strconv.FormatUint(uint64(int64(n)+bias), 16)
}

func short(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

func (s *Session) decompLanguage() string {
	s.decomp.mu.Lock()
	defer s.decomp.mu.Unlock()
	if s.decomp.client == nil {
		return ""
	}
	return s.decomp.client.Ready().Program.LanguageID
}

// hashExe reads the loaded executable and returns its sha256, or "" if it
// cannot. A missing hash disables the mismatch warning rather than blocking
// the feature: not knowing is a weaker position than knowing, but it is not a
// reason to refuse to decompile.
func (s *Session) hashExe(rel string) string {
	if s.files == nil {
		return ""
	}
	// AbsPath is the containment-checked path — the same one exe.load hands to
	// gdb — so opening it directly is consistent rather than a way around the
	// project root.
	abs, err := s.files.AbsPath(rel)
	if err != nil {
		return ""
	}
	f, err := os.Open(abs)
	if err != nil {
		return ""
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return ""
	}
	return hex.EncodeToString(h.Sum(nil))
}

// asGhidraError reports whether err is a *ghidra.Error, which is the far end
// refusing a request rather than the process failing. The distinction matters
// to the caller: one is bad_request, the other is internal.
func asGhidraError(err error, target **ghidra.Error) bool {
	return errors.As(err, target)
}

// Stepping by decompiled line.
//
// gdb's own `next` and `step` need a line table, and without one its step range
// is the whole function — so "step over" in a binary with no debug info runs to
// the function's exit. Measured on a symbols-but-no-DWARF build: `break main`
// then `next` lands at 0x7ffff7c2a601, inside libc, having returned out of main
// altogether. That makes the decompiled view unusable for stepping, which is
// most of what it is for.
//
// The fix is what gdb does internally when it does have a line table: single
// step until the program counter leaves the current line's addresses. The
// difference is only where the range comes from — Ghidra's map instead of
// DWARF's.
//
// It cannot be a loop inside the request handler. Exec commands are
// acknowledgements, not completions: the stop arrives later as an async record
// processed by this same actor, so a handler that waited for one would deadlock
// against itself. It is a mode instead — set here, advanced by onStopped.

// stepLineMax bounds the walk. A decompiled line is a handful of instructions,
// so this is a runaway guard rather than a limit anyone should reach: a line
// whose addresses somehow never stop matching would otherwise step forever.
const stepLineMax = 2000

// stepLine is the state of a step in progress.
type stepLine struct {
	// lines is the function's map, and line is where the walk began. The walk
	// continues while the pc still resolves to that line.
	lines []wire.DecompLine
	// line is the line the walk began on, and 0 when it began somewhere no
	// line exactly claims — a prologue, a spill. The walk ends on reaching a
	// line *exactly*, and a different one than this.
	line int
	// body bounds the function.
	bodyStart, bodyEnd string
	// over steps over calls rather than into them.
	over bool
	// left counts down; zero ends the walk wherever it is.
	left int
	// startFrame is the stack depth when the walk began. Returning out of the
	// frame ends it, or a step-over of the last statement would keep walking
	// through the caller's line addresses by coincidence.
	startFrame int
}

func (s *Session) execStepLine(r *request) (any, *wire.Error) {
	req, werr := decode[wire.ExecStepLineRequest](r.req.Payload)
	if werr != nil {
		return nil, werr
	}
	if werr := s.checkStopSeq(req.StopSeq); werr != nil {
		return nil, werr
	}

	// Which line the walk is leaving. An approximate answer counts as *no*
	// line: the pc is between lines, so the next line reached is a destination
	// rather than somewhere to step past.
	//
	// This is the ordinary case, not an edge one. Breaking on a function puts
	// the pc in the prologue — gdb skips it — and the prologue belongs to no
	// line, so refusing to walk from there would refuse the commonest place
	// anyone starts stepping.
	start, _, startApprox := pcLine(req.Lines, s.currentPC(), req.BodyStart, req.BodyEnd)
	if startApprox {
		start = 0
	}
	// With no map there is nothing to step out of, so one instruction is all
	// that can be claimed.
	if len(req.Lines) == 0 {
		s.st.stepping = nil
	} else {
		s.st.stepping = &stepLine{
			lines:      req.Lines,
			line:       start,
			bodyStart:  req.BodyStart,
			bodyEnd:    req.BodyEnd,
			over:       req.Over,
			left:       stepLineMax,
			startFrame: len(s.st.frames),
		}
	}
	return s.instructionStep(r.ctx, req.Over)
}

// instructionStep issues one machine step.
func (s *Session) instructionStep(ctx context.Context, over bool) (any, *wire.Error) {
	cmd := "-exec-step-instruction"
	if over {
		cmd = "-exec-next-instruction"
	}
	if _, werr := s.send(ctx, cmd); werr != nil {
		s.st.stepping = nil
		return nil, werr
	}
	s.st.runState = wire.RunStateRunning
	return wire.ExecAck{RunState: s.st.runState, StopSeq: s.st.stopSeq}, nil
}

// advanceStepLine decides whether a stop is the end of a line step or the
// middle of one. Returning true means the caller should issue another step and
// say nothing to the browser: a walk of ten instructions must look like one
// step, not ten.
func (s *Session) advanceStepLine(ctx context.Context, reason string) bool {
	st := s.st.stepping
	if st == nil {
		return false
	}
	// Anything other than finishing an instruction step is a real event — a
	// breakpoint, a signal, a watchpoint — and ends the walk where it is.
	if reason != "end-stepping-range" {
		s.st.stepping = nil
		return false
	}
	st.left--
	if st.left <= 0 {
		s.st.stepping = nil
		return false
	}
	// Out of the frame the walk started in: a step over the last statement
	// returned, and continuing would follow the caller's addresses by accident.
	if len(s.st.frames) < st.startFrame {
		s.st.stepping = nil
		return false
	}
	// Keep walking until the pc lands *exactly* on a line other than the one
	// the walk began on.
	//
	// Exactness is what makes this right in both directions. Instructions
	// between a line's tokens resolve approximately to the line they follow,
	// and stopping on one would leave the marker mid-statement; a prologue
	// resolves approximately to the first line, and stopping there would mean
	// a step from a function breakpoint went nowhere.
	now, _, approx := pcLine(st.lines, s.currentPC(), st.bodyStart, st.bodyEnd)
	switch {
	case now == 0:
		// Off the map: out of the function, or somewhere it does not describe.
		s.st.stepping = nil
		return false
	case !approx && now != st.line:
		s.st.stepping = nil
		return false
	}
	if _, werr := s.instructionStep(ctx, st.over); werr != nil {
		s.st.stepping = nil
		return false
	}
	return true
}

// ghidraProcessLog filters the child's own output on its way to the browser.
//
// analyzeHeadless emits hundreds of lines — a JVM banner, every analyzer's
// timing, log4j noise — and forwarding all of it would bury the pane. But
// forwarding none of it leaves a user watching "starting" for a minute with no
// way to tell whether anything is happening, or why it failed. So the
// milestones and the complaints go through, and the rest goes to the server's
// log where -v can find it.
func (s *Session) ghidraProcessLog(format string, args ...any) {
	line := fmt.Sprintf(format, args...)
	s.logf("%s", line)

	// The JVM's own deprecation warnings are four lines about Ghidra's
	// dependencies calling sun.misc.Unsafe, and only some of them name it —
	// the "Please consider reporting this" line does not. The reliable
	// distinction is the spelling: the JVM writes "WARNING:", Ghidra's log4j
	// writes "WARN ".
	if strings.HasPrefix(trimGhidraNoise(line), "WARNING:") {
		return
	}

	switch {
	case strings.Contains(line, "ERROR"):
		s.emitOffActor(wire.EventDecompLog, wire.DecompLog{
			Text: trimGhidraNoise(line), Level: wire.DecompLogError})
	case strings.Contains(line, "REPORT:"),
		strings.Contains(line, "Packed database"),
		strings.Contains(line, "WARN") && !strings.Contains(line, "sun.misc.Unsafe"):
		// REPORT: lines are analyzeHeadless's own milestones — import
		// succeeded, analysis succeeded, which file it is working on. The
		// Unsafe warnings are the JVM complaining about Ghidra's own
		// dependencies and mean nothing here.
		s.emitOffActor(wire.EventDecompLog, wire.DecompLog{
			Text: trimGhidraNoise(line), Level: wire.DecompLogInfo})
	}
}

// trimGhidraNoise strips the log4j decoration so a line reads as a sentence.
func trimGhidraNoise(line string) string {
	line = strings.TrimSpace(line)
	line = strings.TrimPrefix(line, "ghidra: ")
	line = strings.TrimPrefix(line, "ghidra import: ")
	for _, p := range []string{"INFO  ", "WARN  ", "ERROR "} {
		line = strings.TrimPrefix(line, p)
	}
	// The trailing "(SomeClassName)" is Ghidra's logger name.
	if i := strings.LastIndex(line, " ("); i > 0 && strings.HasSuffix(line, ")") {
		line = line[:i]
	}
	return strings.TrimSpace(line)
}

// --- naming frames ---------------------------------------------------------

// maxNameAddresses bounds one decomp.names request. gdb caps a stack at 64
// frames; anything much beyond that is a client asking about something other
// than a stack.
const maxNameAddresses = 128

// decompNames says which function each address falls in.
//
// It exists for the call stack of a stripped binary, which gdb renders as a
// column of "?? ()" — it has no symbol for any of it, and the decompiler is the
// only thing here that knows otherwise. This does not decompile: the sidecar
// answers from Ghidra's function manager, so a whole stack is one round trip.
// The breakpoint list asks the same question about the same kind of address.
//
// With Data set it also names the module-scope labels, for the panes that show
// data rather than code: a watch on a decompiler global is an address and
// nothing else, and DAT_001a08de is the only name it has.
//
// An unready decompiler is an empty answer rather than an error. The caller is
// a panel that has already drawn gdb's "??" and is asking whether it can do
// better; "no" is an ordinary reply, and turning it into an error would put a
// message in the status bar on every stop in a binary Ghidra has never seen.
func (s *Session) decompNames(r *request) (any, *wire.Error) {
	req, werr := decode[wire.DecompNamesRequest](r.req.Payload)
	if werr != nil {
		return nil, werr
	}

	s.decomp.mu.Lock()
	client, state := s.decomp.client, s.decomp.state
	s.decomp.mu.Unlock()

	if client == nil {
		// Same as the pane: asking is what starts it. Configuring -ghidra is
		// the opt-in, and a user who never opens the Decompiled tab should
		// still get their stack named.
		s.maybeStartDecomp()
		if state == "" {
			state = wire.DecompOff
		}
		return wire.DecompNames{State: state}, nil
	}

	out := wire.DecompNames{State: wire.DecompReady}
	if len(req.Addresses) == 0 {
		return out, nil
	}

	bias, _ := s.decompBias(r, client)

	// Into Ghidra's coordinates, and back again on the way out.
	//
	// Addresses outside the program are *not* filtered here, although a stack
	// routinely runs out through libc and the dynamic loader and Ghidra has
	// neither. Ghidra's own getFunctionContaining answers null for them, so a
	// pre-filter would be a second opinion on containment that no test could
	// tell from the first — and it would cost an ELF parse per request to
	// find the image range. The same reasoning covers a bias that could not be
	// established: it is then zero, a PIE's runtime addresses stay far outside
	// the image, and Ghidra names none of them.
	wanted := make([]string, 0, len(req.Addresses))
	back := make(map[string]string, len(req.Addresses))
	for i, a := range req.Addresses {
		if i >= maxNameAddresses {
			break
		}
		n, err := parseAddress(a)
		if err != nil {
			continue
		}
		key := fmt.Sprintf("0x%x", uint64(int64(n)-bias))
		wanted = append(wanted, key)
		back[key] = a
	}
	if len(wanted) == 0 {
		return out, nil
	}

	names, err := client.Names(r.ctx, wanted)
	if err != nil {
		// A naming failure is not worth interrupting a stop for. The panel
		// keeps gdb's "??", which is what it was showing anyway.
		s.decompLog(wire.DecompLogWarn, "naming %d addresses: %v", len(wanted), err)
		return out, nil
	}

	for _, n := range names {
		asked, ok := back[n.Addr]
		if !ok {
			continue
		}
		name := wire.DecompName{
			Addr:      asked,
			Name:      n.Name,
			Signature: n.Signature,
			Entry:     shiftAddr(n.Entry, bias),
			Thunk:     n.Thunk,
			Kind:      wire.SymbolFunction,
		}
		if entry, err := parseAddress(name.Entry); err == nil {
			if at, err := parseAddress(asked); err == nil && at >= entry {
				name.Offset = int(at - entry)
			}
		}
		out.Names = append(out.Names, name)
		delete(back, n.Addr)
	}

	// What is left is in no function, which for a watch is the ordinary case
	// rather than the failure: a global is data, and data is exactly what
	// Ghidra's function manager answers "no" for. The labels come from the name
	// index instead — the same one the symbol pane and the go-to box read — so
	// this costs a map lookup once the index is warm.
	if req.Data {
		// Over the addresses asked about rather than over what is left in the
		// map, so the reply keeps the order of the request instead of a map's.
		for _, key := range wanted {
			asked, ok := back[key]
			if !ok {
				continue // a function claimed it above
			}
			at, err := parseAddress(key)
			if err != nil {
				continue
			}
			e, ok := s.decompDataAt(r, at)
			if !ok {
				continue
			}
			out.Names = append(out.Names, wire.DecompName{
				Addr:  asked,
				Name:  e.Name,
				Entry: asked,
				Kind:  wire.SymbolVariable,
			})
		}
	}
	return out, nil
}
