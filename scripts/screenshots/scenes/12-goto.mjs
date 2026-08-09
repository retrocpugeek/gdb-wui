// The go-to box: type a place, and the view you are looking at goes there.
//
// The picture is the source view after a jump, but the scene drives all four
// views from the same box, because the point of the feature is that one target
// means the right thing in each of them — and a screenshot of a text box says
// nothing about that.

import { breakAtLine, centre, goTo, openFile, run } from "../lib/scene.mjs";

export default {
  name: "goto",
  description: "jumping to a symbol, an address or a line",
  fixtures: ["globals"],
  exe: "globals",

  async run(page, ctx) {
    await openFile(page, "globals.c");
    await breakAtLine(page, 64);
    await run(page);

    // --- a symbol, in the source view ------------------------------------
    await centre(page, "source");
    await goTo(page, "walk");
    await page.waitUntil(
      () => document.querySelector("#source .src-row.is-frame, #source .src-row")
        && document.querySelectorAll('#source .src-row[data-line="42"]').length === 1,
      { what: "walk's line to be on screen" },
    );

    // --- a bare line, against the file already open -----------------------
    await goTo(page, ":17");
    await page.waitFor('#source .src-row[data-line="17"]', { what: "line 17" });

    // --- a file and a line ------------------------------------------------
    await goTo(page, "globals.c:56");
    await page.waitFor('#source .src-row[data-line="56"]', { what: "line 56" });

    // --- the same symbol, in the disassembly ------------------------------
    // The same text, a different destination. This is the assertion the whole
    // feature rests on: the box acts on the focused view, not on one of them.
    await centre(page, "disasm");
    await goTo(page, "walk");
    await page.waitUntil(
      () => {
        const rows = [...document.querySelectorAll("#disasm .asm-row")];
        return rows.length > 0 && rows.some((r) => r.textContent.includes("walk"));
      },
      { what: "walk's instructions" },
    );

    // --- and in the memory view -------------------------------------------
    await centre(page, "memory");
    await goTo(page, "&counter");
    await page.waitFor(".mem-row", { what: "the bytes at counter" });
    await page.waitUntil(
      () => document.querySelector(".mem-sym")?.textContent.includes("counter"),
      { what: "the symbol column to name counter" },
    );

    // --- something that is not there --------------------------------------
    // A refusal has to be visible. Silence here reads as a box that ate the
    // keystroke.
    await goTo(page, "no_such_symbol_anywhere");
    await page.waitUntil(
      () => document.querySelector("#status-message")?.textContent.length > 0
        && document.querySelector("#status-message")?.dataset.state === "closed",
      { what: "the status bar to report the failure" },
    );

    // --- split: the focused slot moves and the other does not -------------
    // This is the assertion the request rests on. With two views up, one box
    // has to act on exactly one of them, and a scene is the only thing that
    // can tell whether the right one moved.
    await centre(page, "source");
    await page.click("#btn-split");
    await page.waitUntil(
      () => document.querySelector("#center")?.dataset.split === "x",
      { what: "the centre to split" },
    );
    await page.waitFor("#disasm .asm-row", { what: "the disassembly beside the source" });

    // Focus the source slot and go somewhere; the disassembly must not follow.
    await page.click("#source");
    await page.waitFor('[data-slot-head="a"].is-focused', { what: "the source slot focused" });
    const asmBefore = await page.evaluate(
      () => document.querySelector("#disasm .asm-row")?.textContent ?? "");
    await goTo(page, "globals.c:36");
    await page.waitFor('#source .src-row[data-line="36"]', { what: "line 36 in the source" });
    await page.waitUntil(
      (was) => (document.querySelector("#disasm .asm-row")?.textContent ?? "") === was,
      { what: "the unfocused disassembly to stay where it was", args: [asmBefore] },
    );

    // Now focus the disassembly and go: this time it is the one that moves.
    await page.click("#disasm");
    await page.waitFor('[data-slot-head="b"].is-focused', { what: "the disassembly focused" });
    await goTo(page, "walk");
    await page.waitUntil(
      () => document.querySelector('[data-slot-name="b"]')
        && [...document.querySelectorAll("#disasm .asm-row")]
          .some((r) => r.textContent.includes("walk")),
      { what: "the disassembly to move to walk" },
    );

    // --- the picture -------------------------------------------------------
    // Both views up, the box holding what was typed, and the disassembly on
    // the function it named. The source pane beside it is what the jump did
    // *not* touch, which is the part worth seeing.
    await page.shot(ctx.image(), { clip: ".panel-source", pad: 4 });
  },
};
