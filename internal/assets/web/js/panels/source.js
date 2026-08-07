// The source viewer.
//
// Decorations — the execution line, breakpoint markers, the selected frame's
// line — are class toggles on already-rendered rows, never re-renders. That is
// the step hot path: holding F10 must move one CSS class per stop, not rebuild
// a file.

import { fetchFile } from "../core/api.js";
import { expressionAt as parseExpression } from "../core/expr.js";
import { createVirtualList, measureRowHeight } from "../core/virtual.js";

export function createSource({ element, pathLabel, metaLabel, onGutterClick }) {
  let path = null;
  let lines = [];
  // Two distinct markers, and the distinction matters. execLine is where the
  // program counter actually is; frameLine is the line of an outer frame the
  // user is inspecting. Conflating them means clicking a caller in the stack
  // moves the "executing here" bar to code that is not executing, and the user
  // loses sight of where the program really is.
  //
  // Both carry the file they belong to. A bare line number gets painted onto
  // whatever file happens to be open, which produces a confident execution
  // marker in a file that is not running.
  let execLine = 0;
  let execPath = null;
  let frameLine = 0;
  let framePath = null;
  // The whole mirror is kept, not just this file's, because the order of
  // "which file is open" and "which breakpoints exist" is not fixed: a reload
  // delivers the snapshot's breakpoints before the source is fetched, and
  // filtering at arrival time would silently drop them all.
  let allBreakpoints = [];
  let breakpoints = new Map(); // line -> {enabled, pending, number}
  let list = null;
  let rowHeight = 19;
  // token guards against a slow fetch landing after the user moved on.
  let token = 0;

  function cssLineHeight() {
    const raw = getComputedStyle(document.documentElement).getPropertyValue("--line-h");
    const n = parseFloat(raw);
    return Number.isFinite(n) && n > 0 ? n : 19;
  }

  function ensureList() {
    if (list) return;
    rowHeight = measureRowHeight(element, cssLineHeight());
    list = createVirtualList({
      container: element,
      rowHeight,
      renderRow: {
        create() {
          const row = document.createElement("div");
          row.className = "src-row";
          const gutter = document.createElement("span");
          gutter.className = "src-gutter";
          const marker = document.createElement("span");
          marker.className = "src-bp";
          const num = document.createElement("span");
          num.className = "src-lineno";
          gutter.append(marker, num);
          const code = document.createElement("span");
          code.className = "src-code";
          row.append(gutter, code);
          return row;
        },
        update(row, index) {
          const line = index + 1;
          row.dataset.line = String(line);
          row.querySelector(".src-lineno").textContent = String(line);
          row.querySelector(".src-code").textContent = lines[index] ?? "";
          applyDecorations(row, line);
        },
      },
    });
  }

  // refilterBreakpoints derives this file's markers from the full mirror. It
  // runs both when the mirror changes and when the open file changes, so
  // neither order loses them.
  function refilterBreakpoints() {
    breakpoints = new Map();
    for (const bp of allBreakpoints) {
      if (bp.path && bp.path === path && bp.line) {
        breakpoints.set(bp.line, {
          number: bp.number,
          enabled: bp.enabled,
          pending: bp.pending,
        });
      }
    }
  }

  function applyDecorations(row, line) {
    const bp = breakpoints.get(line);
    const isExec = line === execLine && path === execPath;
    const isFrame = line === frameLine && path === framePath && !isExec;
    row.classList.toggle("is-exec", isExec);
    row.classList.toggle("is-frame", isFrame);
    row.classList.toggle("has-bp", Boolean(bp));
    row.classList.toggle("bp-disabled", Boolean(bp && !bp.enabled));
    row.classList.toggle("bp-pending", Boolean(bp && bp.pending));
  }

  // redecorate touches only what is on screen. It is O(visible rows), which is
  // what makes a held-down step key survivable.
  function redecorate() {
    if (!list) return;
    list.forEachRendered((row, index) => applyDecorations(row, index + 1));
  }

  function showMessage(text, className) {
    if (list) {
      list.destroy();
      list = null;
    }
    const div = document.createElement("div");
    div.className = className;
    div.textContent = text;
    element.replaceChildren(div);
  }

  async function open(next, { line = 0 } = {}) {
    if (next === path) {
      // Repaint the header even though the file has not changed. The centre
      // tabs blank it while the disassembly or memory view is showing, and a
      // caller re-opening the file it is already on is exactly how you get
      // back — leaving it blank shows a file with no name above it.
      pathLabel.textContent = next;
      pathLabel.title = next;
      if (line) reveal(line);
      return;
    }
    const mine = ++token;
    path = next;
    pathLabel.textContent = next;
    pathLabel.title = next;
    metaLabel.textContent = "loading…";

    let file;
    try {
      file = await fetchFile(next);
    } catch (err) {
      if (mine !== token) return;
      path = null;
      metaLabel.textContent = "";
      showMessage(errorText(err), "src-error");
      return;
    }
    if (mine !== token) return;

    lines = file.text.split("\n");
    if (lines.length > 1 && lines[lines.length - 1] === "") lines.pop();

    element.replaceChildren();
    list = null;
    ensureList();
    refilterBreakpoints();
    list.setCount(lines.length);
    metaLabel.textContent = `${lines.length} lines · ${formatBytes(file.text.length)}`;
    if (line) reveal(line);
  }

  function errorText(err) {
    switch (err.code) {
      case "too_large":
        return "This file is too large to display. The memory viewer arrives in M7.";
      case "not_found":
        return "That file no longer exists.";
      case "path_denied":
        return "That file is outside the project root.";
      default:
        return err.message;
    }
  }

  // reveal scrolls a line into view, with a jitter guard: if it is already
  // comfortably on screen, leave the scroll alone. Without this, stepping
  // through a loop bounces the viewport on every stop.
  function reveal(line) {
    if (!list || line <= 0) return;
    const index = line - 1;
    if (list.isRowVisible(index)) {
      list.refresh();
      return;
    }
    list.scrollToRow(index);
  }

  element.addEventListener("click", (ev) => {
    const gutter = ev.target.closest(".src-gutter");
    if (!gutter) return;
    const row = gutter.closest(".src-row");
    if (!row) return;
    onGutterClick?.(path, Number(row.dataset.line));
  });

  // caretAt maps a viewport point onto a character in a text node.
  //
  // Two spellings because the standard one is recent: Firefox and current
  // Chrome have caretPositionFromPoint, older Chrome and Safari only ever had
  // caretRangeFromPoint. Neither is worth a fallback that guesses from column
  // widths — without one of these there is simply no hover.
  function caretAt(x, y) {
    if (document.caretPositionFromPoint) {
      const pos = document.caretPositionFromPoint(x, y);
      return pos ? { node: pos.offsetNode, offset: pos.offset } : null;
    }
    if (document.caretRangeFromPoint) {
      const range = document.caretRangeFromPoint(x, y);
      return range ? { node: range.startContainer, offset: range.startOffset } : null;
    }
    return null;
  }

  // expressionAt is what the hover controller asks on every mouse move.
  //
  // The rendered line is a single text node — the row renderer sets
  // textContent, not markup — so the offset the caret API reports indexes
  // straight into the source text and no reverse mapping is needed. That is
  // also why this does not survive syntax highlighting unchanged.
  function expressionAt(ev) {
    const code = ev.target?.closest?.(".src-code");
    if (!code) return null;
    const caret = caretAt(ev.clientX, ev.clientY);
    if (!caret || caret.node?.nodeType !== Node.TEXT_NODE) return null;
    if (caret.node.parentElement !== code) return null;

    const found = parseExpression(caret.node.data, caret.offset);
    if (!found) return null;
    // Anchor to the whole chain rather than to the pointer: a tooltip for
    // `cfg.items[2].name` should point at all of it.
    const range = document.createRange();
    range.setStart(caret.node, found.start);
    range.setEnd(caret.node, found.end);
    return { expr: found.expr, rect: range.getBoundingClientRect(), anchor: code };
  }

  return {
    open,
    expressionAt,
    get path() {
      return path;
    },
    clear() {
      path = null;
      lines = [];
      pathLabel.textContent = "No file open";
      metaLabel.textContent = "";
      showMessage("Choose a file from the tree, or load a program and run it.", "src-empty");
    },
    // setExecLine moves the program-counter marker. Same file: two class
    // toggles. Different file: a fetch, then the highlight.
    async setExecLine(nextPath, line) {
      execPath = nextPath ?? path;
      execLine = line;
      // A new stop supersedes whatever frame was being inspected.
      frameLine = 0;
      framePath = null;
      if (nextPath && nextPath !== path) {
        await open(nextPath, { line });
        redecorate();
        return;
      }
      redecorate();
      if (line) reveal(line);
    },
    // setFrameLine marks the line of an outer frame being inspected, leaving
    // the execution marker where the program counter actually is.
    async setFrameLine(nextPath, line) {
      framePath = nextPath ?? path;
      frameLine = line;
      if (nextPath && nextPath !== path) {
        await open(nextPath, { line });
        redecorate();
        return;
      }
      redecorate();
      if (line) reveal(line);
    },
    clearExecLine() {
      execLine = 0;
      execPath = null;
      frameLine = 0;
      framePath = null;
      redecorate();
    },
    // setBreakpoints takes the whole mirror; the server is authoritative.
    setBreakpoints(list_) {
      allBreakpoints = list_ ?? [];
      refilterBreakpoints();
      redecorate();
    },
    breakpointAt(line) {
      return breakpoints.get(line);
    },
  };
}

function formatBytes(n) {
  if (n < 1024) return `${n} B`;
  if (n < 1024 * 1024) return `${(n / 1024).toFixed(1)} KiB`;
  return `${(n / (1024 * 1024)).toFixed(1)} MiB`;
}
