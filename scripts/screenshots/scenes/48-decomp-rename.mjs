// Correcting what the decompiler guessed.
//
// A stripped binary decompiles to FUN_00401154, local_10 and uVar1. None of
// those are wrong exactly — they are what can be known without a symbol table —
// but a reader holds a function in their head by its name, and a page of
// invented ones is the single biggest obstacle to reading recovered C.
//
// The scene renames a local and then the function itself, and checks the second
// one landed in the *call stack* as well as in the pane. That is the assertion
// worth having: the names go into the Ghidra project rather than into a table
// of gdb-wui's own, so everything that asks the decompiler gets the new answer.

import { centre, menuItem } from "../lib/scene.mjs";

export default {
  name: "decomp-rename",
  description: "renaming a decompiler-invented name, in the pane and in the stack",
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

    // Import and analysis are seconds here and minutes on real firmware. Until
    // they finish there is nothing to rename.
    await page.waitFor("#stack .list-main.is-recovered", {
      what: "a frame named by the decompiler",
      timeout: 300_000,
    });

    // The caller: the innermost frame is printf, which the decompiler does not
    // have. Selecting it puts its function in the decompiled pane.
    const level = await page.evaluate(() => {
      const rows = [...document.querySelectorAll("#stack .list-row")];
      const at = rows.findIndex((r) =>
        r.querySelector(".list-main")?.classList.contains("is-recovered"));
      rows[at].querySelector(".list-main").click();
      return Number(rows[at].dataset.level);
    });
    await page.waitUntil(
      (want) => document.querySelector(`#stack .list-row[data-level="${want}"]`)
        ?.getAttribute("aria-selected") === "true",
      { what: `frame #${level} to be selected`, args: [level] },
    );

    await centre(page, "decomp");
    await page.waitFor(".dec-row", { what: "decompiled C", timeout: 300_000 });

    // --- a local ---------------------------------------------------------
    //
    // Which local depends on what Ghidra made of this build, so the scene
    // finds one rather than hard-coding a name that a Ghidra release could
    // change out from under it.
    const local = await page.evaluate(() => {
      const text = [...document.querySelectorAll(".dec-code")]
        .map((e) => e.textContent).join("\n");
      const found = text.match(/\b(?:local_\w+|[a-z]{1,3}Var\d+|param_\d+)\b/);
      return found ? found[0] : null;
    });
    if (!local) {
      throw new Error("no decompiler-invented local in the function; "
        + "there is nothing here to rename");
    }
    const box = await page.textRect("#decomp", local);
    await page.rightClick(page.centre(box));
    await menuItem(page, `Rename ${local}`);
    await page.waitFor(".cell-edit", { what: "an edit box over the name" });
    await page.fill(".cell-edit", "packet_len");
    await page.key("Enter");

    await page.waitUntil(
      () => [...document.querySelectorAll(".dec-code")]
        .some((e) => e.textContent.includes("packet_len")),
      { what: "the decompiled text to use the new name", timeout: 60_000 },
    );
    await page.waitUntil(
      (gone) => ![...document.querySelectorAll(".dec-code")]
        .some((e) => e.textContent.includes(gone)),
      { what: `${local} to be gone`, args: [local] },
    );

    // --- the function ----------------------------------------------------
    const was = await page.evaluate(() =>
      document.querySelector("#stack .list-main.is-recovered")?.textContent ?? "");
    const again = await page.textRect("#decomp", "packet_len");
    await page.rightClick(page.centre(again));
    await menuItem(page, "Rename the function");
    await page.waitFor(".cell-edit", { what: "an edit box for the function name" });
    await page.fill(".cell-edit", "emit_report");
    await page.key("Enter");

    // In the pane…
    await page.waitUntil(
      () => [...document.querySelectorAll(".dec-code")]
        .some((e) => e.textContent.includes("emit_report")),
      { what: "the decompiled text to show the new function name", timeout: 60_000 },
    );
    // …and in the call stack, which is the whole point: this name lives in the
    // Ghidra project, not in the pane that changed it.
    await page.waitUntil(
      () => document.querySelector("#stack .list-main.is-recovered")
        ?.textContent.includes("emit_report"),
      { what: "the call stack to show the new name", timeout: 60_000 },
    );

    const row = await page.evaluate(() => {
      const main = document.querySelector("#stack .list-main.is-recovered");
      return { text: main.textContent, title: main.title };
    });
    if (row.text === was) {
      throw new Error("the stack row did not change");
    }
    // Still marked as recovered, and still saying so: a name someone typed is
    // no more a symbol than FUN_00401154 was. Presenting it as one would be
    // the same lie in better handwriting.
    if (!row.title.includes("decompiler")) {
      throw new Error(`the renamed frame's tooltip is ${JSON.stringify(row.title)}, `
        + "which no longer says the name came from the decompiler");
    }

    // --- renaming from the call stack, with the pane elsewhere -----------
    //
    // The other recovered frame is a different function, and selecting it puts
    // that one in the pane. Renaming the first frame from the stack must not
    // drag the pane back: someone who navigated somewhere on purpose should
    // stay there, and the reply carries a function they are not looking at.
    const other = await page.evaluate(() => {
      const rows = [...document.querySelectorAll("#stack .list-row")]
        .filter((r) => r.querySelector(".list-main")?.classList.contains("is-recovered"));
      const last = rows[rows.length - 1];
      last.querySelector(".list-main").click();
      return last.querySelector(".list-main").textContent;
    });
    if (other.includes("emit_report")) {
      throw new Error("only one recovered frame; there is no second function "
        + "to leave the pane showing");
    }
    await page.waitUntil(
      () => !document.querySelector("#source-meta").textContent.includes("emit_report"),
      { what: "the pane to follow the other frame", timeout: 60_000 },
    );
    const elsewhere = await page.evaluate(() =>
      document.querySelector("#source-meta").textContent);

    // Every value the header takes from here on, not just the one at the end.
    // The pane recovering from a wrong repaint looks the same as never having
    // made one, and only one of those is the behaviour: a flash of somebody
    // else's function is exactly what the guard exists to prevent.
    await page.evaluate(() => {
      window.__metaLog = [];
      const head = document.querySelector("#source-meta");
      new MutationObserver(() => window.__metaLog.push(head.textContent))
        .observe(head, { childList: true, subtree: true, characterData: true });
    });

    const target = await page.evaluate(() => {
      const row = [...document.querySelectorAll("#stack .list-row")]
        .find((r) => r.querySelector(".list-main")?.textContent.includes("emit_report"));
      const r = row.querySelector(".list-main").getBoundingClientRect();
      return { x: r.x, y: r.y, width: r.width, height: r.height };
    });
    await page.rightClick(page.centre(target));
    await menuItem(page, "Rename emit_report");
    await page.waitFor(".cell-edit", { what: "an edit box over the stack row" });
    await page.fill(".cell-edit", "send_report");
    await page.key("Enter");

    await page.waitUntil(
      () => [...document.querySelectorAll("#stack .list-main")]
        .some((e) => e.textContent.includes("send_report")),
      { what: "the stack row to take the new name", timeout: 60_000 },
    );
    const still = await page.evaluate(() =>
      document.querySelector("#source-meta").textContent);
    if (still !== elsewhere) {
      throw new Error(`the pane moved from ${JSON.stringify(elsewhere)} to `
        + `${JSON.stringify(still)}; renaming from the stack navigated it`);
    }
    // Give the broadcast's own refresh time to land before judging.
    await page.sleep(400);
    const flashed = await page.evaluate(() => (window.__metaLog ?? [])
      .filter((t) => t.includes("report")));
    if (flashed.length) {
      throw new Error("the pane showed the edited function while the user was "
        + `reading another: ${JSON.stringify(flashed)}`);
    }

    // Back to the renamed function for the photograph.
    await page.evaluate(() => {
      [...document.querySelectorAll("#stack .list-row")]
        .find((r) => r.querySelector(".list-main")?.textContent.includes("send_report"))
        .querySelector(".list-main").click();
    });
    await page.waitUntil(
      () => document.querySelector("#source-meta").textContent.includes("send_report"),
      { what: "the pane to come back to the renamed function", timeout: 60_000 },
    );

    // Cropped to the code rather than to the pane: this function is sixteen
    // lines and the pane is a thousand pixels tall, so the untrimmed frame is
    // mostly empty background with the subject in the top corner.
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
    // And the same name in the call stack, which is the claim the prose makes.
    // Cropped to the frames for the same reason as above.
    const panel = await page.rect("#stack");
    const lastFrame = await page.evaluate(() => {
      const rows = [...document.querySelectorAll("#stack .list-row")];
      const r = rows[rows.length - 1].getBoundingClientRect();
      return { x: r.x, y: r.y, width: r.width, height: r.height };
    });
    await page.shot(ctx.image("stack"), {
      clip: {
        x: panel.x,
        y: panel.y,
        width: panel.width,
        height: lastFrame.y + lastFrame.height - panel.y + 6,
      },
      pad: 4,
    });
  },
};
