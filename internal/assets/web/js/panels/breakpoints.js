// The breakpoint list.
//
// The server's mirror is authoritative: this panel never keeps its own idea of
// what exists. gdb resolves pending breakpoints asynchronously and can move one
// to a different line than the user clicked, so anything derived from the click
// rather than from gdb's answer would drift.

export function createBreakpoints({ element, onToggle, onDelete, onReveal }) {
  let items = [];

  function render() {
    if (items.length === 0) {
      element.replaceChildren(note("No breakpoints. Click a line number to add one."));
      return;
    }
    const frag = document.createDocumentFragment();
    for (const bp of items) {
      const row = document.createElement("div");
      row.className = "list-row bp-row";
      row.dataset.number = String(bp.number);
      if (!bp.enabled) row.classList.add("is-disabled");
      if (bp.pending) row.classList.add("is-pending");

      const toggle = document.createElement("input");
      toggle.type = "checkbox";
      toggle.className = "bp-toggle";
      toggle.checked = bp.enabled;
      toggle.title = bp.enabled ? "Disable" : "Enable";

      const main = document.createElement("span");
      main.className = "list-main";
      main.textContent = describe(bp);

      const sub = document.createElement("span");
      sub.className = "list-sub";
      sub.textContent = bp.pending ? "pending" : (bp.address ?? "");

      const del = document.createElement("button");
      del.className = "bp-delete";
      del.textContent = "×";
      del.title = "Delete";

      row.append(toggle, main, sub, del);
      frag.append(row);
    }
    element.replaceChildren(frag);
  }

  function describe(bp) {
    const where = bp.path ?? bp.gdbPath ?? bp.original ?? "";
    if (where && bp.line) return `${where}:${bp.line}`;
    if (bp.func) return bp.func;
    return where || `breakpoint ${bp.number}`;
  }

  element.addEventListener("click", (ev) => {
    const row = ev.target.closest(".bp-row");
    if (!row) return;
    const number = Number(row.dataset.number);
    const bp = items.find((b) => b.number === number);
    if (!bp) return;

    if (ev.target.closest(".bp-delete")) {
      onDelete?.(number);
      return;
    }
    if (ev.target.closest(".bp-toggle")) {
      onToggle?.(number, !bp.enabled);
      return;
    }
    if (bp.path && bp.line) onReveal?.(bp.path, bp.line);
  });

  return {
    set(next) {
      items = next ?? [];
      render();
    },
    all() {
      return items;
    },
    find(path, line) {
      return items.find((b) => b.path === path && b.line === line);
    },
  };
}

function note(text) {
  const div = document.createElement("div");
  div.className = "list-note";
  div.textContent = text;
  return div;
}
