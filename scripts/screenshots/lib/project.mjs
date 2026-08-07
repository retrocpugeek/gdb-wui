// Building the programs the screenshots debug, and running gdb-wui over them.

import { spawn } from "node:child_process";
import { copyFile, mkdir, mkdtemp, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { setTimeout as sleep } from "node:timers/promises";

/**
 * The fixtures, and how each is built.
 *
 * Shared with the integration tests rather than invented here — testdata/
 * fixtures/*.c are the programs the features were developed against, so the
 * documentation shows the same code the tests prove the behaviour on.
 *
 * -no-pie throughout, which the tests do not do. A documentation page quotes
 * addresses, and under a PIE every address in the prose is wrong for the
 * reader; without one they are the addresses in the ELF and stay put.
 */
const FIXTURES = {
  // Debug info, the ordinary case.
  hello: { cflags: ["-g", "-O0"] },
  globals: { cflags: ["-g", "-O0"] },
  structs: { cflags: ["-g", "-O0"] },
  interactive: { cflags: ["-g", "-O0"] },
  threads: { cflags: ["-g", "-O0", "-pthread"] },

  // Optimised: locals vanish and line numbers jump, which is a thing a reader
  // will hit and should see documented rather than discover.
  opt: { cflags: ["-g", "-O2"] },

  // No debug info, symbol table intact — a release build.
  minsym: { cflags: ["-O0"] },

  // The same program stripped of DWARF but not of its symbol table, which is
  // the case the Symbols pane and the decompiler pages are about.
  "globals-nodebug": { from: "globals", cflags: ["-g", "-O0"], stripDebug: true },

  // Nothing at all: no DWARF, no symbol table. Disassembly is the only view.
  nodebug: { cflags: ["-O0"], strip: true },
};

/** fixtureNames lists what a scene may ask for, for the error message. */
export const fixtureNames = Object.keys(FIXTURES);

/**
 * prepareProject compiles the named fixtures into one directory.
 *
 * One directory for all of them, because that is what a reader's project looks
 * like — several binaries and their sources side by side — and because the file
 * tree is itself a thing the screenshots show.
 */
export async function prepareProject(repoRoot, names) {
  const dir = await mkdtemp(join(tmpdir(), "gdb-wui-docs-"));

  // Every source first, then every binary. Not merely tidy: gdb compares the
  // two timestamps and warns "globals.c is newer than the program — line
  // numbers may be wrong" in the status bar, which would then appear in the
  // screenshots. Two fixtures built from one source, as globals and
  // globals-nodebug are, is exactly how that happens.
  const sources = new Set();
  for (const name of names) {
    const spec = FIXTURES[name];
    if (!spec) {
      throw new Error(`unknown fixture ${name}; have ${fixtureNames.join(", ")}`);
    }
    sources.add(spec.from ?? name);
  }
  for (const source of sources) {
    await copyFile(
      join(repoRoot, "testdata", "fixtures", `${source}.c`),
      join(dir, `${source}.c`),
    );
  }

  for (const name of names) {
    const spec = FIXTURES[name];
    const dst = join(dir, `${spec.from ?? name}.c`);
    await run("gcc", [...spec.cflags, "-no-pie", "-o", join(dir, name), dst]);
    if (spec.stripDebug) await run("objcopy", ["--strip-debug", join(dir, name)]);
    if (spec.strip) await run("strip", [join(dir, name)]);
  }
  return { dir, cleanup: () => rm(dir, { recursive: true, force: true }) };
}

/** buildServer compiles gdb-wui itself, so the images match the code. */
export async function buildServer(repoRoot) {
  const dir = await mkdtemp(join(tmpdir(), "gdb-wui-bin-"));
  const bin = join(dir, "gdb-wui");
  await run("go", ["build", "-o", bin, "./cmd/gdb-wui"], { cwd: repoRoot });
  return { bin, cleanup: () => rm(dir, { recursive: true, force: true }) };
}

/**
 * startServer runs gdb-wui and returns its single-use login URL.
 *
 * -addr 127.0.0.1:0 so concurrent runs cannot collide, and the URL is read from
 * stdout rather than constructed: it carries a token, and the token is the
 * only way in.
 */
export async function startServer(bin, { project, exe, flags = [] }) {
  const args = [
    "-project", project,
    "-open=false",
    "-addr", "127.0.0.1:0",
    ...(exe ? ["-exe", exe] : []),
    ...flags,
  ];
  const proc = spawn(bin, args, { stdio: ["ignore", "pipe", "pipe"] });

  let stdout = "";
  let stderr = "";
  proc.stdout.on("data", (c) => { stdout += c; });
  proc.stderr.on("data", (c) => { stderr += c; });

  const deadline = Date.now() + 30_000;
  for (;;) {
    const line = stdout.split("\n").find((l) => l.startsWith("http://"));
    if (line) {
      return {
        url: line.trim(),
        args,
        log: () => stderr,
        async stop() {
          proc.kill("SIGTERM");
          // gdb and, if configured, a 2 GB Ghidra hang off this process. Give
          // it a moment to take them with it before giving up on politeness.
          for (let i = 0; i < 40 && proc.exitCode === null; i++) await sleep(50);
          if (proc.exitCode === null) proc.kill("SIGKILL");
        },
      };
    }
    if (proc.exitCode !== null) {
      throw new Error(`gdb-wui exited with ${proc.exitCode}:\n${stderr}`);
    }
    if (Date.now() > deadline) {
      proc.kill("SIGKILL");
      throw new Error(`gdb-wui printed no URL in 30s:\n${stderr}`);
    }
    await sleep(50);
  }
}

/** have reports whether a tool is on PATH, for scenes that need one. */
export function have(tool) {
  return new Promise((resolve) => {
    const proc = spawn("which", [tool], { stdio: "ignore" });
    proc.on("close", (code) => resolve(code === 0));
    proc.on("error", () => resolve(false));
  });
}

export async function ensureDir(path) {
  await mkdir(path, { recursive: true });
}

function run(cmd, args, opts = {}) {
  return new Promise((resolve, reject) => {
    const proc = spawn(cmd, args, { stdio: ["ignore", "pipe", "pipe"], ...opts });
    let out = "";
    proc.stdout.on("data", (c) => { out += c; });
    proc.stderr.on("data", (c) => { out += c; });
    proc.on("error", reject);
    proc.on("close", (code) => {
      if (code === 0) return resolve(out);
      reject(new Error(`${cmd} ${args.join(" ")} failed (${code}):\n${out}`));
    });
  });
}
