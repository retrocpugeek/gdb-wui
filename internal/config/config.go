// Package config reads a JSON config file and applies it to a flag set.
//
// The keys are the flag names, so there is one vocabulary rather than two, no
// mapping table to fall out of step, and docs/reference/flags.md doubles as the
// list of what a config file may contain.
//
// Values are applied with FlagSet.Set, which means the flag package does every
// conversion and every validation. A duration in the file is parsed by the same
// code that parses one on the command line, so the two cannot disagree.
package config

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// FileName is the config file's name in both search locations.
//
// Undotted, so that a project-local config appears in the file tree gdb-wui
// serves rather than being invisible in the directory it configures.
const FileName = "gdb-wui.json"

// Dir is the per-user config directory, honouring $XDG_CONFIG_HOME.
//
// Unlike the decompilation cache, a dotted path is fine here: nothing hands
// this to Ghidra, which is the one component that refuses them.
func Dir() (string, error) {
	if base := os.Getenv("XDG_CONFIG_HOME"); base != "" {
		return filepath.Join(base, "gdb-wui"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("config: locating the home directory: %w", err)
	}
	return filepath.Join(home, ".config", "gdb-wui"), nil
}

// SaveFlag is the flag that writes a config file. Named here because Save has
// to leave it out of what it writes: a config that saved itself on every run
// would be a surprise.
const SaveFlag = "save-config"

// notConfigurable are flags that name an action rather than a setting. Putting
// one in a file would mean a config that runs a different program than the one
// asked for, or that exits before doing anything.
var notConfigurable = map[string]bool{
	"version":   true,
	"print-url": true,
	"config":    true,
	"no-config": true,
	// The MCP bridge joins a running server instead of starting one, so a
	// config file that turned it on would make `gdb-wui` stop being a way to
	// start gdb-wui. Its two permission flags stay out for a sharper reason: a
	// file that quietly grants an agent the right to run your program is
	// exactly the consent that has to be given deliberately, every time.
	"mcp":          true,
	"mcp-annotate": true,
	"mcp-run":      true,
	SaveFlag:       true,
}

// Load applies a config file to fs and returns the file it used, or "" if there
// was none.
//
// Call it after fs.Parse. Flags given on the command line are left alone: which
// ones those are comes from fs.Visit, which visits only the flags actually set.
// Inferring it from the value instead would get -open wrong, since its default
// is true and "not given" and "given as true" produce the same value.
//
// explicit names a file to use instead of searching. disabled skips the search
// entirely.
func Load(fs_ *flag.FlagSet, explicit string, disabled bool) (string, error) {
	if disabled {
		if explicit != "" {
			return "", errors.New("config: -config and -no-config contradict each other")
		}
		return "", nil
	}

	path := explicit
	if path == "" {
		found, err := discover()
		if err != nil {
			return "", err
		}
		if found == "" {
			return "", nil
		}
		path = found
	}

	body, err := read(path, explicit != "")
	if err != nil || body == nil {
		return "", err
	}
	if err := apply(fs_, path, body); err != nil {
		return "", err
	}
	return path, nil
}

// discover looks in the working directory, then in the per-user config
// directory. The first file found is the one used; the other is not read, so
// the settings in effect are always those of a single file.
func discover() (string, error) {
	wd, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("config: getwd: %w", err)
	}
	candidates := []string{filepath.Join(wd, FileName)}

	// A missing home directory is not an error: it only means there is no
	// per-user config to find.
	if dir, err := Dir(); err == nil {
		candidates = append(candidates, filepath.Join(dir, FileName))
	}

	for _, c := range candidates {
		info, err := os.Stat(c)
		if err != nil || info.IsDir() {
			continue
		}
		return c, nil
	}
	return "", nil
}

// read returns the file's bytes, or nil when a discovered file has gone away
// between the search and the open.
//
// A file named with -config must exist. Falling back to the search would mean
// running with settings other than the ones asked for, which is the failure a
// config file is supposed to prevent.
func read(path string, wasExplicit bool) ([]byte, error) {
	info, err := os.Stat(path)
	if err != nil {
		if wasExplicit {
			return nil, fmt.Errorf("config: %s: %w", path, err)
		}
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("config: %s: %w", path, err)
	}
	if err := checkOwnership(path, info); err != nil {
		return nil, err
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config: %s: %w", path, err)
	}
	return body, nil
}

