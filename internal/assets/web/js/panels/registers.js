// The register file.
//
// Pulled lazily: registers are not in the stop event, because most stops are a
// step through source and nobody is looking at rsi. The panel asks when it is
// visible and a stop has happened, and passes stopSeq so a late answer for a
// superseded stop is dropped rather than displayed.
//
// Change highlighting comes from gdb's own -data-list-changed-registers, not
// from diffing here — gdb already tracks it, and its answer accounts for
// register aliasing that a naive value comparison would miss.

import { createVirtualList, measureRowHeight } from "../core/virtual.js";

export function createRegisters({ element, onFetch, onError }) {
  let registers = [];
  let list = null;
  let visible = false;
  let stopSeq = 0;
  let fetchedSeq = -1;
  let format = "x";

  function ensureList() {
    if (list) return;
    const rowHeight = measureRowHeight(element, cssLineHeight());
    list = createVirtualList({
      container: element,
      rowHeight,
      renderRow: {
        create() {
          const row = document.createElement("div");
          row.className = "reg-row";
          row.innerHTML =
            '<span class="reg-num"></span>' +
            '<span class="reg-name"></span>' +
            '<span class="reg-value"></span>';
          return row;
        },
        update(el, index) {
          const reg = registers[index];
          if (!reg) return;
          el.classList.toggle("is-changed", Boolean(reg.changed));
          el.querySelector(".reg-num").textContent = String(reg.number);
          // An empty name is not a bug: gdb's list has gaps at stable indices,
          // and the number is the real identity.
          el.querySelector(".reg-name").textContent = reg.name || `r${reg.number}`;
          el.querySelector(".reg-value").textContent = reg.value;
        },
      },
    });
  }

  function cssLineHeight() {
    const raw = getComputedStyle(document.documentElement).getPropertyValue("--line-h");
    const n = parseFloat(raw);
    return Number.isFinite(n) && n > 0 ? n : 19;
  }

  function render() {
    ensureList();
    list.setCount(registers.length);
  }

  async function refresh() {
    if (!visible || stopSeq === 0 || fetchedSeq === stopSeq) return;
    const seq = stopSeq;
    try {
      const res = await onFetch({ format, stopSeq: seq });
      if (seq !== stopSeq) return; // a newer stop landed; this answer is stale
      registers = res.registers ?? [];
      fetchedSeq = seq;
      render();
    } catch (err) {
      if (err?.code !== "busy") onError?.(err);
    }
  }

  return {
    // onShow is why the panel costs nothing when hidden: no round-trip happens
    // until it is on screen.
    onShow() {
      visible = true;
      refresh();
    },
    onHide() {
      visible = false;
    },
    onStop(seq) {
      stopSeq = seq;
      refresh();
    },
    setFormat(next) {
      format = next;
      fetchedSeq = -1;
      refresh();
    },
    clear() {
      registers = [];
      fetchedSeq = -1;
      render();
    },
  };
}
