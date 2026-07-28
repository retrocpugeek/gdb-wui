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
import { createSource } from "./panels/source.js";
import { createStack } from "./panels/stack.js";
import { createBreakpoints } from "./panels/breakpoints.js";
import { createLog } from "./panels/log.js";

const el = (id) => document.getElementById(id);

const ui = {
  tree: el("tree"),
  source: el("source"),
  sourcePath: el("source-path"),
  sourceMeta: el("source-meta"),
  stack: el("stack"),
  breakpoints: el("breakpoints"),
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
    continue: el("btn-continue"),
    pause: el("btn-pause"),
    next: el("btn-next"),
    step: el("btn-step"),
    finish: el("btn-finish"),
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

const tree = createTree({
  element: ui.tree,
  onOpenFile(path) {
    source.open(path).catch(reportError);
  },
  onError(message) {
    setStatus(message, true);
  },
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
      break;
    case "stopped":
      applyStopped(msg.payload);
      break;
    case "running":
      execBusy = false;
      store.patch({ "session.runState": msg.payload.runState });
      source.clearExecLine();
      stack.clear();
      break;
    case "exited":
      execBusy = false;
      store.patch({
        "session.runState": msg.payload.runState,
        "session.lastStopReason": exitText(msg.payload),
      });
      source.clearExecLine();
      stack.clear();
      log.system(exitText(msg.payload));
      break;
    case "exeLoaded":
      store.patch({
        "session.exePath": msg.payload.path,
        "session.runState": msg.payload.runState,
      });
      log.system(`loaded ${msg.payload.path}`);
      break;
    case "breakpointsChanged":
      breakpoints.set(msg.payload.breakpoints);
      source.setBreakpoints(msg.payload.breakpoints);
      break;
    case "selectionChanged":
      applySelection(msg.payload);
      break;
    case "console":
      log.console(msg.payload.text, msg.payload.stream);
      break;
    case "mi":
      log.mi(msg.payload.direction, msg.payload.text);
      break;
    case "gdbDead":
      execBusy = false;
      store.patch({ "session.runState": "noProgram" });
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

  const frame = frames.find((f) => f.level === selectedFrame);
  if (frame?.source?.available) {
    source.setExecLine(frame.source.path, frame.source.line).catch(reportError);
  } else {
    source.clearExecLine();
  }
}

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
  const frame = (stopped.frames ?? [])[0];
  if (frame?.source?.available) {
    source.setExecLine(frame.source.path, frame.source.line).catch(reportError);
  } else {
    source.clearExecLine();
    if (frame) {
      setStatus(
        frame.source?.gdbPath
          ? `No source for ${frame.source.gdbPath} — disassembly arrives in M6.`
          : `Stopped at ${frame.address} with no source — disassembly arrives in M6.`,
      );
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
  stack.select(sel.frame);
  const frame = stack.frameAt(sel.frame);
  if (frame?.source?.available) {
    source.setExecLine(frame.source.path, frame.source.line).catch(reportError);
  }
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
  exec("exec.run", { stopAtMain: true });
}

ui.buttons.run.addEventListener("click", loadAndRun);
ui.buttons.continue.addEventListener("click", () => exec("exec.continue"));
ui.buttons.pause.addEventListener("click", () => exec("exec.pause"));
ui.buttons.next.addEventListener("click", () => exec("exec.next"));
ui.buttons.step.addEventListener("click", () => exec("exec.step"));
ui.buttons.finish.addEventListener("click", () => exec("exec.finish"));
ui.buttons.kill.addEventListener("click", () => exec("exec.kill"));
el("btn-clear-log").addEventListener("click", () => log.clear());

// Loading a program: the tree cannot tell an executable from a data file
// without reading it, so any non-source click offers to load it and the server
// checks the ELF magic.
tree.setFileHandler((path) => {
  if (/\.(c|h|cc|cpp|hpp|s|md|txt|json|ya?ml|mk|sh)$/i.test(path)) return false;
  send("exe.load", { path })
    .then(() => setStatus(`loaded ${path}`))
    .catch((err) => {
      if (err.code === "bad_request") {
        // Not an executable: fall back to showing it as text.
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
    F6: () => exec("exec.pause"),
    F9: () => {
      const path = source.path;
      if (path) toggleBreakpoint(path, currentLine());
    },
    F10: () => exec("exec.next"),
    F11: () => exec("exec.step"),
    "Shift+F11": () => exec("exec.finish"),
  },
});

// currentLine is the execution line, which is what F9 acts on until M8 adds a
// caret. Returning 0 makes F9 a no-op rather than a surprise.
function currentLine() {
  const frame = stack.frameAt(store.get("selection.frame"));
  return frame?.source?.line ?? 0;
}

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
  ui.buttons.continue.disabled = !stopped;
  ui.buttons.next.disabled = !stopped;
  ui.buttons.step.disabled = !stopped;
  ui.buttons.finish.disabled = !stopped;
  ui.buttons.pause.disabled = !running;
  ui.buttons.kill.disabled = !running && !stopped;
});

conn.connect();
tree.start();
