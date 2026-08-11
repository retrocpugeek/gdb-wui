// Acting on a name in the decompiled text, and reading the panes afterwards.
//
// A stripped binary's recovered C is a page of names that exist nowhere else:
// FUN_00401196 for the function it calls, DAT_00404068 for the global it reads.
// Until now they were only text. This scene right-clicks one of each — a
// breakpoint on the call, a watch on the global — and then checks the two panes
// that have to say what happened.
//
// Both panes had the same problem and it is the reason this is one scene rather
// than two: a breakpoint on a stripped binary reads `*0x401196` and a watch on
// one reads `*(undefined4 *)0x404068`. Each says where and neither says what,
// and the decompiler is the only thing here that knows. So the assertions are
// about the *name* appearing beside the address, not about the row existing.

import { centre, menuItem } from "../lib/scene.mjs";

export default {
  name: "decomp-navigate",
  description: "a breakpoint and a watch made from names in the decompiled text",
  fixtures: ["globals-stripped"],
  exe: "globals-stripped",
  requires: ["ghidra"],
  flags: (caps) => ["-ghidra", caps.ghidra],

  async run(page, ctx) {
    // Stripping leaves the dynamic symbol table, so printf is still nameable
    // and is the only way to stop with the program's own code above it on the
    // stack. Same handle stack-names and decomp-rename use, for the same
    // reason.
    await page.fill("#symbols-search", "printf");
    // Until the filter has actually been applied the pane is still showing the
    // whole table, and the first function row in it is whatever the program
    // starts with. Waiting for a row to *exist* is not enough — one always
    // does — so this waits for every row to be a match. Without it the
    // breakpoint lands on __stack_chk_fail@plt, which is never called, and the
    // scene fails four steps later with "the program did not stop".
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

    // The caller, which is the program's own code and the only thing here the
    // decompiler has anything to say about. Analysis is seconds on this and
    // minutes on firmware.
    await page.waitFor("#stack .list-main.is-recovered", {
      what: "a frame named by the decompiler",
      timeout: 300_000,
    });
    await page.evaluate(() => {
      [...document.querySelectorAll("#stack .list-row")]
        .find((r) => r.querySelector(".list-main")?.classList.contains("is-recovered"))
        .querySelector(".list-main").click();
    });
    await centre(page, "decomp");
    await page.waitFor(".dec-row", { what: "decompiled C", timeout: 300_000 });

    // --- a breakpoint on a name in the text --------------------------------
    //
    // A callee, not the function on screen: its own name is in the header line,
    // and breaking on the function you are already reading proves nothing about
    // resolving a name you found in it. Which FUN_ label that is depends on what
    // Ghidra made of this build, so the scene finds one.
    const call = await page.evaluate(() => {
      const shown = document.querySelector(".dec-code")?.textContent ?? "";
      const text = [...document.querySelectorAll(".dec-code")]
        .map((e) => e.textContent).join("\n");
      const names = [...new Set(text.match(/\bFUN_[0-9a-f]+\b/g) ?? [])];
      return names.find((n) => !shown.includes(n)) ?? null;
    });
    if (!call) {
      throw new Error("no call to another FUN_ label in this function; "
        + "there is nothing here to go to");
    }

    const at = await page.textRect("#decomp", call);
    await page.rightClick(page.centre(at));
    // Named in the label, because the menu is opened on a word and the reader
    // has to be able to see which word it decided on.
    await menuItem(page, `Set breakpoint at ${call}`);

    // The pane, which is the half of this that was invisible before. gdb has no
    // symbol for the address, so the row it draws by itself is `*0x401196` —
    // true, and no help at all in a list of six of them.
    await page.waitUntil(
      (want) => [...document.querySelectorAll("#breakpoints .bp-row .list-main")]
        .some((m) => m.classList.contains("is-recovered") && m.textContent.startsWith(want)),
      { what: `the breakpoint named ${call}`, args: [call], timeout: 60_000 },
    );
    const bp = await page.evaluate((want) => {
      const main = [...document.querySelectorAll("#breakpoints .bp-row .list-main")]
        .find((m) => m.textContent.startsWith(want));
      const row = main.closest(".bp-row");
      return {
        title: main.title,
        sub: row.querySelector(".list-sub")?.textContent?.trim() ?? "",
        pending: row.classList.contains("is-pending"),
      };
    }, call);
    if (bp.pending) {
      throw new Error(`the breakpoint on ${call} is pending; nothing will ever `
        + "define that name, so it would sit there looking set and never fire");
    }
    // The address is still there. The name replaces what gdb had nothing to
    // say in, not the address it resolved to — that number is what a reader
    // checks against the disassembly.
    if (!/^0x[0-9a-f]+$/i.test(bp.sub)) {
      throw new Error(`the row's second cell is ${JSON.stringify(bp.sub)}, `
        + "so the address it resolved to is no longer shown");
    }
    // And it says whose name it is. FUN_00401196 is obviously a guess; the
    // moment somebody renames it in Ghidra it stops looking like one, and the
    // row would be claiming a symbol the binary does not have.
    if (!bp.title.includes("decompiler")) {
      throw new Error(`the row's tooltip is ${JSON.stringify(bp.title)}, which `
        + "does not say the name came from the decompiler");
    }

    // The control, and it is a real one: gdb reports no function for a
    // breakpoint on printf in a stripped binary, and Ghidra calls the PLT thunk
    // at that address printf too. The row already says printf because that is
    // what was asked for, and marking it as recovered would dress the binary's
    // own dynamic symbol up as a guess.
    const marked = await page.evaluate(() =>
      [...document.querySelectorAll("#breakpoints .bp-row .list-main")]
        .filter((m) => m.textContent.trim().startsWith("printf"))
        .map((m) => ({ text: m.textContent.trim(), recovered: m.classList.contains("is-recovered") })));
    if (marked.length !== 1) {
      throw new Error(`${marked.length} breakpoint rows name printf, want 1`);
    }
    if (marked[0].recovered) {
      throw new Error(`the breakpoint reading ${JSON.stringify(marked[0].text)} is `
        + "marked as the decompiler's; that name came out of .dynsym");
    }

    // --- a watch on a global in the same text ------------------------------
    //
    // The other half. A global is at a fixed address, valid at every pc and
    // needing no frame, which makes it the most watchable thing in a stripped
    // function — and DAT_00404068 is the only name it has.
    const global = await page.evaluate(() => {
      const text = [...document.querySelectorAll(".dec-code")]
        .map((e) => e.textContent).join("\n");
      const names = [...new Set(text.match(/\bDAT_[0-9a-f]+\b/g) ?? [])];
      return names[0] ?? null;
    });
    if (!global) {
      throw new Error("no DAT_ label in this function; there is no global here to watch");
    }
    const on = await page.textRect("#decomp", global);
    await page.rightClick(page.centre(on));
    await menuItem(page, `Watch ${global}`);

    await page.waitUntil(
      (want) => [...document.querySelectorAll(".var-row.is-watch-root")]
        .some((r) => r.querySelector(".var-recovered")?.textContent === want),
      { what: `the watch labelled ${global}`, args: [global], timeout: 60_000 },
    );
    const watch = await page.evaluate((want) => {
      const row = [...document.querySelectorAll(".var-row.is-watch-root")]
        .find((r) => r.querySelector(".var-recovered")?.textContent === want);
      return {
        expr: row.querySelector(".var-name").textContent,
        value: row.querySelector(".var-value").textContent,
      };
    }, global);
    // Beside the expression rather than instead of it. The expression is what
    // is being read, and it is the thing a cast or a removal acts on; a row
    // showing only the name would hide which of two labels at neighbouring
    // addresses this watch is actually on.
    if (!watch.expr.includes("0x")) {
      throw new Error(`the watch row reads ${JSON.stringify(watch.expr)}; the `
        + "address it is reading is no longer shown");
    }
    if (watch.value === "") {
      throw new Error("the watch has no value, so the name is on a row that reads nothing");
    }

    // The two pictures: the breakpoint list naming code, and the watch naming
    // data. Both cropped to their rows — each pane is a few lines tall in a
    // panel sized for a stack.
    await page.shot(ctx.image(), { clip: "#breakpoints", pad: 6 });
    await page.shot(ctx.image("watch"), { clip: "#variables", pad: 4 });
  },
};
