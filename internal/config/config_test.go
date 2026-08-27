package config_test

import (
	"errors"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/retrocpugeek/gdb-wui/internal/config"
)

// A stand-in for the real flag set, carrying one flag of each kind the command
// actually uses: a string, a bool whose default is true, and a duration.
type opts struct {
	fs       *flag.FlagSet
	project  string
	gdb      string
	open     bool
	idleExit time.Duration
	version  bool
	run      commands
}

// commands stands in for -gdb-command: a flag that accumulates, which is what
// a JSON list in a config file is for.
type commands []string

func (c *commands) String() string { return strings.Join(*c, " ; ") }
func (c *commands) Set(v string) error {
	if v == "" {
		return errors.New("empty command")
	}
	*c = append(*c, v)
	return nil
}
func (c *commands) Get() any { return []string(*c) }

func newOpts() *opts {
	o := &opts{fs: flag.NewFlagSet("test", flag.ContinueOnError)}
	o.fs.Var(&o.run, "gdb-command", "")
	o.fs.StringVar(&o.project, "project", ".", "")
	o.fs.StringVar(&o.gdb, "gdb", "gdb", "")
	o.fs.BoolVar(&o.open, "open", true, "")
	o.fs.DurationVar(&o.idleExit, "idle-exit", 0, "")
	o.fs.BoolVar(&o.version, "version", false, "")
	o.fs.SetOutput(nopWriter{})
	return o
}

type nopWriter struct{}

func (nopWriter) Write(p []byte) (int, error) { return len(p), nil }

// inDir runs the test with wd as the working directory and home as the home
// config directory, so discovery can be driven without touching either for
// real.
func inDir(t *testing.T, wd, home string) {
	t.Helper()
	t.Chdir(wd)
	t.Setenv("XDG_CONFIG_HOME", home)
}

// writeHome puts a config where Dir() will look for it, under the gdb-wui
// subdirectory of XDG_CONFIG_HOME rather than directly in it. Getting this
// wrong makes "the home config was not read" assertions pass for the wrong
// reason, which it did.
func writeHome(t *testing.T, xdg, body string) string {
	t.Helper()
	return write(t, filepath.Join(xdg, "gdb-wui"), body)
}

func write(t *testing.T, dir, body string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, config.FileName)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestAppliesValues is the base case: every scalar kind reaches its flag, and
// the flag package does the conversion.
func TestAppliesValues(t *testing.T) {
	dir := t.TempDir()
	want := write(t, dir, `{"gdb":"gdb-multiarch","open":false,"idle-exit":"30m"}`)
	inDir(t, dir, t.TempDir())

	o := newOpts()
	if err := o.fs.Parse(nil); err != nil {
		t.Fatal(err)
	}
	got, err := config.Load(o.fs, "", false)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got != want {
		t.Errorf("loaded %q, want %q", got, want)
	}
	if o.gdb != "gdb-multiarch" {
		t.Errorf("gdb = %q", o.gdb)
	}
	if o.open {
		t.Error("open is still true; a false in the file did not reach the flag")
	}
	if o.idleExit != 30*time.Minute {
		t.Errorf("idle-exit = %s, want 30m", o.idleExit)
	}
}

// TestCommandLineWins is the whole precedence rule.
//
// -open is the case that matters: its default is true, so "given as true" and
// "not given" produce the same value and only fs.Visit can tell them apart. A
// value-based implementation passes every other case here and fails this one.
func TestCommandLineWins(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, `{"gdb":"from-config","open":false,"project":"from-config"}`)
	inDir(t, dir, t.TempDir())

	o := newOpts()
	if err := o.fs.Parse([]string{"-gdb", "from-argv", "-open=true"}); err != nil {
		t.Fatal(err)
	}
	if _, err := config.Load(o.fs, "", false); err != nil {
		t.Fatalf("Load: %v", err)
	}

	if o.gdb != "from-argv" {
		t.Errorf("gdb = %q, want the command line to win", o.gdb)
	}
	if !o.open {
		t.Error("open = false; -open=true on the command line was overwritten by the file")
	}
	// Not given on the command line, so the file still applies.
	if o.project != "from-config" {
		t.Errorf("project = %q, want the file to apply to flags not given", o.project)
	}
}

