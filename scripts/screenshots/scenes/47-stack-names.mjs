// Naming the frames gdb cannot name.
//
// A stripped binary gives gdb nothing for its own functions, so the call stack
// reads "?? ()" for every frame inside the program. Ghidra knows what is there,
// and the names are patched in once it has finished analysing.
//
// The libc frames in the same stack are the control: gdb *can* name those, from
// libc's own symbols, and they have to keep those names untouched.

import { menuItem } from "../lib/scene.mjs";

export default {
  name: "stack-names",
  description: "a stripped binary's call stack, named by the decompiler",
  fixtures: ["nodebug"],
  exe: "nodebug",
  requires: ["ghidra"],
  flags: (caps) => ["-ghidra", caps.ghidra],

  async run(page, ctx) {
    // Stripping leaves the dynamic symbol table, so printf is still nameable
    // and is the only way to break somewhere with a stack above it.
    await page.waitFor(".sym-row", { what: "the symbol list" });
    await page.type("#symbols-search", "printf");
    await page.waitFor(".sym-row", { what: "printf in the list" });
    await page.rightClick(".sym-row");
    await menuItem(page, "Set breakpoint");

    await page.click("#btn-run");
    await page.waitFor('#run-state[data-state="stopped"]', { what: "the program to stop" });
    await page.waitFor("#stack .list-row", { what: "a call stack" });

    // gdb's answer first, so the rest of this is known to be measuring a
    // change rather than a state that was always there.
    const before = await page.evaluate(
      () => [...document.querySelectorAll("#stack .list-main")].map((e) => e.textContent));
    const unnamed = before.filter((t) => t.includes("??"));
    if (unnamed.length === 0) {
      throw new Error(`gdb named every frame (${JSON.stringify(before)}); this `
        + "fixture is supposed to be stripped, so there would be nothing to recover");
    }

    // Import and analysis are seconds here and minutes on real firmware. The
    // stack keeps gdb's "??" until they finish, and then changes under you.
    await page.waitFor("#stack .list-main.is-recovered", {
      what: "a frame named by the decompiler",
      timeout: 300_000,
    });
    await page.waitUntil(
      (want) => document.querySelectorAll("#stack .list-main.is-recovered").length >= want,
      { what: `all ${unnamed.length} unnamed frames named`, args: [unnamed.length] },
    );

    const after = await page.evaluate(() => [...document.querySelectorAll("#stack .list-row")]
      .map((row) => {
        const main = row.querySelector(".list-main");
        return {
          text: main.textContent,
          recovered: main.classList.contains("is-recovered"),
          title: main.title,
        };
      }));

    for (const row of after) {
      if (row.text.includes("??")) {
        throw new Error(`a frame still reads ${JSON.stringify(row.text)}`);
      }
      if (!row.recovered) continue;
      // The prototype goes in the tooltip, and it has to say where the name
      // came from: a function renamed in Ghidra looks exactly like a real
      // symbol, and this stack has none.
      if (!row.title.includes("decompiler")) {
        throw new Error(`${row.text} has tooltip ${JSON.stringify(row.title)}, `
          + "which does not say the name was recovered");
      }
    }

    // Every frame but the innermost is a return address partway through a
    // function, so a bare name would be equally true of a hundred
    // instructions. At least one row has to say how far in it is.
    if (!after.some((r) => r.recovered && /\+0x[0-9a-f]+\(\)$/.test(r.text))) {
      throw new Error("no recovered frame shows its offset into the function: "
        + JSON.stringify(after.map((r) => r.text)));
    }

    // The control. libc's frames were named by gdb and must not be relabelled
    // — the decompiler has no libc and any name it produced for one would be
    // an address collision, not knowledge.
    const libc = after.filter((r) => r.text.includes("libc") || r.text.includes("printf"));
    if (libc.length === 0) {
      throw new Error("no libc frame in the stack; the control is missing");
    }
    for (const row of libc) {
      if (row.recovered) {
        throw new Error(`${row.text} was relabelled by the decompiler, which does not have libc`);
      }
    }

    await page.shot(ctx.image(), { clip: "#stack", pad: 6 });
  },
};
