// Wiring. Panels own their DOM, the server owns the truth, and this file only
// introduces them to each other.
//
// One rule runs through all of it: the server is authoritative. Nothing here
// predicts what a command will do — a click sends a request, and the UI changes
// when the resulting event arrives. That is why a page reload, a reconnect and
// a second tab all work without special cases: they are all just "apply a
// snapshot".

import { createStore } from "./core/store.js";
import { createConnection } from "./core/ws.js";
import { createKeymap } from "./core/keys.js";
import { createTree } from "./panels/tree.js";
import { createSymbols } from "./panels/symbols.js";
import { createSource } from "./panels/source.js";
import { createStack } from "./panels/stack.js";
import { createBreakpoints } from "./panels/breakpoints.js";
import { createLog } from "./panels/log.js";
import { createVariables } from "./panels/variables.js";
import { createRegisters } from "./panels/registers.js";
import { createThreads } from "./panels/threads.js";
import { createDisasm } from "./panels/disasm.js";
import { createMemory } from "./panels/memory.js";
import { initLayout, initTheme } from "./core/layout.js";
import { createGdbConsole } from "./panels/gdbconsole.js";
import { createTerminal, decodeBase64, encodeBase64 } from "./core/terminal.js";

const el = (id) => document.getElementById(id);

const ui = {
  tree: el("tree"),
  symbols: el("symbols"),
  symbolsSearch: el("symbols-search"),
  symbolsKind: el("symbols-kind"),
  symbolsCount: el("symbols-count"),
  source: el("source"),
  sourcePath: el("source-path"),
  sourceMeta: el("source-meta"),
  stack: el("stack"),
  breakpoints: el("breakpoints"),
  variables: el("variables"),
  registers: el("registers"),
  threads: el("threads"),
  disasm: el("disasm"),
  memory: el("memory"),
  memAddr: el("mem-addr"),
  locate: el("locate"),
  locateText: el("locate-text"),
  locatePick: el("locate-pick"),
  locateApply: el("locate-apply"),
  restart: el("btn-restart"),
  gdbconsole: el("gdbconsole"),
  inferior: el("inferior"),
  log: el("log"),
  conn: el("conn"),
  runState: el("run-state"),
  stopReason: el("stop-reason"),
  projectRoot: el("project-root"),
  gdbVersion: el("gdb-version"),
  statusMessage: el("status-message"),
  exeName: el("exe-name"),
  logHint: el("log-hint"),
  buttons: {
    run: el("btn-run"),
    runMain: el("btn-run-main"),
    continue: el("btn-continue"),
    pause: el("btn-pause"),
    next: el("btn-next"),
    step: el("btn-step"),
    finish: el("btn-finish"),
    stepi: el("btn-stepi"),
    nexti: el("btn-nexti"),
    kill: el("btn-kill"),
  },
};

const store = createStore({
  connection: "connecting",
  session: {
    projectRoot: "",
    runState: "noProgram",
    stopSeq: 0,
    exePath: "",
    gdbVersion: "",
    lastStopReason: "",
  },
  selection: { thread: 0, frame: 0 },
});

// execBusy is the second half of the double-step guard. stopSeq stops a stale
// request from being applied; this stops one from being sent at all, so holding
// F10 yields at most one step per completed stop.
let execBusy = false;

function setStatus(message, isError = false) {
  ui.statusMessage.textContent = message;
  ui.statusMessage.dataset.state = isError ? "closed" : "";
}

const log = createLog({ element: ui.log });

const source = createSource({
  element: ui.source,
  pathLabel: ui.sourcePath,
  metaLabel: ui.sourceMeta,
  onGutterClick: toggleBreakpoint,
});
source.clear();

const stack = createStack({
  element: ui.stack,
  onSelect(level) {
    send("frame.select", { frame: level, stopSeq: store.get("session.stopSeq") })
      .then((sel) => applySelection(sel))
      .catch(reportError);
  },
});

const breakpoints = createBreakpoints({
  element: ui.breakpoints,
  onToggle(number, enabled) {
    send("bp.setEnabled", { number, enabled }).catch(reportError);
  },
  onDelete(number) {
    send("bp.delete", { number }).catch(reportError);
  },
  onReveal(path, line) {
    source.open(path, { line }).catch(reportError);
  },
});

