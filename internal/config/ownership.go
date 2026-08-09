package config

import (
	"fmt"
	"io/fs"
	"os"
	"syscall"
)

// checkOwnership refuses a config file that somebody else could have written.
//
// This matters more here than it would for most tools. A config file may set
// `gdb`, which names an executable that gdb-wui then runs with the user's full
// privileges, and the working directory is searched — so a gdb-wui.json
// committed to a repository would otherwise choose the program a bare
// `gdb-wui` executes. That is the same shape as direnv's .envrc or git's
// core.fsmonitor, and it is worth two syscalls to narrow.
//
// The rule is the one internal/runfile already relies on: a file only the
// current user can write is as trustworthy as the user, who can run anything
// anyway.
func checkOwnership(path string, info fs.FileInfo) error {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		// Not a platform whose ownership we can read. gdb-wui is Linux-only,
		// so this is unreachable in practice; skipping the check silently
		// would be the wrong way to handle being wrong about that.
		return fmt.Errorf("config: %s: cannot determine the file's owner", path)
	}

	if uid := os.Getuid(); int(stat.Uid) != uid {
		return fmt.Errorf(
			"config: %s is owned by uid %d, not by you (uid %d); "+
				"refusing to read it because a config file can choose which gdb to run. "+
				"Pass -no-config to ignore it, or -config with a file of your own",
			path, stat.Uid, uid)
	}

	mode := info.Mode().Perm()

	// World-writable is refused outright: anyone with an account can rewrite it.
	if mode&0o002 != 0 {
		return writableError(path, mode, "others")
	}

	// Group-writable is only refused when the group is not the user's own.
	//
	// Refusing it outright was the first version, and testing it showed why
	// that is wrong: the default umask here is 0002, so an ordinary
	// `printf > gdb-wui.json` produces mode 0664 and every config file a user
	// created would have been rejected on first use. On a distribution with
	// per-user groups — the Debian and Ubuntu default, USERGROUPS_ENAB — the
	// group has exactly one member and group-writable grants nobody anything.
	//
	// A file whose group is some *other* group is a different matter, since
	// that group may have members, so that case is still refused.
	if mode&0o020 != 0 && int(stat.Gid) != os.Getgid() {
		return writableError(path, mode, fmt.Sprintf("group %d, which is not yours", stat.Gid))
	}
	return nil
}

func writableError(path string, mode fs.FileMode, who string) error {
	return fmt.Errorf(
		"config: %s is writable by %s (mode %04o); "+
			"refusing to read it because a config file can choose which gdb to run. "+
			"Run: chmod go-w %s",
		path, who, mode, path)
}
