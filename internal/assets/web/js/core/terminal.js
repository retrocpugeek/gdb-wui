// Terminal construction, shared by the two terminals.
//
// The theme is built at boot from the same CSS custom properties everything
// else uses, so xterm cannot drift away from the rest of the UI when the theme
// changes. xterm needs concrete colour values, not var() references, which is
// why they are read with getComputedStyle rather than handed over as strings.

import { Terminal } from "../../vendor/xterm-6.0.0/xterm.mjs";
import { FitAddon } from "../../vendor/addon-fit-0.11.0/addon-fit.mjs";

function token(name, fallback) {
  const value = getComputedStyle(document.documentElement).getPropertyValue(name).trim();
  return value || fallback;
}

// buildTheme reads the current token values. Called again on a theme change:
// xterm needs concrete colours, not var() references, so it cannot follow CSS
// on its own.
function buildTheme() {
  return {
    background: token("--bg-1", "#000"),
    foreground: token("--fg-0", "#fff"),
    cursor: token("--accent", "#fff"),
    selectionBackground: token("--accent-dim", "#444"),
    black: token("--bg-0", "#000"),
    red: token("--danger", "#f00"),
    green: token("--ok", "#0f0"),
    yellow: token("--warn", "#ff0"),
    blue: token("--accent", "#00f"),
    magenta: token("--tok-keyword", "#f0f"),
    cyan: token("--tok-type", "#0ff"),
    white: token("--fg-0", "#fff"),
  };
}

export function createTerminal({ element, onData, onResize, scrollback = 5000 }) {
  const term = new Terminal({
    fontFamily: token("--font-mono", "monospace"),
    fontSize: 12,
    lineHeight: 1.2,
    scrollback,
    // The cursor blinks only where typing is expected; a blinking cursor in a
    // read-only log is a distraction that reads as an invitation.
    cursorBlink: Boolean(onData),
    convertEol: false,
    theme: buildTheme(),
  });

  const fit = new FitAddon();
  term.loadAddon(fit);
  term.open(element);

  function resize() {
    try {
      fit.fit();
    } catch {
      // fit throws when the element has no size yet — a hidden tab. Harmless.
      return;
    }
    onResize?.(term.rows, term.cols);
  }

  // A hidden panel has no size, so fitting has to wait until it is shown.
  const observer = new ResizeObserver(() => resize());
  observer.observe(element);
  requestAnimationFrame(resize);

  if (onData) term.onData(onData);

  return {
    term,
    write: (data) => term.write(data),
    writeln: (data) => term.writeln(data),
    clear: () => term.clear(),
    focus: () => term.focus(),
    resize,
    // retheme re-reads the tokens after a theme switch. Without it the
    // terminals keep the palette they were built with, which in a light UI
    // leaves two black rectangles that look like a rendering failure.
    retheme() {
      term.options.theme = buildTheme();
    },
    get rows() {
      return term.rows;
    },
    get cols() {
      return term.cols;
    },
    destroy() {
      observer.disconnect();
      term.dispose();
    },
  };
}

// decodeBase64 turns wire bytes into a string xterm can write.
//
// The server sends base64 because a debuggee's output is arbitrary bytes. Going
// through Uint8Array and TextDecoder rather than atob alone matters: atob
// yields one char per byte, so any multi-byte UTF-8 the program printed would
// render as mojibake.
const decoder = new TextDecoder("utf-8", { fatal: false });

export function decodeBase64(b64) {
  const binary = atob(b64);
  const bytes = new Uint8Array(binary.length);
  for (let i = 0; i < binary.length; i++) bytes[i] = binary.charCodeAt(i);
  return decoder.decode(bytes, { stream: true });
}

// encodeBase64 is the inverse, for keystrokes on their way to the program.
export function encodeBase64(text) {
  const bytes = new TextEncoder().encode(text);
  let binary = "";
  for (const b of bytes) binary += String.fromCharCode(b);
  return btoa(binary);
}
