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

import { editCell } from "../core/edit.js";
import { createVirtualList, measureRowHeight } from "../core/virtual.js";

// How many expansions to re-issue at once after a stop. Unbounded, a deeply
// expanded tree floods the socket ahead of the traffic the user is waiting for.
const REEXPAND_CONCURRENCY = 4;

export function createVariables({
  element, onExpand, onAddWatch, onRemoveWatch, onSetWatchExpr, onAssign, onError,
}) {
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
  // recovered maps an address to the decompiler's name for what is there, for
  // the watches that are nothing but an address. `*(undefined8 *)0x555555619250`
  // says where to read and never what it is; DAT_001a08de is the only name that
  // global has, and without it a column of watches on one is unreadable.
  let recovered = new Map();
  // Paths written by hand since the last stop.
  //
  // gdb's change tracking runs off -var-update, which compares against the
  // previous *stop*, and the flat locals carry no change flag at all. So an
  // edit's mark has to be remembered here, or it would vanish the moment the
  // panel re-read the locals — which is the first thing a write does.
  const written = new Set();

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
            '<span class="var-recovered"></span>' +
            '<span class="var-value"></span>' +
            '<span class="var-type"></span>' +
            '<span class="var-remove"></span>';
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
          el.classList.toggle("is-changed",
            Boolean(row.node.changed) || written.has(row.node.path));
          el.classList.toggle("is-stale", row.node.inScope === false);
          el.classList.toggle("is-optimized-out", Boolean(row.node.optimizedOut));
          el.classList.toggle("is-arg", Boolean(row.node.arg));
          el.classList.toggle("is-header", row.kind === "header");
          el.classList.toggle("is-editable", editable(row));
          el.classList.toggle("is-watch-root", isWatchRoot(row));

          el.querySelector(".var-twisty").textContent = row.node.expandable
            ? (expanded.has(row.node.path) ? "▾" : "▸")
            : "";
          el.querySelector(".var-name").textContent = row.node.name;
          el.querySelector(".var-value").textContent = valueText(row.node);
          el.querySelector(".var-type").textContent = row.node.type ?? "";

          // The decompiler's name for what the address holds, beside the
          // address rather than instead of it: the expression is what is being
          // read and is the thing a cast or a removal acts on, and replacing it
          // with a name would hide which of two labels at nearby addresses this
          // watch is actually on.
          const name = nameFor(row);
          const recov = el.querySelector(".var-recovered");
          recov.textContent = name ? name.name : "";
          if (name) recov.title = "the decompiler's name for this address, not a symbol";
          else recov.removeAttribute("title");

          // A watch is the only row a user put there, so it is the only one
          // they can take away. Locals arrive and leave with the frame.
          const remove = el.querySelector(".var-remove");
          if (isWatchRoot(row)) {
            remove.textContent = "×";
            remove.title = `stop watching ${row.node.name}`;
            remove.setAttribute("role", "button");
            remove.setAttribute("aria-label", `stop watching ${row.node.name}`);
          } else {
            remove.textContent = "";
            remove.removeAttribute("title");
            remove.removeAttribute("role");
            remove.removeAttribute("aria-label");
          }
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

  // editable comes from the server, which asked gdb. Deriving it here from the
  // type string would mean recognising every spelling of an array and every
  // typedef in front of one, and getting a struct row to offer an edit that
  // can only be refused.
  function editable(row) {
    return Boolean(onAssign) && row.kind !== "header" && row.kind !== "note"
      && Boolean(row.node.editable);
  }

  // isWatchRoot is "the user typed this one in". Only a root: a field inside a
  // watched struct is part of what was asked for, not a separate thing to
  // remove or to re-express.
  function isWatchRoot(row) {
    return row?.kind === "watch" && row.depth === 0;
  }

  // addressIn is the address a watch expression names outright, if it names
  // one. Exactly one literal, or nothing: `*(int *)0x4041a0` is an address with
  // a type in front of it, while `buf[i] + 0x10` is arithmetic and `head->next`
  // is an address the expression computes rather than states. Guessing at those
  // would put a label on a row whose address the label does not describe.
  function addressIn(expr) {
    const found = typeof expr === "string" ? expr.match(/0x[0-9a-fA-F]+/g) : null;
    return found?.length === 1 ? found[0] : "";
  }

  function nameFor(row) {
    if (!isWatchRoot(row)) return null;
    const addr = addressIn(row.node.expr);
    return addr ? (recovered.get(normaliseAddr(addr)) ?? null) : null;
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
    // The value cell belongs to the editor. Without this a pointer — which is
    // both expandable and editable — would expand and collapse under the edit
    // box on the way to opening it.
    if (ev.target.closest(".var-value") && node.editable) return;
    if (expanded.has(path)) {
      expanded.delete(path);
      render();
    } else {
      expand(path).catch((err) => onError?.(err));
    }
  });

  element.addEventListener("dblclick", (ev) => {
    const el = ev.target.closest?.(".var-row");
    const cell = ev.target.closest?.(".var-value");
    if (!el) return;
    const row = rows.find((r) => r.node.path === el.dataset.path);
    // The type cell of a watch is a cast. Same gesture as editing a value, on
    // the cell that names what is being changed.
    if (!cell && ev.target.closest?.(".var-type") && isWatchRoot(row)) {
      ev.preventDefault();
      castWatch(row);
      return;
    }
    if (!cell || !row || !editable(row)) return;
    // A double-click on an expandable row would otherwise also have toggled
    // it twice via the click handler, leaving the tree flapping under the
    // editor. Stopping here is enough: the click handler ignores .var-value.
    ev.preventDefault();

    editCell({
      cell,
      value: row.node.value,
      title: `${row.node.name} — a value or an expression, Enter to write`,
      onError,
      commit: (typed) => onAssign({
        path: row.node.path,
        id: row.node.id,
        expr: row.node.expr,
        value: typed,
      }).then((res) => {
        // Repaint from the reply, which carries what gdb stored rather than
        // what was typed: assigning 321 to a char shows 65 immediately.
        row.node.value = res?.value ?? row.node.value;
        if (res?.id) row.node.id = res.id;
        written.add(row.node.path);
        list?.refresh();
      }),
    });
  });

  // castWatch reinterprets a watch without disturbing it.
  //
  // The gesture is the type cell, because the type is what is being changed:
  // `*(undefined8 *)0x555555619250` says what is at the address and not what it
  // means, and `char **` is the correction. The expression is wrapped rather
  // than replaced, so the address the user found stays exactly as they found
  // it — and the cast is visible in the row afterwards, which matters when the
  // number on screen depends on a decision that was made once and forgotten.
  function castWatch(row) {
    if (!onSetWatchExpr || !isWatchRoot(row)) return;
    const el = element.querySelector(`.var-row[data-path="${CSS.escape(row.node.path)}"]`);
    const cell = el?.querySelector(".var-type");
    if (!cell) return;
    editCell({
      cell,
      value: row.node.type ?? "",
      title: `${row.node.name} — a C type to read it as, Enter to apply`,
      onError,
      commit: (typed) => {
        const want = typed.trim();
        if (!want) return Promise.resolve();
        return onSetWatchExpr(row.node.path, `(${want})(${row.node.expr})`);
      },
    });
  }

  return {
    setLocals(next, seq) {
      stopSeq = seq ?? stopSeq;
      locals = next ?? [];
      render();
    },
    // watchAt answers the context menu: which watch, if any, is under a click.
    watchAt(target) {
      const el = target?.closest?.(".var-row");
      if (!el) return null;
      const row = rows.find((r) => r.node.path === el.dataset.path);
      return isWatchRoot(row) ? row.node : null;
    },
    castWatch(path) {
      const row = rows.find((r) => r.node.path === path);
      if (row) castWatch(row);
    },
    setWatches(next, seq) {
      stopSeq = seq ?? stopSeq;
      watches = next ?? [];
      render();
    },
    // unnamedWatches lists the addresses of watches that are an address and
    // nothing else, and that nothing has named yet. The caller asks the
    // decompiler; a program with symbols never produces one of these, because
    // a watch there is spelled with a name in the first place.
    unnamedWatches() {
      const out = [];
      for (const w of watches) {
        const addr = addressIn(w.expr);
        if (addr && !recovered.has(normaliseAddr(addr))) out.push(addr);
      }
      return out;
    },
    setNames(names) {
      if (!names?.length) return;
      for (const n of names) {
        if (n.addr && n.name) recovered.set(normaliseAddr(n.addr), n);
      }
      render();
    },
    // forgetNames drops them after a rename in the decompiler, or a decompiler
    // that has restarted on a different program.
    forgetNames() {
      if (recovered.size === 0) return;
      recovered = new Map();
      render();
    },
    // onStop is the per-stop refresh: values arrive with the stop, and open
    // subtrees are re-fetched because their contents may have changed.
    async onStop(nextLocals, seq) {
      stopSeq = seq;
      locals = nextLocals ?? [];
      // An edit's mark lasts until the program moves, at which point gdb's own
      // change tracking takes over and a mark left here would claim the user
      // wrote something during this stop.
      written.clear();
      render();
      await reexpand();
    },
    // refreshOpen re-fetches every open subtree without waiting for a stop.
    //
    // A write changes values inside those subtrees and does not advance
    // stopSeq, so nothing else would: setting a byte through the hex view
    // would leave the struct field it belongs to showing the old number.
    refreshOpen() {
      return reexpand();
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
      written.clear();
      render();
    },
    expandedPaths() {
      return [...expanded];
    },
  };
}

// normaliseAddr makes two spellings of one address compare equal. gdb pads to
// the pointer width, the decompiler does not, and a watch expression holds
// whatever was typed.
function normaliseAddr(addr) {
  if (typeof addr !== "string") return "";
  const m = /^0x0*([0-9a-f]+)$/i.exec(addr.trim());
  return m ? m[1].toLowerCase() : "";
}
