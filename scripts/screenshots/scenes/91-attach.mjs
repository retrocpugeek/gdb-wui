// Attaching to a process that was already running, and letting it go again.

import { join } from "node:path";
import { setTimeout as sleep } from "node:timers/promises";

export default {
  name: "attach",
  description: "the pill and the detach button over a process gdb-wui did not start",
  fixtures: ["tracee"],

  async run(page, ctx) {
    // A real process, started outside the debugger, exactly as the reader's
    // would be. It prints one line once it has called prctl(PR_SET_PTRACER),
    // which is what makes attaching permitted under the default
    // kernel.yama.ptrace_scope of 1; without that a scene on a stock machine
    // would only ever photograph "Operation not permitted".
    const tracee = ctx.spawn(join(ctx.project, "tracee"), []);
    const ready = new Promise((resolve) => {
      tracee.stdout.on("data", (chunk) => {
        if (String(chunk).includes("ready")) resolve();
      });
    });
    await Promise.race([ready, sleep(3000)]);

    // The box in the console's tab bar, which is what a reader will use. It
    // runs `attach <pid>`, so the console below shows the command and gdb's
    // answer to it — including the refusal, on a machine that does not permit
    // this.
    await page.fill("#attach-pid", String(tracee.pid));
    await page.click("#attach-connect");

    await page.waitFor('#remote-state[data-remote="on"]', {
      what: "gdb to attach to the process",
    });
    await page.waitUntil(
      (pid) => document.querySelector("#remote-state")?.textContent === `attached pid ${pid}`,
      {
        args: [tracee.pid],
        what: "the pill to name the process",
      },
    );
    // The box has to show the whole pid. A field that clips its own value puts
    // a different number next to the pill in this very picture, which is how
    // the first version of it was caught.
    const clipped = await page.evaluate(() => {
      const box = document.querySelector("#attach-pid");
      return box.scrollWidth > box.clientWidth;
    });
    if (clipped) throw new Error("the pid box is too narrow for the pid it holds");

    await page.shot(ctx.image(), { clip: [".remote-bar", "#gdbconsole"], pad: 8 });
  },
};
