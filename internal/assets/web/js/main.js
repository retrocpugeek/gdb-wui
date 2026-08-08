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
import { createHover } from "./core/hover.js";
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
import { createDecomp } from "./panels/decomp.js";
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
  symbolsLoadOpen: el("symbols-load-open"),
  symbolsLoad: el("symbols-load"),
  symbolsLoadPath: el("symbols-load-path"),
  symbolsLoadMode: el("symbols-load-mode"),
  symbolsLoadOffset: el("symbols-load-offset"),
  symbolsLoadGo: el("symbols-load-go"),
  symbolsLoadCancel: el("symbols-load-cancel"),
  source: el("source"),
  sourcePath: el("source-path"),
  sourceMeta: el("source-meta"),
  stack: el("stack"),
  breakpoints: el("breakpoints"),
  variables: el("variables"),
  registers: el("registers"),
  threads: el("threads"),
  disasm: el("disasm"),
  decomp: el("decomp"),
  memory: el("memory"),
  memAddr: el("mem-addr"),
  ctxmenu: el("ctxmenu"),
  hovertip: el("hovertip"),
  confirm: el("confirm"),
  confirmText: el("confirm-text"),
  confirmYes: el("confirm-yes"),
  confirmNo: el("confirm-no"),
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
  remoteState: el("remote-state"),
  remoteAddr: el("remote-addr"),
  remoteConnect: el("remote-connect"),
  remoteDisconnect: el("remote-disconnect"),
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
    // remote is the server's word on whether gdb is attached to a target it
    // did not start. Null until the first snapshot says otherwise.
    remote: null,
    // decomp is the decompiler's state, fetched rather than pushed: most
    // sessions never open the pane and do not need to know.
    decomp: null,
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
  onGutterClick: (address) => toggleAddressBreakpoint(address),
});

// The decompiled view. Its data comes from Ghidra by way of the server, and
// the whole feature is optional: without a decompiler configured the tab
// explains itself and nothing else changes.
const decomp = createDecomp({
  element: ui.decomp,
  onGutterClick: (address) => toggleAddressBreakpoint(address),
});
decomp.clear();

const memory = createMemory({
  element: ui.memory,
  onRead: (req) => send("mem.read", req),
  onSymbols: (req) => send("mem.symbols", req),
  onError: reportError,
});

// Hovering a variable or a register asks gdb what it holds. The panes decide
// what the pointer is over — the source view walks the line's text, the
// disassembly reads the token span — and this evaluates whatever they name.
//
// Only while stopped, which is not a UI nicety: eval.expr is one of the
// requests the server refuses unless there is a stopped inferior, so a hover
// during a run would be a round trip that can only fail.
const hover = createHover({
  element: ui.hovertip,
  isEnabled: () => store.get("session.runState") === "stopped",
  evaluate(expr) {
    return send("eval.expr", {
      expr,
      thread: store.get("selection.thread"),
      frame: store.get("selection.frame"),
      stopSeq: store.get("session.stopSeq"),
    })
      .then((out) => out.value)
      // A failure is the ordinary outcome of pointing at a word that is not a
      // variable here, so it shows nothing rather than shouting in the status
      // bar. The pointer is already an experiment; a wrong guess costs nothing.
      .catch(() => null);
  },
});
hover.attach(ui.source, (ev) => source.expressionAt(ev));
hover.attach(ui.disasm, (ev) => disasm.expressionAt(ev));
hover.attach(ui.decomp, (ev) => decomp.expressionAt(ev));

