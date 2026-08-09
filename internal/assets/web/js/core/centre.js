// Which views the centre area is showing, and which of them the keys act on.
//
// There are two slots. Unsplit, only slot A is on screen; split, both are, side
// by side or stacked. Any of the four views can be in either slot.
//
// A view is placed by writing data-slot on its element, not by moving it.
// Moving a node resets its scrollTop, so a reparenting design loses your place
// in a file every time a view changes slot.
//
// One slot is focused, and that is the answer to "which view do the keys act
// on". Stepping needs a single answer even when the disassembly and the
// decompiled C are both on screen, and the rule a reader already knows from an
// editor is the one they last clicked in.

const STORAGE_KEY = "gdb-wui.centre";

// The complement to open beside a view when the split is first turned on.
// Reading recovered C against the instructions it came from is the case this
// feature exists for, so those two point at each other.
const COMPLEMENT = {
  disasm: "decomp",
  decomp: "disasm",
  source: "disasm",
  memory: "source",
};

export function createCentre({ element, onChange }) {
  const views = new Map();
  for (const el of element.querySelectorAll("[data-center]")) {
    views.set(el.dataset.center, el);
  }

  const heads = {
    a: element.querySelector('[data-slot-head="a"]'),
    b: element.querySelector('[data-slot-head="b"]'),
  };
  const names = {
    a: element.querySelector('[data-slot-name="a"]'),
    b: element.querySelector('[data-slot-name="b"]'),
  };

  const state = {
    split: "off",
    slots: { a: "source", b: "disasm" },
    focus: "a",
    ...restore(),
  };

  /** slotOf returns the slot a view is in, or "" when it is not on screen. */
  function slotOf(name) {
    if (state.slots.a === name) return "a";
    if (state.split !== "off" && state.slots.b === name) return "b";
    return "";
  }

  function isVisible(name) {
    return slotOf(name) !== "";
  }

  function focused() {
    return state.slots[state.focus];
  }

  function visible() {
    return state.split === "off" ? [state.slots.a] : [state.slots.a, state.slots.b];
  }

  /**
   * assign puts a view in a slot.
   *
   * A view already in the other slot swaps rather than appearing twice: two
   * slots showing the same thing is never what was meant, and it would leave
   * one of them permanently unreachable from the tabs.
   */
  function assign(slot, name) {
    if (!views.has(name)) return;
    const other = slot === "a" ? "b" : "a";
    if (state.split !== "off" && state.slots[other] === name) {
      state.slots[other] = state.slots[slot];
    }
    state.slots[slot] = name;
    state.focus = slot;
    apply();
  }

  /**
   * show brings a view up without disturbing more than it has to.
   *
   * A view already on screen is focused where it is rather than being moved
   * into the focused slot, so following a pointer into the disassembly while
   * the decompiled C is beside it does not throw the decompiled C away.
   */
  function show(name) {
    const at = slotOf(name);
    if (at) {
      state.focus = at;
      apply();
      return;
    }
    assign(state.focus, name);
  }

  function focusSlot(slot) {
    if (state.split === "off" || state.focus === slot) return;
    state.focus = slot;
    apply();
  }

  function setSplit(mode) {
    if (state.split === mode) return;
    const wasOff = state.split === "off";
    state.split = mode;
    if (mode !== "off" && wasOff) {
      // Sizing the divider from the real box, because the CSS fallback is a
      // guess and a first split that lands a long way from the middle reads as
      // a bug rather than as a default.
      sizeDivider(mode);
      if (state.slots.b === state.slots.a) {
        state.slots.b = COMPLEMENT[state.slots.a] ?? "disasm";
      }
    }
    if (mode === "off") state.focus = "a";
    apply();
  }

  function sizeDivider(mode) {
    const app = element.closest("#app") ?? document.getElementById("app");
    if (!app) return;
    const prop = mode === "y" ? "--center-a-h" : "--center-a-w";
    if (app.style.getPropertyValue(prop)) return;
    const size = mode === "y" ? element.clientHeight : element.clientWidth;
    if (size > 0) app.style.setProperty(prop, `${Math.round(size / 2)}px`);
  }

  // notify is false for the first pass, which runs while createCentre is still
  // returning: main.js builds the panels after this, so calling back into it
  // here would reach them in their temporal dead zone. main calls sync() once
  // everything exists.
  function apply({ notify = true } = {}) {
    element.dataset.split = state.split;

    for (const [name, el] of views) {
      const slot = slotOf(name);
      if (slot) el.dataset.slot = slot;
      else delete el.dataset.slot;
    }

    heads.b.classList.toggle("is-hidden", state.split === "off");
    for (const slot of ["a", "b"]) {
      heads[slot].classList.toggle("is-focused", state.focus === slot);
      const name = state.slots[slot];
      names[slot].textContent = LABEL[name] ?? name;
    }

    persist(state);
    if (notify) onChange?.({ focused: focused(), visible: visible(), split: state.split });
  }

  // Clicking anywhere in a slot focuses it, so "which view do the keys act on"
  // is answered by where you last clicked. Capture phase, because the panes
  // stop some clicks themselves.
  element.addEventListener("mousedown", (ev) => {
    const body = ev.target.closest?.("[data-slot]");
    if (body?.dataset.slot) focusSlot(body.dataset.slot);
  }, true);

  apply({ notify: false });

  return {
    // sync applies the restored state and tells main about it, once main has
    // finished building the things onChange touches.
    sync: () => apply(),
    isVisible,
    focused,
    visible,
    show,
    assign,
    focusSlot,
    slotOf,
    split: () => state.split,
    focusedSlot: () => state.focus,
    slots: () => ({ ...state.slots }),
    toggleSplit: () => setSplit(state.split === "off" ? "x" : "off"),
    toggleOrientation: () => {
      if (state.split === "off") return;
      setSplit(state.split === "x" ? "y" : "x");
    },
  };
}

// The names in the slot headers. The tab labels, so the header and the tab
// that put it there agree.
const LABEL = {
  source: "Source",
  disasm: "Disassembly",
  decomp: "Decompiled",
  memory: "Memory",
};

function persist(state) {
  try {
    localStorage.setItem(STORAGE_KEY, JSON.stringify({
      split: state.split,
      slots: state.slots,
      focus: state.focus,
    }));
  } catch {
    // Private browsing, a full quota, storage disabled. Losing the layout is
    // not worth an error.
  }
}

// restore validates rather than trusts: localStorage is user-writable, and a
// junk value here would leave the centre area showing nothing, with no way back
// except knowing to clear storage.
function restore() {
  let raw;
  try {
    raw = JSON.parse(localStorage.getItem(STORAGE_KEY) ?? "null");
  } catch {
    return {};
  }
  if (!raw || typeof raw !== "object") return {};

  const out = {};
  if (raw.split === "off" || raw.split === "x" || raw.split === "y") {
    out.split = raw.split;
  }
  if (raw.focus === "a" || raw.focus === "b") out.focus = raw.focus;
  if (raw.slots && LABEL[raw.slots.a] && LABEL[raw.slots.b]) {
    out.slots = { a: raw.slots.a, b: raw.slots.b };
  }
  // A focus on the second slot with no split is not a state the UI can show.
  if (out.split === "off") out.focus = "a";
  return out;
}
