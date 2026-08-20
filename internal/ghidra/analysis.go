package ghidra

import (
	"debug/elf"
	"fmt"
)

// Analysis says how much of a binary Ghidra should chew on at import time.
//
// Ghidra's auto-analysis walks the whole image: it disassembles everything,
// propagates constants between functions, recovers signatures and builds the
// cross-references. That is what makes a small binary pleasant to read, and
// past a few megabytes of code it stops finishing. Measured on a 12 MB MIPS64
// vmlinux, 6.9 MB of it code: analyzeHeadless exhausted its 2 GB heap in
// MipsAddressAnalyzer and the import failed outright.
//
// The decompiler itself needs none of it. It follows flow and translates bytes
// on its own, so a program imported with no analysis at all still decompiles
// one function in tens of milliseconds — see DecompServer.ensureCode, which
// disassembles the one function being asked for. What is lost is the
// whole-program work: cross-references, recovered parameter types, typed
// strings. On an image too big to analyse, none of that was on offer anyway.
type Analysis string

const (
	// AnalysisAuto measures the binary and decides. The default.
	AnalysisAuto Analysis = "auto"
	// AnalysisFull always runs Ghidra's auto-analysis.
	AnalysisFull Analysis = "full"
	// AnalysisNone imports without analysis and disassembles on demand.
	AnalysisNone Analysis = "none"
	// AnalysisLean runs the analyzers that find functions and leaves out the
	// ones that cost the memory. For an image with no symbols to lean on: with
	// nothing to name and nothing to disassemble from, AnalysisNone produces an
	// empty program. Measured on a stripped 12 MB MIPS64 kernel: 89 seconds and
	// 1.28 GB, inside the 2 GB heap, finding 12,955 functions of which 97% sit
	// exactly on a real entry point — but only 57% of the real ones, and every
	// name is FUN_ plus its address. Pattern matching finds where a function is,
	// never what it was called.
	AnalysisLean Analysis = "lean"
)

// AutoAnalysisLimit is how much executable code auto will hand to the full
// analysis.
//
// Between the two measurements that exist: a 2 MB firmware image analyses in
// 71 seconds, and 6.9 MB of MIPS64 kernel does not analyse at all. Set nearer
// the top of that gap deliberately. Dropping to AnalysisNone costs a real
// feature, so an image that has been working should keep working; it is the
// ones with no chance that get diverted.
const AutoAnalysisLimit = 4 << 20

// Resolve reports the mode to use for binary, and why in a form fit to log.
//
// Idempotent: only AnalysisAuto looks at the file, so a caller that has already
// resolved can pass the answer back in and get it unchanged. Both the debugger
// (which logs the reason) and Import (which must not depend on the caller
// having done so) call this.
func (a Analysis) Resolve(binary string) (Analysis, string) {
	if a != AnalysisAuto && a != "" {
		return a, ""
	}
	n, err := CodeBytes(binary)
	if err != nil {
		// Not an ELF, or unreadable. Ghidra will have its own opinion about
		// the file; analysing it is the behaviour that predates this switch.
		return AnalysisFull, ""
	}
	return decide(n, HasFunctionSymbols(binary))
}

// decide is Resolve's judgement, split out so the thresholds can be tested
// without a binary of each shape to hand.
//
// Both branches past the limit are worse than analysing, and which one is less
// bad turns on whether anything else knows where the functions are. A symbol
// table names them all, so nothing needs discovering and the analysis is pure
// cost. Without one there is no program to show at all until something finds
// them, and only the analyzers can.
func decide(codeBytes int64, symbols bool) (Analysis, string) {
	if codeBytes <= AutoAnalysisLimit {
		return AnalysisFull, ""
	}
	if symbols {
		return AnalysisNone, fmt.Sprintf(
			"%s of code is more than Ghidra's analysis will finish", megabytes(codeBytes))
	}
	return AnalysisLean, fmt.Sprintf(
		"%s of code is more than Ghidra's analysis will finish, and it is stripped, "+
			"so the functions have to be found rather than read", megabytes(codeBytes))
}

// HasFunctionSymbols reports whether an ELF says where any of its functions
// are. False for a stripped image, and the difference between an import that
// arrives complete and one that arrives empty.
func HasFunctionSymbols(path string) bool {
	f, err := elf.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	// Both tables: a dynamically linked binary keeps .dynsym when stripped, and
	// its exports are enough to be worth having.
	for _, syms := range []func() ([]elf.Symbol, error){f.Symbols, f.DynamicSymbols} {
		list, err := syms()
		if err != nil {
			continue
		}
		for _, s := range list {
			if elf.ST_TYPE(s.Info) == elf.STT_FUNC && s.Value != 0 {
				return true
			}
		}
	}
	return false
}

// CodeBytes is how much executable code an ELF holds.
//
// Sections rather than the file size, because they are what analysis costs
// scale with: a firmware image that is mostly a filesystem, or a kernel with a
// megabyte of __ksymtab_strings, is cheaper than its size suggests. NOBITS is
// skipped for the same reason — .bss occupies no bytes to disassemble.
func CodeBytes(path string) (int64, error) {
	f, err := elf.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	var n int64
	for _, s := range f.Sections {
		if s.Flags&elf.SHF_EXECINSTR != 0 && s.Type != elf.SHT_NOBITS {
			n += int64(s.Size)
		}
	}
	if n == 0 {
		// No section headers, which a stripped-to-the-bone image can manage.
		// Fall back to the loadable executable segments.
		for _, p := range f.Progs {
			if p.Type == elf.PT_LOAD && p.Flags&elf.PF_X != 0 {
				n += int64(p.Filesz)
			}
		}
	}
	if n == 0 {
		return 0, fmt.Errorf("ghidra: %s holds no executable sections", path)
	}
	return n, nil
}

func megabytes(n int64) string {
	return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
}

// String, Set and Get make Analysis a flag.Value, so the flag package rejects
// a misspelling at parse time rather than the session quietly running in a
// mode nobody asked for. Get is flag.Getter, which is how config.Save learns
// the concrete type.
func (a *Analysis) String() string {
	if a == nil || *a == "" {
		return string(AnalysisAuto)
	}
	return string(*a)
}

func (a *Analysis) Set(s string) error {
	switch Analysis(s) {
	case AnalysisAuto, AnalysisFull, AnalysisLean, AnalysisNone:
		*a = Analysis(s)
		return nil
	}
	return fmt.Errorf("must be one of %s, %s, %s or %s",
		AnalysisAuto, AnalysisFull, AnalysisLean, AnalysisNone)
}

func (a *Analysis) Get() any { return a.String() }