// Right-clicking whatever the pointer is over offers what can be done with it.
// The same resolvers the hover uses, so the menu is about exactly the thing
// whose value was just shown.
//
// There are two different questions and they are easy to conflate: *where a
// thing is kept*, and *what it points at*. A pointer variable has both and
// they are different addresses; a register has only the second; a plain int
// has only the first. The menu offers whichever apply and names the address in
// the label, so the choice is visible rather than inferred from a verb.
for (const [pane, resolve] of [
  [ui.source, (ev) => source.expressionAt(ev)],
  [ui.disasm, (ev) => disasm.expressionAt(ev)],
  [ui.decomp, (ev) => decomp.expressionAt(ev)],
]) {
  pane.addEventListener("contextmenu", (ev) => {
    const hit = resolve(ev);
    if (!hit) return;
    ev.preventDefault();
    hover.hide();
    const x = ev.clientX;
    const y = ev.clientY;

    // The value decides half the menu, so it is fetched before the menu opens
    // rather than the menu offering something that cannot work. One round
    // trip, and the hover has usually just made the same one.
    evaluateForMenu(hit.expr).then((res) => {
      const items = [];
      const label = hit.name ?? hit.expr;

      // Where it is kept. A register is not in memory and has no answer here.
      if (hit.storage !== "register") {
        items.push({
          label: "Show where it is stored",
          title: `the bytes at &(${hit.expr})`,
          run: () => showAddressOf(hit.expr, label),
        });
      }
      // What it points at. Offered for anything whose value is address-shaped,
      // which is how a register holding a pointer becomes followable.
      if (res && res.addr >= LOWEST_PLAUSIBLE_ADDRESS) {
        const target = "0x" + res.addr.toString(16);
        items.push({
          label: `Show what it points to — ${target}`,
          title: "follow the value as an address",
          run: () => showAtAddress(res.addr, `*${label}`),
        });
      }

      if (!items.length) {
        setStatus(res && res.value
          ? `${label} is ${res.value}, which is neither in memory nor an address.`
          : `${label} has no address to show.`);
        return;
      }
      showContextMenu(x, y, label, items);
    });
  });
}

// LOWEST_PLAUSIBLE_ADDRESS keeps "show what it points to" off values that are
// plainly not pointers. The first page is never mapped on any system this runs
// on, so a value below it is a small integer wearing a hex hat — offering to
// follow `3` would be noise on every int in the program.
const LOWEST_PLAUSIBLE_ADDRESS = 0x1000;

function evaluateForMenu(expr) {
  if (store.get("session.runState") !== "stopped") return Promise.resolve(null);
  return send("eval.expr", {
    expr,
    thread: store.get("selection.thread"),
    frame: store.get("selection.frame"),
    stopSeq: store.get("session.stopSeq"),
  }).catch(() => null);
}

// showAtAddress points the memory viewer at an address already in hand.
function showAtAddress(addr, label) {
  send("mem.read", { address: "0x" + addr.toString(16), count: 64,
    stopSeq: store.get("session.stopSeq") })
    .then((res) => {
      showCenter("memory");
      memory.show(res.addr, { expr: label, seq: store.get("session.stopSeq") });
      ui.memAddr.value = "0x" + res.addr.toString(16);
      ui.sourceMeta.textContent = memory.summary();
      setStatus(res.unreadable
        ? `0x${res.addr.toString(16)} is not readable — ${label} is not a valid pointer.`
        : `${label} — 0x${res.addr.toString(16)}`);
    })
    .catch(reportError);
}

