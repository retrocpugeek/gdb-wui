// Connecting to a stub, which is how anything not on this machine is debugged.

import { join } from "node:path";
import { setTimeout as sleep } from "node:timers/promises";

export default {
  name: "remote",
  description: "the architecture warning, and an attached gdbserver",
  fixtures: ["globals"],
  requires: ["gdbserver"],

  async run(page, ctx) {
    // A real stub on a real port. A gdbserver on loopback stands in for the
    // qemu, emulator or probe a reader will actually have; from gdb's side
    // they are the same protocol.
    const port = 41234;
    ctx.spawn("gdbserver", [`127.0.0.1:${port}`, join(ctx.project, "globals")]);
    await sleep(700);

    await page.fill("#remote-addr", `127.0.0.1:${port}`);

    // Deliberately with no program loaded first. `target remote` asks the stub
    // for its registers immediately, and reading that reply needs the
    // architecture, which only loading the ELF establishes — so connecting
    // first is the one ordering mistake that fails destructively rather than
    // politely, and it is worth a picture.
    await page.click("#remote-connect");
    await page.waitFor("#confirm:not(.is-hidden)", { what: "the architecture warning" });
    await page.shot(ctx.image("warning"), { clip: ["#confirm", ".remote-bar"], pad: 8 });
    await page.click("#confirm-no");

    // Now do it in the right order: load the program, then connect.
    await page.click('.tree-row[data-path="globals"]');
    await page.waitUntil(
      () => document.querySelector("#exe-name")?.textContent?.includes("globals"),
      { what: "the program to load" },
    );

    await page.click("#remote-connect");
    await page.waitFor('#remote-state[data-remote="on"]', {
      what: "gdb to attach to the stub",
    });
    await page.shot(ctx.image(), { clip: [".remote-bar", "#gdbconsole"], pad: 8 });
  },
};
