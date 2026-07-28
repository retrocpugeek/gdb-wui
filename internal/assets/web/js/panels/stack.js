// The call stack.
//
// Frames render even when they have no source: a stripped binary reports
// func="??" with an address and nothing else, and a panel that hides those
// frames would misrepresent the stack's depth.

export function createStack({ element, onSelect }) {
  let frames = [];
  let selected = 0;

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
      name.textContent = describe(frame);

      const where = document.createElement("span");
      where.className = "list-sub";
      where.textContent = location(frame);

      row.append(level, name, where);
      frag.append(row);
    }
    element.replaceChildren(frag);
  }

  function describe(frame) {
    const fn = frame.func || "??";
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
      render();
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
