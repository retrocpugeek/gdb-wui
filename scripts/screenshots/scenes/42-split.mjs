// Two views at once: the instructions and the C recovered from them.

import { breakAtLine, centre, openFile, run } from "../lib/scene.mjs";

export default {
  name: "split",
  description: "disassembly beside decompiled C",
  fixtures: ["globals-nodebug"],
  exe: "globals-nodebug",
  requires: ["ghidra"],
  flags: (caps) => ["-ghidra", caps.ghidra],

  async run(page, ctx) {
    // No source, so break by symbol — the case the split is most useful for.
    await page.waitFor(".sym-row", { what: "the symbol list" });
    await page.type("#symbols-search", "walk");
    await page.waitUntil(
      () => document.querySelectorAll(".sym-row").length === 1,
      { what: "walk to be the only match" },
    );
    await page.rightClick(".sym-row");
    await page.click("#ctxmenu .ctxmenu-item");
    await page.click("#btn-run");
    await page.waitFor('#run-state[data-state="stopped"]', { what: "the program to stop" });

    // Disassembly in the focused slot, then split: the complement of the
    // disassembly is the decompiled view, so slot B fills itself in.
    await centre(page, "disasm");
    await page.waitFor(".asm-row", { what: "disassembly" });
    await page.click("#btn-split");

    await page.waitUntil(
      () => document.querySelector("#center")?.dataset.split === "x",
      { what: "the centre to split" },
    );
    // Both must actually be rendering. A split that merely looks right — one
    // slot sized to nothing, or a view placed but never fetched — would pass
    // a screenshot and fail a reader.
    await page.waitUntil(
      () => {
        const asm = document.querySelector("#disasm");
        const dec = document.querySelector("#decomp");
        return asm?.dataset.slot && dec?.dataset.slot
          && asm.clientWidth > 80 && dec.clientWidth > 80
          && document.querySelectorAll(".asm-row").length > 0
          && document.querySelectorAll(".dec-row").length > 0;
      },
      { what: "both views rendered with a usable width", timeout: 300_000 },
    );
    // And the focused slot must be marked, since it is what the keys act on.
    await page.waitFor('[data-slot-head="a"].is-focused', {
      what: "the slot that was focused to be marked",
    });

    // Clicking in the other slot moves the focus, which is what decides how
    // F10 steps. A screenshot cannot show this, so assert it here.
    await page.click("#decomp");
    await page.waitFor('[data-slot-head="b"].is-focused', {
      what: "clicking the second slot to focus it",
    });
    await page.waitUntil(
      () => document.querySelector('.tab[data-center="decomp"]')?.classList.contains("is-active"),
      { what: "the tabs to follow the focus" },
    );

    // Stacking and going back, so both grid shapes are exercised rather than
    // only the one being photographed.
    await page.click("#btn-split-orient");
    await page.waitUntil(
      () => {
        const c = document.querySelector("#center");
        return c?.dataset.split === "y"
          && !document.querySelector("#split-center-y").classList.contains("is-hidden")
          && document.querySelector("#disasm").clientHeight > 40
          && document.querySelector("#decomp").clientHeight > 40;
      },
      { what: "the views to stack" },
    );
    await page.click("#btn-split-orient");
    await page.waitUntil(
      () => document.querySelector("#center")?.dataset.split === "x",
      { what: "the views to go back side by side" },
    );

    // Photograph it with the first slot focused, which is where a reader
    // starts.
    await page.click("#disasm");
    await page.waitFor('[data-slot-head="a"].is-focused', { what: "focus back on the first slot" });

    await page.shot(ctx.image(), { clip: ".panel-source", pad: 4 });
  },
};
