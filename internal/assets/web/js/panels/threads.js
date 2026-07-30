// The thread list.
//
// Threads are shown even while the program runs, because that is when the
// question "what is it doing?" gets asked. -thread-info is the one query gdb
// answers mid-run, and it reports state without a frame — so a running thread
// renders as a row with no location rather than being hidden.

export function createThreads({ element, onSelect }) {
  let threads = [];
  let selected = 0;

  function render() {
    if (threads.length === 0) {
      element.replaceChildren(note("Not running."));
      return;
    }
    const frag = document.createDocumentFragment();
    for (const th of threads) {
      const row = document.createElement("div");
      row.className = "list-row";
      row.dataset.thread = String(th.id);
      if (th.id === selected) row.setAttribute("aria-selected", "true");
      if (th.state === "running") row.classList.add("is-running");

      const id = document.createElement("span");
      id.className = "list-index";
      id.textContent = `#${th.id}`;

      const main = document.createElement("span");
      main.className = "list-main";
      main.textContent = describe(th);

      const sub = document.createElement("span");
      sub.className = "list-sub";
      sub.textContent = th.state === "running" ? "running" : location(th);

      row.append(id, main, sub);
      row.title = th.targetId ?? "";
      frag.append(row);
    }
    element.replaceChildren(frag);
  }

  function describe(th) {
    if (th.name) return th.name;
    if (th.frame?.func) return th.frame.func;
    return th.targetId ?? `thread ${th.id}`;
  }

  function location(th) {
    const src = th.frame?.source;
    if (src?.available) return `${src.path}:${src.line}`;
    if (th.frame?.func) return th.frame.func;
    return th.frame?.address ?? "";
  }

  element.addEventListener("click", (ev) => {
    const row = ev.target.closest(".list-row");
    if (!row) return;
    const id = Number(row.dataset.thread);
    if (id && id !== selected) onSelect?.(id);
  });

  return {
    set(next, selectedID) {
      threads = next ?? [];
      if (selectedID) selected = selectedID;
      render();
    },
    select(id) {
      selected = id;
      render();
    },
    clear() {
      threads = [];
      render();
    },
    count() {
      return threads.length;
    },
  };
}

function note(text) {
  const div = document.createElement("div");
  div.className = "list-note";
  div.textContent = text;
  return div;
}
