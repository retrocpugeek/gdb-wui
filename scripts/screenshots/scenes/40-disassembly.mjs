// The machine view, following the program counter.

import { breakAtLine, centre, openFile, run } from "../lib/scene.mjs";

export default {
  name: "disassembly",
  description: "disassembly with the program counter marked",
  fixtures: ["globals"],
  exe: "globals",

  async run(page, ctx) {
    await openFile(page, "globals.c");
    await breakAtLine(page, 49);
    await run(page);

    await centre(page, "disasm");
    await page.waitFor(".asm-row", { what: "disassembly" });
    await page.shot(ctx.image(), { clip: ".panel-source", pad: 4 });
  },
};
