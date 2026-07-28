// Package srcfs browses the project directory safely.
//
// Containment is enforced with os.Root rather than filepath.Abs plus a prefix
// check. The difference matters: os.Root resolves every path component with
// openat, so ".." cannot escape, a symlink inside the root pointing at
// /etc/shadow is refused, and there is no TOCTOU window between the check and
// the open. A strings.HasPrefix check passes all three of those attacks.
//
// The invariant that keeps it true: no user-supplied path is ever passed to
// filepath.Join, and every path crossing the API is a root-relative slash path
// validated with fs.ValidPath before it reaches the Root.
package srcfs

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Limits. Each exists because the unbounded version has a failure mode that
// looks like a hang or an out-of-memory kill rather than an error.
const (
	// MaxEntries caps one directory listing.
	MaxEntries = 5000
	// MaxFileSize caps a file served over /api/file. Larger files are refused
	// with a clear error so the UI can offer the hex viewer instead.
	MaxFileSize = 2 << 20
	// sniffLen is how much of a file is examined for NUL bytes.
	sniffLen = 8000
)

// Errors. Callers map these onto protocol error codes; they do not string-match.
var (
	// ErrDenied means the path escaped the root or was not a valid relative
	// path.
	ErrDenied = errors.New("srcfs: path is outside the project root")
	// ErrNotFound means no such file or directory.
	ErrNotFound = errors.New("srcfs: not found")
	// ErrTooLarge means the file exceeds MaxFileSize.
	ErrTooLarge = errors.New("srcfs: file is too large")
	// ErrBinary means the file contains NUL bytes and is not text.
	ErrBinary = errors.New("srcfs: file is not text")
	// ErrIsDir means a directory was requested where a file was expected.
	ErrIsDir = errors.New("srcfs: path is a directory")
)

// skipDirs are never listed. They are the two directories that make a naive
// recursive walk of a real project unusable, and neither contains source a
// debugger user wants to open.
var skipDirs = map[string]bool{
	".git":         true,
	"node_modules": true,
}

// FS is a rooted view of the project directory.
type FS struct {
	root *os.Root
	// abs is the resolved absolute path, for display and for source resolution
	// in M8. It is never used to build paths for opening.
	abs string
}

// Open roots a filesystem at dir.
func Open(dir string) (*FS, error) {
	abs, err := absClean(dir)
	if err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(abs)
	if err != nil {
		return nil, fmt.Errorf("srcfs: opening project root %s: %w", abs, err)
	}
	return &FS{root: root, abs: abs}, nil
}

func absClean(dir string) (string, error) {
	if dir == "" {
		dir = "."
	}
	abs, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("srcfs: getwd: %w", err)
	}
	if !strings.HasPrefix(dir, "/") {
		dir = abs + "/" + dir
	}
	// filepath.Clean on a path that is not user input: this is the operator's
	// own -project flag, resolved once at startup, never re-derived per request.
	return cleanAbs(dir), nil
}

// Close releases the root's file descriptor.
func (f *FS) Close() error { return f.root.Close() }

// Abs returns the absolute path of the project root, for display.
func (f *FS) Abs() string { return f.abs }

// clean validates a request path and returns the form os.Root accepts.
//
// The empty string means the root itself. Everything else must satisfy
// fs.ValidPath: slash-separated, no leading slash, no "." or ".." element, not
// empty. Rejecting rather than sanitising is deliberate — silently rewriting
// "../../etc/passwd" into something else is how sanitisers become bypasses.
func clean(p string) (string, error) {
	if p == "" || p == "." {
		return ".", nil
	}
	// A leading slash is a common frontend bug rather than an attack; accept it
	// by trimming, then validate what remains with no further rewriting.
	p = strings.TrimPrefix(p, "/")
	if p == "" {
		return ".", nil
	}
	if !fs.ValidPath(p) {
		return "", fmt.Errorf("%w: %q", ErrDenied, p)
	}
	return p, nil
}

// Tree lists one directory level.
//
// One level, not a recursive walk: on a monorepo a recursive listing is slow to
// produce, large to send, and mostly unread. The frontend asks for each level
// as the user opens it.
func (f *FS) Tree(p string) (Listing, error) {
	rel, err := clean(p)
	if err != nil {
		return Listing{}, err
	}
	dir, err := f.root.Open(rel)
	if err != nil {
		return Listing{}, mapErr(err)
	}
	defer dir.Close()

	info, err := dir.Stat()
	if err != nil {
		return Listing{}, mapErr(err)
	}
	if !info.IsDir() {
		return Listing{}, fmt.Errorf("%w: %s", ErrNotFound, p)
	}

	names, err := dir.Readdirnames(-1)
	if err != nil {
		return Listing{}, mapErr(err)
	}
	sort.Strings(names)

	out := Listing{Path: displayPath(rel)}
	for _, name := range names {
		if skipDirs[name] {
			continue
		}
		if len(out.Entries) >= MaxEntries {
			out.Truncated = true
			break
		}
		child := name
		if rel != "." {
			child = rel + "/" + name
		}
		e := Entry{Name: name, Path: child}

		// Lstat, not Stat: a symlink is reported as what it is rather than as
		// what it points at, so the UI can show it and the user is not
		// surprised when following it is refused.
		fi, err := f.root.Lstat(child)
		if err != nil {
			// A file that vanished between readdir and lstat, or a link the
			// root refuses to resolve. Listing it as an unknown entry beats
			// failing the whole directory.
			out.Entries = append(out.Entries, e)
			continue
		}
		e.Symlink = fi.Mode()&fs.ModeSymlink != 0
		if e.Symlink {
			// Resolve through the root to classify the target. Failure means
			// it points outside, which is exactly what we want to show.
			if target, terr := f.root.Stat(child); terr == nil {
				e.Dir = target.IsDir()
				if !e.Dir {
					e.Size = target.Size()
				}
			}
		} else {
			e.Dir = fi.IsDir()
			if !e.Dir {
				e.Size = fi.Size()
			}
		}
		out.Entries = append(out.Entries, e)
	}

	// Directories first, then names. Sorting here rather than in the frontend
	// keeps the two consistent when a second client is written.
	sort.SliceStable(out.Entries, func(i, j int) bool {
		a, b := out.Entries[i], out.Entries[j]
		if a.Dir != b.Dir {
			return a.Dir
		}
		return a.Name < b.Name
	})
	return out, nil
}

