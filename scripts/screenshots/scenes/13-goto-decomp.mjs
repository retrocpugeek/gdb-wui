// Going to a name that only the decompiler knows, and breaking on it.
//
// On a stripped binary the go-to box has nothing to work with: gdb has never
// heard of FUN_004011d6, and typing it used to be an error. It resolves through
// Ghidra now, and the two halves of that belong in one picture — a name you can
// reach is only half a name, and the other half is being able to stop there.
//
// The breakpoint is the part that used to fail *quietly*. gdb answers an
// unresolvable location under -f with a pending breakpoint, which is right for
// a shared library that has not loaded and wrong here, because nothing will
// ever define this name: it sat there looking set and never fired. So this
// asserts not-pending, rather than merely that a row appeared.

import { centre, goTo, menuItem, run } from "../lib/scene.mjs";

export default {
  name: "goto-decomp",
  description: "a decompiler's name typed into the go-to box, and broken on",
  fixtures: ["globals-stripped"],
  exe: "globals-stripped",
  requires: ["ghidra"],
  flags: (caps) => ["-ghidra", caps.ghidra],

  async run(page, ctx) {
    await page.waitFor(".sym-row.is-decomp", {
      what: "the decompiler's names, once analysis has finished",
      timeout: 300_000,
    });

    // Get the program running first. Disassembly is read out of the inferior's
    // memory, so the view needs a live process — "Cannot disassemble 0x4011d6
    // until the program is running" is what a scene that skipped this gets. On
    // a stripped binary the only way to stop one is a symbol that survived
    // stripping, and printf did, in .dynsym. Same handle stack-names uses, for
    // the same reason.
    await page.fill("#symbols-search", "printf");
    // Wait for the filter to have been applied, not merely for a row to exist:
    // one always does, and until the query comes back the pane is still showing
    // the whole table. Without this the breakpoint below lands on whichever
    // function the program happens to start with — __stack_chk_fail@plt, which
    // is never called — and the scene fails three steps later at "the program
    // did not stop".
    await page.waitUntil(
      () => {
        const rows = [...document.querySelectorAll(".sym-row .list-main")];
        return rows.length > 0 && rows.every((r) => r.textContent.includes("printf"));
      },
      { what: "the symbol list filtered to printf" },
    );
    // The function row, not merely the first match. A filter on "printf" also
    // turns up data symbols, and Set breakpoint is offered only where breaking
    // means something — so the menu on one of those has a single entry and the
    // failure reads as a missing feature rather than as the wrong row.
    const printf = '.sym-row:has(.sym-kind[data-kind="function"])';
    await page.waitFor(printf, { what: "printf in the list, as a function" });
    await page.rightClick(printf);
    await menuItem(page, "Set breakpoint");
    await run(page);
    await page.waitFor('#run-state[data-state="stopped"]', { what: "the program to stop" });

    // Pick a name from the pane rather than hard-coding one. The addresses move
    // with the compiler, and a scene that guessed would fail as a claim about
    // the feature when it was only wrong about the fixture.
    await page.fill("#symbols-search", "FUN_");
    // Again: the rows on screen are the previous filter's until the query comes
    // back, and a decompiler row was among them — PTR_printf_00404008 matches
    // "printf" and is is-decomp, so waiting for one of those found a stale row
    // and read the wrong list.
    await page.waitUntil(
      () => {
        const rows = [...document.querySelectorAll(".sym-row .list-main")];
        return rows.length > 0 && rows.every((r) => r.textContent.includes("FUN_"));
      },
      { what: "the symbol list filtered to the FUN_ labels" },
    );
    const target = await page.evaluate(() => {
      const rows = [...document.querySelectorAll(".sym-row.is-decomp")]
        .filter((r) => r.querySelector(".sym-kind")?.dataset.kind === "function")
        .map((r) => ({
          name: r.querySelector(".list-main").textContent.trim(),
          addr: r.querySelector(".list-sub").textContent.trim(),
        }))
        .filter((r) => r.name.startsWith("FUN_") && /^0x[0-9a-f]+$/.test(r.addr));
      // The highest address: the program's own code sits above the PLT stubs,
      // and a stub disassembles to a single jump, which is a dull picture.
      rows.sort((a, b) => (BigInt(b.addr) > BigInt(a.addr) ? 1 : -1));
      return rows[0] ?? null;
    });
    if (!target) throw new Error("no FUN_ label to go to");

    // --- the go-to box -----------------------------------------------------
    await centre(page, "disasm");
    await goTo(page, target.name);
    await page.waitUntil(
      (want) => [...document.querySelectorAll("#disasm .asm-row")]
        .some((r) => r.dataset.address && BigInt(r.dataset.address) === BigInt(want)),
      { what: `the disassembly at ${target.name}`, args: [target.addr] },
    );

    // The name is not the address it spells. FUN_004011d6 says where Ghidra
    // *linked* the function, and a resolver reading those digits back out of
    // the string would look correct here and be wrong for every relocated PIE.
    // So what is compared is the pane's address against where the jump landed,
    // both of which came from the server rather than from the text.
    const first = await page.evaluate(
      () => document.querySelector("#disasm .asm-row")?.dataset.address ?? "");
    if (first && BigInt(first) > BigInt(target.addr)) {
      throw new Error(`go to ${target.name} started at ${first}, past its own ${target.addr}`);
    }

    // --- and a breakpoint on the same name ---------------------------------
    await page.fill("#symbols-search", target.name);
    await page.waitUntil(
      (want) => {
        const rows = [...document.querySelectorAll(".sym-row .list-main")];
        return rows.length === 1 && rows[0].textContent.trim() === want;
      },
      { what: `${target.name} alone in the list`, args: [target.name] },
    );
    await page.rightClick(printf);
    await menuItem(page, "Set breakpoint");

    await page.waitUntil(
      () => document.querySelectorAll("#breakpoints .bp-row").length >= 2,
      { what: "the second breakpoint" },
    );
    // By cell rather than by row text. A row renders its location and its
    // resolved address side by side with nothing between them, so textContent
    // reads "*0x4011d60x00000000004011d6" and a hex pattern run over that
    // happily matches across the join.
    const bp = await page.evaluate((want) => {
      const rows = [...document.querySelectorAll("#breakpoints .bp-row")];
      const cells = (r) => [...r.querySelectorAll(".list-main, .list-sub")]
        .map((c) => (c.textContent ?? "").trim().replace(/^\*/, ""));
      const at = rows.find((r) => cells(r).some((text) => {
        if (!/^0x[0-9a-f]+$/.test(text)) return false;
        return BigInt(text) === BigInt(want);
      }));
      return at
        ? { found: true, pending: at.classList.contains("is-pending") }
        : { found: false, rows: rows.map((r) => cells(r)) };
    }, target.addr);
    if (!bp.found) {
      throw new Error(`no breakpoint at ${target.addr} (${target.name}); `
        + `the pane holds ${JSON.stringify(bp.rows)}`);
    }
    if (bp.pending) {
      throw new Error(`the breakpoint on ${target.name} is pending: nothing will `
        + "ever define that name, so it would sit there and never fire");
    }

    // The picture: the box holding a name gdb has never heard of, the
    // disassembly it reached, and a breakpoint pane showing a resolved address
    // rather than a pending row.
    await page.shot(ctx.image(), { clip: [".panel-source", "#breakpoints"], pad: 6 });
  },
};
