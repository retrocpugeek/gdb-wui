// One capture-phase keyboard dispatcher.
//
// Capture phase, and one listener: a debugger's function keys must work
// wherever focus happens to be, and per-panel handlers would race each other
// for F10.
//
// The composition rule matters as much as the bindings. Inside a terminal or a
// text input, only function keys and Ctrl+Shift chords are intercepted —
// Ctrl+C, Ctrl+D, Tab and the arrows must reach the terminal, because sending
// Ctrl+C to the inferior is a real debugging need, not an edge case. Escape
// leaves the terminal.

export function createKeymap({ bindings, isTerminalFocus }) {
  function describe(ev) {
    const parts = [];
    if (ev.ctrlKey) parts.push("Ctrl");
    if (ev.altKey) parts.push("Alt");
    if (ev.shiftKey) parts.push("Shift");
    if (ev.metaKey) parts.push("Meta");
    parts.push(ev.key.length === 1 ? ev.key.toUpperCase() : ev.key);
    return parts.join("+");
  }

  function isFunctionKey(ev) {
    return /^F\d{1,2}$/.test(ev.key);
  }

  function inEditableContext(target) {
    if (!target) return false;
    if (isTerminalFocus?.(target)) return true;
    const tag = target.tagName;
    return tag === "INPUT" || tag === "TEXTAREA" || target.isContentEditable;
  }

  function onKeyDown(ev) {
    const combo = describe(ev);
    const handler = bindings[combo];
    if (!handler) return;

    if (inEditableContext(ev.target)) {
      // Only function keys and Ctrl+Shift chords are taken from a terminal.
      const allowed = isFunctionKey(ev) || (ev.ctrlKey && ev.shiftKey);
      if (!allowed) return;
    }

    ev.preventDefault();
    ev.stopPropagation();
    handler(ev);
  }

  document.addEventListener("keydown", onKeyDown, { capture: true });
  return {
    destroy() {
      document.removeEventListener("keydown", onKeyDown, { capture: true });
    },
  };
}
