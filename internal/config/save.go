package config

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// DefaultSave is the value -save-config carries when given without a path,
// which the flag package spells as the string "true" for a boolean-like flag.
const DefaultSave = "true"

// Save writes the settings currently in fs to a config file, and returns the
// path it wrote and the path of any backup it made.
//
// What it writes is the **effective** configuration — every setting that
// differs from its default, whether it arrived on the command line or from a
// config file that was already loaded — not just what was typed.
//
// That distinction matters because the first file found wins. Saving only the
// command line into ./gdb-wui.json, while ~/.config/gdb-wui/gdb-wui.json
// supplied the gdb, would produce a local file that shadows the one it came
// from and silently drops that setting on the next run.
//
// Defaults are left out. A file listing every flag would freeze today's
// defaults and bury the two settings that were actually chosen.
//
// VisitAll rather than Visit, filtered on the default. Visit happens to give
// the same answer today, because Load applies a file through FlagSet.Set and
// that marks a flag as set — but then this would depend on how Load works
// rather than on what the flags hold.
func Save(fs_ *flag.FlagSet, path string) (written, backup string, err error) {
	if path == "" || path == DefaultSave {
		wd, err := os.Getwd()
		if err != nil {
			return "", "", fmt.Errorf("config: getwd: %w", err)
		}
		path = filepath.Join(wd, FileName)
	}

	settings := map[string]any{}
	var bad error
	fs_.VisitAll(func(f *flag.Flag) {
		if bad != nil || notConfigurable[f.Name] || f.Name == SaveFlag {
			return
		}
		if f.Value.String() == f.DefValue {
			return
		}
		v, err := value(f)
		if err != nil {
			bad = fmt.Errorf("config: %q: %w", f.Name, err)
			return
		}
		settings[f.Name] = v
	})
	if bad != nil {
		return "", "", bad
	}

	// MarshalIndent sorts a map's keys, so the file is stable between runs and
	// a diff shows what changed rather than what moved.
	body, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return "", "", fmt.Errorf("config: %w", err)
	}
	body = append(body, '\n')

	backup, err = keepExisting(path)
	if err != nil {
		return "", "", err
	}
	if err := writeAtomic(path, body); err != nil {
		return "", "", err
	}
	return path, backup, nil
}

// value renders one flag as the JSON type that Load will read back.
//
// The flag package's own Values implement Getter, so the concrete Go type is
// available rather than guessed from the string. A duration goes out as a
// string, since JSON has no duration type and Load feeds it back through
// flag.Duration.
func value(f *flag.Flag) (any, error) {
	g, ok := f.Value.(flag.Getter)
	if !ok {
		return f.Value.String(), nil
	}
	switch v := g.Get().(type) {
	case bool:
		return v, nil
	case string:
		return v, nil
	case time.Duration:
		return v.String(), nil
	case int:
		return v, nil
	case int64:
		return v, nil
	case uint:
		return v, nil
	case uint64:
		return v, nil
	case float64:
		return v, nil
	default:
		return nil, fmt.Errorf("cannot write a %T to a config file", v)
	}
}

// keepExisting copies a file that is about to be replaced to <path>.bak.
//
// Overwriting is allowed, because re-saving after changing one flag is the
// second thing anyone does. Losing a config someone assembled by hand is not
// an acceptable price for that, and a copy costs one write.
func keepExisting(path string) (string, error) {
	body, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("config: %s: %w", path, err)
	}
	backup := path + ".bak"
	if err := os.WriteFile(backup, body, 0o600); err != nil {
		return "", fmt.Errorf("config: writing %s: %w", backup, err)
	}
	return backup, nil
}

// writeAtomic writes through a temporary file in the same directory, so an
// interrupted save leaves the previous file intact rather than a truncated one.
//
// Mode 0600, which is also what the ownership check on the way back in wants.
func writeAtomic(path string, body []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".gdb-wui-config-*")
	if err != nil {
		return fmt.Errorf("config: writing %s: %w", path, err)
	}
	defer os.Remove(tmp.Name())

	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("config: writing %s: %w", path, err)
	}
	if _, err := tmp.Write(body); err != nil {
		tmp.Close()
		return fmt.Errorf("config: writing %s: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("config: writing %s: %w", path, err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("config: writing %s: %w", path, err)
	}
	return nil
}
