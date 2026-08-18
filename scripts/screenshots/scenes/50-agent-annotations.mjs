// An agent annotating the binary, watched from the browser.
//
// The scene drives the MCP bridge as a subprocess — the same `gdb-wui -mcp`
// anybody would point Claude Code at — while a browser sits on the same
// session. The annotations arrive in that browser because an edit is broadcast
// to every open tab, which is the payoff of the bridge being a client of the
// ordinary protocol rather than something bolted into the server.
//
// What it proves beyond the photograph:
//
//   The comment and the name reach a browser that was already open, without a
//   reload and without polling.
//
//   They are marked as an agent's. A note something guessed and a note a
//   person concluded must not read alike, and this is where that is visible.

import { centre, menuItem } from "../lib/scene.mjs";

/** A JSON-RPC client for the bridge, over its stdio. */
function bridge(ctx) {
  const proc = ctx.spawn(ctx.server.bin,
    ["-mcp", "-mcp-annotate", "-mcp-run", "-addr", ctx.server.addr],
    { stdin: true });
  let buffer = "";
  const waiting = [];
  proc.stdout.on("data", (chunk) => {
    buffer += chunk;
    for (let at = buffer.indexOf("\n"); at >= 0; at = buffer.indexOf("\n")) {
      const line = buffer.slice(0, at).trim();
      buffer = buffer.slice(at + 1);
      if (!line) continue;
      const next = waiting.shift();
      if (next) next(JSON.parse(line));
    }
  });
  let stderr = "";
  proc.stderr.on("data", (chunk) => { stderr += chunk; });
  proc.on("exit", (code) => {
    while (waiting.length) {
      waiting.shift()({ error: { message: `the bridge exited (${code}): ${stderr}` } });
    }
  });

  let id = 0;
  function rpc(method, params = {}) {
    id += 1;
    return new Promise((resolve, reject) => {
      const timer = setTimeout(
        () => reject(new Error(`the bridge did not answer ${method}: ${stderr}`)),
        180_000);
      waiting.push((msg) => {
        clearTimeout(timer);
        if (msg.error) reject(new Error(`${method}: ${msg.error.message}`));
        else resolve(msg.result);
      });
      proc.stdin.write(`${JSON.stringify({ jsonrpc: "2.0", id, method, params })}\n`);
    });
  }

  return {
    rpc,
    async tool(name, args = {}) {
      const res = await rpc("tools/call", { name, arguments: args });
      const text = res?.content?.[0]?.text ?? "";
      if (res?.isError) throw new Error(`${name}: ${text}`);
      try {
        return JSON.parse(text);
      } catch {
        return text;
      }
    },
  };
}

