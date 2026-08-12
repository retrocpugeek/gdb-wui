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

    // Typed at the console, because that is the only way in: there is no
    // attach button, and the picture should not suggest one.
    await page.click("#gdbconsole");
    await page.type("#gdbconsole .xterm-helper-textarea", `attach ${tracee.pid}`);
    await page.key("Enter");

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
    await page.shot(ctx.image(), { clip: [".remote-bar", "#gdbconsole"], pad: 8 });
  },
};
