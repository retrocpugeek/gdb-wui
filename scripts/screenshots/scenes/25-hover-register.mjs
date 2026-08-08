// Hovering in the machine view, where the things worth reading are registers.

import { breakAtLine, centre, clipFrom, openFile, run } from "../lib/scene.mjs";

export default {
  name: "hover-register",
  description: "a register's value, and the same number in the other base",
  fixtures: ["globals"],
  exe: "globals",

  async run(page, ctx) {
    await openFile(page, "globals.c");
    await breakAtLine(page, 49);
    await run(page);
    await centre(page, "disasm");
    await page.waitFor(".asm-row", { what: "disassembly" });

    // The tokeniser marks registers, so there is no need to guess where one
    // is on the line. A register holding an address is the interesting case:
    // 140737488347136 is a number and 0x7fffffffe000 is somewhere you
    // recognise, so both are shown.
    const reg = await page.evaluate(() => {
      const el = document.querySelector(".asm-row .asm-reg");
      if (!el) return null;
      const r = el.getBoundingClientRect();
      return { x: r.x, y: r.y, width: r.width, height: r.height };
    });
    if (!reg) throw new Error("no register token in the disassembly");

    await page.hover(page.centre(reg));
    const row = await page.rect(".asm-row");
    await page.shot(ctx.image(), {
      clip: clipFrom(row, [await page.rect("#hovertip")]),
    });
  },
};
