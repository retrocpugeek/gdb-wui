// Running to a line without leaving a breakpoint behind.

import { centre, clipFrom, menuItem, openFile, revealLine, stopped } from "../lib/scene.mjs";

export default {
  name: "run-to",
  description: "the run-to entry in the source view's menu, and where it lands",
  fixtures: ["globals"],
  exe: "globals",

  async run(page, ctx) {
    await openFile(page, "globals.c");

    // No breakpoint anywhere, and the program has not started: run-to is the
    // whole journey, which is what makes it worth having next to the gutter.
    await revealLine(page, 51);
    const row = await page.rect('.src-row[data-line="51"]');
    await page.rightClick(page.centre(row));
    await page.waitFor("#ctxmenu:not(.is-hidden)", { what: "the line's menu" });
    await page.shot(ctx.image("menu"), {
      clip: clipFrom(row, [await page.rect("#ctxmenu")]),
    });

    await menuItem(page, "Run to line 51");
    await stopped(page);

    await page.waitFor('.src-row[data-line="51"].is-exec', {
      what: "the program counter on the line that was asked for",
    });

    // The breakpoint it used was temporary, so there is nothing in the gutter
    // and nothing in the pane afterwards — the assertion is the point of the
    // picture as much as the marker is.
    const left = await page.evaluate(
      () => document.querySelectorAll("#breakpoints .list-row").length,
    );
    if (left) throw new Error(`${left} breakpoints left after a run-to`);

    // Cropped to the top of the Breakpoints pane: its emptiness is the
    // evidence, and the rest of the pane is empty for the ordinary reason.
    const pane = await page.rect("#breakpoints");
    await page.shot(ctx.image(), {
      clip: clipFrom(await page.rect(".src-row.is-exec"),
        [{ ...pane, height: 110 }]),
    });

    // The same offer in the disassembly, where the place is an address rather
    // than a line. Not photographed — one picture of a two-item menu is
    // enough — but asserted, because the three views wire this up separately.
    await centre(page, "disasm");
    await page.waitFor(".asm-row", { what: "instructions" });
    const insn = await page.rect(".asm-row.is-pc");
    await page.rightClick(page.centre(insn));
    await page.waitFor("#ctxmenu:not(.is-hidden)", { what: "the instruction's menu" });
    const labels = await page.evaluate(
      () => [...document.querySelectorAll(".ctxmenu-item")].map((b) => b.textContent),
    );
    if (!labels.some((l) => /^Run to 0x[0-9a-f]+$/.test(l))) {
      throw new Error(`the disassembly menu offers ${JSON.stringify(labels)}, `
        + "with nothing to run to");
    }
    await page.key("Escape");
  },
};
