// The Symbols pane: the map you still have when the debug info is gone.

export default {
  name: "symbols",
  description: "the symbol list, filtered, on a binary with no debug info",
  fixtures: ["globals-nodebug"],
  exe: "globals-nodebug",

  async run(page, ctx) {
    await page.waitFor(".sym-row", { what: "the symbol list" });

    // A stripped-of-DWARF binary, so every row here comes from the ELF symbol
    // table alone. The filter narrows to the program's own symbols, past the
    // dozen the toolchain contributes.
    await page.type("#symbols-search", "a");
    await page.waitUntil(
      () => {
        const count = document.querySelector("#symbols-count")?.textContent ?? "";
        const [shown, , total] = count.split(/\s+/);
        return shown && total && shown !== total;
      },
      { what: "the filter to narrow the list" },
    );
    // The Symbols section alone. Clipping the whole left panel would give an
    // empty file tree above it, and the file tree is a different page's
    // subject.
    await page.shot(ctx.image(), {
      clip: ["#symbols-count", ".symbols-filter", "#symbols"],
      pad: 8,
    });
  },
};
