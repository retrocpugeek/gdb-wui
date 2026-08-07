// The disassembly view.
//
// Same virtual list as the source view, because a function can be thousands of
// instructions and the hex viewer will want it too.
//
// The tokenizer is hand-written, forty lines and four classes. highlight.js has
// an x86asm grammar, but it targets assembler *source* — labels, directives,
// comments — and mis-parses objdump-style output, where the columns are an
// address, raw bytes and an AT&T instruction. Nineteen kilobytes to get it
// wrong is a poor trade against forty lines to get it right.

import { bareRegisterExpr, registerExpr, symbolExpr } from "../core/expr.js";
import { createVirtualList, measureRowHeight } from "../core/virtual.js";

// Four classes is all this needs: registers, immediates and addresses,
// mnemonics, and everything else. Anything finer is decoration.
const REGISTER = /^%[a-z0-9]+$/;
const IMMEDIATE = /^\$?-?0x[0-9a-f]+$|^\$-?\d+$/i;

export function createDisasm({ element, onGutterClick }) {
  let instructions = [];
  let pc = "";
  let breakpoints = new Set(); // addresses
  let list = null;
  let meta = { func: "", hasSource: false, truncated: false };

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
          row.className = "asm-row";
          row.innerHTML =
            '<span class="asm-gutter"><span class="asm-bp"></span><span class="asm-pc"></span></span>' +
            '<span class="asm-addr"></span>' +
            '<span class="asm-offset"></span>' +
            '<span class="asm-bytes"></span>' +
            '<span class="asm-text"></span>';
          return row;
        },
        update(el, index) {
          const insn = instructions[index];
          if (!insn) return;
          el.dataset.address = insn.address;
          el.classList.toggle("is-pc", insn.address === pc);
          el.classList.toggle("has-bp", breakpoints.has(insn.address));
          // A new source line starting here gets a rule above it, which is how
          // src+asm mode reads without interleaving whole source lines.
          const prev = instructions[index - 1];
          el.classList.toggle("starts-line", Boolean(insn.line) && insn.line !== prev?.line);

          el.querySelector(".asm-pc").textContent = insn.address === pc ? "▸" : "";
          el.querySelector(".asm-addr").textContent = shortAddress(insn.address);
          el.querySelector(".asm-offset").textContent =
            insn.func ? `<+${insn.offset ?? 0}>` : "";
          el.querySelector(".asm-bytes").textContent = insn.opcodes ?? "";
          el.querySelector(".asm-text").replaceChildren(...tokenize(insn.text ?? ""));
          el.title = insn.line ? `${insn.source?.path ?? ""}:${insn.line}` : insn.address;
        },
      },
    });
  }

  // tokenize splits an AT&T instruction into spans. Deliberately shallow: the
  // mnemonic, registers, immediates, and the rest.
  function tokenize(text) {
    const out = [];
    // The mnemonic is the first word; everything after is operands.
    const match = /^(\s*)(\S+)(.*)$/.exec(text);
    if (!match) {
      out.push(span("asm-plain", text));
      return out;
    }
    const [, lead, mnemonic, rest] = match;
    if (lead) out.push(span("asm-plain", lead));
    out.push(span("asm-mnemonic", mnemonic));

    // Operands split on the characters that separate them, keeping the
    // separators so the text reads unchanged.
    for (const piece of rest.split(/([,()\s]+)/)) {
      if (!piece) continue;
      if (REGISTER.test(piece)) out.push(span("asm-reg", piece));
      else if (IMMEDIATE.test(piece)) out.push(span("asm-imm", piece));
      else if (piece.startsWith("<") || piece.endsWith(">")) out.push(span("asm-sym", piece));
      else out.push(span("asm-plain", piece));
    }
    return out;
  }

  function span(cls, text) {
    const el = document.createElement("span");
    el.className = cls;
    el.textContent = text;
    return el;
  }

  // shortAddress drops the leading zeros gdb pads to 16 digits. The high bits
  // are identical for every instruction on screen and cost a third of the
  // column width.
  function shortAddress(addr) {
    const trimmed = addr.replace(/^0x0*/, "");
    return "0x" + (trimmed || "0");
  }

  element.addEventListener("click", (ev) => {
    const gutter = ev.target.closest(".asm-gutter");
    if (!gutter) return;
    const row = gutter.closest(".asm-row");
    if (row) onGutterClick?.(row.dataset.address);
  });

  // expressionAt answers the hover controller. The tokenizer has already done
  // the parsing: the pointer is over exactly one span, and its class says what
  // kind of thing it is.
  //
  // Immediates are left out on purpose. `$0x10` is a constant printed right
  // there on the line, and a tooltip repeating it back would be the feature
  // getting in the way of reading.
  function expressionAt(ev) {
    const el = ev.target;
    if (!el?.classList) return null;
    let expr = "";
    let storage = "";
    if (el.classList.contains("asm-reg")) {
      expr = registerExpr(el.textContent);
      storage = "register";
    } else if (el.classList.contains("asm-sym")) {
      expr = symbolExpr(el.textContent);
    } else if (el.classList.contains("asm-plain")) {
      // Only x86 decorates its registers. On ARM and MIPS a register is a bare
      // word the tokenizer cannot distinguish from anything else, and this is
      // where they land — so guess, and let a wrong guess come back as `void`.
      expr = bareRegisterExpr(el.textContent);
      storage = "register";
    }
    if (!expr) return null;
    return { expr, storage, rect: el.getBoundingClientRect(), anchor: el };
  }

  function revealPC() {
    if (!list || !pc) return;
    const index = instructions.findIndex((i) => i.address === pc);
    if (index < 0) return;
    if (list.isRowVisible(index)) {
      list.refresh();
      return;
    }
    list.scrollToRow(index);
  }

  return {
    expressionAt,
    set(disasm) {
      instructions = disasm.instructions ?? [];
      pc = disasm.pc ?? "";
      meta = {
        func: disasm.func ?? "",
        hasSource: Boolean(disasm.hasSource),
        truncated: Boolean(disasm.truncated),
      };
      element.replaceChildren();
      list = null;
      ensureList();
      list.setCount(instructions.length);
      revealPC();
    },
    // setPC moves the marker without refetching, for a step that stays inside
    // the window already on screen.
    setPC(next) {
      pc = next ?? "";
      if (!list) return;
      list.forEachRendered((el, index) => {
        el.classList.toggle("is-pc", instructions[index]?.address === pc);
        el.querySelector(".asm-pc").textContent =
          instructions[index]?.address === pc ? "▸" : "";
      });
      revealPC();
    },
    // has reports whether an address is already in the window, which is how
    // the caller decides between moving the marker and refetching.
    has(address) {
      return instructions.some((i) => i.address === address);
    },
    setBreakpoints(list_) {
      breakpoints = new Set(
        (list_ ?? []).map((b) => b.address).filter(Boolean),
      );
      list?.refresh();
    },
    summary() {
      if (instructions.length === 0) return "";
      const parts = [`${instructions.length} instructions`];
      if (meta.func) parts.unshift(meta.func);
      if (meta.truncated) parts.push("truncated");
      if (!meta.hasSource) parts.push("no source");
      return parts.join(" · ");
    },
    clear() {
      instructions = [];
      pc = "";
      element.replaceChildren();
      list = null;
    },
  };
}
