package runfile

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// isolate points the package at a temp runtime directory.
func isolate(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", dir)
	return filepath.Join(dir, "gdb-wui")
}

func TestWriteIsPrivate(t *testing.T) {
	isolate(t)
	path, err := Write(Entry{
		PID: os.Getpid(), Addr: "127.0.0.1:8765",
		Project: "/tmp/p", MintSecret: "s3cret", Started: time.Now(),
	})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}

	// The file permission is the authentication for the mint endpoint. If this
	// is ever not 0600, every local user can obtain a login link.
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("mode = %04o, want 0600 — the secret would be world-readable", perm)
	}

	dirInfo, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if perm := dirInfo.Mode().Perm(); perm != 0o700 {
		t.Errorf("directory mode = %04o, want 0700", perm)
	}
}

func TestFindRoundTrip(t *testing.T) {
	isolate(t)
	want := Entry{
		PID: os.Getpid(), Addr: "127.0.0.1:8765",
		Project: "/tmp/p", MintSecret: "s3cret", Started: time.Now(),
	}
	if _, err := Write(want); err != nil {
		t.Fatal(err)
	}

	got, err := Find("")
	if err != nil {
		t.Fatalf("Find: %v", err)
	}
	if got.Addr != want.Addr || got.MintSecret != want.MintSecret || got.Project != want.Project {
		t.Errorf("got %+v, want %+v", got, want)
	}
	if got.Path() == "" {
		t.Error("Path is empty; the caller cannot clean up a stale entry")
	}
}

// TestStaleEntriesAreRemoved: a server killed with SIGKILL leaves its file
// behind, and -print-url must not be sent to a dead port.
func TestStaleEntriesAreRemoved(t *testing.T) {
	dir := isolate(t)
	if _, err := Write(Entry{
		PID: 999999, Addr: "127.0.0.1:9999", MintSecret: "x", Started: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := Find(""); !errors.Is(err, ErrNoServer) {
		t.Errorf("err = %v, want ErrNoServer", err)
	}
	left, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 0 {
		t.Errorf("%d stale files remain: %v", len(left), left)
	}
}

func TestCorruptEntriesAreRemoved(t *testing.T) {
	dir := isolate(t)
	if _, err := Dir(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "junk.json"), []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Find(""); !errors.Is(err, ErrNoServer) {
		t.Errorf("err = %v, want ErrNoServer", err)
	}
	left, _ := filepath.Glob(filepath.Join(dir, "*.json"))
	if len(left) != 0 {
		t.Errorf("corrupt file was kept: %v", left)
	}
}

// TestSeveralServersNeedAnAddr: guessing which one the user meant would send a
// login link to the wrong project.
func TestSeveralServersNeedAnAddr(t *testing.T) {
	isolate(t)
	for _, addr := range []string{"127.0.0.1:8765", "127.0.0.1:8766"} {
		if _, err := Write(Entry{
			PID: os.Getpid(), Addr: addr, Project: "/tmp/" + addr,
			MintSecret: "s", Started: time.Now(),
		}); err != nil {
			t.Fatal(err)
		}
	}

	_, err := Find("")
	if err == nil {
		t.Fatal("Find picked one of two servers instead of asking")
	}
	if !strings.Contains(err.Error(), "-addr") {
		t.Errorf("err = %v; it should tell the user how to disambiguate", err)
	}

	got, err := Find("127.0.0.1:8766")
	if err != nil {
		t.Fatalf("Find by address: %v", err)
	}
	if got.Addr != "127.0.0.1:8766" {
		t.Errorf("addr = %q", got.Addr)
	}

	// A bare port is accepted too, since that is what a user remembers.
	if got, err := Find("8765"); err != nil || got.Addr != "127.0.0.1:8765" {
		t.Errorf("Find(\"8765\") = %+v, %v", got, err)
	}
}

func TestRemoveIsIdempotent(t *testing.T) {
	isolate(t)
	path, err := Write(Entry{PID: os.Getpid(), Addr: "127.0.0.1:1", Started: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if err := Remove(path); err != nil {
		t.Fatalf("first Remove: %v", err)
	}
	if err := Remove(path); err != nil {
		t.Errorf("second Remove: %v", err)
	}
	if err := Remove(""); err != nil {
		t.Errorf("Remove(\"\"): %v", err)
	}
}
