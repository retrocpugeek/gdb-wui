// Splitters and the theme.
//
// The grid's track sizes are custom properties, so a splitter is just a drag
// that writes one property — no reflow arithmetic, no absolute positioning, and
// the layout rules in layout.css never learn that splitters exist.
//
// Pointer events rather than mouse events: one code path covers mouse, trackpad
// and touch, and pointer capture means a fast drag that leaves the 4px handle
// does not drop the gesture.

const STORAGE_KEY = "gdb-wui.layout";
const THEME_KEY = "gdb-wui.theme";

// Each splitter writes one custom property, clamped so a panel cannot be
// dragged away entirely — a panel at zero width looks like a bug and cannot be
// recovered without knowing to drag an invisible edge.
const SPLITTERS = [
  { id: "split-tree", prop: "--tree-w", axis: "x", min: 140, max: 600, invert: false },
  { id: "split-right", prop: "--right-w", axis: "x", min: 220, max: 700, invert: true },
  { id: "split-bottom", prop: "--bottom-h", axis: "y", min: 80, max: 600, invert: true },
  // The centre split. One spec per orientation, because a splitter's axis and
  // the property it writes are fixed at construction; only the handle matching
  // the current orientation is in the grid, so only one ever gets events. Both
  // persist, so each orientation remembers its own divider.
  { id: "split-center-x", prop: "--center-a-w", axis: "x", min: 160, max: 3000, invert: false },
  { id: "split-center-y", prop: "--center-a-h", axis: "y", min: 80, max: 2000, invert: false },
];

export function initLayout({ app, onResize }) {
  restore(app);

  for (const spec of SPLITTERS) {
    const handle = document.getElementById(spec.id);
    if (!handle) continue;
    handle.addEventListener("pointerdown", (ev) => start(ev, handle, spec));
    // Keyboard reachable: a splitter that only responds to dragging is
    // unusable without a pointer.
    handle.addEventListener("keydown", (ev) => {
      const step = ev.shiftKey ? 40 : 10;
      if (ev.key === "ArrowLeft" || ev.key === "ArrowUp") nudge(spec, -step);
      else if (ev.key === "ArrowRight" || ev.key === "ArrowDown") nudge(spec, step);
      else return;
      ev.preventDefault();
    });
  }

  function currentValue(spec) {
    const raw = getComputedStyle(app).getPropertyValue(spec.prop);
    return parseFloat(raw) || spec.min;
  }

  function apply(spec, value) {
    const clamped = Math.min(spec.max, Math.max(spec.min, value));
    app.style.setProperty(spec.prop, `${Math.round(clamped)}px`);
    persist(app);
    onResize?.();
  }

  function nudge(spec, delta) {
    apply(spec, currentValue(spec) + (spec.invert ? -delta : delta));
  }

  function start(ev, handle, spec) {
    ev.preventDefault();
    handle.setPointerCapture(ev.pointerId);
    const startPos = spec.axis === "x" ? ev.clientX : ev.clientY;
    const startValue = currentValue(spec);
    document.body.classList.add("is-dragging");

    function move(e) {
      const pos = spec.axis === "x" ? e.clientX : e.clientY;
      const delta = pos - startPos;
      apply(spec, startValue + (spec.invert ? -delta : delta));
    }
    function end() {
      handle.releasePointerCapture(ev.pointerId);
      handle.removeEventListener("pointermove", move);
      handle.removeEventListener("pointerup", end);
      handle.removeEventListener("pointercancel", end);
      document.body.classList.remove("is-dragging");
    }
    handle.addEventListener("pointermove", move);
    handle.addEventListener("pointerup", end);
    handle.addEventListener("pointercancel", end);
  }
}

function persist(app) {
  const state = {};
  for (const spec of SPLITTERS) {
    // Only tracks the user has actually moved. Storing empty strings for the
    // others makes the saved layout look corrupt on inspection and means
    // nothing.
    const value = app.style.getPropertyValue(spec.prop);
    if (value) state[spec.prop] = value;
  }
  try {
    localStorage.setItem(STORAGE_KEY, JSON.stringify(state));
  } catch {
    // Private browsing, a full quota, storage disabled. Losing the layout is
    // not worth an error.
  }
}

function restore(app) {
  let state;
  try {
    state = JSON.parse(localStorage.getItem(STORAGE_KEY) ?? "null");
  } catch {
    return;
  }
  if (!state) return;
  for (const spec of SPLITTERS) {
    const value = state[spec.prop];
    // Validated, not trusted: localStorage is user-writable, and a junk value
    // here would break the grid in a way that survives every reload.
    if (typeof value === "string" && /^\d+(\.\d+)?px$/.test(value)) {
      app.style.setProperty(spec.prop, value);
    }
  }
}

// --- theme ------------------------------------------------------------------

export function initTheme({ toggle }) {
  const stored = readTheme();
  if (stored) document.documentElement.dataset.theme = stored;
  update();

  toggle?.addEventListener("click", () => {
    const next = current() === "light" ? "dark" : "light";
    document.documentElement.dataset.theme = next;
    try {
      localStorage.setItem(THEME_KEY, next);
    } catch {
      // See persist().
    }
    update();
    // Terminals build their palette from the tokens at construction, so they
    // have to be told, and reloading them is the only way to apply it.
    document.dispatchEvent(new CustomEvent("gdb-wui:theme", { detail: next }));
  });

  function current() {
    return document.documentElement.dataset.theme
      || (window.matchMedia?.("(prefers-color-scheme: light)").matches ? "light" : "dark");
  }
  function update() {
    if (!toggle) return;
    const now = current();
    toggle.textContent = now === "light" ? "◐" : "◑";
    toggle.title = `Switch to ${now === "light" ? "dark" : "light"} theme`;
    toggle.setAttribute("aria-label", toggle.title);
  }
}

function readTheme() {
  try {
    const value = localStorage.getItem(THEME_KEY);
    return value === "light" || value === "dark" ? value : null;
  } catch {
    return null;
  }
}
