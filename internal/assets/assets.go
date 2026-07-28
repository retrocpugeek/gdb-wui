// Package assets serves the frontend, either from the binary or from disk.
//
// The web tree lives at internal/assets/web rather than a top-level web/
// because //go:embed cannot reach a parent directory. That constraint is why
// the layout looks the way it does; there is no root-level web/.
package assets

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"os"
	"path"
	"sort"
	"strings"
	"time"
)

// all: is required, not stylistic: without it //go:embed skips files whose
// names begin with "_" or ".", which silently drops vendored assets that use
// those names.
//
//go:embed all:web
var embedded embed.FS

func init() {
	// Browsers refuse an ES module served as anything but a JavaScript MIME
	// type, and Go's built-in table does not know .mjs on every platform. A
	// missing entry here shows up as "Failed to load module script" with a
	// blank page, which reads like a code bug rather than a MIME bug.
	for ext, typ := range map[string]string{
		".mjs":  "text/javascript; charset=utf-8",
		".js":   "text/javascript; charset=utf-8",
		".css":  "text/css; charset=utf-8",
		".map":  "application/json",
		".svg":  "image/svg+xml",
		".webm": "video/webm",
	} {
		if err := mime.AddExtensionType(ext, typ); err != nil {
			panic(fmt.Sprintf("assets: registering %s: %v", ext, err))
		}
	}
}

// Assets is a served frontend tree.
type Assets struct {
	fsys fs.FS
	// dev is true when serving from disk, which switches caching off so a
	// reload is enough to see an edit. That is the entire zero-build dev loop.
	dev bool
	// version is a hash of the whole tree, computed once at startup. embed.FS
	// has no mtimes, so without it every response would either be uncacheable
	// or wrongly cached forever across upgrades.
	version string
}

// Embedded returns the assets compiled into the binary.
func Embedded() (*Assets, error) {
	sub, err := fs.Sub(embedded, "web")
	if err != nil {
		return nil, fmt.Errorf("assets: sub: %w", err)
	}
	a := &Assets{fsys: sub}
	v, err := hashTree(sub)
	if err != nil {
		return nil, err
	}
	a.version = v
	return a, nil
}

// Dir returns assets served from a directory on disk.
//
// os.Root, so a symlink in the assets directory cannot be used to serve
// /etc/shadow through the same handler that serves stylesheets.
func Dir(dir string) (*Assets, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, fmt.Errorf("assets: opening %s: %w", dir, err)
	}
	return &Assets{fsys: root.FS(), dev: true, version: "dev"}, nil
}

// FS exposes the underlying filesystem, for tests that assert the tree's shape.
func (a *Assets) FS() fs.FS { return a.fsys }

// Version is the tree hash, used as the ETag.
func (a *Assets) Version() string { return a.version }

// Dev reports whether assets are being served from disk.
func (a *Assets) Dev() bool { return a.dev }

// Handler serves the tree.
//
// There is no SPA catch-all. A request for a file that does not exist gets a
// 404, because silently returning index.html for /js/typo.js turns a typo into
// a blank page with a 200 and no console error.
func (a *Assets) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		name := strings.TrimPrefix(r.URL.Path, "/")
		if name == "" {
			name = "index.html"
		}
		if !fs.ValidPath(name) {
			http.NotFound(w, r)
			return
		}

		f, err := a.fsys.Open(name)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		defer f.Close()

		info, err := f.Stat()
		if err != nil || info.IsDir() {
			http.NotFound(w, r)
			return
		}

		if ctype := mime.TypeByExtension(path.Ext(name)); ctype != "" {
			w.Header().Set("Content-Type", ctype)
		}

		if a.dev {
			// no-store, not no-cache: Chrome will still reuse a no-cache
			// module from memory within a navigation, which is exactly the
			// stale-JS confusion the dev loop exists to avoid.
			w.Header().Set("Cache-Control", "no-store")
		} else {
			etag := `"` + a.version + `"`
			w.Header().Set("ETag", etag)
			w.Header().Set("Cache-Control", "no-cache")
			if match := r.Header.Get("If-None-Match"); match != "" && strings.Contains(match, a.version) {
				w.WriteHeader(http.StatusNotModified)
				return
			}
		}

		rs, ok := f.(io.ReadSeeker)
		if !ok {
			// fs.File is not required to be seekable; without a seeker there
			// is no range support, which for a stylesheet is fine.
			w.Header().Set("Content-Length", fmt.Sprint(info.Size()))
			if r.Method == http.MethodHead {
				return
			}
			_, _ = io.Copy(w, f)
			return
		}
		// ServeContent handles Range, If-Range and HEAD. The zero modtime keeps
		// it from emitting Last-Modified, which would be a lie for embedded
		// files and redundant next to the ETag.
		http.ServeContent(w, r, name, zeroTime, rs)
	})
}

// hashTree hashes every path and its contents, so any edit changes the version.
func hashTree(fsys fs.FS) (string, error) {
	var names []string
	err := fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			names = append(names, p)
		}
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("assets: walking tree: %w", err)
	}
	// Sorted, so the hash does not depend on walk order.
	sort.Strings(names)

	h := sha256.New()
	for _, name := range names {
		fmt.Fprintf(h, "%s\x00", name)
		f, err := fsys.Open(name)
		if err != nil {
			return "", fmt.Errorf("assets: opening %s: %w", name, err)
		}
		if _, err := io.Copy(h, f); err != nil {
			f.Close()
			return "", fmt.Errorf("assets: hashing %s: %w", name, err)
		}
		f.Close()
	}
	return hex.EncodeToString(h.Sum(nil))[:16], nil
}

// zeroTime is the modtime handed to ServeContent: embedded files have none,
// and a fabricated one would produce a Last-Modified header that is not true.
var zeroTime = time.Time{}