const variables = createVariables({
  element: ui.variables,
  onExpand: (req) => send("vars.expand", req),
  onRemoveWatch: (path) => send("watch.remove", { path }).catch(reportError),
  onError: reportError,
});

const registers = createRegisters({
  element: ui.registers,
  onFetch: (req) => send("regs.values", req),
  onError: reportError,
});

// centerTab tracks which of source/disassembly is showing, because the
// disassembly is only fetched while visible — most stops are a source-level
// step and nobody is looking at machine code.
let centerTab = "source";

const disasm = createDisasm({
  element: ui.disasm,
  onGutterClick(address) {
    setStatus(`${address} — address breakpoints arrive with bp.setAddress`);
  },
});

const memory = createMemory({
  element: ui.memory,
  onRead: (req) => send("mem.read", req),
  onError: reportError,
});

const threads = createThreads({
  element: ui.threads,
  onSelect(id) {
    send("thread.select", { thread: id, stopSeq: store.get("session.stopSeq") })
      .then((sel) => applySelection(sel))
      .catch(reportError);
  },
});

const gdbConsole = createGdbConsole({
  element: ui.gdbconsole,
  onSubmit: (line) => send("console.exec", { line }),
  onComplete: (prefix) => send("console.complete", { prefix }),
});

// The program's terminal is separate from gdb's on purpose: interleaving the
// two is the most confusing thing in existing web debuggers, because "what did
// my program print" and "what did the debugger say" answer different questions.
const inferiorTerm = createTerminal({
  element: ui.inferior,
  onData(data) {
    send("inferior.stdin", { dataB64: encodeBase64(data) }).catch((err) => {
      if (err?.code !== "not_ready") reportError(err);
    });
  },
  onResize(rows, cols) {
    send("inferior.resize", { rows, cols }).catch(() => {});
  },
});

const tree = createTree({
  element: ui.tree,
  onOpenFile(path) {
    source.open(path).catch(reportError);
  },
  onError(message) {
    setStatus(message, true);
  },
});

// The symbol pane. Filtering is a server round trip per (debounced) keystroke
// rather than a one-off bulk fetch, so a firmware image with fifty thousand
// symbols costs the same as a hello-world.
const symbols = createSymbols({
  element: ui.symbols,
  input: ui.symbolsSearch,
  kindSelect: ui.symbolsKind,
  countEl: ui.symbolsCount,
  onQuery({ filter, kind }) {
    return send("symbols.list", { filter, kind });
  },
  onJump: jumpToSymbol,
});

const conn = createConnection({
  onStatus(state) {
    store.set("connection", state);
    if (state === "open") log.system("connected");
    if (state === "closed") log.system("disconnected");
  },
  onEvent: handleEvent,
});

function send(type, payload) {
  return conn.send(type, payload);
}

function reportError(err) {
  // "busy" from a stale request is the guard working as designed, not
  // something to shout about.
  if (err?.code === "busy") {
    setStatus(err.message);
    return;
  }
  setStatus(err?.message ?? String(err), true);
  log.system(`error: ${err?.message ?? err}`);
}

// --- events ----------------------------------------------------------------

