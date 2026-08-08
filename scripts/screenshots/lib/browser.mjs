// Launching headless Chrome and driving one page.
//
// Headless matters. CDP's Input domain is inert against the headful Chrome on
// a normal desktop session here — dispatchMouseEvent returns success and the
// page sees nothing — so scenes would have to fake DOM events, which tests the
// fake rather than the application. In true headless the input path is real:
// a dispatched mousePressed becomes a genuine click, with a genuine contextmenu
// where one is due.

import { spawn } from "node:child_process";
import { mkdtemp, readFile, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { setTimeout as sleep } from "node:timers/promises";

import { connect } from "./cdp.mjs";

const CHROME_CANDIDATES = [
  "google-chrome",
  "google-chrome-stable",
  "chromium",
  "chromium-browser",
];

/**
 * launch starts Chrome and attaches to a blank page.
 *
 * width/height are CSS pixels — the size the layout is designed against.
 * scale is the device pixel ratio: 2 gives images that stay sharp when a
 * documentation page displays them at half size, which is how they are read.
 */
export async function launch({ width = 1600, height = 1000, scale = 2 } = {}) {
  const binary = await findChrome();
  // A fresh profile per run, deliberately. gdb-wui persists the splitter
  // positions and the theme in localStorage, so a reused profile would make
  // the screenshots depend on whatever the last run left behind.
  const userDataDir = await mkdtemp(join(tmpdir(), "gdb-wui-shots-"));

  const proc = spawn(binary, [
    "--headless=new",
    "--remote-debugging-port=0",
    `--user-data-dir=${userDataDir}`,
    `--window-size=${width},${height}`,
    "--no-first-run",
    "--no-default-browser-check",
    "--disable-gpu",
    "--hide-scrollbars",
    // Fonts render slightly differently with LCD subpixel antialiasing on,
    // and it is not what a reader's browser will do to a PNG anyway.
    "--disable-lcd-text",
    "about:blank",
  ], { stdio: ["ignore", "ignore", "pipe"] });

  let stderr = "";
  proc.stderr.on("data", (chunk) => {
    stderr += chunk;
  });

  const port = await readDevToolsPort(userDataDir, proc, () => stderr);
  const version = await fetch(`http://127.0.0.1:${port}/json/version`).then((r) => r.json());
  const conn = await connect(version.webSocketDebuggerUrl);

  const { targetId } = await conn.send("Target.createTarget", { url: "about:blank" });
  const { sessionId } = await conn.send("Target.attachToTarget", { targetId, flatten: true });

  await conn.send("Page.enable", {}, sessionId);
  await conn.send("Runtime.enable", {}, sessionId);
  await conn.send("Emulation.setDeviceMetricsOverride", {
    width, height, deviceScaleFactor: scale, mobile: false,
  }, sessionId);
  // Forced, not left to the profile: the UI picks its theme from
  // prefers-color-scheme when localStorage has no preference, and a headless
  // Chrome's default has changed between releases.
  await conn.send("Emulation.setEmulatedMedia", {
    features: [{ name: "prefers-color-scheme", value: "dark" }],
  }, sessionId);

  const page = makePage(conn, sessionId, { width, height });

  return {
    page,
    async close() {
      conn.close();
      proc.kill();
      await rm(userDataDir, { recursive: true, force: true }).catch(() => {});
    },
  };
}

function makePage(conn, sessionId, viewport) {
  const send = (method, params) => conn.send(method, params, sessionId);

  /** evaluate runs fn in the page and returns its value, structured-cloned. */
  async function evaluate(fn, ...args) {
    const expression = `(${fn.toString()})(${args.map((a) => JSON.stringify(a)).join(",")})`;
    const res = await send("Runtime.evaluate", {
      expression, returnByValue: true, awaitPromise: true,
    });
    if (res.exceptionDetails) {
      const text = res.exceptionDetails.exception?.description
        ?? res.exceptionDetails.text;
      throw new Error(`page threw: ${text}`);
    }
    return res.result.value;
  }

  /**
   * waitUntil polls fn until it returns something truthy.
   *
   * Polling rather than a mutation observer because what scenes wait on is
   * usually a class toggle deep in a virtual list, and the honest signal is
   * "the DOM now says X", not "something changed".
   */
  async function waitUntil(fn, { timeout = 20_000, what = "condition", ...rest } = {}) {
    const deadline = Date.now() + timeout;
    let last;
    for (;;) {
      last = await evaluate(fn, ...(rest.args ?? []));
      if (last) return last;
      if (Date.now() > deadline) {
        throw new Error(`timed out after ${timeout}ms waiting for ${what}`);
      }
      await sleep(50);
    }
  }

  /** waitFor waits for a selector to match, and returns how many did. */
  function waitFor(selector, opts = {}) {
    return waitUntil(
      (sel) => document.querySelectorAll(sel).length || false,
      { what: selector, args: [selector], ...opts },
    );
  }

  /** waitForText waits for a selector's text to contain a string. */
  function waitForText(selector, text, opts = {}) {
    return waitUntil(
      (sel, want) => {
        const el = document.querySelector(sel);
        return Boolean(el && el.textContent.includes(want));
      },
      { what: `${text.slice(0, 40)} in ${selector}`, args: [selector, text], ...opts },
    );
  }

  /** rect is a selector's bounding box in CSS pixels. Throws if it is absent. */
  async function rect(selector) {
    const box = await evaluate((sel) => {
      const el = document.querySelector(sel);
      if (!el) return null;
      const r = el.getBoundingClientRect();
      return { x: r.x, y: r.y, width: r.width, height: r.height };
    }, selector);
    if (!box) throw new Error(`no element matches ${selector}`);
    if (box.width === 0 && box.height === 0) {
      throw new Error(`${selector} has no box; it is hidden`);
    }
    return box;
  }

  /**
   * textRect is the box around a substring inside an element.
   *
   * Hover scenes need this. The hover evaluator reads the character under the
   * pointer with caretPositionFromPoint, so pointing at an element's centre
   * lands on whatever token happens to be in the middle of the line. Pointing
   * at a Range around the identifier lands on the identifier.
   */
  async function textRect(selector, needle) {
    const box = await evaluate((sel, want) => {
      const root = document.querySelector(sel);
      if (!root) return null;
      const walker = document.createTreeWalker(root, NodeFilter.SHOW_TEXT);
      for (let node = walker.nextNode(); node; node = walker.nextNode()) {
        const at = node.data.indexOf(want);
        if (at < 0) continue;
        const range = document.createRange();
        range.setStart(node, at);
        range.setEnd(node, at + want.length);
        const r = range.getBoundingClientRect();
        if (r.width === 0) continue;
        return { x: r.x, y: r.y, width: r.width, height: r.height };
      }
      return null;
    }, selector, needle);
    if (!box) throw new Error(`no text ${JSON.stringify(needle)} inside ${selector}`);
    return box;
  }

  const centre = (box) => ({ x: box.x + box.width / 2, y: box.y + box.height / 2 });

  async function mouse(type, { x, y }, { button = "left", clickCount = 1 } = {}) {
    await send("Input.dispatchMouseEvent", {
      type, x: Math.round(x), y: Math.round(y), button, clickCount,
      buttons: type === "mousePressed" ? (button === "right" ? 2 : 1) : 0,
    });
  }

  async function clickAt(point, { button = "left", clickCount = 1 } = {}) {
    await mouse("mouseMoved", point, { button: "none", clickCount: 0 });
    await mouse("mousePressed", point, { button, clickCount });
    await mouse("mouseReleased", point, { button, clickCount });
  }

  async function click(selector, opts = {}) {
    await clickAt(centre(await rect(selector)), opts);
  }

  /**
   * rightClick opens a context menu and waits for it.
   *
   * Chrome raises contextmenu from a dispatched right press, but only when the
   * page has not already swallowed it, so the wait is the assertion: if no menu
   * appears the scene fails here rather than capturing a screenshot of a menu
   * that never opened.
   */
  async function rightClick(selectorOrPoint, { expect = "#ctxmenu:not(.is-hidden)" } = {}) {
    const point = typeof selectorOrPoint === "string"
      ? centre(await rect(selectorOrPoint))
      : selectorOrPoint;
    await clickAt(point, { button: "right" });
    if (expect) await waitFor(expect, { what: `a context menu at ${JSON.stringify(point)}` });
  }

  /**
   * hover rests the pointer somewhere and waits out the dwell.
   *
   * The tooltip is deliberately slow — 300ms of stillness — so that dragging a
   * mouse across source does not fire a burst of evaluations at gdb. A scene
   * has to wait longer than that, and then for the tooltip itself.
   */
  async function hover(point, { expect = "#hovertip:not(.is-hidden)", settle = 700 } = {}) {
    await mouse("mouseMoved", point, { button: "none", clickCount: 0 });
    // A second move a pixel away, because the dwell timer starts on movement
    // and one event into a fresh page can arrive before the listener is up.
    await sleep(60);
    await mouse("mouseMoved", { x: point.x + 1, y: point.y }, { button: "none", clickCount: 0 });
    await sleep(settle);
    if (expect) await waitFor(expect, { what: "the hover tooltip", timeout: 5000 });
  }

  async function type(selector, text) {
    await click(selector);
    await send("Input.insertText", { text });
  }

  /**
   * fill replaces an input's contents rather than appending to them.
   *
   * type() appends, which is right for an empty filter box and wrong for the
   * remote address box — that one ships with `localhost:1234` in it, and
   * typing into it produced `localhost:1234127.0.0.1:41234` and a connection
   * that never came.
   */
  async function fill(selector, text) {
    await click(selector);
    await evaluate((sel) => {
      const el = document.querySelector(sel);
      el.focus();
      el.select();
    }, selector);
    await send("Input.insertText", { text });
  }

  async function key(name, { modifiers = 0 } = {}) {
    const codes = {
      Enter: { windowsVirtualKeyCode: 13, key: "Enter", code: "Enter", text: "\r" },
      Tab: { windowsVirtualKeyCode: 9, key: "Tab", code: "Tab" },
      Escape: { windowsVirtualKeyCode: 27, key: "Escape", code: "Escape" },
      F5: { windowsVirtualKeyCode: 116, key: "F5", code: "F5" },
      F9: { windowsVirtualKeyCode: 120, key: "F9", code: "F9" },
      F10: { windowsVirtualKeyCode: 121, key: "F10", code: "F10" },
      F11: { windowsVirtualKeyCode: 122, key: "F11", code: "F11" },
    };
    const spec = codes[name];
    if (!spec) throw new Error(`no key mapping for ${name}`);
    await send("Input.dispatchKeyEvent", { type: "keyDown", modifiers, ...spec });
    await send("Input.dispatchKeyEvent", { type: "keyUp", modifiers, ...spec });
  }

  /**
   * answerNextPrompt arms a one-shot reply to the next window.prompt.
   *
   * With the Page domain enabled, Chrome hands dialogs to the client and blocks
   * until one is answered — so a scene that clicks "+ watch" without this would
   * hang rather than fail. Adding a watch really does go through prompt(), so
   * this drives the application as it is rather than around it.
   */
  function answerNextPrompt(text) {
    const off = conn.on("Page.javascriptDialogOpening", () => {
      off();
      send("Page.handleJavaScriptDialog", { accept: true, promptText: text })
        .catch(() => {});
    });
  }

  async function goto(url) {
    await send("Page.navigate", { url });
    await conn.once("Page.loadEventFired", { timeout: 30_000 });
  }

  /**
   * assertNothingLocal refuses to capture the developer's home directory.
   *
   * These images are published. gdb, and Ghidra especially, print absolute
   * paths — Ghidra derives its cache directory from the OS user name, so a log
   * pane happily shows /var/tmp/<whoever>-ghidra — and a screenshot is the one
   * artefact nobody greps before committing. It cost one round of review here
   * already.
   *
   * Checked against the text actually inside the capture region, not the whole
   * page, so a scene is not blocked by something it was never going to show.
   */
  async function assertNothingLocal(region) {
    const home = process.env.HOME;
    const user = process.env.USER || home?.split("/").pop();
    const needles = [home, user].filter((s) => s && s.length > 2);
    if (!needles.length) return;

    const found = await evaluate((box, words) => {
      const within = (r) => !box || !(
        r.right < box.x || r.left > box.x + box.width ||
        r.bottom < box.y || r.top > box.y + box.height
      );
      const walker = document.createTreeWalker(document.body, NodeFilter.SHOW_TEXT);
      const hits = [];
      for (let node = walker.nextNode(); node; node = walker.nextNode()) {
        const word = words.find((w) => node.data.includes(w));
        if (!word) continue;
        const range = document.createRange();
        range.selectNodeContents(node);
        if (within(range.getBoundingClientRect())) hits.push(node.data.trim().slice(0, 160));
      }
      return hits;
    }, region ?? null, needles);

    if (found.length) {
      throw new Error(
        `the capture region shows a local path (${needles.join(" or ")}), which must ` +
        `not be published:\n  ${found.slice(0, 3).join("\n  ")}`,
      );
    }
  }

  /**
   * shot captures a PNG.
   *
   * clip takes a selector, a box, or a list of selectors whose union is the
   * region — a page about the memory pane wants the memory pane, not sixteen
   * hundred pixels of everything else. pad widens the result so a highlighted
   * row does not sit flush against the edge.
   */
  async function shot(path, { clip, pad = 0 } = {}) {
    let region;
    if (clip) {
      const boxes = [];
      for (const item of Array.isArray(clip) ? clip : [clip]) {
        boxes.push(typeof item === "string" ? await rect(item) : item);
      }
      const left = Math.min(...boxes.map((b) => b.x)) - pad;
      const top = Math.min(...boxes.map((b) => b.y)) - pad;
      const right = Math.max(...boxes.map((b) => b.x + b.width)) + pad;
      const bottom = Math.max(...boxes.map((b) => b.y + b.height)) + pad;
      region = {
        x: Math.max(0, Math.round(left)),
        y: Math.max(0, Math.round(top)),
        width: Math.min(viewport.width, Math.round(right - left)),
        height: Math.min(viewport.height, Math.round(bottom - top)),
        scale: 1,
      };
    }
    await assertNothingLocal(region);

    const { data } = await send("Page.captureScreenshot", {
      format: "png",
      ...(region ? { clip: region } : {}),
      captureBeyondViewport: false,
    });
    await writeFile(path, Buffer.from(data, "base64"));
    await shrink(path);
    return path;
  }

  return {
    evaluate, waitUntil, waitFor, waitForText,
    rect, textRect, centre,
    click, clickAt, rightClick, hover, type, fill, key, answerNextPrompt,
    goto, shot, sleep,
  };
}

/**
 * shrink quantises a PNG in place, if pngquant is installed.
 *
 * These are screenshots of a dark UI with a few dozen distinct colours, so a
 * palette costs nothing visible and saves about two thirds of the bytes — which
 * matters when two dozen of them live in the repository forever. Optional
 * rather than required: the images are correct either way, and refusing to
 * capture over a missing optimiser would be absurd.
 */
async function shrink(path) {
  const pngquant = await which("pngquant");
  if (!pngquant) return;
  await new Promise((resolve) => {
    const proc = spawn(pngquant, [
      "--quality=70-95", "--speed", "1", "--force", "--skip-if-larger",
      "--output", path, "--", path,
    ], { stdio: "ignore" });
    // Exit 98 means "the result was larger", which is a decision, not a
    // failure; every other non-zero leaves the unquantised file in place.
    proc.on("close", resolve);
    proc.on("error", resolve);
  });
}

async function findChrome() {
  if (process.env.CHROME) return process.env.CHROME;
  for (const name of CHROME_CANDIDATES) {
    const found = await which(name);
    if (found) return found;
  }
  throw new Error(
    `no Chrome found. Tried: ${CHROME_CANDIDATES.join(", ")}. ` +
    "Install one, or set CHROME to a binary.",
  );
}

function which(name) {
  return new Promise((resolve) => {
    const proc = spawn("which", [name], { stdio: ["ignore", "pipe", "ignore"] });
    let out = "";
    proc.stdout.on("data", (c) => { out += c; });
    proc.on("close", (code) => resolve(code === 0 ? out.trim() : null));
    proc.on("error", () => resolve(null));
  });
}

// Chrome writes the port it actually chose into DevToolsActivePort. Asking for
// port 0 and reading it back is what keeps concurrent runs from colliding.
async function readDevToolsPort(userDataDir, proc, stderr) {
  const path = join(userDataDir, "DevToolsActivePort");
  const deadline = Date.now() + 20_000;
  for (;;) {
    if (proc.exitCode !== null) {
      throw new Error(`Chrome exited with ${proc.exitCode}:\n${stderr()}`);
    }
    try {
      const body = await readFile(path, "utf8");
      const port = Number(body.split("\n")[0]);
      if (Number.isInteger(port) && port > 0) return port;
    } catch {
      // Not written yet.
    }
    if (Date.now() > deadline) {
      throw new Error(`Chrome never reported a debugging port:\n${stderr()}`);
    }
    await sleep(50);
  }
}
