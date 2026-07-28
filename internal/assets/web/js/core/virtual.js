// A virtualized fixed-height list.
//
// Why hand-written rather than one-node-per-line: a 20k-line file is 20k DOM
// nodes, which is slow to build and slower to keep. `content-visibility` does
// not help — the browser still creates every node, and the scrollbar drifts as
// it guesses at unrendered heights.
//
// The shape is: a tall empty sizer establishes the scroll range, and a pool of
// row nodes just big enough to cover the viewport is moved with a single
// transform. Scrolling recycles rows rather than creating them, so the cost is
// proportional to what is visible, not to the file.
//
// The disassembly and hex viewers in later milestones reuse this.

export function createVirtualList({ container, rowHeight, renderRow, overscan = 6 }) {
  let count = 0;
  let firstRendered = -1;
  let pool = [];
  let scheduled = false;

  // The scroller owns the scrollbar; the sizer gives it something to scroll.
  const sizer = document.createElement("div");
  sizer.className = "vlist-sizer";
  const viewport = document.createElement("div");
  viewport.className = "vlist-viewport";
  sizer.append(viewport);
  container.append(sizer);

  function visibleRange() {
    const scrollTop = container.scrollTop;
    const height = container.clientHeight || rowHeight * 20;
    const first = Math.max(0, Math.floor(scrollTop / rowHeight) - overscan);
    const visible = Math.ceil(height / rowHeight) + overscan * 2;
    return { first, last: Math.min(count, first + visible) };
  }

  function ensurePool(size) {
    while (pool.length < size) {
      const row = renderRow.create();
      pool.push(row);
      viewport.append(row);
    }
    while (pool.length > size) {
      pool.pop().remove();
    }
  }

  function render() {
    scheduled = false;
    const { first, last } = visibleRange();
    ensurePool(last - first);

    // One transform for the whole pool instead of positioning each row: the
    // rows keep their relative offsets and the browser does one composite.
    viewport.style.transform = `translateY(${first * rowHeight}px)`;

    for (let i = first; i < last; i++) {
      renderRow.update(pool[i - first], i);
    }
    firstRendered = first;
  }

  function schedule() {
    if (scheduled) return;
    scheduled = true;
    requestAnimationFrame(render);
  }

  container.addEventListener("scroll", schedule, { passive: true });

  const resizeObserver = new ResizeObserver(schedule);
  resizeObserver.observe(container);

  return {
    setCount(n) {
      count = n;
      sizer.style.height = `${n * rowHeight}px`;
      // A full rebuild: every pooled row now shows a different index.
      firstRendered = -1;
      render();
    },
    // refresh re-renders what is on screen without touching the scroll
    // position. Decoration changes go through here.
    refresh: schedule,
    // renderNow bypasses the frame budget, for tests and for a fresh file where
    // an empty frame would flash.
    renderNow: render,
    scrollToRow(index, { center = true } = {}) {
      const height = container.clientHeight || rowHeight * 20;
      const top = center
        ? index * rowHeight - height / 2 + rowHeight / 2
        : index * rowHeight;
      container.scrollTop = Math.max(0, top);
      render();
    },
    // isRowVisible answers the jitter guard's question: is this row already
    // comfortably on screen?
    isRowVisible(index, margin = 0.2) {
      const height = container.clientHeight || rowHeight * 20;
      const top = container.scrollTop + height * margin;
      const bottom = container.scrollTop + height * (1 - margin);
      const rowTop = index * rowHeight;
      return rowTop >= top && rowTop + rowHeight <= bottom;
    },
    rowAt(index) {
      if (index < firstRendered || index >= firstRendered + pool.length) return null;
      return pool[index - firstRendered];
    },
    forEachRendered(fn) {
      for (let i = 0; i < pool.length; i++) {
        const index = firstRendered + i;
        if (index >= 0 && index < count) fn(pool[i], index);
      }
    },
    destroy() {
      resizeObserver.disconnect();
      sizer.remove();
      pool = [];
    },
  };
}

// measureRowHeight verifies the CSS --line-h against reality.
//
// The virtual list's arithmetic is only correct if a row really is that tall.
// A font that fails to load, a browser minimum font size, or a stray zoom level
// silently breaks the mapping between scroll offset and line number — the
// symptom is a gutter that drifts out of alignment as you scroll, which is
// baffling unless you know to look for it.
export function measureRowHeight(container, declared) {
  const probe = document.createElement("div");
  probe.className = "src-row";
  probe.style.position = "absolute";
  probe.style.visibility = "hidden";
  probe.innerHTML = `<span class="src-gutter">8888</span><span class="src-code">probe</span>`;
  container.append(probe);
  const measured = probe.getBoundingClientRect().height;
  probe.remove();

  if (!measured) return declared;
  if (Math.abs(measured - declared) > 0.5) {
    console.warn(
      `gdb-wui: --line-h is ${declared}px but a row measures ${measured.toFixed(2)}px; ` +
        `using the measured value. Check for a missing monospace font or page zoom.`,
    );
    return measured;
  }
  return declared;
}