function handleEvent(msg) {
  switch (msg.event) {
    case "hello":
      applySnapshot(msg.payload);
      symbols.refresh();
      break;
    case "stopped":
      applyStopped(msg.payload);
      break;
    case "running":
      execBusy = false;
      store.patch({ "session.runState": msg.payload.runState });
      source.clearExecLine();
      stack.clear();
      variables.clear();
      break;
    case "exited":
      execBusy = false;
      store.patch({
        "session.runState": msg.payload.runState,
        "session.lastStopReason": exitText(msg.payload),
      });
      source.clearExecLine();
      stack.clear();
      variables.clear();
      registers.clear();
      threads.clear();
      disasm.clear();
      memory.clear();
      log.system(exitText(msg.payload));
      break;
    case "exeLoaded":
      store.patch({
        "session.exePath": msg.payload.path,
        "session.runState": msg.payload.runState,
      });
      log.system(`loaded ${msg.payload.path}`);
      symbols.refresh();
      break;
    case "breakpointsChanged":
      breakpoints.set(msg.payload.breakpoints);
      source.setBreakpoints(msg.payload.breakpoints);
      disasm.setBreakpoints(msg.payload.breakpoints);
      break;
    case "selectionChanged":
      applySelection(msg.payload);
      break;
    case "watchesChanged":
      variables.setWatches(msg.payload.watches, msg.payload.stopSeq);
      break;
    case "symbolsInvalidated":
      symbols.refresh();
      break;
    case "varsInvalidated":
      variables.invalidate();
      registers.clear();
      break;
    case "console":
      log.console(msg.payload.text, msg.payload.stream);
      gdbConsole.output(msg.payload.text);
      break;
    case "inferiorOutput":
      inferiorTerm.write(decodeBase64(msg.payload.dataB64));
      break;
    case "threadsChanged":
      threads.set(msg.payload.threads, msg.payload.selected);
      break;
    case "mi":
      log.mi(msg.payload.direction, msg.payload.text);
      break;
    case "gdbDead":
      execBusy = false;
      store.patch({ "session.runState": "noProgram" });
      // A button, not an automatic restart: gdb dying means something went
      // wrong, and quietly starting another would hide it.
      ui.restart.classList.remove("is-hidden");
      setStatus(`gdb died: ${msg.payload.reason}`, true);
      log.system(`gdb died: ${msg.payload.reason}`);
      for (const line of msg.payload.stderr ?? []) log.system(`gdb stderr: ${line}`);
      break;
    case "shuttingDown":
      setStatus("server is shutting down");
      break;
    case "error":
      setStatus(msg.payload?.message ?? "server error", true);
      break;
    default:
      // Unknown events are ignored so a newer server can add one.
      break;
  }
}

function exitText(payload) {
  if (payload.signal) return `exited on ${payload.signal}`;
  if (payload.exitCode != null) return `exited (${payload.exitCode})`;
  return "exited";
}

// applySnapshot repaints everything from a hello. It is the same code path for
// a first connection, a reload and a reconnect.
function applySnapshot(hello) {
  store.patch({
    "session.projectRoot": hello.projectRoot ?? "",
    "session.runState": hello.runState,
    "session.stopSeq": hello.stopSeq ?? 0,
    "session.exePath": hello.exePath ?? "",
    "session.gdbVersion": hello.gdbVersion ?? "",
    "session.lastStopReason": describeReason(hello.lastStopReason),
  });

  breakpoints.set(hello.breakpoints ?? []);
  source.setBreakpoints(hello.breakpoints ?? []);

  const frames = hello.frames ?? [];
  const selectedFrame = hello.selection?.frame ?? 0;
  stack.set(frames, selectedFrame);
  store.patch({
    "selection.thread": hello.selection?.threadId ?? 0,
    "selection.frame": selectedFrame,
  });

  threads.set(hello.threads ?? [], hello.selection?.threadId ?? 0);
  variables.setLocals(localsToNodes(hello.locals), hello.stopSeq ?? 0);
  registers.onStop(hello.stopSeq ?? 0);
  if (hello.runState === "stopped") {
    send("watch.list", {})
      .then((out) => variables.setWatches(out.watches, out.stopSeq))
      .catch(() => {});
  }

  const frame = frames.find((f) => f.level === selectedFrame);
  if (frame?.source?.available) {
    source.setExecLine(frame.source.path, frame.source.line).catch(reportError);
  } else {
    source.clearExecLine();
  }
}

// showLocate offers to find a file gdb named but we could not resolve. It is an
// offer rather than an error: the file is usually present under a different
// prefix, and one substitution fixes every later frame in that tree.
function showLocate(src) {
  if (!src || src.available || !src.gdbPath) {
    ui.locate.classList.add("is-hidden");
    return;
  }
  ui.locate.classList.remove("is-hidden");
  ui.locateText.textContent = `No local source for ${src.gdbPath}`;
  ui.locatePick.replaceChildren();
  const candidates = src.candidates ?? [];
  for (const path of candidates) {
    const option = document.createElement("option");
    option.value = path;
    option.textContent = path;
    ui.locatePick.append(option);
  }
  const usable = candidates.length > 0;
  ui.locatePick.classList.toggle("is-hidden", !usable);
  ui.locateApply.classList.toggle("is-hidden", !usable);
  ui.locateApply.dataset.gdbPath = src.gdbPath;
  if (!usable) {
    ui.locateText.textContent =
      `No local source for ${src.gdbPath}, and no file here shares its name.`;
  }
}

