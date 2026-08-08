// Two markers, because "where it stopped" and "what I am looking at" are two
// different places.

import { breakAtLine, openFile, run } from "../lib/scene.mjs";

export default {
  name: "stack",
  description: "an outer frame selected, with the pc marker left alone",
  fixtures: ["globals"],
  exe: "globals",

  async run(page, ctx) {
    await openFile(page, "globals.c");
    await breakAtLine(page, 49);
    await run(page);

    // Frame #1 is main. Selecting it moves a blue bar to the call site; the
    // green one stays on line 49, where the program actually is.
    await page.click("#stack .list-row:nth-child(2)");
    await page.waitFor(".src-row.is-frame", { what: "the selected-frame marker" });
    await page.waitFor(".src-row.is-exec", { what: "the pc marker to have survived" });

    await page.shot(ctx.image(), { clip: [".panel-source", "#stack"], pad: 4 });
  },
};
