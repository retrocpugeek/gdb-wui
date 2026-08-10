#!/usr/bin/env node
// Regenerate the documentation screenshots.
//
//   node scripts/screenshots/capture.mjs              every scene
//   node scripts/screenshots/capture.mjs memory       scenes matching "memory"
//   node scripts/screenshots/capture.mjs --list       what there is
//   node scripts/screenshots/capture.mjs --out /tmp   somewhere other than docs/images
//
// Each scene gets its own gdb-wui, its own gdb and its own compiled fixture, so
// one scene cannot leave state behind that makes the next one's image a lie.
// Every scene waits for the thing it is about to photograph and fails if it
// does not appear — a screenshot of the wrong state is worse than none, because
// nothing downstream can tell.

import { spawn as nodeSpawn } from "node:child_process";
import { readdir } from "node:fs/promises";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath, pathToFileURL } from "node:url";

import { launch } from "./lib/browser.mjs";
import { buildServer, ensureDir, have, prepareProject, startServer } from "./lib/project.mjs";

const here = dirname(fileURLToPath(import.meta.url));
const repoRoot = resolve(here, "..", "..");

const opts = parseArgs(process.argv.slice(2));
const scenes = await loadScenes();

if (opts.list) {
  for (const s of scenes) {
    console.log(`${s.name.padEnd(28)} ${s.description ?? ""}`);
  }
  process.exit(0);
}

const selected = opts.filters.length
  ? scenes.filter((s) => opts.filters.some((f) => s.name.includes(f)))
  : scenes;

if (!selected.length) {
  console.error(`no scene matches ${opts.filters.join(", ")}; --list shows them all`);
  process.exit(1);
}

const outDir = opts.out ?? join(repoRoot, "docs", "images");
await ensureDir(outDir);

const capabilities = {
  ghidra: await findGhidra(),
  gdbserver: (await have("gdbserver")) ? "gdbserver" : null,
};

console.log(`gdb-wui screenshots → ${outDir}`);
console.log(`  ghidra:    ${capabilities.ghidra ?? "not found — those scenes will be skipped"}`);
console.log(`  gdbserver: ${capabilities.gdbserver ?? "not found — those scenes will be skipped"}`);

const server = await buildServer(repoRoot);
const browser = await launch({ width: opts.width, height: opts.height, scale: opts.scale });

const results = [];
try {
  for (const scene of selected) {
    results.push(await runScene(scene));
  }
} finally {
  await browser.close();
  await server.cleanup();
}

report(results);

async function runScene(scene) {
  const missing = (scene.requires ?? []).filter((cap) => !capabilities[cap]);
  if (missing.length) {
    return { scene: scene.name, status: "skipped", note: `needs ${missing.join(", ")}` };
  }

  const started = Date.now();
  let project;
  let running;
  const written = [];
  const helpers = [];
  try {
    project = await prepareProject(repoRoot, scene.fixtures ?? []);
    running = await startServer(server.bin, {
      project: project.dir,
      exe: scene.exe,
      flags: typeof scene.flags === "function"
        ? scene.flags(capabilities)
        : (scene.flags ?? []),
    });

    const page = browser.page;
    await page.goto(running.url);
    // The app is not ready when the document loads — the socket has to open and
    // the server's hello has to arrive. Everything else depends on this.
    await page.waitFor('#conn[data-state="open"]', { what: "the websocket to connect" });

    const ctx = {
      project: project.dir,
      repoRoot,
      capabilities,
      // The server this scene is photographing, for a scene that has to reach
      // it as something other than a browser — the MCP bridge, which joins the
      // same session an agent would.
      server: { bin: server.bin, url: running.url, addr: new URL(running.url).host },
      /**
       * spawn starts a process for the scene's own use — a gdbserver to
       * connect to, say. Killed when the scene ends, however it ends, because
       * a scene that fails halfway must not leave a stub holding a port that
       * the next run needs.
       */
      spawn(cmd, args, { stdin = false } = {}) {
        // stdin is closed unless asked for. A gdbserver wants nothing typed at
        // it; the MCP bridge is spoken to that way, and giving every helper a
        // pipe nobody drains is how a scene deadlocks.
        const proc = nodeSpawn(cmd, args, {
          stdio: [stdin ? "pipe" : "ignore", "pipe", "pipe"],
        });
        helpers.push(proc);
        return proc;
      },
      image(suffix) {
        const name = suffix ? `${scene.name}-${suffix}` : scene.name;
        const path = join(outDir, `${name}.png`);
        written.push(path);
        return path;
      },
    };

    await scene.run(page, ctx);
    if (!written.length) {
      throw new Error("the scene captured nothing; it must call ctx.image()");
    }
    return {
      scene: scene.name,
      status: "ok",
      note: `${written.length} image${written.length > 1 ? "s" : ""}, ${Date.now() - started}ms`,
    };
  } catch (err) {
    return {
      scene: scene.name,
      status: "failed",
      note: err.message.split("\n")[0],
      detail: `${err.stack}\n${running ? running.log() : ""}`,
    };
  } finally {
    for (const proc of helpers) proc.kill("SIGKILL");
    if (running) await running.stop();
    if (project) await project.cleanup();
  }
}

