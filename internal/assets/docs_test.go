package assets_test

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// Checks that the documentation site has not quietly fallen behind the code.
//
// Same argument as the rest of this package: the site has no compiler either.
// A flag that exists and is undocumented, a page pointing at an image nobody
// generates, or a screenshot whose subject no page ever shows — none of those
// break a build, and all three are the kind of wrongness a reader discovers
// and an author does not.
//
// The precedent is TestProtocolDocumented next door, and the motivating bug is
// real: -decomp-dir advertised a path Ghidra refuses for three commits after
// the code stopped using it, because nothing compared the two.

const docsDir = "docs"

// TestFlagsDocumented: every flag the command registers appears in the flags
// reference.
func TestFlagsDocumented(t *testing.T) {
	root := repoRoot(t)

	main, err := os.ReadFile(filepath.Join(root, "cmd", "gdb-wui", "main.go"))
	if err != nil {
		t.Fatal(err)
	}
	// flag.StringVar(&opt.project, "project", ...) and its Bool/Duration kin.
	declared := regexp.MustCompile(`flag\.\w+Var\([^,]+,\s*"([^"]+)"`)
	var flags []string
	for _, m := range declared.FindAllSubmatch(main, -1) {
		flags = append(flags, string(m[1]))
	}
	if len(flags) < 5 {
		t.Fatalf("found %d flags in main.go; the pattern has stopped matching", len(flags))
	}

	page, err := os.ReadFile(filepath.Join(root, docsDir, "reference", "flags.md"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(page)

	for _, name := range flags {
		// Backticked, as the page writes them, so a flag mentioned only in
		// passing prose does not count as documented.
		if !strings.Contains(text, "`-"+name) {
			t.Errorf("flag -%s is not in docs/reference/flags.md", name)
		}
	}
}

// TestRelativeLinksPluginIsConfigured pins the thing the pages' links rely on.
//
// Pages link to each other as `[Install](install.md)`, which is what keeps one
// link working both on the site and when the same file is read on GitHub. On
// the site that only works because jekyll-relative-links rewrites it to
// install.html at build time; without the plugin every such link 404s.
//
// This is not hypothetical. The links shipped broken exactly once, and the
// source-level check below did not catch it — install.md really was there, so
// it had no reason to complain. Only the built HTML could tell, which is what
// scripts/check-links.py is for; this test is the cheap half that runs without
// Ruby.
func TestRelativeLinksPluginIsConfigured(t *testing.T) {
	root := repoRoot(t)

	config, err := os.ReadFile(filepath.Join(root, docsDir, "_config.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(config), "jekyll-relative-links") {
		t.Error("docs/_config.yml does not enable jekyll-relative-links; " +
			"every .md link between pages will 404 on the built site")
	}

	// And the links it rewrites really do exist, or the plugin has nothing to
	// work with and leaves them alone.
	var found int
	link := regexp.MustCompile(`\]\(([^):]+\.md(#[^)]*)?)\)`)
	for _, page := range markdownPages(t, root) {
		body, err := os.ReadFile(page)
		if err != nil {
			t.Fatal(err)
		}
		found += len(link.FindAllSubmatch(body, -1))
	}
	if found == 0 {
		t.Error("no page links to another with a .md target; either the pages " +
			"have stopped cross-linking or they now hardcode .html, which " +
			"breaks reading them on GitHub")
	}
}

// TestDocImagesExist: every image a page references is actually there.
func TestDocImagesExist(t *testing.T) {
	root := repoRoot(t)
	link := regexp.MustCompile(`!\[[^\]]*\]\(([^)]+)\)`)

	for _, page := range markdownPages(t, root) {
		body, err := os.ReadFile(page)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range link.FindAllSubmatch(body, -1) {
			src := string(m[1])
			if strings.HasPrefix(src, "http") {
				continue
			}
			// Relative to the page, which is how Jekyll resolves it.
			path := filepath.Join(filepath.Dir(page), src)
			if _, err := os.Stat(path); err != nil {
				rel, _ := filepath.Rel(root, page)
				t.Errorf("%s references %s, which does not exist", rel, src)
			}
		}
	}
}

// TestScreenshotScenesAreUsed: every scene produces an image some page shows.
//
// Without this a scene can go on being captured, and reviewed, and committed,
// long after the page that used to display it was rewritten — which is the
// same rot as an undocumented flag, running the other way.
func TestScreenshotScenesAreUsed(t *testing.T) {
	root := repoRoot(t)

	scenes, err := filepath.Glob(filepath.Join(root, "scripts", "screenshots", "scenes", "*.mjs"))
	if err != nil {
		t.Fatal(err)
	}
	if len(scenes) == 0 {
		t.Skip("no screenshot scenes")
	}

	var referenced []string
	for _, page := range markdownPages(t, root) {
		body, err := os.ReadFile(page)
		if err != nil {
			t.Fatal(err)
		}
		referenced = append(referenced, string(body))
	}
	all := strings.Join(referenced, "\n")

	name := regexp.MustCompile(`(?m)^\s*name:\s*"([^"]+)"`)
	for _, scene := range scenes {
		body, err := os.ReadFile(scene)
		if err != nil {
			t.Fatal(err)
		}
		m := name.FindSubmatch(body)
		if m == nil {
			t.Errorf("%s declares no name", filepath.Base(scene))
			continue
		}
		// A scene may write several images, all prefixed with its name, so a
		// page showing any one of them counts.
		if !strings.Contains(all, "images/"+string(m[1])) {
			t.Errorf("scene %q is captured but no page shows images/%s.png",
				m[1], m[1])
		}
	}
}

// markdownPages lists the site's pages, in a stable order.
func markdownPages(t *testing.T, root string) []string {
	t.Helper()
	var pages []string
	err := filepath.WalkDir(filepath.Join(root, docsDir), func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && strings.HasSuffix(path, ".md") {
			pages = append(pages, path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(pages) == 0 {
		t.Fatal("no markdown under docs/; this test would pass vacuously")
	}
	sort.Strings(pages)
	return pages
}