ui.locateApply.addEventListener("click", () => {
  const gdbPath = ui.locateApply.dataset.gdbPath;
  const path = ui.locatePick.value;
  if (!gdbPath || !path) return;
  send("path.substitute", { gdbPath, path })
    .then((out) => {
      ui.locate.classList.add("is-hidden");
      const count = out.substitutions?.length ?? 0;
      setStatus(`gdb taught where source lives (${count} mapping${count === 1 ? "" : "s"})`);
    })
    .catch(reportError);
});

function applyStopped(stopped) {
  execBusy = false;
  store.patch({
    "session.runState": stopped.runState,
    "session.stopSeq": stopped.stopSeq,
    "session.lastStopReason": describeStop(stopped),
    "selection.thread": stopped.threadId,
    "selection.frame": 0,
  });

  stack.set(stopped.frames ?? [], 0);
  threads.set(stopped.threads ?? [], stopped.threadId);
  disasmPin = null;
  refreshDisasm(stopped.frames?.[0]?.address);
  // Memory is the thing most likely to have changed, so the cache goes.
  memory.onStop(stopped.stopSeq);
  // The stop event already carries frame-0 locals, so the panel repaints with
  // no round-trip; only open subtrees need re-fetching.
  variables.onStop(localsToNodes(stopped.locals), stopped.stopSeq);
  registers.onStop(stopped.stopSeq);
  const frame = (stopped.frames ?? [])[0];
  showLocate(frame?.source);
  if (frame?.source?.available) {
    source.setExecLine(frame.source.path, frame.source.line).catch(reportError);
    if (frame.source.stale) {
      setStatus(`${frame.source.path} is newer than the program — line numbers may be wrong.`);
    }
  } else {
    source.clearExecLine();
    if (frame) {
      // No source: the machine view is the only one there is, so switch to it
      // rather than leaving a blank pane and an explanation.
      const where = frame.from ? ` in ${frame.from}` : "";
      setStatus(`Stopped at ${frame.address}${where} with no source — showing disassembly.`);
      if (centerTab !== "disasm") showCenter("disasm");
      else refreshDisasm(frame.address);
    }
  }
}

function describeStop(stopped) {
  switch (stopped.reason) {
    case "breakpoint-hit":
      return `breakpoint ${stopped.breakpointNumber}`;
    case "function-finished":
      return stopped.returnValue ? `returned ${stopped.returnValue}` : "returned";
    case "signal-received":
      return `${stopped.signal ?? "signal"}${stopped.signalMeaning ? ` (${stopped.signalMeaning})` : ""}`;
    default:
      return describeReason(stopped.reason);
  }
}

// describeReason renders a bare gdb reason. The snapshot carries only the
// reason string, with none of the detail a live stop event has, so a reload
// goes through here — without it the status bar reads "end-stepping-range"
// after a reload and "stepped" before one.
function describeReason(reason) {
  switch (reason) {
    case "end-stepping-range":
      return "stepped";
    case "breakpoint-hit":
      return "breakpoint";
    case "function-finished":
      return "returned";
    case "location-reached":
      return "reached";
    case "exited-normally":
      return "exited (0)";
    default:
      return reason ?? "";
  }
}

function applySelection(sel) {
  if (!sel) return;
  store.patch({ "selection.thread": sel.threadId, "selection.frame": sel.frame });
  // Frames arrive when the selection changed the stack — switching threads.
  // Without this the panel keeps rendering the previous thread's frames, which
  // looks exactly like a working UI showing the wrong data.
  if (sel.frames?.length) stack.set(sel.frames, sel.frame);
  else stack.select(sel.frame);
  if (sel.locals) variables.setLocals(localsToNodes(sel.locals), sel.stopSeq);
  if (sel.threadId) threads.select(sel.threadId);

  const frame = stack.frameAt(sel.frame);
  if (frame?.source?.available) {
    // Frame 0 is where the program counter is; anything above it is a caller
    // being inspected, and gets its own marker so the execution point stays
    // visible and unambiguous.
    if (sel.frame === 0) {
      source.setExecLine(frame.source.path, frame.source.line).catch(reportError);
    } else {
      source.setFrameLine(frame.source.path, frame.source.line).catch(reportError);
    }
  }
  // The machine view follows the selected frame too, when it is showing.
  refreshDisasm(frame?.address);
}