function report(rows) {
  console.log("");
  for (const r of rows) {
    const mark = { ok: "  ok    ", skipped: "  skip  ", failed: "  FAIL  " }[r.status];
    console.log(`${mark}${r.scene.padEnd(28)} ${r.note}`);
  }
  const failed = rows.filter((r) => r.status === "failed");
  const skipped = rows.filter((r) => r.status === "skipped");
  console.log("");
  console.log(`${rows.length - failed.length - skipped.length} captured, ` +
    `${skipped.length} skipped, ${failed.length} failed`);
  for (const r of failed) {
    console.error(`\n--- ${r.scene} ---\n${r.detail}`);
  }
  if (failed.length) process.exit(1);
}

async function loadScenes() {
  const dir = join(here, "scenes");
  const files = (await readdir(dir)).filter((f) => f.endsWith(".mjs")).sort();
  const loaded = [];
  for (const file of files) {
    const mod = await import(pathToFileURL(join(dir, file)));
    const scene = mod.default;
    if (!scene?.name || typeof scene.run !== "function") {
      throw new Error(`${file} must default-export { name, run }`);
    }
    loaded.push(scene);
  }
  return loaded;
}

// Ghidra is not on PATH — it is a zip you unpack somewhere — so this mirrors
// the search internal/ghidra/locate.go does, and for the same reason: "Ghidra
// not found" on a machine that has Ghidra is a maddening thing to read.
async function findGhidra() {
  const { access } = await import("node:fs/promises");
  const { glob } = await import("node:fs/promises");
  const home = process.env.HOME ?? "";
  const candidates = [
    process.env.GHIDRA_INSTALL_DIR,
    "/opt/ghidra",
    "/usr/share/ghidra",
    "/usr/local/ghidra",
    home && join(home, "ghidra"),
  ].filter(Boolean);

  for (const pattern of ["/opt/ghidra_*", "/usr/share/ghidra_*", home && join(home, "ghidra_*")]) {
    if (!pattern) continue;
    try {
      for await (const dir of glob(pattern)) candidates.push(dir);
    } catch {
      // node:fs/promises.glob is newer than the rest of this; without it the
      // explicit paths above still work.
    }
  }

  for (const dir of candidates) {
    try {
      await access(join(dir, "support", "analyzeHeadless"));
      return dir;
    } catch {
      // Not here.
    }
  }
  return null;
}

function parseArgs(argv) {
  const out = { filters: [], list: false, out: null, width: 1600, height: 1000, scale: 2 };
  for (let i = 0; i < argv.length; i++) {
    const arg = argv[i];
    if (arg === "--list") out.list = true;
    else if (arg === "--out") out.out = resolve(argv[++i]);
    else if (arg === "--width") out.width = Number(argv[++i]);
    else if (arg === "--height") out.height = Number(argv[++i]);
    else if (arg === "--scale") out.scale = Number(argv[++i]);
    else if (arg.startsWith("-")) {
      console.error(`unknown flag ${arg}`);
      process.exit(2);
    } else out.filters.push(arg);
  }
  return out;
}
