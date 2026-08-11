// The breakpoint list.
//
// The server's mirror is authoritative: this panel never keeps its own idea of
// what exists. gdb resolves pending breakpoints asynchronously and can move one
// to a different line than the user clicked, so anything derived from the click
// rather than from gdb's answer would drift.
//
// A breakpoint gdb cannot name is shown as `*0x4011d6`, which says where it is
// and nothing about what it is in. The decompiler knows, so the addresses are
// sent off and the names patched in when they come back — the same late fill
// the call stack does, and marked the same way, because a recovered name is
// Ghidra's guess rather than a symbol out of the binary.

// UNNAMED is what gdb reports for code it has no symbol for.
const UNNAMED = "??";

export function createBreakpoints({ element, onToggle, onDelete, onReveal }) {
  let items = [];
  // recovered maps a breakpoint address to a decompiler name. Keyed by address
  // for the reason the stack's map is: it is the only thing about an unnamed
  // breakpoint that means anything.
  let recovered = new Map();

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
      const found = nameFor(bp);
      main.textContent = found ? describeRecovered(found) : describe(bp);
      if (found) {
        main.classList.add("is-recovered");
        main.title = `${describe(bp)} — ${found.kind === "variable"
          ? "the decompiler's name for this data, not a symbol"
          : "recovered by the decompiler, not a symbol"}`;
      }

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
    if (bp.func && bp.func !== UNNAMED) return bp.func;
    return where || `breakpoint ${bp.number}`;
  }

  function describeRecovered(found) {
    // The offset, for the same reason the stack carries it: a breakpoint set by
    // clicking a decompiled line is somewhere in the middle of a function, and
    // a bare FUN_004011d6 is equally true of every instruction in it.
    const at = found.offset ? `+0x${found.offset.toString(16)}` : "";
    return `${found.name}${at}`;
  }

  // isUnnamed is "there is nothing here but an address". A breakpoint with a
  // source line or a function name keeps what gdb said: that came out of the
  // binary, and the decompiler's guess must not displace a fact.
  //
  // The location the user asked for counts as a name too. `break printf`
  // resolves through .dynsym on a stripped binary and gdb reports no function
  // for it, but the row already reads printf — and Ghidra calls the PLT thunk
  // there printf as well, so relabelling it would present the binary's own
  // knowledge as a recovery. Measured: without this the printf row went italic.
  function isUnnamed(bp) {
    if (bp.path && bp.line) return false;
    if (bp.gdbPath && bp.line) return false;
    if (bp.func && bp.func !== UNNAMED) return false;
    const asked = (bp.original ?? "").trim();
    return asked === "" || /^\*?(0x)?[0-9a-f]+$/i.test(asked);
  }

  function nameFor(bp) {
    if (!isUnnamed(bp) || !bp.address) return null;
    return recovered.get(normaliseAddr(bp.address)) ?? null;
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
      // The recovered names are kept across a change, unlike the stack's. They
      // are keyed by address and an address means the same thing all session:
      // dropping them would blank every name for one round trip each time a
      // breakpoint was added anywhere.
      render();
    },
    all() {
      return items;
    },
    // unnamed lists the addresses of breakpoints gdb could not name and that
    // have not been named yet. Empty is the ordinary answer — a program with
    // debug info never gets here — and means no request needs making at all.
    unnamed() {
      return items
        .filter((bp) => isUnnamed(bp) && bp.address && !recovered.has(normaliseAddr(bp.address)))
        .map((bp) => bp.address);
    },
    setNames(names) {
      if (!names?.length) return;
      for (const n of names) {
        if (n.addr && n.name) recovered.set(normaliseAddr(n.addr), n);
      }
      render();
    },
    // forgetNames drops them after something that can change a name: a rename
    // in the decompiler, or a decompiler that has restarted on another program.
    forgetNames() {
      if (recovered.size === 0) return;
      recovered = new Map();
      render();
    },
    find(path, line) {
      return items.find((b) => b.path === path && b.line === line);
    },
    // findAddress makes the machine-level gutters a toggle rather than an
    // accumulator. gdb pads addresses to the pointer width and the panes do
    // not, so the comparison has to be numeric.
    findAddress(address) {
      const want = normaliseAddr(address);
      if (!want) return undefined;
      return items.find((b) => normaliseAddr(b.address) === want);
    },
  };
}

// normaliseAddr strips the zero padding gdb applies, so "0x0000000000001040"
// and "0x1040" compare equal.
function normaliseAddr(addr) {
  if (typeof addr !== "string") return "";
  const m = /^0x0*([0-9a-f]+)$/i.exec(addr.trim());
  return m ? m[1].toLowerCase() : "";
}

function note(text) {
  const div = document.createElement("div");
  div.className = "list-note";
  div.textContent = text;
  return div;
}
