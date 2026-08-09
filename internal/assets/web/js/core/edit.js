// Editing a value in place.
//
// Double-click a register, a variable or a byte and type over it; Enter writes
// it through gdb, Escape puts it back.
//
// The input is *not* a child of the cell, and that is the whole design. Every
// panel that has editable values renders on the virtual list, which recycles a
// small pool of row elements: the node showing row 40 becomes the node showing
// row 12 as soon as you scroll, and any input parked inside it would be
// overwritten by the next update() — or worse, silently reattached to a
// different value. So the input is positioned over the cell instead, and
// anything that could move the cell out from under it ends the edit.
//
// Ending rather than following is a deliberate choice. Following a cell through
// a scroll is possible and it is the wrong behaviour anyway: an edit box
// floating over a list that has scrolled somewhere else no longer says which
// value it is about.

let active = null;

/**
 * editCell opens an editor over one cell.
 *
 * commit is called with the typed text and returns a promise. While it is in
 * flight the input stays up and disabled, so a slow gdb does not look like a
 * lost keystroke, and a rejection puts the cursor back in the cell with the
 * text intact — a typo is corrected where it was made rather than retyped.
 */
export function editCell({ cell, value, title = "", commit, onError }) {
  cancelEdit();
  if (!cell?.isConnected) return;

  const rect = cell.getBoundingClientRect();
  const input = document.createElement("input");
  input.type = "text";
  input.className = "cell-edit";
  input.value = value ?? "";
  input.spellcheck = false;
  input.autocomplete = "off";
  if (title) input.title = title;
  input.setAttribute("aria-label", title || "New value");

  // Wide enough to type a replacement into, even over a narrow cell: a
  // one-character byte cell is not somewhere you can see what you are writing.
  const width = Math.max(rect.width + 8, 90);
  input.style.left = `${Math.round(rect.left - 3)}px`;
  input.style.top = `${Math.round(rect.top - 2)}px`;
  input.style.width = `${Math.round(width)}px`;
  input.style.height = `${Math.round(rect.height + 4)}px`;

  document.body.append(input);
  input.focus();
  input.select();

  const session = { input, done: false, finish };
  active = session;

  function finish() {
    if (session.done) return;
    session.done = true;
    if (active === session) active = null;
    input.remove();
    window.removeEventListener("scroll", onScroll, true);
    window.removeEventListener("resize", finish);
  }

  // Capture phase, so a scroll inside a panel counts and not only the page's —
  // but not the input's own. A text field scrolls its contents when the caret
  // is placed near the end, and that scroll used to close the box the instant
  // it was clicked in.
  function onScroll(ev) {
    if (ev.target === input) return;
    finish();
  }
  window.addEventListener("scroll", onScroll, true);
  window.addEventListener("resize", finish);

  // Enter and Escape reach here because the global keymap takes only function
  // keys and Ctrl+Shift chords out of a text input — the same rule that lets
  // Ctrl+C reach the terminal.
  input.addEventListener("keydown", (ev) => {
    if (ev.key === "Escape") {
      ev.preventDefault();
      finish();
      return;
    }
    if (ev.key !== "Enter") return;
    ev.preventDefault();

    const typed = input.value.trim();
    if (typed === "" || typed === (value ?? "")) {
      // Nothing typed, or nothing changed. Writing the same value back is not
      // harmless — it would mark the row as changed and claim an edit
      // happened — so this is a cancel.
      finish();
      return;
    }

    // readOnly rather than disabled: disabling a focused input blurs it, and
    // blur ends the edit — so a rejected write would take its own error
    // message down with it and look like nothing happened.
    input.readOnly = true;
    input.classList.add("is-busy");
    input.classList.remove("is-bad");
    Promise.resolve()
      .then(() => commit(typed))
      .then(() => finish())
      .catch((err) => {
        if (session.done) return;
        input.readOnly = false;
        input.classList.remove("is-busy");
        input.classList.add("is-bad");
        input.select();
        onError?.(err);
      });
  });

  // A click elsewhere abandons the edit. Not a commit: a value written because
  // the pointer moved is a value nobody asked to write.
  input.addEventListener("blur", () => finish());
}

export function cancelEdit() {
  active?.finish();
}
