// Editing a value: double-click, type, Enter.
//
// The screenshot is one open edit box, but the scene drives all three of the
// places a value can be edited — a variable, a register and a byte of memory —
// and checks each one landed. A picture of an edit box proves nothing about
// whether Enter writes anything.

import { breakAtLine, memoryAt, openFile, right, run } from "../lib/scene.mjs";

export default {
  name: "edit-value",
  description: "changing a variable by hand",
  fixtures: ["globals"],
  exe: "globals",

  async run(page, ctx) {
    await openFile(page, "globals.c");
    // Line 65, after walk() has filled s in, so the tree has real values in it.
    await breakAtLine(page, 65);
    await run(page);

    // --- a flat local, which has no varobj until it is touched -----------
    await page.waitFor('.var-row[data-path="local:i"]', { what: "the locals" });
    await page.doubleClick('.var-row[data-path="local:i"] .var-value');
    await page.waitFor(".cell-edit", { what: "an edit box over the value" });
    await page.fill(".cell-edit", "99");
    await page.key("Enter");

    await page.waitUntil(
      () => {
        const row = document.querySelector('.var-row[data-path="local:i"]');
        return row?.querySelector(".var-value")?.textContent === "99"
          && row.classList.contains("is-changed");
      },
      { what: "i to read 99 and be marked as changed" },
    );
    // And gone from the DOM, not merely invisible: an editor left behind would
    // catch the next keystroke meant for the program.
    await page.waitUntil(() => !document.querySelector(".cell-edit"),
      { what: "the edit box to close" });

    // --- a field inside a struct, which is a varobj child ----------------
    await page.click('.var-row[data-path="local:s"] .var-twisty');
    await page.waitFor('.var-row[data-path="local:s"].is-expanded', { what: "s to expand" });
    // is-expanded goes on before the children arrive, so wait for a child.
    await page.waitFor('.var-row[data-path="local:s.visited"]', { what: "s's fields" });
    await page.doubleClick('.var-row[data-path="local:s.visited"] .var-value');
    await page.waitFor(".cell-edit", { what: "an edit box over the field" });
    await page.fill(".cell-edit", "7");
    await page.key("Enter");
    await page.waitUntil(
      () => document.querySelector('.var-row[data-path="local:s.visited"] .var-value')
        ?.textContent === "7",
      { what: "s.visited to read 7" },
    );

    // A struct offers no edit — gdb will not assign to one, so the row must
    // not invite it.
    await page.waitUntil(
      () => {
        const row = document.querySelector('.var-row[data-path="local:s"]');
        return row && !row.classList.contains("is-editable");
      },
      { what: "the struct row to be marked uneditable" },
    );

    // --- a value gdb refuses ---------------------------------------------
    // The box has to survive the refusal with the text still in it, or the
    // correction has to be retyped from scratch — and worse, a write that did
    // not happen would look like one that did.
    await page.doubleClick('.var-row[data-path="local:i"] .var-value');
    await page.waitFor(".cell-edit", { what: "an edit box for the bad value" });
    await page.fill(".cell-edit", "no_such_variable_here");
    await page.key("Enter");
    await page.waitFor(".cell-edit.is-bad", { what: "the box to mark itself rejected" });
    await page.waitUntil(
      () => document.querySelector(".cell-edit")?.value === "no_such_variable_here",
      { what: "the rejected text to still be there to correct" },
    );
    await page.waitUntil(
      () => document.querySelector('.var-row[data-path="local:i"] .var-value')
        ?.textContent === "99",
      { what: "i to be untouched by the refused write" },
    );
    await page.key("Escape");
    await page.waitUntil(() => !document.querySelector(".cell-edit"),
      { what: "Escape to abandon the bad value" });

    // --- a register ------------------------------------------------------
    await right(page, "registers");
    await page.waitFor(".reg-row.is-editable", { what: "an editable register" });
    const reg = await page.evaluate(() => {
      const row = document.querySelector(".reg-row.is-editable");
      return { number: row.dataset.number, name: row.querySelector(".reg-name").textContent };
    });
    await page.doubleClick(`.reg-row[data-number="${reg.number}"] .reg-value`);
    await page.waitFor(".cell-edit", { what: "an edit box over the register" });
    await page.fill(".cell-edit", "0x2a");
    await page.key("Enter");
    await page.waitUntil(
      (number) => {
        const row = document.querySelector(`.reg-row[data-number="${number}"]`);
        return row?.querySelector(".reg-value")?.textContent === "0x2a";
      },
      { what: `$${reg.name} to read 0x2a`, args: [reg.number] },
    );

    // --- a byte of memory ------------------------------------------------
    // Pointed at the field just edited, so this also shows that a write in one
    // view reaches the others: the bytes and the tree are the same memory.
    await right(page, "variables");
    await memoryAt(page, "&s.visited");
    await page.waitFor(".mem-byte.is-editable", { what: "readable bytes" });
    await page.doubleClick('.mem-row:first-child .mem-byte[data-index="0"]');
    await page.waitFor(".cell-edit", { what: "an edit box over the byte" });
    await page.fill(".cell-edit", "0c");
    await page.key("Enter");
    await page.waitUntil(
      () => document.querySelector('.mem-row:first-child .mem-byte[data-index="0"]')
        ?.textContent === "0c",
      { what: "the byte to read 0c" },
    );
    // And the expanded struct field over the same bytes has followed. Nothing
    // stopped, so only the write's own announcement can have done this.
    await page.waitUntil(
      () => document.querySelector('.var-row[data-path="local:s.visited"] .var-value')
        ?.textContent === "12",
      { what: "s.visited to follow the byte to 12" },
    );

    // --- the picture -----------------------------------------------------
    // Re-opened, because an open edit box is the thing worth showing and the
    // ones above were all committed. Over a struct field rather than the loop
    // counter, so the tree around it says what is being edited.
    await right(page, "variables");
    await page.waitFor('.var-row[data-path="local:s.total"]', { what: "the tree again" });
    await page.doubleClick('.var-row[data-path="local:s.total"] .var-value');
    await page.waitFor(".cell-edit", { what: "the edit box to reopen" });
    await page.shot(ctx.image(), { clip: ["#variables", ".cell-edit"], pad: 6 });
  },
};
