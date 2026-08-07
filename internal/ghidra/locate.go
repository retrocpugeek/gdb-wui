package ghidra

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Finding a Ghidra installation.
//
// Ghidra is not on PATH and has no single canonical location: it is a zip you
// unpack somewhere. So unlike gdb, which is `exec.LookPath("gdb")`, this has to
// go looking — and must fail with a message that says where it looked, because
// "Ghidra not found" on a machine that has Ghidra is a maddening thing to read.

// analyzeHeadless is the entry point under an installation directory.
const analyzeHeadless = "support/analyzeHeadless"

// EnvInstall is the variable Ghidra's own tooling uses. Honouring it means a
// machine already set up for Ghidra development needs no flag.
const EnvInstall = "GHIDRA_INSTALL_DIR"

// Install describes a located Ghidra.
type Install struct {
	// Dir is the installation root.
	Dir string
	// Headless is the absolute path to support/analyzeHeadless.
	Headless string
	// Version is read from the application properties, empty if unreadable.
	// It is part of the cache key: two Ghidra releases decompile differently,
	// and serving one's output as the other's is a silent wrong answer.
	Version string
}

// Locate finds a Ghidra installation, preferring an explicit path.
//
// The search order is explicit flag, then environment, then the places the
// distribution is conventionally unpacked. A glob is included because the
// official zip unpacks to a version-stamped directory, which is exactly the
// thing a user will not have renamed.
func Locate(explicit string) (*Install, error) {
	var tried []string

	consider := func(dir string) (*Install, bool) {
		if dir == "" {
			return nil, false
		}
		dir = os.ExpandEnv(dir)
		abs, err := filepath.Abs(dir)
		if err != nil {
			return nil, false
		}
		tried = append(tried, abs)
		headless := filepath.Join(abs, analyzeHeadless)
		info, err := os.Stat(headless)
		if err != nil || info.IsDir() || info.Mode()&0o111 == 0 {
			return nil, false
		}
		return &Install{Dir: abs, Headless: headless, Version: readVersion(abs)}, true
	}

	if explicit != "" {
		if in, ok := consider(explicit); ok {
			return in, nil
		}
		// An explicit path that does not work is an error, not a reason to go
		// hunting: the user said where it is and is owed a straight answer.
		return nil, fmt.Errorf("ghidra: %s is not a Ghidra installation (no %s)",
			explicit, analyzeHeadless)
	}

	if in, ok := consider(os.Getenv(EnvInstall)); ok {
		return in, nil
	}

	home, _ := os.UserHomeDir()
	candidates := []string{
		"/opt/ghidra",
		"/usr/share/ghidra",
		"/usr/local/ghidra",
	}
	if home != "" {
		candidates = append(candidates, filepath.Join(home, "ghidra"))
	}
	for _, dir := range candidates {
		if in, ok := consider(dir); ok {
			return in, nil
		}
	}

	// The version-stamped directory the official zip unpacks to. Newest first,
	// so a machine with several releases gets the one most likely intended.
	var globbed []string
	for _, pattern := range []string{"/opt/ghidra_*", "/usr/share/ghidra_*"} {
		m, _ := filepath.Glob(pattern)
		globbed = append(globbed, m...)
	}
	if home != "" {
		m, _ := filepath.Glob(filepath.Join(home, "ghidra_*"))
		globbed = append(globbed, m...)
	}
	sort.Sort(sort.Reverse(sort.StringSlice(globbed)))
	for _, dir := range globbed {
		if in, ok := consider(dir); ok {
			return in, nil
		}
	}

	return nil, fmt.Errorf("ghidra: no installation found; set %s or pass -ghidra. Looked in: %s",
		EnvInstall, strings.Join(tried, ", "))
}

// readVersion pulls the version out of the application properties. A missing
// or malformed file is not an error: it costs a cache key, not the feature.
func readVersion(dir string) string {
	body, err := os.ReadFile(filepath.Join(dir, "Ghidra", "application.properties"))
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(body), "\n") {
		name, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if ok && strings.TrimSpace(name) == "application.version" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

// CheckProjectPath rejects a path Ghidra will not accept as a project location.
//
// Ghidra refuses any path element beginning with a dot — not merely the last
// one — with "Path element starting with '.' is not permitted". Measured: a
// location of .../x/.hidden/sub/ghidra fails exactly as .../x/.gdbwui/ghidra
// does, while the same tree without dots imports fine.
//
// That rules out every conventional per-user cache location, since $XDG_CACHE_HOME
// is ~/.cache and $XDG_STATE_HOME is ~/.local/state. It also means a project
// living under a dotted directory cannot hold its own cache.
//
// Checked here rather than left to Ghidra, because Ghidra's own message names
// neither the path nor the offending element, and arrives after a JVM start.
func CheckProjectPath(path string) error {
	abs, err := filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("ghidra: %s: %w", path, err)
	}
	for _, part := range strings.Split(abs, string(filepath.Separator)) {
		if strings.HasPrefix(part, ".") && part != "." && part != ".." {
			return fmt.Errorf(
				"ghidra: cannot use %s as a project location: the element %q starts "+
					"with a dot, and Ghidra refuses any such path element", abs, part)
		}
	}
	return nil
}