// --- actions ---------------------------------------------------------------

function toggleBreakpoint(path, line) {
  if (!path || !line) return;
  const existing = breakpoints.find(path, line);
  if (existing) {
    send("bp.delete", { number: existing.number }).catch(reportError);
    return;
  }
  send("bp.setSource", { path, line }).catch(reportError);
}

// exec sends one exec command, guarded so a held-down key cannot queue.
function exec(type, extra = {}) {
  if (execBusy) return;
  const runState = store.get("session.runState");
  if (type !== "exec.pause" && type !== "exec.kill" && runState === "running") return;

  execBusy = true;
  send(type, { stopSeq: store.get("session.stopSeq"), ...extra })
    .catch((err) => {
      execBusy = false;
      reportError(err);
    });
}

function loadAndRun() {
  const exePath = store.get("session.exePath");
  if (!exePath) {
    setStatus("No program loaded. Click an executable in the tree to load it.");
    return;
  }
  // No stopAtMain: the user set breakpoints and expects to hit them. Stopping
  // at main regardless would make Run mean something different depending on
  // whether main happens to be where they put a breakpoint. Ctrl+Shift+F5
  // exists for "run and stop at main".
  exec("exec.run", {});
}

function runToMain() {
  if (!store.get("session.exePath")) {
    setStatus("No program loaded.");
    return;
  }
  exec("exec.run", { stopAtMain: true });
}

ui.buttons.run.addEventListener("click", loadAndRun);
el("btn-run-main").addEventListener("click", runToMain);
el("btn-run-entry").addEventListener("click", () => {
  if (!store.get("session.exePath")) {
    setStatus("No program loaded.");
    return;
  }
  // The only way into a stripped binary: --start needs a main symbol, which a
  // stripped binary does not have, so it would run to completion instead.
  exec("exec.run", { stopAtEntry: true });
  showCenter("disasm");
});

// The address bar. Any gdb expression is accepted — "&cfg", "$sp", "buf+16" —
// because that is what a user has in their head, not a hex number they would
// have to look up first.
ui.memAddr.addEventListener("keydown", (ev) => {
  if (ev.key !== "Enter") return;
  const expr = ui.memAddr.value.trim();
  if (!expr) return;
  send("mem.read", { address: expr, count: 16, stopSeq: store.get("session.stopSeq") })
    .then((res) => {
      if (res.unreadable) {
        setStatus(`${expr} → 0x${res.addr.toString(16)} is not readable`);
      }
      memory.show(res.addr, { expr, seq: store.get("session.stopSeq") });
      ui.sourceMeta.textContent = memory.summary();
    })
    .catch(reportError);
});

// showCenter switches the centre tab programmatically.
function showCenter(name) {
  document.querySelector(`.tab[data-center="${name}"]`)?.click();
}
ui.buttons.continue.addEventListener("click", () => exec("exec.continue"));
ui.buttons.pause.addEventListener("click", () => exec("exec.pause"));
ui.buttons.next.addEventListener("click", () => exec("exec.next"));
ui.buttons.step.addEventListener("click", () => exec("exec.step"));
ui.buttons.finish.addEventListener("click", () => exec("exec.finish"));
ui.buttons.stepi.addEventListener("click", () => exec("exec.stepi"));
ui.buttons.nexti.addEventListener("click", () => exec("exec.nexti"));
ui.buttons.kill.addEventListener("click", () => exec("exec.kill"));
el("btn-clear-log").addEventListener("click", () => {
  log.clear();
  gdbConsole.clear();
  inferiorTerm.clear();
});

// Clicking a file does one of two quite different things, and which one is
// decided by the server's `kind` rather than by guessing from the filename: a
// compiled program usually has no extension, so a filename guess is wrong in
// both directions. The tree badges the difference, so the outcome is visible
// before the click.
tree.setFileHandler((path, kind) => {
  if (kind !== "elf") return false;
  send("exe.load", { path })
    .then(() => setStatus(`loaded ${path}`))
    .catch((err) => {
      if (err.code === "bad_request") {
        // Executable but not something gdb will take: show it as text instead
        // of leaving the click with no visible effect.
        source.open(path).catch(reportError);
        return;
      }
      reportError(err);
    });
  return true;
});

