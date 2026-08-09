// Locals, expanded, with a watch expression alongside them.

import { breakAtLine, openFile, run } from "../lib/scene.mjs";

export default {
  name: "variables",
  description: "an expanded struct and a watch expression",
  fixtures: ["globals"],
  exe: "globals",

  async run(page, ctx) {
    await openFile(page, "globals.c");
    // Line 65, after walk() has filled s in, so expanding it shows values
    // rather than an uninitialised struct.
    await breakAtLine(page, 65);
    await run(page);

    await page.click('.var-row[data-path="local:s"] .var-twisty');
    await page.waitFor('.var-row[data-path="local:s"].is-expanded', {
      what: "s to expand",
    });

    // A watch really is typed into the in-place editor drawn over the panel
    // head, so this drives the application rather than the store behind it.
    await page.click("#btn-add-watch");
    await page.waitFor(".cell-edit", { what: "an edit box for the expression" });
    await page.fill(".cell-edit", "hidden_total");
    await page.key("Enter");
    await page.waitUntil(() => !document.querySelector(".cell-edit"),
      { what: "the edit box to close once the watch was added" });
    // "Watches" in the DOM; the uppercase in the screenshot is CSS.
    await page.waitForText("#variables", "Watches", { what: "the watches section" });

    await page.shot(ctx.image(), { clip: "#variables", pad: 4 });
  },
};
