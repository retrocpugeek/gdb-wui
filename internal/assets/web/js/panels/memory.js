// The memory viewer.
//
// Rows are *computed*, not stored: row N shows the bytes at base + N*16. That
// one decision is what makes a gigabyte-wide region free — the virtual list
// asks for a row index, arithmetic gives the address, and only the bytes for
// visible rows are ever fetched.
//
// Bytes live in a sparse cache of 4 KiB chunks with an LRU bound. Missing bytes
// render "??" rather than zeros, because a hex viewer that invents zeros for
// unmapped memory is worse than useless: it looks like data.

import { createVirtualList, measureRowHeight } from "../core/virtual.js";

const BYTES_PER_ROW = 16;

// How long to wait after the last scroll before asking what the visible rows
// are called. Symbolising costs one gdb round trip per row, so a drag through
// a megabyte must not ask about every row it passes.
const SYMBOL_DEBOUNCE_MS = 150;
const CHUNK = 4096;
// 512 chunks is 2 MiB of cache, which covers a lot of scrolling and is a
// rounding error next to the page holding it.
const MAX_CHUNKS = 512;

export function createMemory({ element, onRead, onSymbols, onError }) {
  let base = 0n;
  let rows = 0;
  let expression = "";
  let list = null;
  let stopSeq = 0;

  // chunks maps chunk index (address / CHUNK) to a Uint8Array, or null when
  // that chunk is known to be unreadable.
  const chunks = new Map();
  const order = [];
  const inflight = new Set();

  function cssLineHeight() {
    const raw = getComputedStyle(document.documentElement).getPropertyValue("--line-h");
    const n = parseFloat(raw);
    return Number.isFinite(n) && n > 0 ? n : 19;
  }

  function chunkIndexOf(addr) {
    return Number(addr / BigInt(CHUNK));
  }

  function byteAt(addr) {
    const chunk = chunks.get(chunkIndexOf(addr));
    if (chunk === undefined) return undefined; // not fetched yet
    if (chunk === null) return null; // known unreadable
    const offset = Number(addr % BigInt(CHUNK));
    return chunk[offset];
  }

  function remember(index, data) {
    if (!chunks.has(index)) order.push(index);
    chunks.set(index, data);
    while (order.length > MAX_CHUNKS) {
      chunks.delete(order.shift());
    }
  }

  // symbols maps a row address (as a decimal string, since BigInt is not a
  // usable Map key across values) to gdb's name for it. Null records "asked,
  // and there is none", so a stack row is not asked about repeatedly.
  let symbols = new Map();
  let symbolTimer = 0;

  // requestSymbols asks about the rows actually on screen, and only those.
  // The view is virtual over a 4 KiB chunk: symbolising the whole chunk would
  // be 256 round trips for a screenful of forty.
  function requestSymbols() {
    if (!onSymbols || !list) return;
    const wanted = [];
    list.forEachRendered((_, index) => {
      const addr = base + BigInt(index) * BigInt(BYTES_PER_ROW);
      const key = addr.toString();
      if (!symbols.has(key)) {
        symbols.set(key, undefined); // in flight
        wanted.push("0x" + addr.toString(16));
      }
    });
    if (!wanted.length) return;
    const seq = stopSeq;
    onSymbols({ addresses: wanted, stopSeq: seq })
      .then((res) => {
        if (seq !== stopSeq) return;
        // Everything asked for is now answered: a name, or null for none.
        for (const a of wanted) symbols.set(BigInt(a).toString(), null);
        for (const s of res.symbols ?? []) {
          symbols.set(BigInt(s.addr).toString(), s.name);
        }
        list?.refresh();
      })
      .catch(() => {
        // Leave them unasked rather than marked absent, so a transient
        // failure does not permanently blank the column.
        for (const a of wanted) symbols.delete(BigInt(a).toString());
      });
  }

  function scheduleSymbols() {
    clearTimeout(symbolTimer);
    symbolTimer = setTimeout(requestSymbols, SYMBOL_DEBOUNCE_MS);
  }

  // fetchChunk asks for one 4 KiB chunk. Requests are deduplicated, so a scroll
  // that crosses the same boundary twice does not double the traffic.
  async function fetchChunk(index) {
    if (chunks.has(index) || inflight.has(index)) return;
    inflight.add(index);
    const seq = stopSeq;
    try {
      const res = await onRead({
        address: "0x" + (BigInt(index) * BigInt(CHUNK)).toString(16),
        count: CHUNK,
        stopSeq: seq,
      });
      // A stop landed while this was in flight: the bytes describe a program
      // state that no longer exists.
      if (seq !== stopSeq) return;

      if (res.unreadable || !res.ranges?.length) {
        remember(index, null);
      } else {
        const data = new Uint8Array(CHUNK);
        const known = new Uint8Array(CHUNK);
        for (const range of res.ranges) {
          const start = BigInt(range.addr);
          const bytes = hexToBytes(range.dataHex);
          for (let i = 0; i < bytes.length; i++) {
            const offset = Number(start + BigInt(i) - BigInt(index) * BigInt(CHUNK));
            if (offset >= 0 && offset < CHUNK) {
              data[offset] = bytes[i];
              known[offset] = 1;
            }
          }
        }
        // Bytes gdb did not return are holes inside an otherwise readable
        // chunk; 0xff in `known` distinguishes them from a real zero byte.
        data.known = known;
        remember(index, data);
      }
      list?.refresh();
    } catch (err) {
      if (err?.code === "busy") return;
      remember(index, null);
      list?.refresh();
      if (err?.code !== "not_ready") onError?.(err);
    } finally {
      inflight.delete(index);
    }
  }

  function knownAt(addr) {
    const chunk = chunks.get(chunkIndexOf(addr));
    if (!chunk) return chunk === null ? false : undefined;
    if (!chunk.known) return true;
    return chunk.known[Number(addr % BigInt(CHUNK))] === 1;
  }

  element.addEventListener("scroll", scheduleSymbols, { passive: true });

  function ensureList() {
    if (list) return;
    list = createVirtualList({
      container: element,
      rowHeight: measureRowHeight(element, cssLineHeight()),
      renderRow: {
        create() {
          const row = document.createElement("div");
          row.className = "mem-row";
          row.innerHTML =
            '<span class="mem-addr"></span><span class="mem-hex"></span>' +
            '<span class="mem-ascii"></span><span class="mem-sym"></span>';
          return row;
        },
        update(el, index) {
          const rowAddr = base + BigInt(index) * BigInt(BYTES_PER_ROW);
          el.querySelector(".mem-addr").textContent =
            rowAddr.toString(16).padStart(12, "0");

          let hex = "";
          let ascii = "";
          let missing = false;
          for (let i = 0; i < BYTES_PER_ROW; i++) {
            const addr = rowAddr + BigInt(i);
            const value = byteAt(addr);
            const isKnown = value !== undefined && value !== null && knownAt(addr);
            if (!isKnown) {
              missing = true;
              hex += "?? ";
              ascii += value === null ? "." : " ";
            } else {
              hex += value.toString(16).padStart(2, "0") + " ";
              ascii += value >= 0x20 && value < 0x7f ? String.fromCharCode(value) : ".";
            }
            if (i === 7) hex += " ";
          }
          el.querySelector(".mem-hex").textContent = hex;
          el.querySelector(".mem-ascii").textContent = ascii;
          el.classList.toggle("has-holes", missing);

          const sym = symbols.get(rowAddr.toString());
          el.querySelector(".mem-sym").textContent = sym ? sym : "";

          // Fetch what this row needs. One request per chunk, deduplicated, so
          // a render pass over twenty rows in the same chunk asks once.
          for (const chunkIdx of new Set([
            chunkIndexOf(rowAddr),
            chunkIndexOf(rowAddr + BigInt(BYTES_PER_ROW - 1)),
          ])) {
            fetchChunk(chunkIdx);
          }
        },
      },
    });
  }

  return {
    // show points the viewer at an address. rowCount is how much of the region
    // to make scrollable; the bytes behind it are fetched only as they are
    // scrolled into view, so a large number costs nothing.
    show(addr, { expr = "", rowCount = 4096, seq = stopSeq } = {}) {
      base = BigInt(addr);
      expression = expr;
      rows = rowCount;
      stopSeq = seq;
      element.replaceChildren();
      list = null;
      ensureList();
      list.setCount(rows);
      list.scrollToRow(0, { center: false });
      scheduleSymbols();
    },
    // onStop drops the cache: the bytes described the previous stop, and memory
    // is exactly the thing that changes while a program runs.
    onStop(seq) {
      stopSeq = seq;
      chunks.clear();
      order.length = 0;
      // Names survive a stop only if the program did not move: a re-run
      // relocates everything, and a stale name on a live address is worse
      // than none.
      symbols = new Map();
      list?.refresh();
      scheduleSymbols();
    },
    summary() {
      if (!list) return "";
      const parts = ["0x" + base.toString(16)];
      if (expression && expression !== "0x" + base.toString(16)) {
        parts.unshift(expression + " →");
      }
      return parts.join(" ");
    },
    clear() {
      chunks.clear();
      order.length = 0;
      element.replaceChildren();
      list = null;
    },
  };
}

function hexToBytes(hex) {
  const out = new Uint8Array(Math.floor(hex.length / 2));
  for (let i = 0; i < out.length; i++) {
    out[i] = parseInt(hex.substr(i * 2, 2), 16);
  }
  return out;
}