createKeymap({
  isTerminalFocus: (target) => Boolean(target.closest?.(".xterm")),
  bindings: {
    F5: () => exec("exec.continue"),
    "Ctrl+F5": () => loadAndRun(),
    "Ctrl+Shift+F5": () => runToMain(),
    F6: () => exec("exec.pause"),
    F9: () => {
      const path = source.path;
      if (path) toggleBreakpoint(path, currentLine());
    },
    F10: () => exec("exec.next"),
    F11: () => exec("exec.step"),
    "Shift+F11": () => exec("exec.finish"),
    "Alt+F11": () => exec("exec.stepi"),
    "Alt+F10": () => exec("exec.nexti"),
  },
});

// currentLine is the execution line, which is what F9 acts on until M8 adds a
// caret. Returning 0 makes F9 a no-op rather than a surprise.
function currentLine() {
  const frame = stack.frameAt(store.get("selection.frame"));
  return frame?.source?.line ?? 0;
}

// disasmPin is a target the user asked to look at — the name of a symbol they
// double-clicked — which overrides the machine view's usual habit of following
// the program counter. Without it, switching to the disassembly tab to show a
// symbol immediately refetches around the PC and the user lands somewhere they
// did not ask for. Cleared on the next stop, because a stop means the PC moved
// and following it is once again what they want.
let disasmPin = null;

// disasmPinExplicit marks the next pinned fetch as one the user asked for by
// hand, so a failure is reported rather than swallowed. It has to be a flag
// rather than an argument because the fetch is usually triggered indirectly,
// by the tab switch that showing the disassembly implies.
let disasmPinExplicit = false;

function fetchPinned() {
  const explicit = disasmPinExplicit;
  disasmPinExplicit = false;
  return showDisasmAt(disasmPin, { explicit });
}

// refreshDisasm keeps the machine view in step with the program counter.
//
// Three cases, cheapest first: the panel is hidden, so do nothing; the new PC
// is already in the window, so move the marker; otherwise fetch. Refetching on
// every instruction step would make stepping through a loop far slower than it
// needs to be.
function refreshDisasm(pc) {
  if (centerTab !== "disasm") return;
  if (disasmPin) {
    fetchPinned();
    return;
  }
  if (pc && disasm.has(pc)) {
    disasm.setPC(pc);
    updateCenterMeta();
    return;
  }
  send("disasm.function", { stopSeq: store.get("session.stopSeq") })
    .then((out) => {
      disasm.set(out);
      disasm.setBreakpoints(breakpoints.all());
      updateCenterMeta();
    })
    .catch((err) => {
      if (err?.code !== "busy" && err?.code !== "not_ready") reportError(err);
    });
}

function updateCenterMeta() {
  if (centerTab !== "disasm") return;
  ui.sourceMeta.textContent = disasm.summary();
}

// showDisasmAt disassembles the function containing a target, which may be an
// address or a symbol name. Names are preferred and the reason is not cosmetic:
// -symbol-info-* reports link-time addresses, so for a position-independent
// executable every one of them is wrong once the program is running and
// relocated. gdb is the only thing that knows the load bias, so let it resolve.
function showDisasmAt(target, { explicit = false } = {}) {
  return send("disasm.function", {
    address: target,
    stopSeq: store.get("session.stopSeq"),
  })
    .then((out) => {
      disasm.set(out);
      disasm.setBreakpoints(breakpoints.all());
      updateCenterMeta();
      return true;
    })
    .catch((err) => {
      // Silence is right for a background refresh and wrong for a double
      // click: the user asked for something and must be told it did not
      // happen.
      if (explicit) {
        setStatus(err?.code === "not_ready"
          ? `Cannot disassemble ${target} until the program is running.`
          : (err?.message ?? String(err)), true);
      } else if (err?.code !== "busy" && err?.code !== "not_ready") {
        reportError(err);
      }
      return false;
    });
}

