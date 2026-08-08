// The three things you can do with an ELF, and why they are three.

export default {
  name: "files",
  description: "the file tree's ELF menu",
  fixtures: ["globals", "globals-nodebug", "hello"],

  async run(page, ctx) {
    await page.waitFor('.tree-row[data-path="globals"]', { what: "the file tree" });
    await page.rightClick('.tree-row[data-path="globals"]');
    await page.shot(ctx.image(), { clip: [".panel-tree", "#ctxmenu"], pad: 8 });
  },
};
