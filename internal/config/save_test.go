package config_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/retrocpugeek/gdb-wui/internal/config"
)

func read(t *testing.T, path string) map[string]any {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("the file we wrote is not valid JSON: %v\n%s", err, body)
	}
	return got
}

// TestSaveWritesNonDefaultsOnly.
//
// A file listing every flag would freeze today's defaults and bury the two
// settings that were actually chosen.
func TestSaveWritesNonDefaultsOnly(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	o := newOpts()
	if err := o.fs.Parse([]string{"-gdb", "gdb-multiarch", "-open=false"}); err != nil {
		t.Fatal(err)
	}
	written, backup, err := config.Save(o.fs, "")
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	if want := filepath.Join(dir, config.FileName); written != want {
		t.Errorf("wrote %q, want %q", written, want)
	}
	if backup != "" {
		t.Errorf("backed up %q with no file to replace", backup)
	}

	got := read(t, written)
	if len(got) != 2 {
		t.Errorf("wrote %d keys, want only the two that were set: %v", len(got), got)
	}
	if got["gdb"] != "gdb-multiarch" {
		t.Errorf("gdb = %v", got["gdb"])
	}
	if got["open"] != false {
		t.Errorf("open = %v (%T), want the JSON boolean false", got["open"], got["open"])
	}
	if _, ok := got["project"]; ok {
		t.Error("project was written despite being left at its default")
	}
}

// TestSaveRoundTrips is the strong one: everything Save writes, Load must read
// back to the same values. A type written the wrong way — a duration as a
// number, a bool as a string — passes an eyeball and fails here.
func TestSaveRoundTrips(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	before := newOpts()
	args := []string{
		"-gdb", "gdb-multiarch",
		"-project", "/some/where",
		"-open=false",
		"-idle-exit", "90s",
	}
	if err := before.fs.Parse(args); err != nil {
		t.Fatal(err)
	}
	if _, _, err := config.Save(before.fs, ""); err != nil {
		t.Fatalf("Save: %v", err)
	}

	after := newOpts()
	if err := after.fs.Parse(nil); err != nil {
		t.Fatal(err)
	}
	if _, err := config.Load(after.fs, "", false); err != nil {
		t.Fatalf("Load: %v", err)
	}

	if after.gdb != before.gdb {
		t.Errorf("gdb: saved %q, read back %q", before.gdb, after.gdb)
	}
	if after.project != before.project {
		t.Errorf("project: saved %q, read back %q", before.project, after.project)
	}
	if after.open != before.open {
		t.Errorf("open: saved %v, read back %v", before.open, after.open)
	}
	if after.idleExit != before.idleExit {
		t.Errorf("idle-exit: saved %s, read back %s", before.idleExit, after.idleExit)
	}
	if after.idleExit != 90*time.Second {
		t.Errorf("idle-exit = %s, want 90s", after.idleExit)
	}
}

// TestSaveWritesTheEffectiveConfig.
//
// Saving only what was typed would be wrong, and the reason is the
// first-found-wins rule: a local file that omitted the gdb which came from the
// home config would shadow that config and silently drop the setting on the
// next run.
func TestSaveWritesTheEffectiveConfig(t *testing.T) {
	wd, home := t.TempDir(), t.TempDir()
	writeHome(t, home, `{"gdb":"gdb-multiarch"}`)
	inDir(t, wd, home)

	o := newOpts()
	if err := o.fs.Parse([]string{"-project", "/typed"}); err != nil {
		t.Fatal(err)
	}
	if _, err := config.Load(o.fs, "", false); err != nil {
		t.Fatalf("Load: %v", err)
	}
	written, _, err := config.Save(o.fs, "")
	if err != nil {
		t.Fatalf("Save: %v", err)
	}

	got := read(t, written)
	if got["project"] != "/typed" {
		t.Errorf("project = %v, want the command line's value", got["project"])
	}
	if got["gdb"] != "gdb-multiarch" {
		t.Errorf("gdb = %v, want the value the home config supplied; "+
			"a local file without it would shadow that config", got["gdb"])
	}
}