// jumpToSymbol is what double-clicking a symbol does.
//
// Where it goes depends on what the symbol knows about itself, and the three
// cases are genuinely different rather than fallbacks for one another. Debug
// info means a source line. An ELF symbol means an address and therefore
// disassembly. A symbol whose source file is real but outside the project is
// neither: the honest answer is to say where it claims to live, because the
// user can then add a substitution and try again.
function jumpToSymbol(sym) {
  if (sym.file) {
    disasmPin = null;
    showCenter("source");
    source.open(sym.file, { line: sym.line })
      .then(() => setStatus(`${sym.name} — ${sym.file}:${sym.line}`))
      .catch(reportError);
    return;
  }
  // A variable is data, and disassembling data is meaningless. The memory
  // viewer is the right destination, addressed by &(name) rather than by the
  // name alone: a variable that *does* have debug info evaluates to its value,
  // and showing memory at the address 7 because LogType happens to hold 7 is
  // not what anyone meant.
  if (sym.kind === "variable") {
    disasmPin = null;
    showCenter("memory");
    const expr = `&(${sym.name})`;
    send("mem.read", {
      address: expr,
      count: 64,
      stopSeq: store.get("session.stopSeq"),
    })
      .then((res) => {
        memory.show(res.addr, { expr: sym.name, seq: store.get("session.stopSeq") });
        ui.memAddr.value = expr;
        ui.sourceMeta.textContent = memory.summary();
        setStatus(res.unreadable
          ? `${sym.name} is at 0x${res.addr.toString(16)}, which is not readable.`
          : `${sym.name} — 0x${res.addr.toString(16)}`);
      })
      .catch((err) => {
        setStatus(err?.code === "not_ready"
          ? `Cannot read ${sym.name} until the program is running.`
          : (err?.message ?? String(err)), true);
      });
    return;
  }
  if (sym.address) {
    // Pin the name, not the address, for the reason in showDisasmAt.
    disasmPin = sym.name;
    disasmPinExplicit = true;
    const alreadyShowing = centerTab === "disasm";
    showCenter("disasm");
    // Switching tabs fires refreshDisasm, which honours the pin. Fetch here
    // only when the tab was already showing, so the two paths do not both ask.
    if (alreadyShowing) fetchPinned();
    setStatus(`${sym.name} — ${sym.address}, no debug info`);
    return;
  }
  if (sym.gdbPath) {
    setStatus(
      `${sym.name} is at ${sym.gdbPath}:${sym.line}, which is not inside the project.`,
      true,
    );
    return;
  }
  setStatus(`${sym.name} has no location to jump to.`, true);
}

// localsToNodes lifts the flat locals carried by a stop event into tree rows.
// The server sends the same shape from vars.locals; doing it here as well means
// the panel repaints from the stop event with no extra round-trip.
function localsToNodes(locals) {
  return (locals ?? []).map((v) => ({
    path: `local:${v.name}`,
    name: v.name,
    expr: v.name,
    type: v.type,
    value: v.value,
    expandable: Boolean(v.expandable),
    inScope: true,
    arg: Boolean(v.arg),
    optimizedOut: Boolean(v.optimizedOut),
  }));
}

// Tabs. Hidden panels do no work: registers only fetch once shown, which is
// what keeps a stop from costing a register read nobody asked for.
for (const tab of document.querySelectorAll(".tab")) {
  tab.addEventListener("click", () => {
    const name = tab.dataset.tab;
    for (const other of document.querySelectorAll(".tab")) {
      other.classList.toggle("is-active", other === tab);
    }
    for (const panel of document.querySelectorAll("[data-panel]")) {
      panel.classList.toggle("is-hidden", panel.dataset.panel !== name);
    }
    if (name === "registers") registers.onShow();
    else registers.onHide();
  });
}