// apply sets every key in the file that was not given on the command line.
func apply(fs_ *flag.FlagSet, path string, body []byte) error {
	dec := json.NewDecoder(strings.NewReader(string(body)))
	// UseNumber so that a large integer is not routed through float64, which
	// would silently round it.
	dec.UseNumber()

	var raw map[string]any
	if err := dec.Decode(&raw); err != nil {
		return fmt.Errorf("config: %s: %w", path, err)
	}
	if dec.More() {
		return fmt.Errorf("config: %s: trailing content after the JSON object", path)
	}

	// Flags set on the command line win, so collect them and skip those keys.
	given := map[string]bool{}
	fs_.Visit(func(f *flag.Flag) { given[f.Name] = true })

	// Sorted, so that an error names the same key on every run and a test can
	// assert on it.
	keys := make([]string, 0, len(raw))
	for k := range raw {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	for _, key := range keys {
		if notConfigurable[key] {
			return fmt.Errorf("config: %s: %q cannot be set in a config file; "+
				"it is a command-line action", path, key)
		}
		if fs_.Lookup(key) == nil {
			return fmt.Errorf("config: %s: unknown key %q%s", path, key, nearest(fs_, key))
		}
		if given[key] {
			continue
		}
		// A list for a flag that accumulates: each element is one occurrence,
		// applied in the order written, which is the same thing repeating the
		// flag on the command line does.
		if list, ok := raw[key].([]any); ok {
			if !repeatable(fs_.Lookup(key)) {
				return fmt.Errorf("config: %s: %q takes one value, not a list", path, key)
			}
			for i, item := range list {
				text, err := literal(item)
				if err != nil {
					return fmt.Errorf("config: %s: %q[%d]: %w", path, key, i, err)
				}
				if err := fs_.Set(key, text); err != nil {
					return fmt.Errorf("config: %s: %q[%d]: %w", path, key, i, err)
				}
			}
			continue
		}
		text, err := literal(raw[key])
		if err != nil {
			return fmt.Errorf("config: %s: %q: %w", path, key, err)
		}
		// Set does the conversion and the validation, so a bad duration or a
		// bad boolean fails here with the flag package's own message.
		if err := fs_.Set(key, text); err != nil {
			return fmt.Errorf("config: %s: %q: %w", path, key, err)
		}
	}
	return nil
}

// repeatable reports whether a flag accumulates rather than replaces, and so
// may be written as a list.
//
// Asked of the value rather than of a registry: a Value whose Get answers a
// []string is one that collected several, which is the same signal Save reads
// to write the list back out. Nothing has to be kept in step.
func repeatable(f *flag.Flag) bool {
	g, ok := f.Value.(flag.Getter)
	if !ok {
		return false
	}
	_, ok = g.Get().([]string)
	return ok
}

// literal renders a JSON scalar as the text the flag package expects.
//
// Only scalars: an object in a file whose keys are all flags is a mistake, and
// naming it is more use than ignoring it. A list is handled by the caller, for
// the flags that take one.
func literal(v any) (string, error) {
	switch t := v.(type) {
	case string:
		return t, nil
	case bool:
		if t {
			return "true", nil
		}
		return "false", nil
	case json.Number:
		return t.String(), nil
	case nil:
		return "", errors.New("null is not a value; remove the key to use the default")
	case []any:
		return "", errors.New("a list, for a setting that takes one value")
	default:
		return "", fmt.Errorf("want a string, number or boolean, got %T", v)
	}
}

// nearest suggests a flag name for a key that looks like a typo, since the
// commonest cause of an unknown key is one.
func nearest(fs_ *flag.FlagSet, key string) string {
	var best string
	fs_.VisitAll(func(f *flag.Flag) {
		if best != "" || notConfigurable[f.Name] {
			return
		}
		if strings.Contains(f.Name, key) || strings.Contains(key, f.Name) {
			best = f.Name
		}
	})
	if best == "" {
		return ""
	}
	return fmt.Sprintf(" (did you mean %q?)", best)
}
