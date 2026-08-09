// The call stack.
//
// Frames render even when they have no source: a stripped binary reports
// func="??" with an address and nothing else, and a panel that hides those
// frames would misrepresent the stack's depth.
//
// Those frames can be named after the fact. gdb has no symbol for them, but a
// decompiler does, so the addresses are sent off and the names patched in when
// they come back. A recovered name is marked as one — it is Ghidra's guess or
// somebody's rename in Ghidra, not a symbol out of the binary, and a stack
// that presented the two alike would be claiming more than it knows.

// UNNAMED is what gdb reports for a frame it has no symbol for.
const UNNAMED = "??";

export function createStack({ element, onSelect }) {
  let frames = [];
  let selected = 0;
  // recovered maps a frame address to a decompiler name. Keyed by address
  // because that is the only stable thing about a frame gdb cannot name.
  let recovered = new Map();

  function render() {
    if (frames.length === 0) {
      element.replaceChildren(note("Not running."));
      return;
    }
    const frag = document.createDocumentFragment();
    for (const frame of frames) {
      const row = document.createElement("div");
      row.className = "list-row";
      row.dataset.level = String(frame.level);
      if (frame.level === selected) row.setAttribute("aria-selected", "true");

      const level = document.createElement("span");
      level.className = "list-index";
      level.textContent = `#${frame.level}`;

      const name = document.createElement("span");
      name.className = "list-main";
      const found = nameFor(frame);
      name.textContent = describe(frame, found);
      if (found) {
        name.classList.add("is-recovered");
        name.title = found.signature
          ? `${found.signature} — recovered by the decompiler, not a symbol`
          : "recovered by the decompiler, not a symbol";
      }

      const where = document.createElement("span");
      where.className = "list-sub";
      where.textContent = location(frame);

      row.append(level, name, where);
      frag.append(row);
    }
    element.replaceChildren(frag);
  }

  // isUnnamed is the rule, asked twice: once to decide what to send to the
  // decompiler and once to decide what to show. A frame gdb named keeps that
  // name — a real symbol beats a recovered one, and the two disagree on
  // exactly the code where it matters, the PLT thunks that live inside the
  // program and that Ghidra will happily rename.
  function isUnnamed(frame) {
    return !frame.func || frame.func === UNNAMED;
  }

  function nameFor(frame) {
    if (!isUnnamed(frame)) return null;
    return recovered.get(frame.address) ?? null;
  }

  function describe(frame, found) {
    if (found) {
      // The offset matters as much as the name: a bare FUN_0010d2b0 is equally
      // true of every instruction in the function, and on a stack every frame
      // but the innermost is a return address partway through one.
      const at = found.offset ? `+0x${found.offset.toString(16)}` : "";
      return `${found.name}${at}()`;
    }
    const fn = frame.func || UNNAMED;
    const args = (frame.args ?? [])
      .map((a) => (a.value ? `${a.name}=${a.value}` : a.name))
      .join(", ");
    return `${fn}(${args})`;
  }

  function location(frame) {
    if (frame.source?.available) return `${frame.source.path}:${frame.source.line}`;
    if (frame.source?.gdbPath) return `${frame.source.gdbPath}:${frame.source.line ?? 0}`;
    return frame.address ?? "";
  }

  element.addEventListener("click", (ev) => {
    const row = ev.target.closest(".list-row");
    if (!row) return;
    onSelect?.(Number(row.dataset.level));
  });

  return {
    set(next, selectedLevel = 0) {
      frames = next ?? [];
      selected = selectedLevel;
      // The recovered names described the previous stack. Keeping them would
      // put a name on whatever frame happened to land at the same address.
      recovered = new Map();
      render();
    },
    // unnamed lists the addresses of frames gdb could not name, for whoever
    // is going to ask the decompiler about them. Empty is the ordinary answer
    // and means no request needs to be made at all.
    unnamed() {
      return frames.filter((f) => isUnnamed(f) && f.address).map((f) => f.address);
    },
    // setNames patches in what the decompiler answered. Late by construction —
    // the stack was drawn without it — so it must not disturb the selection.
    setNames(names) {
      if (!names?.length) return;
      for (const n of names) {
        if (n.addr && n.name) recovered.set(n.addr, n);
      }
      render();
    },
    // recoveredAt is the decompiler's answer for one frame, for a caller
    // offering to correct it. Null for a frame gdb named itself: that name
    // came from a symbol table and is not the decompiler's to change.
    recoveredAt(level) {
      const frame = frames.find((f) => f.level === level);
      return frame ? nameFor(frame) : null;
    },
    select(level) {
      selected = level;
      render();
    },
    frameAt(level) {
      return frames.find((f) => f.level === level);
    },
    clear() {
      frames = [];
      recovered = new Map();
      render();
    },
  };
}

function note(text) {
  const div = document.createElement("div");
  div.className = "list-note";
  div.textContent = text;
  return div;
}