// The centre pane's tabs. A stripped binary has no source at all, so the
// disassembly is not an advanced view here — it is the only one.
for (const tab of document.querySelectorAll(".tab[data-center]")) {
  tab.addEventListener("click", () => {
    centerTab = tab.dataset.center;
    for (const other of document.querySelectorAll(".tab[data-center]")) {
      other.classList.toggle("is-active", other === tab);
    }
    for (const panel of document.querySelectorAll("[data-center]:not(.tab)")) {
      panel.classList.toggle("is-hidden", panel.dataset.center !== centerTab);
    }
    ui.memAddr.classList.toggle("is-hidden", centerTab !== "memory");
    if (centerTab === "source") {
      ui.sourcePath.textContent = ui.sourcePath.dataset.saved ?? "";
      ui.sourceMeta.textContent = "";
      return;
    }
    // The source path is meaningless in the other views, and "No file open"
    // beside a screenful of instructions reads as a broken panel.
    if (ui.sourcePath.textContent) ui.sourcePath.dataset.saved = ui.sourcePath.textContent;
    ui.sourcePath.textContent = "";

    if (centerTab === "disasm") {
      const frame = stack.frameAt(store.get("selection.frame"));
      refreshDisasm(frame?.address);
    } else if (centerTab === "memory") {
      ui.sourceMeta.textContent = memory.summary();
      ui.memAddr.focus();
    }
  });
}

// The bottom pane's tabs. xterm cannot measure a hidden element, so a terminal
// has to be refitted when its tab becomes visible.
for (const tab of document.querySelectorAll(".tab[data-bottom]")) {
  tab.addEventListener("click", () => {
    const name = tab.dataset.bottom;
    for (const other of document.querySelectorAll(".tab[data-bottom]")) {
      other.classList.toggle("is-active", other === tab);
    }
    for (const panel of document.querySelectorAll("[data-bottom]:not(.tab)")) {
      panel.classList.toggle("is-hidden", panel.dataset.bottom !== name);
    }
    if (name === "gdb") {
      gdbConsole.resize();
      gdbConsole.focus();
    } else if (name === "inferior") {
      inferiorTerm.resize();
      inferiorTerm.focus();
    }
  });
}

el("btn-add-watch").addEventListener("click", () => {
  const expr = window.prompt("Watch expression:");
  if (!expr) return;
  send("watch.add", { expr })
    .then((out) => variables.setWatches(out.watches, out.stopSeq))
    .catch(reportError);
});

// --- rendering -------------------------------------------------------------

store.subscribe("connection", (state) => {
  ui.conn.dataset.state = state.connection;
  ui.conn.textContent =
    { connecting: "connecting…", open: "connected", closed: "disconnected" }[state.connection] ??
    state.connection;
});

store.subscribe("session", (state) => {
  const s = state.session;
  ui.projectRoot.textContent = s.projectRoot;
  ui.projectRoot.title = s.projectRoot;
  ui.gdbVersion.textContent = s.gdbVersion;
  ui.exeName.textContent = s.exePath;
  ui.stopReason.textContent = s.lastStopReason;

  ui.runState.dataset.state = s.runState;
  ui.runState.textContent =
    { noProgram: "no program", stopped: "stopped", running: "running", exited: "exited" }[
      s.runState
    ] ?? s.runState;

  const stopped = s.runState === "stopped";
  const running = s.runState === "running";
  const loaded = Boolean(s.exePath);
  ui.buttons.run.disabled = !loaded || running;
  ui.buttons.runMain.disabled = !loaded || running;
  ui.buttons.continue.disabled = !stopped;
  ui.buttons.next.disabled = !stopped;
  ui.buttons.step.disabled = !stopped;
  ui.buttons.finish.disabled = !stopped;
  ui.buttons.stepi.disabled = !stopped;
  ui.buttons.nexti.disabled = !stopped;
  ui.buttons.pause.disabled = !running;
  ui.buttons.kill.disabled = !running && !stopped;
});

gdbConsole.ready("gdb console — type a command, Tab completes, ↑ recalls history");

ui.restart.addEventListener("click", () => {
  send("session.restart", {})
    .then((out) => {
      ui.restart.classList.add("is-hidden");
      setStatus(`gdb restarted; ${out.breakpointsRestored ?? 0} breakpoint(s) restored`);
      log.system("gdb restarted");
    })
    .catch(reportError);
});

initLayout({
  app: document.getElementById("app"),
  onResize() {
    // The terminals measure their container, so a splitter drag has to tell
    // them; nothing else in the layout needs to know.
    gdbConsole.resize();
    inferiorTerm.resize();
  },
});
initTheme({ toggle: el("btn-theme") });
document.addEventListener("gdb-wui:theme", () => {
  // The terminals were built with the old palette, so they have to be told.
  gdbConsole.retheme();
  inferiorTerm.retheme();
});

conn.connect();
tree.start();