// TestWorkingDirectoryBeatsHome: the first file found is used and the other is
// not read at all.
func TestWorkingDirectoryBeatsHome(t *testing.T) {
	wd, home := t.TempDir(), t.TempDir()
	want := write(t, wd, `{"gdb":"from-wd"}`)
	writeHome(t, home, `{"gdb":"from-home","project":"from-home"}`)
	inDir(t, wd, home)

	o := newOpts()
	_ = o.fs.Parse(nil)
	got, err := config.Load(o.fs, "", false)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got != want {
		t.Errorf("loaded %q, want the working directory's %q", got, want)
	}
	if o.gdb != "from-wd" {
		t.Errorf("gdb = %q", o.gdb)
	}
	// The home file also sets project. If it had been merged rather than
	// skipped, this would be "from-home".
	if o.project != "." {
		t.Errorf("project = %q, want the home config not to have been read", o.project)
	}
}

// TestHomeUsedWhenNoLocal covers the other half of discovery.
func TestHomeUsedWhenNoLocal(t *testing.T) {
	wd, home := t.TempDir(), t.TempDir()
	want := writeHome(t, home, `{"gdb":"from-home"}`)
	inDir(t, wd, home)

	o := newOpts()
	_ = o.fs.Parse(nil)
	got, err := config.Load(o.fs, "", false)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got != want {
		t.Errorf("loaded %q, want %q", got, want)
	}
	if o.gdb != "from-home" {
		t.Errorf("gdb = %q", o.gdb)
	}
}

// TestNoConfigFileIsNotAnError: running with no config at all is the ordinary
// case and must stay silent.
func TestNoConfigFileIsNotAnError(t *testing.T) {
	inDir(t, t.TempDir(), t.TempDir())
	o := newOpts()
	_ = o.fs.Parse(nil)
	got, err := config.Load(o.fs, "", false)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got != "" {
		t.Errorf("loaded %q, want no file", got)
	}
	if o.gdb != "gdb" {
		t.Errorf("gdb = %q, want the flag default", o.gdb)
	}
}

// TestUnknownKeyIsAnError. A typo that quietly does nothing is the failure this
// is here to prevent.
func TestUnknownKeyIsAnError(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, `{"gbd":"gdb-multiarch"}`)
	inDir(t, dir, t.TempDir())

	o := newOpts()
	_ = o.fs.Parse(nil)
	_, err := config.Load(o.fs, "", false)
	if err == nil {
		t.Fatal("an unknown key was accepted")
	}
	if !strings.Contains(err.Error(), `"gbd"`) {
		t.Errorf("error does not name the key: %v", err)
	}
}

// TestUnknownKeySuggests pins what the explicit Lookup buys.
//
// Without that check an unknown key still fails, because FlagSet.Set rejects
// it — so "is it an error" cannot tell whether the check is there. What the
// check adds is the near-miss suggestion, and this is the assertion that holds
// it in place.
func TestUnknownKeySuggests(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, `{"gdbb":"gdb-multiarch"}`)
	inDir(t, dir, t.TempDir())

	o := newOpts()
	_ = o.fs.Parse(nil)
	_, err := config.Load(o.fs, "", false)
	if err == nil {
		t.Fatal("an unknown key was accepted")
	}
	if !strings.Contains(err.Error(), `did you mean "gdb"`) {
		t.Errorf("error suggests nothing for a near miss: %v", err)
	}
}

// TestActionFlagsAreRefused: -version in a file would exit before doing
// anything, every run, with no way to see why from the command line.
func TestActionFlagsAreRefused(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, `{"version":true}`)
	inDir(t, dir, t.TempDir())

	o := newOpts()
	_ = o.fs.Parse(nil)
	_, err := config.Load(o.fs, "", false)
	if err == nil {
		t.Fatal("version was accepted from a config file")
	}
	if o.version {
		t.Error("version was applied despite the error")
	}
}

