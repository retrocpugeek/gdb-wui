// The register file at a stop.

import { breakAtLine, openFile, right, run } from "../lib/scene.mjs";

export default {
  name: "registers",
  description: "the register list",
  fixtures: ["globals"],
  exe: "globals",

  async run(page, ctx) {
    await openFile(page, "globals.c");
    await breakAtLine(page, 49);
    await run(page);

    await right(page, "registers");
    await page.waitFor(".reg-row", { what: "registers" });
    await page.shot(ctx.image(), { clip: "#registers", pad: 4 });
  },
};