// showAddressOf points the memory viewer at where a variable lives.
//
// The address, not the value: `&(...)` wrapped around whatever expression the
// pane resolved. The parentheses matter — `&*(int *)($rbp - 8)` is the slot
// and `&(*(int *)($rbp - 8))` is the same thing said unambiguously, while for
// a bare name `&buf + 16` would be a different place entirely.
function showAddressOf(expr, label) {
  send("mem.read", {
    address: `&(${expr})`,
    count: 64,
    stopSeq: store.get("session.stopSeq"),
  })
    .then((res) => {
      showCenter("memory");
      memory.show(res.addr, { expr: label, seq: store.get("session.stopSeq") });
      ui.memAddr.value = `&(${expr})`;
      ui.sourceMeta.textContent = memory.summary();
      setStatus(res.unreadable
        ? `${label} is at 0x${res.addr.toString(16)}, which is not readable.`
        : `${label} — 0x${res.addr.toString(16)}`);
    })
    .catch((err) => {
      // gdb's own words are the useful ones here: "Address requested for
      // identifier ... which is in register $rax" says exactly what is wrong.
      setStatus(err?.message ?? String(err), true);
    });
}

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
      // A value was only true for the stop it was read at.
      hover.hide();
      source.clearExecLine();
      stack.clear();
      variables.clear();
      break;
    case "exited":
      execBusy = false;
      // The session the prompt was guarding has ended on its own.
      hideConfirm();
      hover.hide();
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
      // Whatever the prompt was protecting is already gone — possibly because
      // another browser tab loaded something.
      hideConfirm();
      break;
    case "breakpointsChanged":
      breakpoints.set(msg.payload.breakpoints);
      source.setBreakpoints(msg.payload.breakpoints);
      disasm.setBreakpoints(msg.payload.breakpoints);
      decomp.setBreakpoints(msg.payload.breakpoints);
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
    case "decompLog":
      log.decomp(msg.payload?.text ?? "", msg.payload?.level, msg.payload?.millis);
      break;
    case "decompChanged":
      refreshDecompStatus().then((st) => {
        if (st?.state === "ready" && centerTab === "decomp") {
          refreshDecomp(stack.frameAt(store.get("selection.frame"))?.address);
        }
      });
      break;
    case "remoteChanged":
      store.set("session.remote", msg.payload);
      applyRemote(msg.payload);
      log.system(msg.payload?.connected
        ? `connected to ${msg.payload.address || "remote target"}`
        : "disconnected from the remote target");
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
    "session.remote": hello.remote ?? null,
  });
  applyRemote(hello.remote);

  breakpoints.set(hello.breakpoints ?? []);
  source.setBreakpoints(hello.breakpoints ?? []);
  decomp.setBreakpoints(hello.breakpoints ?? []);

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
  hover.hide();
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
  refreshDecomp(stopped.frames?.[0]?.address);
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
      // No source. The rescue is only for someone looking at the source view,
      // which has nothing to show them — every other centre tab does.
      //
      // This used to fire from any tab but the disassembly, which was right
      // when the disassembly was the only fallback and became wrong the moment
      // there was a second one: on a binary with no debug info *every* stop
      // takes this branch, so stepping in the decompiled view flipped to the
      // disassembly every single time.
      const where = frame.from ? ` in ${frame.from}` : "";
      if (centerTab === "source") {
        setStatus(`Stopped at ${frame.address}${where} with no source — showing disassembly.`);
        showCenter("disasm");
      } else {
        setStatus(`Stopped at ${frame.address}${where}`);
      }
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
  // Locals are per-frame, so a tooltip read in the previous frame is now about
  // a different variable of the same name.
  hover.hide();
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
  // The machine and decompiled views follow the selected frame too, when they
  // are showing.
  refreshDisasm(frame?.address);
  refreshDecomp(frame?.address);
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

// toggleAddressBreakpoint is the machine-level counterpart of toggleBreakpoint.
//
// Deleting an existing one rather than stacking a second is what makes a
// gutter a toggle in both panes; without it a second click on the same line
// silently accumulates breakpoints at one address.
function toggleAddressBreakpoint(address) {
  if (!address) return;
  const existing = breakpoints.findAddress(address);
  if (existing) {
    send("bp.delete", { number: existing.number }).catch(reportError);
    return;
  }
  send("bp.setAddress", { location: address })
    .then((bp) => setStatus(`breakpoint ${bp.number} at ${bp.address ?? address}`))
    .catch(reportError);
}

// setFunctionBreakpoint breaks on a symbol by *name*, not by its address.
//
// The distinction matters and is easy to get backwards: gdb skips the prologue
// for a named function — on the vwfw firmware `break process_packet` stops at
// entry+24, past the register spills — while an address stops on the very
// first instruction, before the frame exists and before any argument has been
// stored. The name is what a user means by "break on this function".
function setFunctionBreakpoint(name) {
  if (!name) return;
  send("bp.setAddress", { location: name })
    .then((bp) => setStatus(`breakpoint ${bp.number} at ${name}${bp.address ? ` (${bp.address})` : ""}`))
    .catch(reportError);
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

// stepOver and stepInto choose their granularity from what is actually on
// screen.
//
// gdb's own next and step need a line table. Without one its step range is the
// whole function, so "step over" in a binary with no debug info runs to the
// function's exit — which makes the decompiled view unusable for the thing it
// is mostly for. When that view is showing and knows which line the pc is on,
// the step walks out of that line's addresses instead.
//
// Source keeps using gdb's own stepping. Where DWARF exists it is better than
// anything reconstructed: it knows about inlining, and about statements the
// decompiler merged.
function decompStepMap() {
  if (centerTab !== "decomp") return null;
  const map = decomp.stepMap();
  return map?.lines?.length ? map : null;
}

function stepOver() {
  const map = decompStepMap();
  if (map) exec("exec.stepLine", { ...map, over: true });
  else exec("exec.next");
}

function stepInto() {
  const map = decompStepMap();
  if (map) exec("exec.stepLine", { ...map, over: false });
  else exec("exec.step");
}

// showCenter switches the centre tab programmatically.
function showCenter(name) {
  document.querySelector(`.tab[data-center="${name}"]`)?.click();
}
ui.buttons.continue.addEventListener("click", () => exec("exec.continue"));
ui.buttons.pause.addEventListener("click", () => exec("exec.pause"));
ui.buttons.next.addEventListener("click", () => stepOver());
ui.buttons.step.addEventListener("click", () => stepInto());
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
  // Loading a program replaces the inferior, so a stray click on the wrong
  // row throws away a live session — the process, its stack, and wherever the
  // user had painstakingly got it to. Ask first, and only when there is
  // something to lose: with no program loaded, or after one has exited, the
  // click is unambiguous and a prompt would just be in the way.
  openElf(path);
  return true;
});