func TestBadValues(t *testing.T) {
	cases := map[string]string{
		"wrong type for a duration":  `{"idle-exit":"half an hour"}`,
		"object where a scalar goes": `{"gdb":{"path":"gdb"}}`,
		"array where a scalar goes":  `{"gdb":["gdb"]}`,
		"null":                       `{"gdb":null}`,
		"a number for a bool":        `{"open":3}`,
		"malformed json":             `{"gdb":`,
		"trailing content":           `{"gdb":"a"} {"gdb":"b"}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			write(t, dir, body)
			inDir(t, dir, t.TempDir())
			o := newOpts()
			_ = o.fs.Parse(nil)
			if _, err := config.Load(o.fs, "", false); err == nil {
				t.Fatalf("%s was accepted", name)
			}
		})
	}
}

// TestExplicitFileMustExist. Falling back to the search would run with settings
// other than the ones asked for, which is the failure a config file prevents.
func TestExplicitFileMustExist(t *testing.T) {
	dir := t.TempDir()
	// A discoverable file, so a fallback would succeed and be wrong.
	write(t, dir, `{"gdb":"from-wd"}`)
	inDir(t, dir, t.TempDir())

	o := newOpts()
	_ = o.fs.Parse(nil)
	_, err := config.Load(o.fs, filepath.Join(dir, "nope.json"), false)
	if err == nil {
		t.Fatal("a missing -config file was accepted")
	}
	if o.gdb != "gdb" {
		t.Errorf("gdb = %q; it fell back to the discovered file", o.gdb)
	}
}

func TestExplicitFileIsUsed(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, `{"gdb":"from-wd"}`)
	other := filepath.Join(t.TempDir(), "elsewhere.json")
	if err := os.WriteFile(other, []byte(`{"gdb":"from-explicit"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	inDir(t, dir, t.TempDir())

	o := newOpts()
	_ = o.fs.Parse(nil)
	got, err := config.Load(o.fs, other, false)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got != other {
		t.Errorf("loaded %q, want %q", got, other)
	}
	if o.gdb != "from-explicit" {
		t.Errorf("gdb = %q, want the explicit file to beat the discovered one", o.gdb)
	}
}

func TestNoConfigSkipsDiscovery(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, `{"gdb":"from-wd"}`)
	inDir(t, dir, t.TempDir())

	o := newOpts()
	_ = o.fs.Parse(nil)
	got, err := config.Load(o.fs, "", true)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got != "" {
		t.Errorf("loaded %q with -no-config", got)
	}
	if o.gdb != "gdb" {
		t.Errorf("gdb = %q, want the flag default", o.gdb)
	}
}

// TestNoConfigWithExplicitIsAnError: the two contradict, and picking one
// silently would mean a command line that does not do what it says.
func TestNoConfigWithExplicitIsAnError(t *testing.T) {
	o := newOpts()
	_ = o.fs.Parse(nil)
	if _, err := config.Load(o.fs, "/tmp/whatever.json", true); err == nil {
		t.Fatal("-config with -no-config was accepted")
	}
}

// TestXDGConfigHomeIsHonoured. Without this the per-user location is a guess
// about where $HOME is, and a user who moves their config has no way to say so.
func TestXDGConfigHomeIsHonoured(t *testing.T) {
	xdg := t.TempDir()
	want := writeHome(t, xdg, `{"gdb":"from-xdg"}`)

	t.Chdir(t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", xdg)

	dir, err := config.Dir()
	if err != nil {
		t.Fatal(err)
	}
	if dir != filepath.Join(xdg, "gdb-wui") {
		t.Errorf("Dir() = %q, want it under XDG_CONFIG_HOME", dir)
	}

	o := newOpts()
	_ = o.fs.Parse(nil)
	got, err := config.Load(o.fs, "", false)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got != want {
		t.Errorf("loaded %q, want %q", got, want)
	}
}

// TestDirFallsBackToDotConfig: with XDG_CONFIG_HOME unset, the location is the
// conventional ~/.config/gdb-wui.
func TestDirFallsBackToDotConfig(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("HOME", "/home/somebody")
	dir, err := config.Dir()
	if err != nil {
		t.Fatal(err)
	}
	if want := "/home/somebody/.config/gdb-wui"; dir != want {
		t.Errorf("Dir() = %q, want %q", dir, want)
	}
}

// TestWorldWritableIsRefused.
//
// A config file may set `gdb`, which names a program gdb-wui then runs with the
// user's privileges, and the working directory is searched — so a writable
// gdb-wui.json chooses what a bare `gdb-wui` executes.
func TestWorldWritableIsRefused(t *testing.T) {
	dir := t.TempDir()
	path := write(t, dir, `{"gdb":"/tmp/evil"}`)
	if err := os.Chmod(path, 0o666); err != nil {
		t.Fatal(err)
	}
	inDir(t, dir, t.TempDir())

	o := newOpts()
	_ = o.fs.Parse(nil)
	_, err := config.Load(o.fs, "", false)
	if err == nil {
		t.Fatal("a world-writable config was read")
	}
	if !strings.Contains(err.Error(), "chmod") {
		t.Errorf("error does not say how to fix it: %v", err)
	}
	if o.gdb != "gdb" {
		t.Errorf("gdb = %q; the file was applied before the check", o.gdb)
	}
}

// TestGroupWritableInOwnGroupIsAccepted.
//
// The first version of the ownership check refused every group-writable file,
// which testing showed to be wrong: the default umask on this machine is 0002,
// so an ordinary redirect produces mode 0664 and every config a user created
// would have been rejected on first use. With per-user groups — the Debian and
// Ubuntu default — that group has one member and grants nobody anything.
func TestGroupWritableInOwnGroupIsAccepted(t *testing.T) {
	dir := t.TempDir()
	path := write(t, dir, `{"gdb":"fine"}`)
	if err := os.Chmod(path, 0o664); err != nil {
		t.Fatal(err)
	}
	inDir(t, dir, t.TempDir())

	o := newOpts()
	_ = o.fs.Parse(nil)
	if _, err := config.Load(o.fs, "", false); err != nil {
		t.Fatalf("mode 0664 in the user's own group was refused: %v", err)
	}
	if o.gdb != "fine" {
		t.Errorf("gdb = %q", o.gdb)
	}
}

// TestGroupWritableInAnotherGroupIsRefused is the half that still bites: a
// group that is not the user's own may have other members.
//
// Skipped unless the user is in a second group, since the test cannot create
// one. On this developer's machine and on CI there is usually one available.
func TestGroupWritableInAnotherGroupIsRefused(t *testing.T) {
	other := otherGroup(t)
	dir := t.TempDir()
	path := write(t, dir, `{"gdb":"/tmp/evil"}`)
	if err := os.Chown(path, os.Getuid(), other); err != nil {
		t.Skipf("cannot chgrp to %d: %v", other, err)
	}
	if err := os.Chmod(path, 0o664); err != nil {
		t.Fatal(err)
	}
	inDir(t, dir, t.TempDir())

	o := newOpts()
	_ = o.fs.Parse(nil)
	if _, err := config.Load(o.fs, "", false); err == nil {
		t.Fatal("a config writable by a group that is not the user's own was read")
	}
}

func otherGroup(t *testing.T) int {
	t.Helper()
	groups, err := os.Getgroups()
	if err != nil {
		t.Skipf("cannot list groups: %v", err)
	}
	for _, g := range groups {
		if g != os.Getgid() {
			return g
		}
	}
	t.Skip("the user belongs to only one group; nothing to test against")
	return 0
}

// TestOwnModeIsAccepted keeps the check above from passing vacuously: a file
// with ordinary permissions must still load.
func TestOwnModeIsAccepted(t *testing.T) {
	for _, mode := range []os.FileMode{0o600, 0o644, 0o400} {
		dir := t.TempDir()
		path := write(t, dir, `{"gdb":"fine"}`)
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
		inDir(t, dir, t.TempDir())

		o := newOpts()
		_ = o.fs.Parse(nil)
		if _, err := config.Load(o.fs, "", false); err != nil {
			t.Errorf("mode %04o was refused: %v", mode, err)
		}
		if o.gdb != "fine" {
			t.Errorf("mode %04o: gdb = %q", mode, o.gdb)
		}
	}
}

// TestDirectoryNamedLikeAConfigIsIgnored: a directory called gdb-wui.json in
// the working directory should not stop the search.
func TestDirectoryNamedLikeAConfigIsIgnored(t *testing.T) {
	wd, home := t.TempDir(), t.TempDir()
	if err := os.MkdirAll(filepath.Join(wd, config.FileName), 0o755); err != nil {
		t.Fatal(err)
	}
	want := writeHome(t, home, `{"gdb":"from-home"}`)
	inDir(t, wd, home)

	o := newOpts()
	_ = o.fs.Parse(nil)
	got, err := config.Load(o.fs, "", false)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got != want {
		t.Errorf("loaded %q, want the search to have continued to %q", got, want)
	}
}

// TestAListIsAppliedInOrder pins what a repeated flag looks like in a file.
//
// Order is the whole content of the setting: `set architecture` before `target
// remote` works and the other way round does not, so a file that applied them
// in map order or backwards would be a config that connects wrongly rather
// than one that fails.
func TestAListIsAppliedInOrder(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, `{
		"gdb-command": ["set architecture arm", "target remote 127.0.0.1:9999"]
	}`)
	inDir(t, dir, t.TempDir())

	o := newOpts()
	if err := o.fs.Parse(nil); err != nil {
		t.Fatal(err)
	}
	if _, err := config.Load(o.fs, "", false); err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := []string{"set architecture arm", "target remote 127.0.0.1:9999"}
	if len(o.run) != len(want) {
		t.Fatalf("gdb-command = %q, want %q", o.run, want)
	}
	for i := range want {
		if o.run[i] != want[i] {
			t.Errorf("gdb-command[%d] = %q, want %q", i, o.run[i], want[i])
		}
	}
}

// TestALoneValueStillWorks is the other half: a flag that takes a list takes a
// bare string too, because writing one command as an array is a pedantry
// nobody should have to observe.
func TestALoneValueStillWorks(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, `{"gdb-command": "target remote :9999"}`)
	inDir(t, dir, t.TempDir())

	o := newOpts()
	if err := o.fs.Parse(nil); err != nil {
		t.Fatal(err)
	}
	if _, err := config.Load(o.fs, "", false); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(o.run) != 1 || o.run[0] != "target remote :9999" {
		t.Errorf("gdb-command = %q", o.run)
	}
}

// TestAListOnAScalarIsRefused: two values for a setting that keeps one means
// somebody meant one of them to win, and the file does not say which. Silently
// taking the last is the reading that produces a session configured to
// something nobody wrote down.
func TestAListOnAScalarIsRefused(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, `{"gdb": ["gdb", "gdb-multiarch"]}`)
	inDir(t, dir, t.TempDir())

	o := newOpts()
	if err := o.fs.Parse(nil); err != nil {
		t.Fatal(err)
	}
	_, err := config.Load(o.fs, "", false)
	if err == nil {
		t.Fatal("a list was accepted for a setting that takes one value")
	}
	if !strings.Contains(err.Error(), "gdb") || !strings.Contains(err.Error(), "list") {
		t.Errorf("the error does not say what is wrong: %v", err)
	}
	if o.gdb != "gdb" {
		t.Errorf("gdb = %q; a refused key was applied anyway", o.gdb)
	}
}
