// Package runfile records where a running gdb-wui server can be reached, so a
// second invocation can ask it for a fresh login link.
//
// The file is the authentication. It lives in the user's runtime directory with
// mode 0600, so only the same uid can read the mint secret it holds — and the
// same uid is already fully trusted, since it can run anything as the user,
// which is precisely what gdb-wui does for a living. What the file protects
// against is the case the threat model actually cares about: another local user
// or an unprivileged process reaching the loopback port.
package runfile

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Entry describes one running server.
type Entry struct {
	PID     int    `json:"pid"`
	Addr    string `json:"addr"`
	Project string `json:"project"`
	// MintSecret authorises /api/bootstrap-url. It never appears in argv, in a
	// URL, or in any log.
	MintSecret string    `json:"mintSecret"`
	Started    time.Time `json:"started"`

	// path is where this entry was read from; not serialised.
	path string `json:"-"`
}

// Path returns the file this entry came from.
func (e Entry) Path() string { return e.path }

// Dir returns the directory run files live in, creating it if needed.
//
// XDG_RUNTIME_DIR is the right home: it is per-user, mode 0700, usually a
// tmpfs, and cleaned up at logout, so a stale file cannot outlive a reboot.
// The fallback is a uid-suffixed directory under TMPDIR, created 0700.
func Dir() (string, error) {
	base := os.Getenv("XDG_RUNTIME_DIR")
	if base == "" {
		base = filepath.Join(os.TempDir(), fmt.Sprintf("gdb-wui-%d", os.Getuid()))
	} else {
		base = filepath.Join(base, "gdb-wui")
	}
	if err := os.MkdirAll(base, 0o700); err != nil {
		return "", fmt.Errorf("runfile: creating %s: %w", base, err)
	}
	return base, nil
}

// Write records a running server. The returned path should be removed at exit.
func Write(e Entry) (string, error) {
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	// Named by port, so two servers on different ports coexist and a restart on
	// the same port replaces its own entry.
	name := strings.ReplaceAll(e.Addr, "/", "_") + ".json"
	path := filepath.Join(dir, name)

	body, err := json.MarshalIndent(e, "", "  ")
	if err != nil {
		return "", fmt.Errorf("runfile: encoding: %w", err)
	}
	// 0600 before any content is written: the secret must never be readable by
	// anyone else, not even briefly.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return "", fmt.Errorf("runfile: creating %s: %w", path, err)
	}
	defer f.Close()
	if _, err := f.Write(body); err != nil {
		return "", fmt.Errorf("runfile: writing %s: %w", path, err)
	}
	return path, nil
}

// Remove deletes a run file, ignoring a file that is already gone.
func Remove(path string) error {
	if path == "" {
		return nil
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// ErrNoServer means no live server was found.
var ErrNoServer = errors.New("no running gdb-wui found")

// List returns the live servers, newest first.
//
// Entries whose process is gone are deleted as they are found: a server killed
// with SIGKILL has no chance to clean up after itself, and a stale file would
// otherwise send the next -print-url to a dead port.
func List() ([]Entry, error) {
	dir, err := Dir()
	if err != nil {
		return nil, err
	}
	names, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		return nil, fmt.Errorf("runfile: listing %s: %w", dir, err)
	}

	var out []Entry
	for _, path := range names {
		body, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var e Entry
		if err := json.Unmarshal(body, &e); err != nil {
			_ = os.Remove(path)
			continue
		}
		if !alive(e.PID) {
			_ = os.Remove(path)
			continue
		}
		e.path = path
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Started.After(out[j].Started) })
	return out, nil
}

// Find returns the server matching addr, or the only one if addr is empty.
func Find(addr string) (Entry, error) {
	entries, err := List()
	if err != nil {
		return Entry{}, err
	}
	if len(entries) == 0 {
		return Entry{}, ErrNoServer
	}
	if addr == "" {
		if len(entries) == 1 {
			return entries[0], nil
		}
		var addrs []string
		for _, e := range entries {
			addrs = append(addrs, fmt.Sprintf("%s (%s)", e.Addr, e.Project))
		}
		return Entry{}, fmt.Errorf("several servers are running; choose one with -addr:\n  %s",
			strings.Join(addrs, "\n  "))
	}
	for _, e := range entries {
		if e.Addr == addr || strings.HasSuffix(e.Addr, ":"+strings.TrimPrefix(addr, ":")) {
			return e, nil
		}
	}
	return Entry{}, fmt.Errorf("%w at %s", ErrNoServer, addr)
}

// alive reports whether a pid exists. Signal 0 performs the permission and
// existence checks without delivering anything.
func alive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	if err == nil {
		return true
	}
	// EPERM means it exists but belongs to someone else, which for a file only
	// we can read means the pid was recycled. Treat it as gone.
	return false
}

// PortOf extracts the port from an address, for display.
func PortOf(addr string) string {
	i := strings.LastIndex(addr, ":")
	if i < 0 {
		return ""
	}
	if _, err := strconv.Atoi(addr[i+1:]); err != nil {
		return ""
	}
	return addr[i+1:]
}
