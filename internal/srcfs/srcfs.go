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
				e.Kind = f.classify(child, fi.Mode())
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

// Entry kinds. The tree uses these to tell a program apart from a file, which
// decides whether clicking it loads a debuggee or opens a text buffer.
const (
	// KindPlain is anything that is not executable.
	KindPlain = ""
	// KindELF is an executable whose first four bytes are the ELF magic: a
	// candidate debuggee.
	KindELF = "elf"
	// KindExec is executable but not ELF — a shell script, usually. Worth
	// distinguishing, because handing one to gdb produces an error and the UI
	// should not invite that.
	KindExec = "exec"
)

// Entry is one directory entry.
type Entry struct {
	Name    string
	Path    string
	Dir     bool
	Size    int64
	Symlink bool
	// Kind is one of the Kind constants; empty for directories.
	Kind string
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

// Head reads the first n bytes of a file.
//
// It exists because ReadFile refuses anything containing NUL, and the one place
// that legitimately needs to look at a binary is the ELF magic check before a
// program is handed to gdb.
func (f *FS) Head(p string, n int) ([]byte, error) {
	rel, err := clean(p)
	if err != nil {
		return nil, err
	}
	fh, err := f.root.Open(rel)
	if err != nil {
		return nil, mapErr(err)
	}
	defer fh.Close()

	info, err := fh.Stat()
	if err != nil {
		return nil, mapErr(err)
	}
	if info.IsDir() {
		return nil, ErrIsDir
	}
	buf := make([]byte, n)
	read, err := io.ReadFull(fh, buf)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("srcfs: reading %s: %w", p, err)
	}
	return buf[:read], nil
}

// AbsPath returns the absolute path of a root-relative path, after checking
// that it resolves inside the root.
//
// It exists for one reason: gdb is a separate process that does not share our
// os.Root, so a program or breakpoint location has to be named by an absolute
// path. The containment check is done here, through the Root, before the string
// is handed over.
//
// There is a TOCTOU gap between this check and gdb's open. It is not worth
// closing: gdb runs as the same user with the same privileges and is about to
// execute the binary anyway, so an attacker who can win that race can already
// do everything the race would buy them.
func (f *FS) AbsPath(p string) (string, error) {
	rel, err := clean(p)
	if err != nil {
		return "", err
	}
	if _, err := f.root.Stat(rel); err != nil {
		return "", mapErr(err)
	}
	if rel == "." {
		return f.abs, nil
	}
	return f.abs + "/" + rel, nil
}

// RelPath maps an absolute path back to a root-relative one.
//
// The prefix comparison here is a *mapping heuristic*, not a security control:
// it answers "does this path gdb reported look like it belongs to the project",
// and the answer is then verified by statting through the Root, which is what
// actually enforces containment. ok is false for anything outside — a libc
// frame, or a build-time path that does not exist on this machine.
func (f *FS) RelPath(abs string) (string, bool) {
	if abs == "" || !strings.HasPrefix(abs, "/") {
		return "", false
	}
	cleaned := path.Clean(abs)
	if cleaned == f.abs {
		return "", false
	}
	prefix := f.abs + "/"
	if !strings.HasPrefix(cleaned, prefix) {
		return "", false
	}
	rel := strings.TrimPrefix(cleaned, prefix)
	if _, err := f.root.Stat(rel); err != nil {
		return "", false
	}
	return rel, true
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

// classify decides whether an entry is a program.
//
// The execute bit is checked first because it is already in hand and rules out
// almost everything; only the survivors are opened to look at four bytes. The
// alternative — guessing from the filename, which is what the frontend used to
// do — is wrong in both directions: a compiled program usually has no
// extension, and plenty of extensionless files are not programs.
func (f *FS) classify(rel string, mode fs.FileMode) string {
	if !mode.IsRegular() || mode.Perm()&0o111 == 0 {
		return KindPlain
	}
	head, err := f.Head(rel, 4)
	if err != nil || len(head) < 4 {
		return KindExec
	}
	if head[0] == 0x7f && head[1] == 'E' && head[2] == 'L' && head[3] == 'F' {
		return KindELF
	}
	return KindExec
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