// openElf is the left-click action and the menu's "Load program", which must
// be the same thing including the guard: the risk is the same either way.
function openElf(path) {
  if (hasLiveInferior()) {
    askBeforeLoad(path);
    return;
  }
  loadExe(path);
}

// hasLiveInferior reports whether a process exists to be lost. "exited" does
// not count: the program is already gone, and only its corpse is on screen.
function hasLiveInferior() {
  const state = store.get("session.runState");
  return state === "stopped" || state === "running";
}

function loadExe(path) {
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
}

// askConfirm shows the inline confirmation bar. One bar, reused: two of these
// stacked would compete for the same space and the same attention.
//
// Focus lands on the cancelling button. Enter and Space are how a keyboard
// user dismisses something unexpected, and they should not detonate it.
let confirmAction = null;

function askConfirm(text, yesLabel, onYes) {
  ui.confirmText.textContent = text;
  ui.confirmYes.textContent = yesLabel;
  confirmAction = onYes;
  ui.confirm.classList.remove("is-hidden");
  ui.confirmNo.focus();
}

function hideConfirm() {
  ui.confirm.classList.add("is-hidden");
  confirmAction = null;
}

function askBeforeLoad(path) {
  const current = store.get("session.exePath");
  const running = store.get("session.runState") === "running";
  const what = current
    ? `${current} is still ${running ? "running" : "being debugged"}`
    : `a program is still ${running ? "running" : "being debugged"}`;
  askConfirm(`${what}. Loading ${path} will end that session.`,
    "load anyway", () => loadExe(path));
}

ui.confirmYes.addEventListener("click", () => {
  const act = confirmAction;
  hideConfirm();
  act?.();
});
ui.confirmNo.addEventListener("click", hideConfirm);
ui.confirm.addEventListener("keydown", (ev) => {
  if (ev.key === "Escape") {
    ev.preventDefault();
    hideConfirm();
  }
});

// --- context menu -----------------------------------------------------------

// A tiny menu, built per open rather than kept in the DOM: the items depend on
// what was right-clicked, and three stale hidden menus would be three things
// to keep in sync.
function showContextMenu(x, y, heading, items) {
  const frag = document.createDocumentFragment();
  if (heading) {
    const h = document.createElement("div");
    h.className = "ctxmenu-head";
    h.textContent = heading;
    frag.append(h);
  }
  for (const item of items) {
    const b = document.createElement("button");
    b.type = "button";
    b.className = "ctxmenu-item";
    b.setAttribute("role", "menuitem");
    b.textContent = item.label;
    if (item.title) b.title = item.title;
    b.addEventListener("click", () => {
      hideContextMenu();
      item.run();
    });
    frag.append(b);
  }
  ui.ctxmenu.replaceChildren(frag);
  ui.ctxmenu.classList.remove("is-hidden");

  // Measure after showing, then clamp: a menu opened near the bottom right
  // would otherwise hang off the window with its items unreachable.
  const box = ui.ctxmenu.getBoundingClientRect();
  ui.ctxmenu.style.left = `${Math.max(0, Math.min(x, window.innerWidth - box.width - 4))}px`;
  ui.ctxmenu.style.top = `${Math.max(0, Math.min(y, window.innerHeight - box.height - 4))}px`;
  ui.ctxmenu.querySelector(".ctxmenu-item")?.focus();
}

