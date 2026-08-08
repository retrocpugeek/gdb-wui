# Documentation screenshots

Every image under `docs/images/` is generated. Nothing there is captured by hand,
so nothing there quietly stops matching the application.

```sh
node scripts/screenshots/capture.mjs            # all of them
node scripts/screenshots/capture.mjs memory     # scenes matching "memory"
node scripts/screenshots/capture.mjs --list     # what there is
```

Needs `node` ≥ 22, `gcc`, `gdb`, and a Chrome or Chromium on `PATH` (or `CHROME`
pointing at one). Scenes that need Ghidra or `gdbserver` are skipped with a
printed reason when those are absent, and their existing images are left alone.

## How it works

`capture.mjs` builds `gdb-wui` from the working tree, then for each scene:
compiles the fixtures it asks for into a fresh temporary project, starts a server
on a random loopback port, reads the single-use login URL off its stdout, points
headless Chrome at it, runs the scene, and tears the whole lot down.

One server, one gdb and one project per scene, so a scene cannot leave state
behind that makes the next one's image a lie.

There are no dependencies. `lib/cdp.mjs` speaks the DevTools protocol over
Node's built-in `WebSocket` in about a hundred lines, which is enough — and a
screenshot tool that pulled in a browser-automation framework would undo this
repository's claim to build with nothing but `go build`.

## Writing a scene

```js
// scenes/60-registers.mjs
import { breakAtLine, openFile, right, run } from "../lib/scene.mjs";

export default {
  name: "registers",
  description: "the register list at a stop",
  fixtures: ["globals"],
  exe: "globals",
  requires: [],            // "ghidra" and "gdbserver" are the ones there are

  async run(page, ctx) {
    await openFile(page, "globals.c");
    await breakAtLine(page, 49);
    await run(page);
    await right(page, "registers");
    await page.shot(ctx.image(), { clip: "#registers", pad: 4 });
  },
};
```

`ctx.image()` names the file after the scene; `ctx.image("menu")` gives a second
one, `registers-menu.png`.

**A scene must wait for what it is about to photograph.** The helpers in
`lib/scene.mjs` all do, and they throw when the thing does not appear. This is
the rule the whole tool rests on: a screenshot of the wrong state is worse than
no screenshot, because nothing downstream can tell the difference — not the
build, not a reviewer, not the reader.
