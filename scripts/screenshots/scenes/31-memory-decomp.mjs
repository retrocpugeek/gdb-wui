// The memory viewer's symbol column, on a program gdb cannot name.
//
// gdb fills that column by decorating an address — `(void*)0x404040` prints as
// `0x404040 <counter>` — so on a stripped binary it is blank down its whole
// length, which is the program whose hex most needs placing. The decompiler's
// labels fill it instead.
//
// The scene walks the route a reader actually takes: read a name in the
// recovered C or the symbol pane, type it into the go-to box, and land on the
// bytes. Until now the jump worked and the rows said nothing.
//
// The blank rows *below* the label are the control, and they are the honest
// half of the feature: an untyped label covers its own address and no further,
// because Ghidra represents an unexamined byte as one undefined item however
// far the data really runs. A column that labelled every row here would be
// guessing, and this asserts that it does not.

import { centre, goTo, menuItem } from "../lib/scene.mjs";

export default {
  name: "memory-decomp",
  description: "a stripped binary's hex, with the decompiler's labels beside it",
  fixtures: ["globals-stripped"],
  exe: "globals-stripped",
  requires: ["ghidra"],
  flags: (caps) => ["-ghidra", caps.ghidra],

  async run(page, ctx) {
    await page.waitFor(".sym-row.is-decomp", {
      what: "the decompiler's names, once analysis has finished",
      timeout: 300_000,
    });

    // The program has to be running: the bytes come out of the inferior, and
    // the symbol column is answered by asking gdb to evaluate a cast, which
    // needs one too. Stripping leaves the dynamic symbol table, so printf is
    // the one name available to stop on — the same handle the other stripped
    // scenes use.
    await page.fill("#symbols-search", "printf");
    await page.waitUntil(
      () => {
        const rows = [...document.querySelectorAll(".sym-row .list-main")];
        return rows.length > 0 && rows.every((r) => r.textContent.includes("printf"));
      },
      { what: "the symbol list filtered to printf" },
    );
    const printf = '.sym-row:has(.sym-kind[data-kind="function"])';
    await page.waitFor(printf, { what: "printf in the list, as a function" });
    await page.rightClick(printf);
    await menuItem(page, "Set breakpoint");
    await page.click("#btn-run");
    await page.waitFor('#run-state[data-state="stopped"]', { what: "the program to stop" });

    // A global, picked from the pane rather than hard-coded: the addresses move
    // with the compiler, and a scene that guessed would fail as a claim about
    // the feature when it was only wrong about the fixture.
    await page.fill("#symbols-search", "DAT_");
    await page.waitUntil(
      () => {
        const rows = [...document.querySelectorAll(".sym-row .list-main")];
        return rows.length > 0 && rows.every((r) => r.textContent.includes("DAT_"));
      },
      { what: "the symbol list filtered to the DAT_ labels" },
    );
    const target = await page.evaluate(() => {
      const rows = [...document.querySelectorAll(".sym-row.is-decomp")]
        .filter((r) => r.querySelector(".sym-kind")?.dataset.kind === "variable")
        .map((r) => ({
          name: r.querySelector(".list-main").textContent.trim(),
          addr: r.querySelector(".list-sub").textContent.trim(),
        }))
        .filter((r) => r.name.startsWith("DAT_") && /^0x[0-9a-f]+$/.test(r.addr));
      // The lowest address, which is the initialised data. The labels above it
      // are .bss, and a screenful of zeroes says nothing about a symbol column
      // — measured: picking the highest gave forty rows of 00.
      rows.sort((a, b) => (BigInt(a.addr) > BigInt(b.addr) ? 1 : -1));
      return rows[0] ?? null;
    });
    if (!target) throw new Error("no DAT_ label to look at");

    // The reader's route: the name into the go-to box, with memory focused.
    // gdb has never heard of it — the server resolves it through Ghidra.
    await centre(page, "memory");
    await goTo(page, target.name);
    await page.waitFor(".mem-row", { what: `memory at ${target.name}` });

    // The label, on the row that holds it, marked as the decompiler's.
    await page.waitUntil(
      (want) => [...document.querySelectorAll(".mem-row")].some((row) => {
        const sym = row.querySelector(".mem-sym");
        return sym?.textContent.trim() === want && sym.classList.contains("is-recovered");
      }),
      { what: `${target.name} in the symbol column`, args: [target.name], timeout: 60_000 },
    );

    const rows = await page.evaluate(() => [...document.querySelectorAll(".mem-row")]
      .map((row) => ({
        sym: row.querySelector(".mem-sym")?.textContent.trim() ?? "",
        recovered: Boolean(row.querySelector(".mem-sym")?.classList.contains("is-recovered")),
        title: row.querySelector(".mem-sym")?.title ?? "",
      })));

    // Marked, and saying why. FUN_ and DAT_ are plainly guesses, but a label
    // somebody renamed is not, and this column would otherwise present a
    // recovery as something the binary states.
    const named = rows.find((r) => r.sym);
    if (!named.title.includes("decompiler")) {
      throw new Error(`the labelled row's tooltip is ${JSON.stringify(named.title)}, `
        + "which does not say the name came from the decompiler");
    }

    // The control: rows this label does not cover are blank. An untyped label
    // is one byte as far as Ghidra is concerned, so labelling the rest of the
    // screen would be inventing an extent nobody established.
    const blank = rows.filter((r) => !r.sym).length;
    if (blank === 0) {
      throw new Error("every row on screen carries a name; an untyped label "
        + `covers one address, so ${rows.length} of them cannot be inside it`);
    }

    await page.shot(ctx.image(), { clip: ".panel-source", pad: 4 });
  },
};
