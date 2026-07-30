package srcfs

import (
	"io/fs"
	"path"
	"strings"
	"sync"
)

// The basename index, for matching paths gdb reports against files that are
// actually here.
//
// A program built anywhere but this machine reports paths that do not exist
// locally: /build/src/parser.c from a container, or ./nptl/cancellation.c from
// glibc. The file is often present under a different prefix, and finding it is
// the difference between a working source view and a blank pane.
//
// Matching is by **longest trailing path-component count**, not by basename
// alone. Any real project has several files called util.c, and picking the wrong
// one silently shows the wrong code with the right line numbers — which is worse
// than showing nothing, because nothing is obviously nothing.

// maxIndexed bounds the index. A monorepo has hundreds of thousands of files
// and indexing all of them would cost more than the feature is worth.
const maxIndexed = 200000

// Index maps basenames to the root-relative paths that share them.
type Index struct {
	once   sync.Once
	mu     sync.RWMutex
	byBase map[string][]string
	built  bool
	// truncated records that the walk hit the cap, so a failure to match can be
	// explained rather than looking arbitrary.
	truncated bool
}

// Index returns the project's basename index, building it on first use.
//
// Lazily, because most sessions never need it: a program built here reports
// paths that resolve directly, and walking the tree for nothing would make
// startup slower for the common case.
func (f *FS) Index() *Index {
	f.index.once.Do(func() { f.buildIndex() })
	return &f.index
}

func (f *FS) buildIndex() {
	byBase := map[string][]string{}
	var count int
	var truncated bool

	err := fs.WalkDir(f.root.FS(), ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			// A directory we cannot read is not a reason to abandon the walk.
			return nil
		}
		if d.IsDir() {
			if skipDirs[d.Name()] {
				return fs.SkipDir
			}
			return nil
		}
		if count >= maxIndexed {
			truncated = true
			return fs.SkipAll
		}
		// Only files that could plausibly be source. Indexing object files and
		// images makes the index bigger and the matches no better.
		if !isSourceName(d.Name()) {
			return nil
		}
		base := d.Name()
		byBase[base] = append(byBase[base], p)
		count++
		return nil
	})
	if err != nil {
		// A partial index is still useful; a missing one is not.
		_ = err
	}

	f.index.mu.Lock()
	f.index.byBase = byBase
	f.index.built = true
	f.index.truncated = truncated
	f.index.mu.Unlock()
}

// sourceExtensions is what gets indexed. Deliberately narrow: these are the
// files a debugger reports line numbers in.
var sourceExtensions = map[string]bool{
	".c": true, ".h": true, ".cc": true, ".cpp": true, ".cxx": true,
	".hh": true, ".hpp": true, ".hxx": true, ".inc": true,
	".s": true, ".S": true, ".asm": true,
	".m": true, ".mm": true,
}

func isSourceName(name string) bool {
	return sourceExtensions[path.Ext(name)]
}

// Locate finds the local file best matching a path gdb reported.
//
// The score is how many trailing path components agree. A single shared
// basename is a weak match and several candidates with the same score is no
// match at all: "src/util.c" and "vendor/util.c" both end in util.c, and
// guessing between them would show the wrong file with plausible line numbers.
func (ix *Index) Locate(gdbPath string) (rel string, ok bool) {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	if !ix.built || gdbPath == "" {
		return "", false
	}

	// gdb reports paths in whatever form the compiler recorded, including
	// unnormalised ones like "./nptl/./nptl/cancellation.c".
	wanted := path.Clean(strings.ReplaceAll(gdbPath, "\\", "/"))
	base := path.Base(wanted)
	candidates := ix.byBase[base]
	if len(candidates) == 0 {
		return "", false
	}
	if len(candidates) == 1 {
		return candidates[0], true
	}

	wantParts := strings.Split(wanted, "/")
	best, bestScore, tied := "", 0, false
	for _, candidate := range candidates {
		score := commonSuffixLength(wantParts, strings.Split(candidate, "/"))
		switch {
		case score > bestScore:
			best, bestScore, tied = candidate, score, false
		case score == bestScore:
			tied = true
		}
	}
	// A tie is a refusal. Showing one of two equally plausible files is how a
	// user ends up debugging the wrong code without knowing it.
	if tied || bestScore == 0 {
		return "", false
	}
	return best, true
}

// Candidates returns every local file sharing a basename, for a "locate this
// file" prompt where the user picks.
func (ix *Index) Candidates(gdbPath string) []string {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	if !ix.built {
		return nil
	}
	base := path.Base(path.Clean(gdbPath))
	return append([]string(nil), ix.byBase[base]...)
}

// Stats reports the index's size, for diagnostics.
func (ix *Index) Stats() (files int, truncated bool) {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	for _, list := range ix.byBase {
		files += len(list)
	}
	return files, ix.truncated
}

// commonSuffixLength counts trailing components two paths share.
func commonSuffixLength(a, b []string) int {
	n := 0
	for i, j := len(a)-1, len(b)-1; i >= 0 && j >= 0; i, j = i-1, j-1 {
		if a[i] != b[j] {
			break
		}
		n++
	}
	return n
}

// SubstitutionFor computes the prefix pair to hand gdb's substitute-path.
//
// Given that gdb said /build/src/util.c and the file is really at
// <root>/src/util.c, the shared suffix is "src/util.c", so the substitution is
// /build -> <root>. Teaching gdb the prefix once fixes every later frame in
// that tree, plus `list`, `info line` and anything the user types at the
// console. Rewriting paths one file at a time in the UI is a losing game by
// comparison: gdb keeps reporting the old ones.
func (f *FS) SubstitutionFor(gdbPath, rel string) (from, to string, ok bool) {
	cleaned := path.Clean(strings.ReplaceAll(gdbPath, "\\", "/"))
	shared := commonSuffixLength(strings.Split(cleaned, "/"), strings.Split(rel, "/"))
	if shared == 0 {
		return "", "", false
	}
	gdbParts := strings.Split(cleaned, "/")
	relParts := strings.Split(rel, "/")

	fromPrefix := strings.Join(gdbParts[:len(gdbParts)-shared], "/")
	relPrefix := strings.Join(relParts[:len(relParts)-shared], "/")

	toPrefix := f.abs
	if relPrefix != "" {
		toPrefix = f.abs + "/" + relPrefix
	}
	if fromPrefix == "" {
		// Nothing to substitute: gdb's path is already relative to the shared
		// suffix, and -environment-directory covers that case instead.
		return "", "", false
	}
	if fromPrefix == toPrefix {
		return "", "", false
	}
	return fromPrefix, toPrefix, true
}