// Entry is one directory entry.
type Entry struct {
	Name    string
	Path    string
	Dir     bool
	Size    int64
	Symlink bool
}

// Listing is one directory level.
type Listing struct {
	Path      string
	Entries   []Entry
	Truncated bool
}

// File is a file's contents plus what the HTTP layer needs for caching.
type File struct {
	Path    string
	Content []byte
	Size    int64
	ModTime time.Time
	// ETag is derived from size, mtime and inode. Content hashing would be
	// stronger, but this is a local filesystem and the file is about to be sent
	// anyway; hashing every read to save a conditional request is the wrong
	// trade.
	ETag string
}

// ReadFile reads a text file under the root.
func (f *FS) ReadFile(p string) (File, error) {
	rel, err := clean(p)
	if err != nil {
		return File{}, err
	}
	if rel == "." {
		return File{}, ErrIsDir
	}
	fh, err := f.root.Open(rel)
	if err != nil {
		return File{}, mapErr(err)
	}
	defer fh.Close()

	info, err := fh.Stat()
	if err != nil {
		return File{}, mapErr(err)
	}
	if info.IsDir() {
		return File{}, ErrIsDir
	}
	if info.Size() > MaxFileSize {
		return File{}, fmt.Errorf("%w: %d bytes, limit is %d", ErrTooLarge, info.Size(), MaxFileSize)
	}

	// LimitReader at one byte over the cap: Size() can lie for files being
	// written, and the read is what actually has to be bounded.
	buf, err := io.ReadAll(io.LimitReader(fh, MaxFileSize+1))
	if err != nil {
		return File{}, fmt.Errorf("srcfs: reading %s: %w", p, err)
	}
	if len(buf) > MaxFileSize {
		return File{}, fmt.Errorf("%w: over %d bytes", ErrTooLarge, MaxFileSize)
	}
	if isBinary(buf) {
		return File{}, ErrBinary
	}

	return File{
		Path:    rel,
		Content: buf,
		Size:    int64(len(buf)),
		ModTime: info.ModTime(),
		ETag:    etag(info),
	}, nil
}

// Stat reports on one path.
func (f *FS) Stat(p string) (fs.FileInfo, error) {
	rel, err := clean(p)
	if err != nil {
		return nil, err
	}
	info, err := f.root.Stat(rel)
	if err != nil {
		return nil, mapErr(err)
	}
	return info, nil
}

// isBinary reports whether the content looks like something the source viewer
// should refuse. A NUL byte in the first few KB is the same heuristic git uses,
// and it is right often enough that the alternative — rendering an ELF file as
// text — is not worth entertaining.
func isBinary(b []byte) bool {
	if len(b) > sniffLen {
		b = b[:sniffLen]
	}
	for _, c := range b {
		if c == 0 {
			return true
		}
	}
	return false
}

func etag(info fs.FileInfo) string {
	var ino uint64
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		ino = st.Ino
	}
	return `"` + strconv.FormatInt(info.Size(), 16) + "-" +
		strconv.FormatInt(info.ModTime().UnixNano(), 16) + "-" +
		strconv.FormatUint(ino, 16) + `"`
}

// mapErr turns filesystem errors into the package's own, so callers never
// string-match on errno text.
func mapErr(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, fs.ErrNotExist):
		return fmt.Errorf("%w: %v", ErrNotFound, err)
	case errors.Is(err, fs.ErrPermission):
		return fmt.Errorf("%w: %v", ErrDenied, err)
	case errors.Is(err, os.ErrInvalid):
		return fmt.Errorf("%w: %v", ErrDenied, err)
	}
	// os.Root refuses an escaping path with a plain error; treat anything else
	// from it as a denial rather than leaking the reason.
	return fmt.Errorf("%w: %v", ErrDenied, err)
}

// displayPath renders the root as "" rather than ".", which is what the
// protocol uses.
func displayPath(rel string) string {
	if rel == "." {
		return ""
	}
	return rel
}

// cleanAbs is path.Clean for an absolute OS path. srcfs deals only in slash
// paths, and the project root is the one OS path it handles.
func cleanAbs(p string) string { return path.Clean(p) }