function hideContextMenu() {
  ui.ctxmenu.classList.add("is-hidden");
  ui.ctxmenu.replaceChildren();
}

// Anything that moves the menu away from what it points at should close it.
document.addEventListener("mousedown", (ev) => {
  if (!ui.ctxmenu.contains(ev.target)) hideContextMenu();
});
window.addEventListener("blur", hideContextMenu);
window.addEventListener("resize", hideContextMenu);
document.addEventListener("keydown", (ev) => {
  if (ev.key === "Escape") hideContextMenu();
});
ui.ctxmenu.addEventListener("keydown", (ev) => {
  const items = [...ui.ctxmenu.querySelectorAll(".ctxmenu-item")];
  const i = items.indexOf(document.activeElement);
  if (ev.key === "ArrowDown") {
    ev.preventDefault();
    items[(i + 1) % items.length]?.focus();
  } else if (ev.key === "ArrowUp") {
    ev.preventDefault();
    items[(i - 1 + items.length) % items.length]?.focus();
  }
});

// Right-clicking an ELF is the no-typing route to everything you can do with
// one. The labels say which action sets the architecture, because that is the
// distinction that decides whether a remote session works at all.
//
// The contextmenu event covers the keyboard menu key too, so this is not
// mouse-only.
ui.tree.addEventListener("contextmenu", (ev) => {
  const row = ev.target.closest(".tree-row");
  if (!row || row.dataset.kind !== "elf") return;
  ev.preventDefault();
  const path = row.dataset.path;
  showContextMenu(ev.clientX, ev.clientY, path, [
    {
      label: "Load program",
      title: "file — symbols and the program to run, and the only thing that sets the architecture",
      run: () => openElf(path),
    },
    {
      label: "Replace symbols",
      title: "symbol-file — symbols only, no program to run. Does not set the architecture.",
      run: () => loadSymbols(path, "replace", ""),
    },
    {
      label: "Add symbols…",
      title: "add-symbol-file with an offset, for an image that does not run where it was linked",
      run: () => {
        ui.symbolsLoadPath.value = path;
        ui.symbolsLoadMode.value = "add";
        showSymbolsLoad();
        ui.symbolsLoadOffset.focus();
      },
    },
  ]);
});

// Right-clicking a symbol is the no-typing route to a breakpoint on it, which
// in a stripped binary is otherwise a console command and a hand-copied
// address. The contextmenu event covers the keyboard menu key too.
ui.symbols.addEventListener("contextmenu", (ev) => {
  const row = ev.target.closest(".sym-row");
  if (!row) return;
  const sym = symbols.symbolAt(row);
  if (!sym) return;
  ev.preventDefault();

  const items = [];
  if (sym.kind === "function") {
    items.push({
      label: "Set breakpoint",
      title: "break by name — gdb skips the prologue, which is where you mean to stop",
      run: () => setFunctionBreakpoint(sym.name),
    });
  }
  items.push({
    label: "Go to",
    title: "source, disassembly or memory, depending on what the symbol knows about itself",
    run: () => jumpToSymbol(sym),
  });
  showContextMenu(ev.clientX, ev.clientY, sym.name, items);
});

// --- loading symbols --------------------------------------------------------

// Separate from loading a program, because they are separate acts that only
// coincide when gdb starts the program itself. Against an emulator stub or a
// process someone else started, the code is already in the target's memory and
// the only thing missing is what the addresses mean. Declaring an exec file
// there would leave Run offering to start a second, local copy.
//
// "add" with an offset is for an image that does not run where it was linked,
// which is the ordinary case for bare metal.
function showSymbolsLoad() {
  // Prefill from the loaded program when there is one: reloading its symbols
  // after connecting to a target is the common reason to open this.
  if (!ui.symbolsLoadPath.value) {
    ui.symbolsLoadPath.value = store.get("session.exePath") ?? "";
  }
  ui.symbolsLoad.classList.remove("is-hidden");
  syncSymbolsLoadMode();
  ui.symbolsLoadPath.focus();
  ui.symbolsLoadPath.select();
}

function hideSymbolsLoad() {
  ui.symbolsLoad.classList.add("is-hidden");
}

