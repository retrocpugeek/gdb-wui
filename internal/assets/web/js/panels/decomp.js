// The decompiled view.
//
// Recovered C for a binary with no source, with the program counter marked on
// it. Same virtual list as the source and disassembly views, because a
// decompiled function is occasionally thousands of lines.
//
// Three things this pane has to be honest about, and they are the difference
// between it being useful and being a confident liar:
//
//   The text is a model, not the truth. It sits beside the disassembly, never
//   instead of it.
//
//   The highlighted line is sometimes ambiguous. On optimised code about one
//   address in five is claimed by two decompiled lines; the server picks the
//   lowest and says when it had to, and that is shown rather than hidden.
//
//   Most variables cannot be read. Of Ghidra's storage kinds only stack is
//   readable anywhere in the frame; a register is valid only near one pc, and
//   a decompiler temporary exists nowhere in the machine at all. Rows for
//   those are drawn and marked, because a blank is honest and a missing row
//   is not.

import { expressionAt as parseExpression } from "../core/expr.js";
import { createVirtualList, measureRowHeight } from "../core/virtual.js";

export function createDecomp({ element, onGutterClick }) {
  let fn = null;
  let lines = [];
  // addrsByLine and lineByAddr are the two directions the pane needs: one to
  // put a breakpoint on a line, the other to mark the line a pc is on.
  let addrsByLine = new Map();
  let vars = new Map();
  let list = null;

  function cssLineHeight() {
    const raw = getComputedStyle(document.documentElement).getPropertyValue("--line-h");
    const n = parseFloat(raw);
    return Number.isFinite(n) && n > 0 ? n : 19;
  }

  function ensureList() {
    if (list) return;
    list = createVirtualList({
      container: element,
      rowHeight: measureRowHeight(element, cssLineHeight()),
      renderRow: {
        create() {
          const row = document.createElement("div");
          row.className = "dec-row";
          const gutter = document.createElement("span");
          gutter.className = "dec-gutter";
          const num = document.createElement("span");
          num.className = "dec-lineno";
          gutter.append(num);
          const code = document.createElement("span");
          code.className = "dec-code";
          row.append(gutter, code);
          return row;
        },
        update(row, index) {
          const n = index + 1;
          row.dataset.line = String(n);
          row.querySelector(".dec-lineno").textContent = String(n);
          row.querySelector(".dec-code").textContent = lines[index] ?? "";
          // A line with no addresses is a brace, a declaration or blank: it
          // cannot hold a breakpoint and must not look like it can.
          const mapped = addrsByLine.has(n);
          row.classList.toggle("is-mapped", mapped);
          row.classList.toggle("is-pc", n === fn?.pcLine);
          row.classList.toggle("is-pc-ambiguous",
            n === fn?.pcLine && Boolean(fn?.pcLineAmbiguous));
          // Approximate means no line claimed the pc and this is the nearest
          // one below it — a prologue or an epilogue. Drawn differently,
          // because "the program is here" and "the program is somewhere after
          // here" are not the same claim.
          row.classList.toggle("is-pc-approx",
            n === fn?.pcLine && Boolean(fn?.pcLineApprox));
        },
      },
    });
  }

  function reveal(n) {
    if (!list || !n) return;
    const index = n - 1;
    if (list.isRowVisible(index)) {
      list.refresh();
      return;
    }
    list.scrollToRow(index);
  }

  element.addEventListener("click", (ev) => {
    const gutter = ev.target.closest(".dec-gutter");
    if (!gutter) return;
    const row = gutter.closest(".dec-row");
    if (!row) return;
    const addrs = addrsByLine.get(Number(row.dataset.line));
    if (!addrs?.length) return;
    // The lowest address of the line is where execution reaches it. The others
    // are the same statement's other pieces, which a breakpoint would hit at
    // an arbitrary point mid-expression.
    onGutterClick?.(addrs[0], Number(row.dataset.line));
  });

  // expressionAt answers the hover controller. The word under the pointer is
  // found with the same parser the source view uses, then looked up in the
  // variable table the server sent — Ghidra's names are its own invention, so
  // there is nothing gdb could resolve without that map.
  function expressionAt(ev) {
    const code = ev.target?.closest?.(".dec-code");
    if (!code) return null;
    const caret = caretAt(ev.clientX, ev.clientY);
    if (!caret || caret.node?.nodeType !== Node.TEXT_NODE) return null;
    if (caret.node.parentElement !== code) return null;

    const found = parseExpression(caret.node.data, caret.offset);
    if (!found) return null;
    // Only the bare name is looked up: `cfg->count` means nothing here,
    // because the decompiler's locals are not gdb's.
    const v = vars.get(found.expr);
    if (!v?.expr) return null;
    const range = document.createRange();
    range.setStart(caret.node, found.start);
    range.setEnd(caret.node, found.end);
    return { expr: v.expr, rect: range.getBoundingClientRect(), anchor: code };
  }

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

  function showMessage(text, className) {
    if (list) {
      list.destroy();
      list = null;
    }
    fn = null;
    lines = [];
    addrsByLine = new Map();
    vars = new Map();
    const div = document.createElement("div");
    div.className = className;
    div.textContent = text;
    element.replaceChildren(div);
  }

  return {
    expressionAt,

    set(out) {
      fn = out;
      lines = (out.text ?? "").split("\n");
      if (lines.length > 1 && lines[lines.length - 1] === "") lines.pop();

      addrsByLine = new Map();
      for (const l of out.lines ?? []) {
        if (l.addrs?.length) addrsByLine.set(l.n, l.addrs);
      }
      vars = new Map();
      for (const v of out.vars ?? []) vars.set(v.name, v);

      element.replaceChildren();
      list = null;
      ensureList();
      list.setCount(lines.length);
      reveal(out.pcLine);
    },

    // summary is the header line, and it carries the caveats. Where they are
    // not shown, a user has no way to know the addresses are link-time or that
    // the highlight was a coin toss.
    summary() {
      if (!fn) return "";
      const parts = [fn.name];
      if (fn.lineCount || lines.length) parts.push(`${lines.length} lines`);
      if (!fn.biasFrom) {
        // No symbol was shared with gdb, so nothing anchored these addresses
        // to the running program.
        parts.push("link-time addresses");
      }
      if (fn.pcLineAmbiguous) parts.push("pc line ambiguous");
      if (fn.pcLineApprox) parts.push("pc between lines");
      return parts.join(" · ");
    },

    // marks moves the program-counter highlight without refetching, for a step
    // that stays inside the function already on screen.
    setPCLine(n, ambiguous, approx) {
      if (!fn) return false;
      fn.pcLine = n;
      fn.pcLineAmbiguous = Boolean(ambiguous);
      fn.pcLineApprox = Boolean(approx);
      list?.refresh();
      reveal(n);
      return true;
    },

    // has reports whether an address belongs to the function on screen, which
    // is how the caller decides between moving the marker and refetching.
    has(address) {
      if (!fn) return false;
      const want = normalise(address);
      for (const addrs of addrsByLine.values()) {
        for (const a of addrs) {
          if (normalise(a) === want) return true;
        }
      }
      return false;
    },

    // lineFor maps an address back to the line claiming it, applying the same
    // lowest-wins rule the server uses so the two never disagree.
    lineFor(address) {
      const want = normalise(address);
      let best = 0;
      let count = 0;
      for (const [n, addrs] of addrsByLine) {
        for (const a of addrs) {
          if (normalise(a) !== want) continue;
          count++;
          if (best === 0 || n < best) best = n;
          break;
        }
      }
      return { line: best, ambiguous: count > 1 };
    },

    pcLineApprox() {
      return Boolean(fn?.pcLineApprox);
    },

    message: showMessage,

    clear() {
      showMessage("Nothing decompiled yet.", "src-empty");
    },
  };
}

// normalise makes two spellings of one address comparable. gdb pads to the
// pointer width and the decompiler does not.
function normalise(addr) {
  if (typeof addr !== "string") return "";
  const m = /^0x0*([0-9a-f]+)$/i.exec(addr.trim());
  return m ? m[1].toLowerCase() : addr.trim().toLowerCase();
}
