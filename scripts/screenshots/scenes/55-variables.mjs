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

    // A watch really does go through window.prompt, so this drives the
    // application rather than the store behind it.
    page.answerNextPrompt("hidden_total");
    await page.click("#btn-add-watch");
    // "Watches" in the DOM; the uppercase in the screenshot is CSS.
    await page.waitForText("#variables", "Watches", { what: "the watches section" });

    await page.shot(ctx.image(), { clip: "#variables", pad: 4 });
  },
};