// The offset only means anything for "add" — symbol-file has nowhere to put
// one — so showing it under "replace" would be an invitation to a silent no-op.
function syncSymbolsLoadMode() {
  const isAdd = ui.symbolsLoadMode.value === "add";
  ui.symbolsLoadOffset.classList.toggle("is-hidden", !isAdd);
}

function loadSymbols(path, mode, offset) {
  ui.symbolsLoadGo.disabled = true;
  return send("symbols.load", { path, mode, offset })
    .then((out) => {
      hideSymbolsLoad();
      setStatus(out.available
        ? `${out.mode === "add" ? "added" : "loaded"} symbols from ${out.path} — ${out.available} symbols`
        : `${out.path} loaded, but it contains no symbols`);
      symbols.refresh();
    })
    .catch(reportError)
    .finally(() => { ui.symbolsLoadGo.disabled = false; });
}

function submitSymbolsLoad() {
  const path = ui.symbolsLoadPath.value.trim();
  if (!path) {
    setStatus("Give a path to an ELF file, relative to the project.", true);
    ui.symbolsLoadPath.focus();
    return;
  }
  const mode = ui.symbolsLoadMode.value;
  loadSymbols(path, mode, mode === "add" ? ui.symbolsLoadOffset.value.trim() : "");
}

ui.symbolsLoadOpen.addEventListener("click", () => {
  if (ui.symbolsLoad.classList.contains("is-hidden")) showSymbolsLoad();
  else hideSymbolsLoad();
});
ui.symbolsLoadMode.addEventListener("change", syncSymbolsLoadMode);
ui.symbolsLoadGo.addEventListener("click", submitSymbolsLoad);
ui.symbolsLoadCancel.addEventListener("click", hideSymbolsLoad);
ui.symbolsLoad.addEventListener("keydown", (ev) => {
  if (ev.key === "Enter") {
    ev.preventDefault();
    submitSymbolsLoad();
  } else if (ev.key === "Escape") {
    ev.preventDefault();
    hideSymbolsLoad();
  }
});

// --- remote targets ---------------------------------------------------------

// Connecting is `target remote <addr>` and disconnecting is `disconnect` —
// the same commands the user would type. The buttons are a shortcut for
// typing, not a separate mechanism, which is what keeps one source of truth
// for a connection a console command can also make or break. It also means
// the console below shows exactly what ran, including gdb's own error text
// when a stub refuses, which is far more use than a generic failure.
function applyRemote(remote) {
  const connected = Boolean(remote?.connected);
  ui.remoteState.dataset.remote = connected ? "on" : "off";
  ui.remoteState.textContent = connected
    ? `remote ${remote.address || "connected"}`
    : "no target";
  ui.remoteConnect.disabled = connected;
  ui.remoteDisconnect.disabled = !connected;
  if (connected && remote.address) ui.remoteAddr.value = remote.address;
}

function remoteBusy(text) {
  ui.remoteState.dataset.remote = "busy";
  ui.remoteState.textContent = text;
  ui.remoteConnect.disabled = true;
  ui.remoteDisconnect.disabled = true;
}

// runRemoteCommand sends one console command and lets the resulting
// remoteChanged event settle the indicator. Nothing here predicts the outcome:
// on failure the pill goes back to whatever the server still believes, which
// for a refused connection is "no target".
function runRemoteCommand(line, pending) {
  const before = store.get("session.remote");
  remoteBusy(pending);
  send("console.exec", { line })
    .catch(reportError)
    .finally(() => {
      // If the command changed anything, remoteChanged has already repainted
      // this and applyRemote below is a no-op on identical state.
      applyRemote(store.get("session.remote") ?? before);
    });
}

// Connecting before gdb knows the architecture is the one ordering mistake
// that matters, and it fails destructively rather than politely.
//
// `target remote` immediately asks the stub for its registers, and how to read
// that reply depends on the architecture. Get it wrong and gdb misparses
// everything — a MIPS64 target read as x86-64 reports a nonsense pc and can
// upset the far end badly enough to end the session.
//
// Only `file` establishes the architecture, by reading it out of the ELF
// header. Measured against gdb 17.1 with a MIPS64 image: `file` gives
// mips:octeon/big, while `symbol-file` and `add-symbol-file` both leave it at
// the host's i386/little. Loading symbols is therefore not enough, and it is
// easy to miss because the symbols pane looks as though it worked.
//
// exePath is the signal because it is set only by exe.load, which is the
// tree click, which is `file`. Someone who set the architecture by hand at the
// console will see this warning too; that is the wrong way round, but a
// needless prompt is cheaper than a wrecked target.
function connectRemote(addr) {
  runRemoteCommand(`target remote ${addr}`, "connecting…");
}