export default {
  name: "agent-annotations",
  description: "an agent's comment and name, arriving in an open browser",
  fixtures: ["nodebug"],
  exe: "nodebug",
  requires: ["ghidra"],
  flags: (caps) => ["-ghidra", caps.ghidra],

  async run(page, ctx) {
    const agent = bridge(ctx);
    await agent.rpc("initialize", { protocolVersion: "2025-06-18" });

    // Nothing an agent can read until Ghidra has finished. One call rather
    // than a poll, which is the whole reason that tool exists.
    const ready = await agent.tool("wait_for_decompiler", { timeout_seconds: 300 });
    if (ready.state !== "ready") {
      throw new Error(`the decompiler is ${ready.state}, so there is nothing to annotate`);
    }

    // Break where the program's own code is on the stack. printf survives
    // stripping in the dynamic symbol table.
    await agent.tool("set_breakpoint", { location: "printf" });
    const stopped = await agent.tool("run", { timeout_seconds: 120 });
    if (stopped.state !== "stopped") {
      throw new Error(`the program is ${stopped.state}, not stopped`);
    }

    // Out to the program's own frame, named by the decompiler because gdb has
    // nothing to call it.
    const stack = await agent.tool("stack");
    const named = await agent.tool("name_addresses",
      { addresses: stack.frames.map((f) => f.address) });
    const inside = stack.frames.find((f) => named.names.some(
      (n) => n.addr === f.address && !n.name.startsWith("printf")));
    if (!inside) throw new Error("no frame inside the program to annotate");

    // Stopping in printf raises the bar offering to locate printf.c, which
    // this machine has no copy of.
    await page.waitFor("#locate:not(.is-hidden)",
      { what: "the offer to locate libc's source" });

    // Select that frame in the browser as well, the way a person watching
    // would — and the offer goes with it. It was about frame 0's file, and
    // this frame is from a stripped binary with no file of any kind; leaving
    // it up would name printf.c across the top of a function nothing on screen
    // is about.
    await page.evaluate((level) => {
      document.querySelector(`#stack .list-row[data-level="${level}"] .list-main`).click();
    }, inside.level);
    await page.waitUntil(
      (level) => document.querySelector(`#stack .list-row[data-level="${level}"]`)
        ?.getAttribute("aria-selected") === "true",
      { what: "the browser to follow the frame", args: [inside.level] },
    );
    await page.waitFor("#locate.is-hidden",
      { what: "the offer to be dropped with the frame it was about" });

    const fn = await agent.tool("decompile_function", { target: inside.address });
    await agent.tool("select_frame", { frame: inside.level });

    // The step that needs a debugger: read what the recovered variable holds
    // in this process, right now, and write *that* down.
    const readable = fn.vars.filter((v) => v.expr);
    if (!readable.length) throw new Error("no recovered variable could be read");
    const seen = await agent.tool("evaluate", { expr: readable[0].expr });

    const mapped = fn.lines.find((l) => l.addrs?.length);
    await agent.tool("comment", {
      kind: "line",
      function: fn.entry,
      address: mapped.addrs[0],
      text: `observed ${readable[0].name} = ${seen.value} on the first pass`,
    });
    await agent.tool("comment", {
      kind: "function",
      function: fn.entry,
      text: "sums five terms and prints the total",
    });
    await agent.tool("rename", {
      kind: "function",
      function: fn.entry,
      name: fn.name,
      new_name: "sum_and_print",
    });

    // The pane repaints by itself: nothing below asks the page to reload.
    await centre(page, "decomp");
    await page.waitUntil(
      () => [...document.querySelectorAll(".dec-code")]
        .some((e) => e.textContent.includes("observed")),
      { what: "the agent's comment to arrive in the open pane", timeout: 120_000 },
    );
    await page.waitUntil(
      () => document.querySelector("#stack .list-main.is-recovered")
        ?.textContent.includes("sum_and_print"),
      { what: "the call stack to take the agent's name", timeout: 60_000 },
    );

    // Marked as the agent's, which is the claim the prose makes.
    const marks = await page.evaluate(() => {
      const rows = [...document.querySelectorAll(".dec-row.is-comment")];
      return rows.map((r) => ({
        text: r.querySelector(".dec-code").textContent.trim(),
        agent: r.classList.contains("is-agent"),
        title: r.title,
      }));
    });
    if (!marks.length) throw new Error("no comment lines in the pane");
    const unmarked = marks.filter((m) => !m.agent);
    if (unmarked.length) {
      throw new Error("an agent's comment is drawn as though a person wrote it: "
        + JSON.stringify(unmarked));
    }
    if (!marks[0].title.includes("agent")) {
      throw new Error(`the row's tooltip is ${JSON.stringify(marks[0].title)}, `
        + "which does not say who wrote the note");
    }

    // And the offer to take the lot back, which is what makes letting an agent
    // write at all a reasonable thing to agree to.
    const box = await page.textRect("#decomp", "observed");
    await page.rightClick(page.centre(box));
    await menuItem(page, "Undo the agent's last");
    await page.waitUntil(
      () => ![...document.querySelectorAll(".dec-code")]
        .some((e) => e.textContent.includes("observed")),
      { what: "the agent's run to come back off", timeout: 60_000 },
    );

    // Put the work back for the photograph — the point of the picture is what
    // an annotated function looks like, not what its absence looks like.
    await agent.tool("rename", {
      kind: "function",
      function: fn.entry,
      name: fn.name,
      new_name: "sum_and_print",
    });
    await agent.tool("comment", {
      kind: "function",
      function: fn.entry,
      text: "sums five terms and prints the total",
    });
    await agent.tool("comment", {
      kind: "line",
      function: fn.entry,
      address: mapped.addrs[0],
      text: `observed ${readable[0].name} = ${seen.value} on the first pass`,
    });
    await page.waitUntil(
      () => [...document.querySelectorAll(".dec-code")]
        .some((e) => e.textContent.includes("observed")),
      { what: "the annotations to come back", timeout: 60_000 },
    );

    await page.waitUntil(
      () => document.querySelector("#source-meta")?.textContent.includes("sum_and_print"),
      { what: "the pane to show the agent's name for the function", timeout: 60_000 },
    );

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
  },
};
