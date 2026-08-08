// The gdb console, which is a real one.

import { breakAtLine, openFile, run } from "../lib/scene.mjs";

export default {
  name: "console",
  description: "a gdb command and its answer, tab completion included",
  fixtures: ["globals"],
  exe: "globals",

  async run(page, ctx) {
    await openFile(page, "globals.c");
    await breakAtLine(page, 49);
    await run(page);

    // Typed into the terminal, not sent behind it: this has to be the real
    // path, because "anything you can type at gdb" is the claim being made.
    await page.click("#gdbconsole");
    await page.type("#gdbconsole .xterm-helper-textarea", "info frame");
    await page.key("Enter");
    await page.waitForText("#gdbconsole", "rip = ", { what: "gdb's answer" });

    await page.shot(ctx.image(), { clip: ".panel-bottom", pad: 4 });
  },
};
