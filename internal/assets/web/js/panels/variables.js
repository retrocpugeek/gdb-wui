// The variables tree: locals, and watches, in one panel.
//
// Two decisions shape this panel:
//
//   - Rows live in a flat array and render on the virtual list. Arrays of
//     structs produce thousands of rows, and a nested-DOM tree would fall over
//     on the ones worth opening.
//   - Expansion is keyed by the server's stable *path*, never by varobj id.
//     The varobj behind a row is deleted and recreated on every re-run and on
//     LRU eviction; the path survives, so the tree stays open while stepping,
//     which is when a value is being watched.

import { createVirtualList, measureRowHeight } from "../core/virtual.js";

// How many expansions to re-issue at once after a stop. Unbounded, a deeply
// expanded tree floods the socket ahead of the traffic the user is waiting for.
const REEXPAND_CONCURRENCY = 4;

export function createVariables({ element, onExpand, onAddWatch, onRemoveWatch, onError }) {
  // expanded holds paths, so it survives every id churn underneath.
  const expanded = new Set();
  // children maps path -> array of nodes.
  const children = new Map();
  let locals = [];
  let watches = [];
  let rows = [];
  let list = null;
  let stopSeq = 0;
  let pending = new Set();

  function ensureList() {
    if (list) return;
    const rowHeight = measureRowHeight(element, cssLineHeight());
    list = createVirtualList({
      container: element,
      rowHeight,
      renderRow: {
        create() {
          const row = document.createElement("div");
          row.className = "var-row";
          row.innerHTML =
            '<span class="var-twisty"></span>' +
            '<span class="var-name"></span>' +
            '<span class="var-value"></span>' +
            '<span class="var-type"></span>';
          return row;
        },
        update(el, index) {
          const row = rows[index];
          if (!row) return;
          el.dataset.path = row.node.path;
          el.dataset.kind = row.kind;
          el.style.paddingLeft = `${4 + row.depth * 12}px`;
          el.classList.toggle("is-expandable", row.node.expandable);
          el.classList.toggle("is-expanded", expanded.has(row.node.path));
          el.classList.toggle("is-changed", Boolean(row.node.changed));
          el.classList.toggle("is-stale", row.node.inScope === false);
          el.classList.toggle("is-optimized-out", Boolean(row.node.optimizedOut));
          el.classList.toggle("is-arg", Boolean(row.node.arg));
          el.classList.toggle("is-header", row.kind === "header");

          el.querySelector(".var-twisty").textContent = row.node.expandable
            ? (expanded.has(row.node.path) ? "▾" : "▸")
            : "";
          el.querySelector(".var-name").textContent = row.node.name;
          el.querySelector(".var-value").textContent = valueText(row.node);
          el.querySelector(".var-type").textContent = row.node.type ?? "";
          el.title = row.node.type ? `${row.node.name}: ${row.node.type}` : row.node.name;
        },
      },
    });
  }

  function valueText(node) {
    if (node.optimizedOut) return "<optimized out>";
    if (node.inScope === false) return "<out of scope>";
    if (node.value != null && node.value !== "") return node.value;
    // An aggregate under --simple-values has no value. Saying so beats an
    // empty cell that reads like a bug.
    if (node.expandable) return node.type?.includes("[") ? "[…]" : "{…}";
    return "";
  }

  function cssLineHeight() {
    const raw = getComputedStyle(document.documentElement).getPropertyValue("--line-h");
    const n = parseFloat(raw);
    return Number.isFinite(n) && n > 0 ? n : 19;
  }

  // flatten walks the visible tree into the array the virtual list indexes.
  function flatten() {
    const out = [];
    if (watches.length) {
      out.push({ node: { path: "hdr:watches", name: "Watches", expandable: false }, depth: 0, kind: "header" });
      for (const w of watches) walk(w, 0, "watch");
    }
    out.push({ node: { path: "hdr:locals", name: "Locals", expandable: false }, depth: 0, kind: "header" });
    if (locals.length === 0) {
      out.push({ node: { path: "hdr:empty", name: "(none)", expandable: false }, depth: 1, kind: "note" });
    }
    for (const l of locals) walk(l, 0, "local");
    return out;

    function walk(node, depth, kind) {
      out.push({ node, depth, kind });
      if (!expanded.has(node.path)) return;
      for (const child of children.get(node.path) ?? []) walk(child, depth + 1, kind);
    }
  }

  function render() {
    rows = flatten();
    ensureList();
    list.setCount(rows.length);
  }

  async function expand(path) {
    const node = findNode(path);
    if (!node) return;
    expanded.add(path);
    render();
    if (children.has(path)) return;
    await fetchChildren(node);
  }

  async function fetchChildren(node) {
    if (pending.has(node.path)) return;
    pending.add(node.path);
    const seq = stopSeq;
    try {
      const res = await onExpand({
        path: node.path,
        id: node.id,
        expr: node.expr,
        stopSeq: seq,
      });
      // Guard: a stop landed while this was in flight, so the answer describes
      // a program state that no longer exists.
      if (seq !== stopSeq) return;
      children.set(node.path, res.children ?? []);
      if (res.hasMore) {
        children.get(node.path).push({
          path: node.path + "#more",
          name: `… ${res.numChild - (res.children?.length ?? 0)} more`,
          expandable: false,
          inScope: true,
        });
      }
      render();
    } catch (err) {
      expanded.delete(node.path);
      render();
      if (err?.code !== "busy") onError?.(err);
    } finally {
      pending.delete(node.path);
    }
  }

  function findNode(path) {
    for (const row of rows) {
      if (row.node.path === path) return row.node;
    }
    return null;
  }

  // reexpand re-issues every open subtree after a stop, breadth-first with a
  // small concurrency cap so the deep case does not flood the socket.
  async function reexpand() {
    const paths = [...expanded].sort((a, b) => depthOf(a) - depthOf(b));
    children.clear();
    for (let i = 0; i < paths.length; i += REEXPAND_CONCURRENCY) {
      const batch = paths.slice(i, i + REEXPAND_CONCURRENCY);
      const seq = stopSeq;
      await Promise.all(batch.map((path) => {
        const node = findNode(path);
        return node ? fetchChildren(node) : Promise.resolve();
      }));
      if (seq !== stopSeq) return; // superseded; the next stop will redo it
      render();
    }
  }

  function depthOf(path) {
    return (path.match(/[.[]/g) ?? []).length;
  }

  element.addEventListener("click", (ev) => {
    const row = ev.target.closest(".var-row");
    if (!row) return;
    const path = row.dataset.path;
    if (ev.target.closest(".var-remove")) {
      onRemoveWatch?.(path);
      return;
    }
    const node = findNode(path);
    if (!node?.expandable) return;
    if (expanded.has(path)) {
      expanded.delete(path);
      render();
    } else {
      expand(path).catch((err) => onError?.(err));
    }
  });

  return {
    setLocals(next, seq) {
      stopSeq = seq ?? stopSeq;
      locals = next ?? [];
      render();
    },
    setWatches(next, seq) {
      stopSeq = seq ?? stopSeq;
      watches = next ?? [];
      render();
    },
    // onStop is the per-stop refresh: values arrive with the stop, and open
    // subtrees are re-fetched because their contents may have changed.
    async onStop(nextLocals, seq) {
      stopSeq = seq;
      locals = nextLocals ?? [];
      render();
      await reexpand();
    },
    // invalidate drops every varobj-derived row. The server sends this when it
    // has deleted the objects wholesale, so the ids we hold are dead.
    invalidate() {
      children.clear();
      render();
    },
    clear() {
      locals = [];
      children.clear();
      render();
    },
    expandedPaths() {
      return [...expanded];
    },
  };
}
