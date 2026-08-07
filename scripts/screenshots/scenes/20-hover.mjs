// Hover-to-evaluate, and the two questions the right-click menu keeps apart.

import { breakAtLine, clipFrom, hoverText, openFile, run } from "../lib/scene.mjs";

export default {
  name: "hover",
  description: "the value tooltip, and the memory menu behind it",
  fixtures: ["globals"],
  exe: "globals",

  async run(page, ctx) {
    await openFile(page, "globals.c");
    await breakAtLine(page, 51);
    await run(page);

    // `name` inside `out->last = n->name` — the whole chain is read, not the
    // word, which is the behaviour worth photographing. Anchoring on the
    // assignment rather than the bare identifier avoids landing on the `name`
    // in the struct declaration further up.
    const box = await hoverText(page, ".src-row.is-exec .src-code", "n->name");
    const row = await page.rect(".src-row.is-exec");
    await page.shot(ctx.image(), {
      clip: clipFrom(row, [await page.rect("#hovertip")]),
    });

    // Right-click the same token: a pointer has both an address of its own and
    // an address it holds, and the menu names each rather than leaving the
    // reader to guess from a verb.
    await page.rightClick(page.centre(box));
    await page.shot(ctx.image("menu"), {
      clip: clipFrom(row, [await page.rect("#ctxmenu")]),
    });
  },
};
