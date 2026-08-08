// The log, and the raw MI stream behind it.

import { bottom, breakAtLine, logClip, openFile, run } from "../lib/scene.mjs";

export default {
  name: "log",
  description: "the raw GDB/MI traffic under -mi-log",
  fixtures: ["globals"],
  exe: "globals",
  flags: ["-mi-log"],

  async run(page, ctx) {
    await openFile(page, "globals.c");
    await breakAtLine(page, 49);
    await run(page);

    await bottom(page, "log");
    // -mi-log is a flag, unlike the decompiler's activity, because this is
    // every line of a conversation rather than one line per operation.
    // By class, not by text: the pane keeps a bounded number of lines and
    // renders only what is on screen, so searching its text for a particular
    // record is a race. Both directions must be there — a log with only the
    // commands we sent would be half the point.
    await page.waitFor(".log-line.log-mi-out", { what: "commands sent to gdb" });
    await page.waitFor(".log-line.log-mi-in", { what: "gdb's replies" });
    await page.shot(ctx.image(), { clip: await logClip(page) });
  },
};
