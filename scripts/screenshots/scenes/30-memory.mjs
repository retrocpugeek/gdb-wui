// The memory viewer, and the column that says what you are looking at.

import { breakAtLine, memoryAt, openFile, run } from "../lib/scene.mjs";

export default {
  name: "memory",
  description: "the hex view with its symbol column",
  fixtures: ["globals"],
  exe: "globals",

  async run(page, ctx) {
    await openFile(page, "globals.c");
    await breakAtLine(page, 49);
    await run(page);

    // &counter, not a bare address: the box takes an expression, and this is
    // the start of the run of globals, so several named rows land on one
    // screen with the unnamed padding between them honestly blank.
    await memoryAt(page, "&counter");
    await page.waitUntil(
      () => [...document.querySelectorAll(".mem-sym")].filter((s) => s.textContent).length >= 3,
      { what: "at least three named rows" },
    );

    // Stop a couple of rows past the last named one. Everything after it is
    // zero padding, and thirty rows of it says nothing the first two do not.
    const lastNamed = await page.evaluate(() => {
      const named = [...document.querySelectorAll(".mem-row")]
        .filter((r) => r.querySelector(".mem-sym").textContent);
      const r = named[named.length - 1].getBoundingClientRect();
      return { x: r.x, y: r.y, width: r.width, height: r.height };
    });
    const panel = await page.rect(".panel-source");
    await page.shot(ctx.image(), {
      clip: {
        x: panel.x,
        y: panel.y,
        width: panel.width,
        height: lastNamed.y + lastNamed.height * 3 - panel.y,
      },
    });
  },
};
