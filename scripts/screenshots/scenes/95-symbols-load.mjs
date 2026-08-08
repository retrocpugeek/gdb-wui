// Loading symbols for an image that does not run where it was linked.

export default {
  name: "symbols-load",
  description: "the load-symbols bar, with an offset",
  fixtures: ["globals", "globals-nodebug"],
  exe: "globals-nodebug",

  async run(page, ctx) {
    await page.click("#symbols-load-open");
    await page.waitFor("#symbols-load:not(.is-hidden)", { what: "the load bar" });

    await page.type("#symbols-load-path", "globals");
    // "add" rather than "replace" reveals the offset field: an image relocated
    // by a bootloader, or a firmware blob mapped somewhere other than its link
    // address, needs the difference applied or every symbol is wrong by a
    // constant.
    await page.evaluate(() => {
      const sel = document.querySelector("#symbols-load-mode");
      sel.value = "add";
      sel.dispatchEvent(new Event("change", { bubbles: true }));
    });
    await page.waitFor("#symbols-load-offset:not(.is-hidden)", {
      what: "the offset field to appear",
    });
    await page.type("#symbols-load-offset", "0x8000");

    await page.shot(ctx.image(), { clip: "#symbols-load", pad: 8 });
  },
};