func TestSaveToAnExplicitPath(t *testing.T) {
	t.Chdir(t.TempDir())
	want := filepath.Join(t.TempDir(), "somewhere", "mine.json")
	if err := os.MkdirAll(filepath.Dir(want), 0o755); err != nil {
		t.Fatal(err)
	}

	o := newOpts()
	_ = o.fs.Parse([]string{"-gdb", "gdb-multiarch"})
	written, _, err := config.Save(o.fs, want)
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	if written != want {
		t.Errorf("wrote %q, want %q", written, want)
	}
	if read(t, want)["gdb"] != "gdb-multiarch" {
		t.Error("the explicit path did not get the settings")
	}
	if _, err := os.Stat(config.FileName); err == nil {
		t.Error("a file was also written to the working directory")
	}
}

// TestSaveNeverWritesActionFlags. -version in a config file is refused on the
// way in, so writing one would produce a file that cannot be read.
func TestSaveNeverWritesActionFlags(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	o := newOpts()
	_ = o.fs.Parse([]string{"-gdb", "gdb-multiarch", "-version"})
	written, _, err := config.Save(o.fs, "")
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	if _, ok := read(t, written)["version"]; ok {
		t.Fatal("version was written to the config file")
	}

	// And what was written is readable, which is the property that matters.
	back := newOpts()
	_ = back.fs.Parse(nil)
	if _, err := config.Load(back.fs, "", false); err != nil {
		t.Fatalf("the file we wrote cannot be read back: %v", err)
	}
}

// TestSaveKeepsTheOldFile. Re-saving after changing one flag is the second
// thing anyone does; losing a config assembled by hand is not an acceptable
// price for it.
func TestSaveKeepsTheOldFile(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	existing := write(t, dir, `{"gdb":"the-old-one","project":"/old"}`)

	o := newOpts()
	_ = o.fs.Parse([]string{"-gdb", "the-new-one"})
	written, backup, err := config.Save(o.fs, "")
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	if written != existing {
		t.Errorf("wrote %q, want %q", written, existing)
	}
	if backup != existing+".bak" {
		t.Errorf("backup = %q, want %q", backup, existing+".bak")
	}
	if read(t, written)["gdb"] != "the-new-one" {
		t.Error("the new value was not written")
	}
	if got := read(t, backup); got["gdb"] != "the-old-one" || got["project"] != "/old" {
		t.Errorf("the backup does not hold the previous file: %v", got)
	}
}

// TestSavedFileIsReadableByUsOnly: what Save writes must pass the ownership
// check on the way back in, or the feature produces files it then refuses.
func TestSavedFileIsReadableByUsOnly(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	o := newOpts()
	_ = o.fs.Parse([]string{"-gdb", "gdb-multiarch"})
	written, _, err := config.Save(o.fs, "")
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	info, err := os.Stat(written)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("mode %04o, want 0600", mode)
	}

	back := newOpts()
	_ = back.fs.Parse(nil)
	if _, err := config.Load(back.fs, "", false); err != nil {
		t.Fatalf("the file we wrote was refused on the way back in: %v", err)
	}
}

// TestSaveIsStable: the same settings produce the same bytes, so a config file
// under version control does not churn.
func TestSaveIsStable(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	first := filepath.Join(dir, "a.json")
	second := filepath.Join(dir, "b.json")
	for _, path := range []string{first, second} {
		o := newOpts()
		_ = o.fs.Parse([]string{"-gdb", "g", "-project", "p", "-open=false", "-idle-exit", "1m"})
		if _, _, err := config.Save(o.fs, path); err != nil {
			t.Fatalf("Save: %v", err)
		}
	}
	a, _ := os.ReadFile(first)
	b, _ := os.ReadFile(second)
	if string(a) != string(b) {
		t.Errorf("two saves of the same settings differ:\n%s\n---\n%s", a, b)
	}
	if !strings.HasSuffix(string(a), "\n") {
		t.Error("the file does not end in a newline")
	}
}

func TestSaveToAnUnwritableDirectoryFails(t *testing.T) {
	t.Chdir(t.TempDir())
	o := newOpts()
	_ = o.fs.Parse([]string{"-gdb", "g"})
	if _, _, err := config.Save(o.fs, "/proc/nope/gdb-wui.json"); err == nil {
		t.Fatal("saving into an unwritable directory reported success")
	}
}
