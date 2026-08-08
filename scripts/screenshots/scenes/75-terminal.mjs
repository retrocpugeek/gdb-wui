// The program's own terminal, which is a pty rather than a pipe.

export default {
  name: "terminal",
  description: "typing into the debuggee",
  fixtures: ["interactive"],
  exe: "interactive",

  async run(page, ctx) {
    await page.click('.tab[data-bottom="inferior"]');
    await page.waitFor("#inferior:not(.is-hidden)", { what: "the Program tab" });

    await page.click("#btn-run");
    // "name? " has no trailing newline. On a pipe libc would hold it in its
    // buffer and it would never appear; on a tty it is flushed at once, which
    // is the whole reason the debuggee gets its own terminal.
    await page.waitForText("#inferior", "name?", { what: "the unterminated prompt" });

    await page.type("#inferior .xterm-helper-textarea", "world");
    await page.key("Enter");
    await page.waitForText("#inferior", "hello world", { what: "the program's reply" });

    await page.shot(ctx.image(), { clip: ".panel-bottom", pad: 4 });
  },
};
