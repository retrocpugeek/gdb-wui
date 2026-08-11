// The Symbols pane on a binary that has no symbols.
//
// This is the pane at its emptiest and its most needed at the same time: strip
// a program and gdb has nothing to list, which is exactly the program you
// cannot read without a list. Ghidra's names fill it — FUN_ for the functions
// it recovered, DAT_ for the globals something references — and the scene's job
// is to prove they are *usable* rather than merely printed: a name here has an
// address, and that address is what a jump or a breakpoint acts on.
//
// The rows are drawn differently on purpose, and that is checked here too. A
// decompiler name is a guess and a symbol is a record, and a list that showed
// them alike would be claiming knowledge the binary does not contain.

export default {
  name: "symbols-decomp",
  description: "a stripped binary's functions and globals, listed by the decompiler",
  fixtures: ["globals-stripped"],
  exe: "globals-stripped",
  requires: ["ghidra"],
  flags: (caps) => ["-ghidra", caps.ghidra],

  async run(page, ctx) {
    await page.waitFor("#symbols", { what: "the symbols pane" });

    // What the binary itself offers, before Ghidra has said anything. Stripping
    // leaves the dynamic symbol table, so this is not empty — it is printf and
    // the loader's handful — but none of the program's own functions is in it.
    await page.waitUntil(
      () => {
        const rows = [...document.querySelectorAll(".sym-row .list-main")];
        return rows.length > 0 && !rows.some((r) => r.textContent.trim() === "walk");
      },
      { what: "a symbol list without the program's own functions" },
    );

    // Import and analysis: seconds here, minutes on firmware. The pane says so
    // rather than saying the program has no symbols, and then fills itself.
    await page.waitFor(".sym-row.is-decomp", {
      what: "a name from the decompiler",
      timeout: 300_000,
    });

    // Both populations have to be there. Functions alone would leave out the
    // readable half: a global is at a fixed address, valid at every pc and
    // needing no frame, and DAT_ is the only name it has.
    const kinds = await page.evaluate(() => {
      const rows = [...document.querySelectorAll(".sym-row.is-decomp")];
      const of = (k) => rows.filter((r) => r.querySelector(".sym-kind")?.dataset.kind === k);
      return {
        functions: of("function").map((r) => r.querySelector(".list-main").textContent),
        variables: of("variable").map((r) => r.querySelector(".list-main").textContent),
        withoutAddress: rows
          .filter((r) => !/0x[0-9a-f]+/.test(r.querySelector(".list-sub")?.textContent ?? ""))
          .map((r) => r.querySelector(".list-main").textContent),
      };
    });
    if (!kinds.functions.some((n) => n.startsWith("FUN_"))) {
      throw new Error(`no FUN_ label among the listed functions: ${JSON.stringify(kinds.functions)}`);
    }
    if (!kinds.variables.some((n) => n.startsWith("DAT_"))) {
      throw new Error(`no DAT_ label among the listed globals: ${JSON.stringify(kinds.variables)}`);
    }
    // A name with no address is a name you cannot act on, which is the whole
    // difference between this pane and reading the decompiled text.
    if (kinds.withoutAddress.length) {
      throw new Error("decompiler entries with no address: "
        + JSON.stringify(kinds.withoutAddress));
    }

    // The control. The binary's own remaining symbols — printf and friends from
    // .dynsym — must not be relabelled as the decompiler's, or the distinction
    // the colour is drawing would be decoration.
    const mislabelled = await page.evaluate(() => [...document.querySelectorAll(".sym-row")]
      .filter((r) => r.classList.contains("is-decomp"))
      .map((r) => r.querySelector(".list-main").textContent)
      .filter((n) => n === "printf" || n === "puts"));
    if (mislabelled.length) {
      throw new Error(`the binary's own symbols are marked as the decompiler's: `
        + JSON.stringify(mislabelled));
    }

    // The picture: both kinds on screen at once. Filtering to a prefix would
    // show one or the other, and the point is that the pane holds both.
    await page.type("#symbols-search", "_00");
    await page.waitUntil(
      () => {
        const rows = [...document.querySelectorAll(".sym-row.is-decomp")];
        const kind = (k) => rows.some((r) => r.querySelector(".sym-kind")?.dataset.kind === k);
        return rows.length > 3 && kind("function") && kind("variable");
      },
      { what: "functions and globals together in the filtered list" },
    );

    await page.shot(ctx.image(), {
      clip: ["#symbols-count", ".symbols-filter", "#symbols"],
      pad: 8,
    });
  },
};
