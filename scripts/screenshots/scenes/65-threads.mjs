// Several threads, each with its own stack.

import { breakAtLine, openFile, run } from "../lib/scene.mjs";

export default {
  name: "threads",
  description: "the thread list with workers stopped in the same function",
  fixtures: ["threads"],
  exe: "threads",

  async run(page, ctx) {
    await openFile(page, "threads.c");
    // Inside the worker loop, so the main thread and the workers all exist and
    // the list has something to say about each.
    await breakAtLine(page, 30);
    await run(page);

    await page.waitUntil(
      () => document.querySelectorAll("#threads .list-row").length >= 2,
      { what: "more than one thread" },
    );
    await page.shot(ctx.image(), { clip: ["#stack", "#threads"], pad: 4 });
  },
};
