// The hover evaluator: point at a variable or a register, see what it holds.
//
// This is a controller, not a panel. Each pane knows how to turn a point in
// itself into an expression — the source view walks text, the disassembly view
// reads the token span under the pointer — and hands that back. Everything
// after that is the same for both: wait, ask, place, hide.
//
// Two properties matter more than the feature does.
//
// It must not flood gdb. A pointer crossing a screenful of source passes over
// dozens of identifiers, and a request per identifier would put the debugger's
// command queue behind the mouse. So nothing is sent until the pointer has
// rested on the *same* expression for DWELL_MS, and an expression already on
// screen is never re-asked.
//
// It must not lie. A value is only true for the stop it was read at, so the
// tooltip goes away the moment the program moves, and a reply that arrives
// after the pointer has moved on is dropped rather than shown.

import { alternateBase } from "./expr.js";

const DWELL_MS = 300;

export function createHover({ element, evaluate, isEnabled }) {
  // pending is what the pointer is over; shown is what the tooltip displays.
  // They differ while the dwell timer runs, which is most of the time.
  let pending = null;
  let shown = null;
  let timer = 0;
  // seq drops a reply whose request the pointer has already outlived.
  let seq = 0;
  // The native tooltip of whatever we are covering, borrowed while ours is up.
  // The disassembly puts source:line in a title attribute, and two tooltips
  // fighting over the same word is worse than either alone.
  let borrowed = null;

  function clearTimer() {
    if (timer) {
      clearTimeout(timer);
      timer = 0;
    }
  }

  function restoreTitle() {
    if (!borrowed) return;
    // Only put it back if nothing else claimed it. The virtual list recycles
    // rows, so the node may since have been re-rendered as a different line.
    if (!borrowed.el.title) borrowed.el.title = borrowed.title;
    borrowed = null;
  }

  function hide() {
    clearTimer();
    seq++;
    pending = null;
    shown = null;
    restoreTitle();
    element.classList.add("is-hidden");
    element.replaceChildren();
  }

  function show(hit, value) {
    const expr = document.createElement("span");
    expr.className = "hovertip-expr";
    expr.textContent = hit.expr;
    const val = document.createElement("span");
    val.className = "hovertip-value";
    val.textContent = value;
    element.replaceChildren(expr, document.createTextNode(" = "), val);

    const alt = alternateBase(value);
    if (alt) {
      const extra = document.createElement("span");
      extra.className = "hovertip-alt";
      extra.textContent = alt;
      element.append(extra);
    }

    restoreTitle();
    const titled = hit.anchor?.closest?.("[title]");
    if (titled?.title) {
      borrowed = { el: titled, title: titled.title };
      titled.title = "";
    }

    element.classList.remove("is-hidden");
    place(hit.rect);
    shown = hit.expr;
  }

  // place puts the tooltip above the word it describes, or below when there is
  // no room above. Above is preferred because the pointer is on the word and a
  // tooltip under it would sit where the user is about to look next.
  function place(rect) {
    const box = element.getBoundingClientRect();
    const margin = 4;
    let top = rect.top - box.height - margin;
    if (top < margin) top = rect.bottom + margin;
    const left = Math.max(
      margin,
      Math.min(rect.left, window.innerWidth - box.width - margin),
    );
    element.style.left = `${left}px`;
    element.style.top = `${Math.min(top, window.innerHeight - box.height - margin)}px`;
  }

  function schedule(hit) {
    clearTimer();
    pending = hit;
    const mine = ++seq;
    timer = setTimeout(() => {
      timer = 0;
      Promise.resolve(evaluate(hit.expr)).then((value) => {
        if (mine !== seq) return;
        // gdb answers `void` for a convenience variable that was never set,
        // which is how a guess at a bare register name comes back wrong. It is
        // also what an expression of type void evaluates to. Either way there
        // is nothing to tell the user.
        if (value == null || value === "" || value === "void") {
          hide();
          return;
        }
        show(hit, value);
      }, () => {
        // An error here is ordinary — a name out of scope, a word that only
        // looked like one — and belongs nowhere near the status bar.
        if (mine === seq) hide();
      });
    }, DWELL_MS);
  }

  // attach wires one pane. resolve(event) returns {expr, rect, anchor} or null.
  function attach(pane, resolve) {
    pane.addEventListener("mousemove", (ev) => {
      // A held button means a drag or a text selection, neither of which wants
      // a tooltip appearing under the pointer.
      if (ev.buttons) {
        hide();
        return;
      }
      if (isEnabled && !isEnabled()) {
        if (shown || pending) hide();
        return;
      }
      const hit = resolve(ev);
      if (!hit) {
        hide();
        return;
      }
      // Still over the same thing: leave both the timer and the tooltip alone.
      // Without this, moving within one word restarts the dwell forever and
      // the tooltip never appears.
      if (hit.expr === shown || hit.expr === pending?.expr) return;
      hide();
      schedule(hit);
    });
    pane.addEventListener("mouseleave", hide);
    pane.addEventListener("mousedown", hide);
    // The virtual list scrolls the pane itself, so a scroll moves the word out
    // from under a tooltip that would otherwise stay put.
    pane.addEventListener("scroll", hide, { passive: true });
    pane.addEventListener("wheel", hide, { passive: true });
  }

  // Anything that moves the page, changes the program, or takes the user
  // somewhere else invalidates a value read at a particular stop.
  window.addEventListener("blur", hide);
  window.addEventListener("resize", hide);
  document.addEventListener("keydown", hide);

  return { attach, hide };
}