ui.remoteConnect.addEventListener("click", () => {
  const addr = ui.remoteAddr.value.trim();
  if (!addr) {
    setStatus("Enter an address such as localhost:1234.", true);
    ui.remoteAddr.focus();
    return;
  }
  if (!store.get("session.exePath")) {
    askConfirm(
      "No program is loaded, so gdb will assume this machine's architecture. " +
      "If the target is a different one, click its ELF in the file tree first — " +
      "loading symbols does not set the architecture, and connecting with the " +
      "wrong one can disrupt the target.",
      "connect anyway", () => connectRemote(addr));
    return;
  }
  connectRemote(addr);
});

ui.remoteDisconnect.addEventListener("click", () => {
  // disconnect, not detach: detach resumes the target, and someone who
  // connected to look at a stopped machine rarely wants it to run on.
  runRemoteCommand("disconnect", "disconnecting…");
});

ui.remoteAddr.addEventListener("keydown", (ev) => {
  if (ev.key === "Enter" && !ui.remoteConnect.disabled) {
    ev.preventDefault();
    ui.remoteConnect.click();
  }
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
    F10: () => stepOver(),
    F11: () => stepInto(),
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
  if (centerTab === "disasm") ui.sourceMeta.textContent = disasm.summary();
  else if (centerTab === "decomp") ui.sourceMeta.textContent = decomp.summary();
}

// refreshDecomp keeps the decompiled view in step with the program counter.
//
// Same three cases as the disassembly, cheapest first: hidden, so do nothing;
// the pc is already in the function on screen, so move the marker; otherwise
// fetch. The middle case matters more here than there — a decompiled function
// is one round trip to Ghidra, and stepping within one should not pay it.
function refreshDecomp(pc) {
  if (centerTab !== "decomp") return;
  if (pc && decomp.has(pc)) {
    const { line, ambiguous } = decomp.lineFor(pc);
    decomp.setPCLine(line, ambiguous);
    updateCenterMeta();
    return;
  }
  send("decomp.function", {
    thread: store.get("selection.thread"),
    frame: store.get("selection.frame"),
    stopSeq: store.get("session.stopSeq"),
  })
    .then((out) => {
      decomp.set(out);
      decomp.setBreakpoints(breakpoints.all());
      updateCenterMeta();
    })
    .catch((err) => {
      if (err?.code === "busy") return;
      // not_ready is the ordinary answer while the decompiler is starting or
      // absent, and the pane says so in place of the code rather than putting
      // it in the status bar where it would be missed.
      decomp.message(decompMessage(err), "src-empty");
      updateCenterMeta();
    });
}

// decompMessage turns the server's refusal into something actionable. "Not
// ready" alone leaves a user with no idea whether to wait, configure something
// or give up.
function decompMessage(err) {
  const msg = err?.message ?? String(err);
  if (/still starting/i.test(msg)) {
    return "Ghidra is starting. Importing and analysing a binary takes " +
      "seconds for a small one and minutes for firmware; this pane fills in " +
      "when it finishes.";
  }
  if (/no decompiler is configured/i.test(msg)) {
    return "No decompiler configured. Start gdb-wui with -ghidra pointing at " +
      "a Ghidra installation, or set GHIDRA_INSTALL_DIR.";
  }
  return msg;
}

// refreshDecompStatus reports what the decompiler is doing, and the two
// caveats that a user cannot otherwise discover: that it may hold a different
// build than gdb does, and that it may be starting.
function refreshDecompStatus() {
  return send("decomp.status", {})
    .then((st) => {
      store.set("session.decomp", st);
      if (st.mismatch) setStatus(st.mismatch, true);
      return st;
    })
    .catch(() => null);
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
// neither, so the answer is to say where it claims to live; the user can then
// add a substitution and try again.
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
    hover.hide();
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
    } else if (centerTab === "decomp") {
      const frame = stack.frameAt(store.get("selection.frame"));
      refreshDecompStatus();
      refreshDecomp(frame?.address);
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
