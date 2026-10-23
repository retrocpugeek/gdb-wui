package debugger

// Which file the decompiler describes.
//
// Normally the program loaded from the file tree (exe.load). But gdb can have
// a program gdb-wui never loaded: `target remote` to a gdbserver started with
// one, `attach`, or a `file` typed in the console. The decompiler then asked
// for a program that was not there, and the pane said no decompiler was
// configured — measured with gdbserver running dropbear in the try-online
// guest, where -kuna was plainly on the command line.
//
// So when gdb-wui has loaded nothing, gdb is asked what it has. A path on this
// machine is read where it is: gdb already has that file open, and the
// decompiler only reads it. A `target:` path is a file on the far side of a
// gdbserver, which may be another machine with a different file at the same
// path — so it is copied from there, with gdb's own `remote get`, into a
// directory of this session's, and the copy is what is decompiled.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/retrocpugeek/gdb-wui/internal/wire"
)

// decompProgram is the file the decompiler should describe, and its sha256,
// or empty when there is none yet.
func (s *Session) decompProgram() (abs, sha string) {
	if s.st.exePath != "" {
		if s.files == nil {
			return "", ""
		}
		abs, err := s.files.AbsPath(s.st.exePath)
		if err != nil {
			return "", ""
		}
		return abs, s.st.exeSHA256
	}
	s.decomp.mu.Lock()
	defer s.decomp.mu.Unlock()
	return s.decomp.gdbAbs, s.decomp.gdbSHA
}

// learnGDBProgram asks gdb which program it has, when gdb-wui loaded none,
// and fetches it for the decompiler if it is on a remote target. A different
// program from the last one asked about puts a per-program decompiler back to
// not started, as loading one from the tree does (switchSidecar).
func (s *Session) learnGDBProgram(ctx context.Context) {
	if s.st.exePath != "" {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	rec, werr := s.send(ctx, "-list-thread-groups")
	if werr != nil {
		return
	}
	exe := ""
	if groups, ok := rec.Results.List("groups"); ok {
		for _, g := range groups {
			if e, _ := g.StrOK("executable"); e != "" {
				exe = e
				break
			}
		}
	}
	s.decomp.mu.Lock()
	known := s.decomp.gdbExe
	s.decomp.mu.Unlock()
	if exe == known {
		return
	}
	abs, sha, err := s.fetchGDBProgram(ctx, exe)
	if err != nil {
		s.decompLog(wire.DecompLogWarn, "%s", err)
	}
	s.decomp.mu.Lock()
	s.decomp.gdbExe, s.decomp.gdbAbs, s.decomp.gdbSHA = exe, abs, sha
	s.decomp.mu.Unlock()
	if known != "" || exe != "" {
		s.switchSidecar()
	}
}

// fetchGDBProgram makes gdb's executable readable here: a local path as it
// is, a target: one copied over the remote protocol.
func (s *Session) fetchGDBProgram(ctx context.Context, exe string) (abs, sha string, err error) {
	if exe == "" {
		return "", "", nil
	}
	remote, isRemote := strings.CutPrefix(exe, "target:")
	if !isRemote {
		abs = exe
	} else {
		dir, err := s.remoteProgramDir()
		if err != nil {
			return "", "", fmt.Errorf("cannot copy %s from the target: %v", remote, err)
		}
		abs = filepath.Join(dir, filepath.Base(remote))
		cmd := "remote get " + gdbQuote(remote) + " " + gdbQuote(abs)
		if _, werr := s.send(ctx, "-interpreter-exec console "+quote(cmd)); werr != nil {
			return "", "", fmt.Errorf("cannot copy %s from the target for the decompiler: %s",
				remote, werr.Message)
		}
		s.decompLog(wire.DecompLogInfo, "copied %s from the target, for the decompiler", remote)
	}
	f, err := os.Open(abs)
	if err != nil {
		return "", "", fmt.Errorf("the decompiler cannot read %s: %v", exe, err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", "", fmt.Errorf("the decompiler cannot read %s: %v", exe, err)
	}
	return abs, hex.EncodeToString(h.Sum(nil)), nil
}

// remoteProgramDir is where programs copied from a target go, made on first
// use and removed with the session (closeDecomp).
func (s *Session) remoteProgramDir() (string, error) {
	s.decomp.mu.Lock()
	defer s.decomp.mu.Unlock()
	if s.decomp.fetchDir != "" {
		return s.decomp.fetchDir, nil
	}
	dir, err := os.MkdirTemp("", "gdb-wui-target-")
	if err != nil {
		return "", err
	}
	s.decomp.fetchDir = dir
	return dir, nil
}
