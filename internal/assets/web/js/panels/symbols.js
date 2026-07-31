// The symbol list: what the program contains, as opposed to what it is doing.
//
// Filtering happens on the server, over a cached table, so this panel holds
// only the page it is showing. That is what lets it work on a firmware image
// with fifty thousand symbols without shipping fifty thousand rows to the
// browser to support one search box.
//
// Two populations live in the same list and they behave differently on a
// double-click. A symbol with debug info knows its source file and jumps
// there; one from the ELF table alone knows only an address and jumps to the
// disassembly. The row says which before you click, because a jump that
// silently does something other than what you expected is worse than one that
// is labelled.

// searchDebounce is the pause after a keystroke before asking the server. Long
// enough that typing a whole word is one request, short enough to feel live.
const SEARCH_DEBOUNCE_MS = 120;

export function createSymbols({ element, input, kindSelect, countEl, onQuery, onJump }) {
  let symbols = [];
  let selected = -1;
  let timer = 0;
  // seq guards against a slow reply for an old filter overwriting a fast reply
  // for a newer one. Without it, deleting characters quickly can leave the
  // list showing results for a prefix the box no longer contains.
  let seq = 0;
  let lastNote = "Load a program to list its symbols.";

  function render() {
    if (symbols.length === 0) {
      element.replaceChildren(note(lastNote));
      return;
    }
    const frag = document.createDocumentFragment();
    symbols.forEach((sym, i) => {
      const row = document.createElement("div");
      row.className = "list-row sym-row";
      row.dataset.index = String(i);
      row.title = describeFully(sym);
      if (!sym.debug) row.classList.add("is-nondebug");
      if (i === selected) row.setAttribute("aria-selected", "true");

      const kind = document.createElement("span");
      kind.className = "sym-kind";
      kind.dataset.kind = sym.kind;
      kind.textContent = sym.kind === "variable" ? "var" : "fn";

      const name = document.createElement("span");
      name.className = "list-main";
      name.textContent = sym.name;

      const where = document.createElement("span");
      where.className = "list-sub";
      where.textContent = location(sym);

      row.append(kind, name, where);
      frag.append(row);
    });
    element.replaceChildren(frag);
  }

  function location(sym) {
    if (sym.file) return `${sym.file}:${sym.line}`;
    if (sym.gdbPath) return `${baseName(sym.gdbPath)}:${sym.line}`;
    return sym.address ?? "";
  }

  function describeFully(sym) {
    const parts = [sym.type ? `${sym.type} ${sym.name}` : sym.name];
    if (sym.gdbPath) parts.push(`${sym.gdbPath}:${sym.line}`);
    if (sym.address) parts.push(sym.address);
    if (!sym.debug) parts.push("no debug info — jumps to disassembly");
    return parts.join("\n");
  }

  function setCount(reply) {
    if (!countEl) return;
    if (reply.available === 0) {
      countEl.textContent = "";
      return;
    }
    countEl.textContent = reply.truncated
      ? `${reply.symbols.length} of ${reply.matched}`
      : `${reply.matched} of ${reply.available}`;
  }

  // query asks the server for the current filter. Every path that changes the
  // filter goes through here, so there is one place where the sequence number
  // is bumped and one place that can fail.
  function query() {
    const mine = ++seq;
    const filter = input?.value ?? "";
    const kind = kindSelect?.value ?? "";
    return Promise.resolve(onQuery?.({ filter, kind }))
      .then((reply) => {
        if (mine !== seq || !reply) return;
        symbols = reply.symbols ?? [];
        selected = -1;
        lastNote = filter
          ? `No symbol matches "${filter}".`
          : "This program has no symbols.";
        setCount(reply);
        render();
      })
      .catch((err) => {
        if (mine !== seq) return;
        symbols = [];
        // not_ready is the ordinary state before a program is loaded, not a
        // failure worth a red message.
        lastNote = err?.code === "not_ready"
          ? "Load a program to list its symbols."
          : err?.message || "Symbols are unavailable.";
        if (countEl) countEl.textContent = "";
        render();
      });
  }

  function schedule() {
    clearTimeout(timer);
    timer = setTimeout(query, SEARCH_DEBOUNCE_MS);
  }

  function jump(index) {
    const sym = symbols[index];
    if (!sym) return;
    selected = index;
    render();
    onJump?.(sym);
  }

  function move(delta) {
    if (symbols.length === 0) return;
    selected = Math.max(0, Math.min(symbols.length - 1, selected + delta));
    render();
    element.querySelector('[aria-selected="true"]')
      ?.scrollIntoView({ block: "nearest" });
  }

  element.addEventListener("click", (ev) => {
    const row = ev.target.closest(".sym-row");
    if (!row) return;
    selected = Number(row.dataset.index);
    render();
  });

  element.addEventListener("dblclick", (ev) => {
    const row = ev.target.closest(".sym-row");
    if (row) jump(Number(row.dataset.index));
  });

  // The list is reachable by keyboard as well as by mouse: arrows move,
  // Enter jumps. A pane whose only affordance is a double-click is unusable
  // without a pointer.
  element.addEventListener("keydown", (ev) => {
    switch (ev.key) {
      case "ArrowDown": ev.preventDefault(); move(1); break;
      case "ArrowUp":   ev.preventDefault(); move(-1); break;
      case "Enter":     ev.preventDefault(); jump(selected); break;
      default: return;
    }
  });

  input?.addEventListener("input", schedule);
  input?.addEventListener("keydown", (ev) => {
    // Enter from the box jumps to the top hit — the whole point of typing a
    // name you already know into a filter.
    if (ev.key === "Enter") {
      ev.preventDefault();
      clearTimeout(timer);
      query().then(() => jump(0));
      return;
    }
    if (ev.key === "ArrowDown" && symbols.length) {
      ev.preventDefault();
      element.focus();
      move(1);
    }
  });
  kindSelect?.addEventListener("change", query);

  return {
    // refresh re-runs the current filter. Called when the program changes.
    refresh: query,
    clear() {
      seq++;
      symbols = [];
      selected = -1;
      lastNote = "Load a program to list its symbols.";
      if (countEl) countEl.textContent = "";
      render();
    },
    focusSearch() {
      input?.focus();
      input?.select();
    },
  };
}

function baseName(p) {
  const i = p.lastIndexOf("/");
  return i < 0 ? p : p.slice(i + 1);
}

function note(text) {
  const div = document.createElement("div");
  div.className = "list-note";
  div.textContent = text;
  return div;
}
