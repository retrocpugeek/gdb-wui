package assets_test

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The virtual list, driven through node with just enough of a DOM for it.
//
// A list that shrinks while its panel is hidden keeps its old scrollTop: a
// hidden scroller is never laid out, so nothing clamps it to the new end. The
// first visible row then came out past the last, the pool was asked for a
// negative number of rows, and it popped past empty (`pool.pop().remove()` on
// undefined) — an exception on every render until the panel was shown.
func TestVirtualListShrinksWhileHidden(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed; skipping the virtual list tests")
	}
	mod := filepath.Join(repoRoot(t), "internal", "assets", "web", "js", "core", "virtual.js")
	script := `
const el = () => ({
  style: {}, children: [],
  append(c) { this.children.push(c); c.parent = this; },
  remove() { if (this.parent) this.parent.children.splice(this.parent.children.indexOf(this), 1); },
  addEventListener() {},
});
globalThis.document = { createElement: el };
globalThis.ResizeObserver = class { observe() {} disconnect() {} };
globalThis.requestAnimationFrame = () => {};
const { createVirtualList } = await import(` + jsString(mod) + `);

// A hidden panel: no height, and a scrollTop from when it had 100 rows
const container = Object.assign(el(), { scrollTop: 0, clientHeight: 0 });
let rows = 0;
const list = createVirtualList({ container, rowHeight: 16,
  renderRow: { create: () => (rows++, el()), update() {} } });
list.setCount(100);
container.scrollTop = 90 * 16;
list.renderNow();
list.setCount(10);
const hidden = container.children[0].children[0].children.length;
// Shown again, at the top
container.scrollTop = 0;
list.renderNow();
const shown = container.children[0].children[0].children.length;
console.log(JSON.stringify({ hidden, shown }));
`
	cmd := exec.Command(node, "--input-type=module", "-")
	cmd.Stdin = strings.NewReader(script)
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			t.Fatalf("node failed: %v\n%s", err, ee.Stderr)
		}
		t.Fatalf("running node: %v", err)
	}
	if got, want := strings.TrimSpace(string(out)), `{"hidden":0,"shown":10}`; got != want {
		t.Fatalf("rows rendered: got %s, want %s", got, want)
	}
}
