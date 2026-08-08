// Ghidra's recovered C, with the program counter marked in it.

import { bottom, centre, logClip, menuItem } from "../lib/scene.mjs";

export default {
  name: "decompiled",
  description: "recovered C for a stripped binary, and the decompiler's log",
  fixtures: ["globals-nodebug"],
  exe: "globals-nodebug",
  requires: ["ghidra"],
  flags: (caps) => ["-ghidra", caps.ghidra],

  async run(page, ctx) {
    // No source and no line table, so there is no gutter to click. Breaking by
    // symbol name is the way in, and it is what the Symbols pane is for.
    await page.waitFor(".sym-row", { what: "the symbol list" });
    await page.type("#symbols-search", "walk");
    await page.waitUntil(
      () => document.querySelectorAll(".sym-row").length === 1,
      { what: "walk to be the only match" },
    );
    await page.rightClick(".sym-row");
    await menuItem(page, "Set breakpoint");

    await page.click("#btn-run");
    await page.waitFor('#run-state[data-state="stopped"]', { what: "the program to stop" });

    await centre(page, "decomp");
    // Import and analysis are minutes on real firmware and seconds here, but
    // the pane says "Ghidra is starting" until they finish either way.
    await page.waitFor(".dec-row", { what: "decompiled C", timeout: 300_000 });

    // A breakpoint on a function's name lands in its prologue, which no
    // decompiled line claims — the pane says "pc between lines" and draws an
    // outline rather than a fill, because "the program is here" and "the
    // program is somewhere after here" are different claims.
    //
    // Step until it is the first. This is also the feature being photographed:
    // gdb's own step needs a line table and, without one, its step range is
    // the whole function, so a step over would run to the exit. With this tab
    // showing, the step walks to the next decompiled line instead.
    for (let i = 0; i < 6; i++) {
      const exact = await page.evaluate(() => {
        const row = document.querySelector(".dec-row.is-pc");
        return Boolean(row) && !row.classList.contains("is-pc-approx");
      });
      if (exact) break;
      await page.click("#btn-next");
      await page.waitFor('#run-state[data-state="stopped"]', { what: "the step to finish" });
      await page.sleep(250);
    }
    await page.waitUntil(
      () => {
        const row = document.querySelector(".dec-row.is-pc");
        return Boolean(row) && !row.classList.contains("is-pc-approx");
      },
      { what: "the pc on an exact decompiled line" },
    );

    const last = await page.evaluate(() => {
      const rows = [...document.querySelectorAll(".dec-row")];
      const r = rows[rows.length - 1].getBoundingClientRect();
      return { x: r.x, y: r.y, width: r.width, height: r.height };
    });
    const panel = await page.rect(".panel-source");
    await page.shot(ctx.image(), {
      clip: {
        x: panel.x, y: panel.y, width: panel.width,
        height: last.y + last.height * 2 - panel.y,
      },
    });

    // The decompiler's own activity, one line per operation. Not behind a flag,
    // because without it a slow start looks exactly like a stuck one.
    await bottom(page, "log");
    await page.waitForText("#log", "ghidra", { what: "Ghidra's activity in the log" });
    await page.shot(ctx.image("log"), { clip: await logClip(page) });
  },
};
