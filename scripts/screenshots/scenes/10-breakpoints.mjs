// Breakpoints: the marker in the gutter and the pane that mirrors it.

import { breakAtLine, openFile, run } from "../lib/scene.mjs";

export default {
  name: "breakpoints",
  description: "a gutter marker and the Breakpoints pane",
  fixtures: ["globals"],
  exe: "globals",

  async run(page, ctx) {
    await openFile(page, "globals.c");
    await breakAtLine(page, 49);
    await breakAtLine(page, 62);
    await run(page);

    // Both together, because the point is that they are the same two
    // breakpoints seen from two places — the gutter is not a separate list.
    await page.shot(ctx.image(), {
      clip: [".panel-source", "#breakpoints"],
      pad: 4,
    });
  },
};
