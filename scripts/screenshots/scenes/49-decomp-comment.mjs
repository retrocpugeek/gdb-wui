// Writing down what a line of recovered C actually does.
//
// Renaming corrects what the decompiler guessed; a comment records what you
// worked out, and it has nowhere else to go — there is no source file to write
// it in. The note goes into the Ghidra project, so it is there the next time
// this binary is debugged.
//
// Three things this scene is here to prove, none of which are visible in a
// screenshot alone:
//
//   The comment reaches the decompiler and comes back in its output. Ghidra
//   displays PRE comments and the entry point's PLATE comment and nothing else
//   (finding 39), so a comment written the wrong way would be stored and never
//   seen.
//
//   A comment line is not a code line. It claims no addresses, so the gutter
//   must not offer a breakpoint on it and the program counter can never land
//   there.
//
//   Pointing at a comment offers to edit *that* comment, which is the gesture
//   anybody would try first.

import { centre, menuItem } from "../lib/scene.mjs";

export default {
  name: "decomp-comment",
  description: "a comment written onto recovered C, and onto the function",
  fixtures: ["nodebug"],
  exe: "nodebug",
  requires: ["ghidra"],
  flags: (caps) => ["-ghidra", caps.ghidra],

  async run(page, ctx) {
    // Stripping leaves the dynamic symbol table, so printf is still nameable
    // and is the only way to break somewhere with the program's own code above
    // it on the stack.
    await page.waitFor(".sym-row", { what: "the symbol list" });
    await page.type("#symbols-search", "printf");
    await page.waitFor(".sym-row", { what: "printf in the list" });
    await page.rightClick(".sym-row");
    await menuItem(page, "Set breakpoint");

    await page.click("#btn-run");
    await page.waitFor('#run-state[data-state="stopped"]', { what: "the program to stop" });
    await page.waitFor("#stack .list-row", { what: "a call stack" });
    await page.waitFor("#stack .list-main.is-recovered", {
      what: "a frame named by the decompiler",
      timeout: 300_000,
    });

    // The caller: the innermost frame is printf, which the decompiler does not
    // have. Selecting it puts the program's own function in the pane.
    await page.evaluate(() => {
      document.querySelector("#stack .list-main.is-recovered").click();
    });
    await centre(page, "decomp");
    await page.waitFor(".dec-row", { what: "decompiled C", timeout: 300_000 });
    await page.waitFor(".dec-row.is-mapped", { what: "a line that came from an address" });

    // --- a line ----------------------------------------------------------
    //
    // A mapped line, because only those came from an address and only an
    // address can hold a comment. Which line depends on what Ghidra made of
    // this build, so the scene finds one rather than naming it.
    const line = await page.evaluate(() => {
      const row = [...document.querySelectorAll(".dec-row.is-mapped")]
        .find((r) => r.querySelector(".dec-code")?.textContent.trim().length > 4);
      if (!row) return null;
      const r = row.querySelector(".dec-code").getBoundingClientRect();
      return {
        n: Number(row.dataset.line),
        text: row.querySelector(".dec-code").textContent.trim(),
        box: { x: r.x, y: r.y, width: r.width, height: r.height },
      };
    });
    if (!line) throw new Error("no mapped line with any code on it to comment");

    await page.rightClick(page.centre(line.box));
    await menuItem(page, "Comment this line");
    await page.waitFor(".cell-edit", { what: "an edit box for the comment" });
    await page.fill(".cell-edit", "counts bytes, not items — the caller doubles it");
    await page.key("Enter");

    await page.waitUntil(
      () => [...document.querySelectorAll(".dec-code")]
        .some((e) => e.textContent.includes("counts bytes, not items")),
      { what: "the comment to appear in the decompiled text", timeout: 60_000 },
    );

    // It is drawn as comment, and the server said so from the decompiler's own
    // markup rather than the client guessing from the text.
    const commented = await page.evaluate(() => {
      const row = [...document.querySelectorAll(".dec-row")]
        .find((r) => r.querySelector(".dec-code")?.textContent.includes("counts bytes"));
      return {
        isComment: row.classList.contains("is-comment"),
        isMapped: row.classList.contains("is-mapped"),
        n: Number(row.dataset.line),
      };
    });
    if (!commented.isComment) {
      throw new Error("the comment line is not marked as comment, so it is "
        + "drawn as though it were code");
    }
    if (commented.isMapped) {
      throw new Error("the comment line claims addresses; the gutter would "
        + "offer a breakpoint on a comment and the pc could land on it");
    }

    // --- editing it by pointing at it ------------------------------------
    const at = await page.evaluate((n) => {
      const row = document.querySelector(`.dec-row[data-line="${n}"]`);
      const r = row.querySelector(".dec-code").getBoundingClientRect();
      return { x: r.x, y: r.y, width: r.width, height: r.height };
    }, commented.n);
    await page.rightClick(page.centre(at));
    // The menu says "Edit", not "Comment": right-clicking a comment found the
    // address it hangs on, which is the only way back from the page to the
    // thing the note is about.
    await menuItem(page, "Edit the comment on this line");
    await page.waitFor(".cell-edit", { what: "an edit box over the comment" });
    // Prefilled with what was typed rather than with how it was printed — the
    // decompiler wraps and decorates its rendering, so the stored text is the
    // only thing an editor can start from.
    const prefilled = await page.evaluate(() =>
      document.querySelector(".cell-edit").value);
    if (prefilled !== "counts bytes, not items — the caller doubles it") {
      throw new Error(`the editor opened on ${JSON.stringify(prefilled)}, `
        + "not on the comment as it was typed");
    }
    await page.key("Escape");
    await page.waitUntil(() => !document.querySelector(".cell-edit"),
      { what: "the edit box to close" });

    // --- the function ----------------------------------------------------
    //
    // A different Ghidra API and a different place on the page: this one is the
    // entry point's plate comment, which the decompiler prints as the header
    // above the prototype.
    await page.rightClick(page.centre(line.box));
    await menuItem(page, "Comment the function");
    await page.waitFor(".cell-edit", { what: "an edit box for the function comment" });
    await page.fill(".cell-edit", "formats one report line and prints it");
    await page.key("Enter");

    await page.waitUntil(
      () => [...document.querySelectorAll(".dec-code")]
        .some((e) => e.textContent.includes("formats one report line")),
      { what: "the header comment to appear", timeout: 60_000 },
    );
    const above = await page.evaluate(() => {
      const rows = [...document.querySelectorAll(".dec-row")];
      const header = rows.findIndex((r) =>
        r.querySelector(".dec-code")?.textContent.includes("formats one report line"));
      const body = rows.findIndex((r) =>
        r.querySelector(".dec-code")?.textContent.includes("counts bytes"));
      return { header, body };
    });
    if (above.header < 0 || above.header > above.body) {
      throw new Error("the function's comment is not above the body; it was "
        + "written as something other than a header comment");
    }

    // Cropped to the code: this function is a few lines and the pane is a
    // thousand pixels tall, so the untrimmed frame is mostly background.
    const pane = await page.rect(".panel-source");
    const last = await page.evaluate(() => {
      const rows = [...document.querySelectorAll(".dec-row")];
      const r = rows[rows.length - 1].getBoundingClientRect();
      return { x: r.x, y: r.y, width: r.width, height: r.height };
    });
    await page.shot(ctx.image(), {
      clip: {
        x: pane.x,
        y: pane.y,
        width: pane.width,
        height: last.y + last.height - pane.y + 8,
      },
    });
  },
};
