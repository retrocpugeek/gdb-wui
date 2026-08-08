// Breaking on a function you can only see in the symbol table.

export default {
  name: "symbol-menu",
  description: "the Symbols pane's right-click menu",
  fixtures: ["globals-nodebug"],
  exe: "globals-nodebug",

  async run(page, ctx) {
    await page.waitFor(".sym-row", { what: "the symbol list" });
    await page.type("#symbols-search", "walk");
    const row = await page.waitUntil(
      () => {
        const rows = [...document.querySelectorAll(".sym-row")];
        return rows.length === 1 && rows[0].textContent.includes("walk");
      },
      { what: "walk to be the only match" },
    );
    if (!row) throw new Error("unreachable");

    await page.rightClick(".sym-row");
    await page.shot(ctx.image(), {
      clip: [".symbols-filter", "#symbols", "#ctxmenu"],
      pad: 8,
    });
  },
};
