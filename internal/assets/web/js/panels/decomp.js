// The decompiled view.
//
// Recovered C for a binary with no source, with the program counter marked on
// it. Same virtual list as the source and disassembly views, because a
// decompiled function is occasionally thousands of lines.
//
// Three things this pane has to state accurately, because a reader cannot
// otherwise tell how much to trust it:
//
//   The text is a model rather than the truth, so it sits beside the
//   disassembly rather than in place of it.
//
//   The highlighted line is sometimes ambiguous. In optimised code about one
//   address in five is claimed by two decompiled lines. The server picks the
//   lowest and reports when it had to, and that is shown rather than hidden.
//
//   Most variables cannot be read. Of Ghidra's storage kinds, only stack is
//   readable anywhere in the frame; a register is valid only near one pc, and
//   a decompiler temporary exists nowhere in the machine. Rows for those are
//   drawn and marked, so that the row is visibly blank rather than missing.

import { expressionAt as parseExpression } from "../core/expr.js";
import { createVirtualList, measureRowHeight } from "../core/virtual.js";

export function createDecomp({ element, onGutterClick }) {
  let fn = null;
  let lines = [];
  // addrsByLine and lineByAddr are the two directions the pane needs: one to
  // put a breakpoint on a line, the other to mark the line a pc is on.
  let addrsByLine = new Map();
  // commentByLine is the other direction of the same idea, for the lines that
  // are comment rather than code: they claim no addresses — the server leaves
  // them out of the map so the program counter is never put on one — but they
  // do belong to an address, and that is how pointing at a comment can edit it.
  let commentByLine = new Map();
  let comments = new Map();
  let vars = new Map();
  // bpLines is derived from the whole breakpoint mirror rather than filtered on
  // arrival, because the order of "which function is shown" and "which
  // breakpoints exist" is not fixed. A reload delivers the snapshot's
  // breakpoints before any function is fetched.
  let allBreakpoints = [];
  let bpLines = new Set();
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
          // A line with no addresses is a brace, a declaration or blank. It
          // cannot hold a breakpoint, so it must not look as though it can.
          const mapped = addrsByLine.has(n);
          row.classList.toggle("is-mapped", mapped);
          const comment = commentByLine.has(n)
            ? comments.get(normalise(commentByLine.get(n)))
            : null;
          row.classList.toggle("is-comment", commentByLine.has(n));
          // Whose note this is. A comment an agent wrote is a guess with a
          // sentence around it, and reading it as somebody's conclusion when
          // nobody concluded it is the mistake this prevents.
          row.classList.toggle("is-agent", comment?.author === "agent");
          if (comment?.author === "agent") {
            row.title = "written by an agent, not by you";
          } else if (row.title) {
            row.removeAttribute("title");
          }
          row.classList.toggle("has-bp", bpLines.has(n));
          row.classList.toggle("is-pc", n === fn?.pcLine);
          row.classList.toggle("is-pc-ambiguous",
            n === fn?.pcLine && Boolean(fn?.pcLineAmbiguous));
          // Approximate means no line claimed the pc and this is the nearest
          // one below it, such as a prologue or an epilogue. It is drawn
          // differently, because "the program is here" and "the program is
          // somewhere after here" are different claims.
          row.classList.toggle("is-pc-approx",
            n === fn?.pcLine && Boolean(fn?.pcLineApprox));
        },
      },
    });
  }

  // refilterBreakpoints maps the mirror's addresses onto lines of the function
  // currently shown. A breakpoint lands on the line that claims its address,
  // by the same lowest-wins rule as the program counter.
  function refilterBreakpoints() {
    bpLines = new Set();
    for (const bp of allBreakpoints) {
      if (!bp.address) continue;
      const want = normalise(bp.address);
      for (const [n, addrs] of addrsByLine) {
        if (addrs.some((a) => normalise(a) === want)) {
          bpLines.add(n);
          break;
        }
      }
    }
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
    // are the rest of the same statement, where a breakpoint would stop part
    // way through an expression.
    onGutterClick?.(addrs[0], Number(row.dataset.line));
  });

  // symbolAt finds the decompiler symbol under the pointer. The word is parsed
  // with the same parser the source view uses and looked up in the variable
  // table the server sent — Ghidra's names are its own, so without that map
  // there is nothing to resolve against.
  //
  // Everything in the map is returned, including the symbols that can never be
  // shown a value: a decompiler temporary is not readable but it is a real
  // symbol in Ghidra, and renaming it is exactly as useful as renaming a stack
  // slot.
  function symbolAt(ev) {
    const code = ev.target?.closest?.(".dec-code");
    if (!code) return null;
    const caret = caretAt(ev.clientX, ev.clientY);
    if (!caret || caret.node?.nodeType !== Node.TEXT_NODE) return null;
    if (caret.node.parentElement !== code) return null;

    const found = parseExpression(caret.node.data, caret.offset);
    if (!found) return null;
    // Only the bare name is looked up. `cfg->count` means nothing here, because
    // the decompiler's locals are not gdb's.
    const v = vars.get(found.expr);
    if (!v) return null;
    const range = document.createRange();
    range.setStart(caret.node, found.start);
    range.setEnd(caret.node, found.end);
    // storage travels with the hit so that a caller knows what can be done with
    // it. A register has no address to show in the memory view; a global is
    // renamed through its address rather than through a symbol id.
    return {
      expr: v.expr ?? "",
      storage: v.storage,
      name: v.name,
      id: v.id ?? "",
      addr: v.addr ?? "",
      type: v.type ?? "",
      // Where the name came from, so a caller can say "you named this" rather
      // than implying it about every name on the page.
      source: v.source ?? "",
      param: Boolean(v.param),
      rect: range.getBoundingClientRect(),
      anchor: code,
    };
  }

  // expressionAt answers the hover controller and the value menu, which both
  // need something gdb can evaluate. That is the one difference from symbolAt.
  function expressionAt(ev) {
    const hit = symbolAt(ev);
    return hit?.expr ? hit : null;
  }

  // commentTarget answers "what would a comment written here hang on".
  //
  // A comment goes on an address, so a line that came from none — a brace, a
  // declaration, a blank — cannot hold one. Those are exactly the lines that
  // cannot hold a breakpoint either, and for the same reason.
  //
  // Pointing at an existing comment names the address it already annotates, so
  // the obvious gesture for editing a comment is to right-click it. text is
  // what is stored, undecorated and unwrapped, so the editor opens on what was
  // typed rather than on how it was printed.
  function commentTarget(ev) {
    const row = ev.target?.closest?.(".dec-row");
    if (!row) return null;
    const n = Number(row.dataset.line);
    const addr = commentByLine.get(n) ?? addrsByLine.get(n)?.[0] ?? "";
    if (!addr) return null;
    const code = row.querySelector(".dec-code");
    return {
      line: n,
      addr,
      text: comments.get(normalise(addr))?.text ?? "",
      rect: (code ?? row).getBoundingClientRect(),
    };
  }

  // functionComment is the header comment, which hangs on the entry point
  // rather than on any line and is reachable whatever is scrolled into view.
  function functionComment() {
    if (!fn) return "";
    return comments.get(normalise(fn.entry))?.text ?? "";
  }

  // shown is the function on screen, for the menu items that are about the
  // function rather than about a word under the pointer — renaming
  // FUN_0010d2b0 does not require finding its name in the text.
  function shown() {
    if (!fn) return null;
    return {
      name: fn.name,
      entry: fn.entry,
      signature: fn.signature ?? "",
      source: fn.source ?? "",
    };
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
    commentByLine = new Map();
    comments = new Map();
    vars = new Map();
    const div = document.createElement("div");
    div.className = className;
    div.textContent = text;
    element.replaceChildren(div);
  }

  return {
    expressionAt,
    symbolAt,
    commentTarget,
    functionComment,
    shown,

    set(out) {
      fn = out;
      lines = (out.text ?? "").split("\n");
      if (lines.length > 1 && lines[lines.length - 1] === "") lines.pop();

      addrsByLine = new Map();
      for (const l of out.lines ?? []) {
        if (l.addrs?.length) addrsByLine.set(l.n, l.addrs);
      }
      commentByLine = new Map();
      for (const c of out.commentLines ?? []) commentByLine.set(c.n, c.addr ?? "");
      comments = new Map();
      for (const c of out.comments ?? []) comments.set(normalise(c.addr), c);
      vars = new Map();
      for (const v of out.vars ?? []) vars.set(v.name, v);
      refilterBreakpoints();

      element.replaceChildren();
      list = null;
      ensureList();
      list.setCount(lines.length);
      reveal(out.pcLine);
    },

    // summary is the header line, and carries the caveats. Without them a user
    // has no way to know that the addresses are link-time, or that the
    // highlighted line was one of two candidates.
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

    // Moves the program-counter highlight without refetching, for a step that
    // stays inside the function already on screen.
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

    // stepMap is what a step-by-line walk needs: the whole map rather than the
    // current line. A line's addresses are sparse, holding only the ones its
    // tokens carry, so "step until the pc leaves this line's addresses" would
    // end at the first instruction between them. The server steps until the pc
    // resolves to a different line, and needs every line to resolve against.
    //
    // An approximate pc is not a reason to refuse. Breaking on a function
    // leaves the pc in the prologue, which belongs to no line, and that is the
    // most common place to start stepping. Refusing there fell back to gdb's
    // own next, which with no line table runs to the function's exit. The
    // server treats "approximate" as "between lines" and walks to the next real
    // one.
    stepMap() {
      if (!fn?.lines?.length) return null;
      return {
        lines: fn.lines ?? [],
        bodyStart: fn.bodyStart,
        bodyEnd: fn.bodyEnd,
      };
    },

    // setBreakpoints takes the whole mirror; the server is authoritative.
    setBreakpoints(list_) {
      allBreakpoints = list_ ?? [];
      refilterBreakpoints();
      list?.refresh();
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
