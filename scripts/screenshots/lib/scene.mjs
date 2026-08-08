// The actions scenes share.
//
// Each one ends by waiting for the state it was supposed to produce, so a
// scene that has gone wrong fails at the step that went wrong rather than at
// the screenshot — or, worse, not at all.

/** openFile clicks a file in the tree and waits for its lines to render. */
export async function openFile(page, path) {
  await page.click(`.tree-row[data-path="${path}"]`);
  await page.waitFor(".src-row", { what: `${path} to render` });
}

/**
 * revealLine scrolls a source line into view.
 *
 * The source view is virtualised — only the rows on screen exist in the DOM —
 * so a line further down cannot be clicked until it has been scrolled to.
 */
export async function revealLine(page, line) {
  await page.evaluate((n) => {
    const body = document.querySelector("#source");
    const row = document.querySelector(".src-row");
    const height = row ? row.getBoundingClientRect().height : 19;
    // A third of the way down, so the line has context above and below it.
    body.scrollTop = Math.max(0, (n - 1) * height - body.clientHeight / 3);
  }, line);
  await page.waitFor(`.src-row[data-line="${line}"]`, { what: `line ${line} on screen` });
}

/** breakAtLine sets a breakpoint by clicking the gutter, as a reader would. */
export async function breakAtLine(page, line) {
  await revealLine(page, line);
  await page.click(`.src-row[data-line="${line}"] .src-gutter`);
  await page.waitFor(`.src-row[data-line="${line}"].has-bp`, {
    what: `a breakpoint marker on line ${line}`,
  });
}

/** run starts the program and waits for it to stop somewhere. */
export async function run(page) {
  await page.click("#btn-run");
  await stopped(page);
}

/** stopped waits for the run state to settle on a stop with a frame on screen. */
export async function stopped(page) {
  await page.waitFor('#run-state[data-state="stopped"]', { what: "the program to stop" });
  await page.waitFor("#stack .list-row", { what: "a call stack" });
}

/** centre switches the main tab: source, disasm, decomp or memory. */
export async function centre(page, tab) {
  await page.click(`.tab[data-center="${tab}"]`);
  await page.waitFor(`#${tab}:not(.is-hidden)`, { what: `the ${tab} tab` });
}

/** right switches the right-hand tab: variables or registers. */
export async function right(page, tab) {
  await page.click(`.tab[data-tab="${tab}"]`);
  await page.waitFor(`#${tab}:not(.is-hidden)`, { what: `the ${tab} tab` });
}

/** bottom switches the console tab: gdb, inferior or log. */
export async function bottom(page, tab) {
  await page.click(`.tab[data-bottom="${tab}"]`);
  await page.waitFor(`#${panelFor(tab)}:not(.is-hidden)`, { what: `the ${tab} tab` });
}

function panelFor(tab) {
  return { gdb: "gdbconsole", inferior: "inferior", log: "log" }[tab] ?? tab;
}

/**
 * memoryAt types an address into the memory view's box and reads it.
 *
 * The box takes an expression, not just a number, which is the point: `&head`
 * is the thing a reader has in their head, and gdb resolves it.
 */
export async function memoryAt(page, expression) {
  await centre(page, "memory");
  await page.type("#mem-addr", expression);
  await page.key("Enter");
  await page.waitFor(".mem-row", { what: `memory at ${expression}` });
}

/**
 * menuItem clicks an open context menu's entry by its label.
 *
 * By label rather than position, so a scene keeps working when an entry is
 * added above it — and fails loudly, naming what it wanted, when one is
 * renamed.
 */
export async function menuItem(page, label) {
  const found = await page.evaluate((want) => {
    const items = [...document.querySelectorAll(".ctxmenu-item")];
    const at = items.findIndex((b) => b.textContent.includes(want));
    if (at < 0) return { labels: items.map((b) => b.textContent) };
    const r = items[at].getBoundingClientRect();
    return { box: { x: r.x, y: r.y, width: r.width, height: r.height } };
  }, label);
  if (!found.box) {
    throw new Error(`no menu entry matching ${JSON.stringify(label)}; ` +
      `the menu offers ${JSON.stringify(found.labels)}`);
  }
  await page.clickAt(page.centre(found.box));
  await page.waitUntil(
    () => document.querySelector("#ctxmenu").classList.contains("is-hidden"),
    { what: "the menu to close" },
  );
}

/** hoverText rests the pointer on a token inside an element. */
export async function hoverText(page, selector, needle, opts) {
  const box = await page.textRect(selector, needle);
  await page.hover(page.centre(box), opts);
  return box;
}

/**
 * clipFrom builds a capture box anchored on one element's left edge and
 * reaching just past the others.
 *
 * The union of two boxes is usually the wrong frame. A tooltip beside a line
 * of source unioned with that source line gives the full panel width with the
 * subject in one corner; unioned with the token it points at, it gives a
 * fragment with no context. What reads well is "from the left margin to a
 * little past the tooltip".
 */
export function clipFrom(from, to, pad = 20) {
  const all = [from, ...to];
  const top = Math.min(...all.map((b) => b.y)) - pad;
  const bottom = Math.max(...all.map((b) => b.y + b.height)) + pad;
  const right = Math.max(...to.map((b) => b.x + b.width)) + pad;
  return { x: from.x, y: top, width: right - from.x, height: bottom - top };
}

/**
 * logClip is the console panel cropped to whole log lines.
 *
 * The log fills its panel and the last line is usually cut in half by the
 * bottom edge, which in a screenshot reads as a rendering fault rather than as
 * a scroll.
 */
export async function logClip(page) {
  const panel = await page.rect(".panel-bottom");
  const bottom = await page.evaluate((limit) => {
    let last = null;
    for (const row of document.querySelectorAll(".log-line")) {
      const box = row.getBoundingClientRect();
      if (box.bottom <= limit) last = box.bottom;
    }
    return last;
  }, panel.y + panel.height);
  return bottom ? { ...panel, height: bottom - panel.y } : panel;
}

/** rowsBelow is the box of an element plus n further rows of its own height. */
export function rowsBelow(box, n) {
  return { ...box, height: box.height * (n + 1) };
}
