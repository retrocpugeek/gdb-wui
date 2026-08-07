// The hero image: everything at once, which is the whole argument for the tool.

import { breakAtLine, openFile, run } from "../lib/scene.mjs";

export default {
  name: "overview",
  description: "the whole window, stopped at a breakpoint",
  fixtures: ["globals", "globals-nodebug", "hello", "threads"],
  exe: "globals",

  async run(page, ctx) {
    await openFile(page, "globals.c");
    // Inside walk(), so the call stack has two frames of the program's own and
    // the Variables pane has a parameter worth expanding.
    await breakAtLine(page, 49);
    await run(page);
    await page.shot(ctx.image());
  },
};
