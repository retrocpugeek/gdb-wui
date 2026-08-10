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
import { createCentre } from "./core/centre.js";
import { cancelEdit, editCell } from "./core/edit.js";
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
  sourcePathB: el("source-path-b"),
  sourceMetaB: el("source-meta-b"),
  centre: el("center"),
  splitBtn: el("btn-split"),
  splitOrientBtn: el("btn-split-orient"),
  stack: el("stack"),
  breakpoints: el("breakpoints"),
  variables: el("variables"),
  registers: el("registers"),
  threads: el("threads"),
  disasm: el("disasm"),
  decomp: el("decomp"),
  memory: el("memory"),
  goto: el("goto"),
  ctxmenu: el("ctxmenu"),
  hovertip: el("hovertip"),
  about: el("about"),
  aboutOpen: el("btn-about"),
  aboutClose: el("about-close"),
  aboutVersion: el("about-version"),
  aboutGdb: el("about-gdb"),
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
    // server is the gdb-wui build, which only the About box shows. It comes
    // from the snapshot rather than being baked into the page, so a browser
    // left open across a server upgrade reports the server it is talking to.
    server: "",
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

// Double-clicking a value edits it. Every write carries the stop it was read
// at, so a value typed against numbers that have since been superseded is
// refused rather than landing in whatever frame is current now.
const variables = createVariables({
  element: ui.variables,
  onExpand: (req) => send("vars.expand", req),
  onRemoveWatch: (path) => send("watch.remove", { path }).catch(reportError),
  onAssign: (req) => send("vars.assign", {
    ...req,
    thread: store.get("selection.thread"),
    frame: store.get("selection.frame"),
    stopSeq: store.get("session.stopSeq"),
  }),
  onError: reportError,
});

const registers = createRegisters({
  element: ui.registers,
  onFetch: (req) => send("regs.values", req),
  onWrite: (req) => send("regs.write", {
    ...req,
    thread: store.get("selection.thread"),
    stopSeq: store.get("session.stopSeq"),
  }),
  onError: reportError,
});

// Where the source view's header goes while it is off screen. Detached, so a
// file loading in the background writes somewhere that is never displayed.
const offscreenPath = document.createElement("span");
const offscreenMeta = document.createElement("span");

// The centre area's two slots. A view is fetched only while it is on screen —
// most stops are a source-level step and nobody is looking at machine code —
// so isVisible gates the refreshes, and focused() answers the questions that
// need exactly one view, such as what F10 steps by.
const centre = createCentre({
  element: ui.centre,
  onChange: onCentreChange,
});

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
  onWrite: (req) => send("mem.write", {
    ...req,
    stopSeq: store.get("session.stopSeq"),
  }),
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
    // The decompiled view has a second menu even where there is no value: its
    // *names* are editable, and the ones most worth changing — a decompiler
    // temporary, a function nobody can evaluate — are exactly the ones with
    // nothing to show a value for.
    const editable = pane === ui.decomp ? decompEditTarget(ev) : null;
    if (!hit && !editable) return;
    ev.preventDefault();
    hover.hide();
    const x = ev.clientX;
    const y = ev.clientY;

    // The value decides half the menu, so it is fetched before the menu opens
    // rather than the menu offering something that cannot work. One round
    // trip, and the hover has usually just made the same one.
    evaluateForMenu(hit?.expr).then((res) => {
      const items = [];
      const label = hit?.name ?? hit?.expr ?? editable?.label ?? "";

      if (hit) {
        // Where it is kept. A register is not in memory and has no answer here.
        if (hit.storage !== "register") {
          items.push({
            label: "Show where it is stored",
            title: `the bytes at &(${hit.expr})`,
            run: () => showAddressOf(hit.expr, label),
          });
        }
        // What it points at. Offered for anything whose value is
        // address-shaped, which is how a register holding a pointer becomes
        // followable.
        if (res && res.addr >= LOWEST_PLAUSIBLE_ADDRESS) {
          const target = "0x" + res.addr.toString(16);
          items.push({
            label: `Show what it points to — ${target}`,
            title: "follow the value as an address",
            run: () => showAtAddress(res.addr, `*${label}`),
          });
        }
      }
      items.push(...decompEditItems(editable));

      if (!items.length) {
        setStatus(res && res.value
          ? `${label} is ${res.value}, which is neither in memory nor an address.`
          : `${label} has no address to show.`);
        return;
      }
      showContextMenu(x, y, label || editable?.fn?.name || "", items);
    });
  });
}

// decompEditTarget works out what a right-click in the decompiled view is
// about: the symbol under the pointer if there is one, and the function on
// screen either way.
//
// Both, rather than one or the other, because a right-click on a name inside
// FUN_0010d2b0 is a plausible way to reach for renaming the function too.
function decompEditTarget(ev) {
  const fn = decomp.shown();
  if (!fn) return null;
  const sym = decomp.symbolAt(ev);
  return {
    fn,
    sym,
    // The line, for commenting it. Null on a line that came from no address —
    // a brace, a declaration — which has nothing to hang a comment on.
    line: decomp.commentTarget(ev),
    label: sym?.name ?? fn.name,
    // Where to put the editor for an edit that is about the function rather
    // than about a word: under the pointer, which is where the user was
    // looking. The function's name is on a line that may be scrolled away.
    at: { left: ev.clientX, top: ev.clientY, width: 260, height: 22 },
  };
}

// decompEditItems offers to correct what the decompiler guessed.
//
// The menu is shown even when the project may not be written to, with the
// reason in the item's title. An item that is simply absent teaches nobody
// that the feature exists, and "why can't I rename this" is a question the UI
// should answer where it is asked.
function decompEditItems(target) {
  if (!target) return [];
  const st = store.get("session.decomp");
  const why = st?.editable ? "" :
    "this Ghidra project is yours; gdb-wui only edits the one it imported itself";
  const items = [];
  const { fn, sym } = target;

  if (sym) {
    const kind = sym.storage === "global" ? "global" : "variable";
    items.push({
      label: `Rename ${sym.name}…`,
      title: why || `give ${sym.name} a name of your own, in the decompiler`
        + sourceNote(sym.source),
      disabled: Boolean(why),
      run: () => renameDecompSymbol(sym, fn, kind),
    });
    if (kind === "variable") {
      items.push({
        label: `Set the type of ${sym.name}…`,
        title: why || `currently ${sym.type || "unknown"}`,
        disabled: Boolean(why),
        run: () => retypeDecompSymbol(sym, fn),
      });
    }
  }
  items.push({
    label: `Rename the function ${fn.name}…`,
    title: why || "the name shown here, in the call stack and in the symbol list"
      + sourceNote(fn.source),
    disabled: Boolean(why),
    run: () => renameDecompFunction(fn, target.at),
  });
  items.push({
    label: `Edit the prototype of ${fn.name}…`,
    title: why || fn.signature || "return type, parameters and name at once",
    disabled: Boolean(why),
    run: () => retypeDecompFunction(fn, target.at),
  });

  // Comments last, because they are the note-taking rather than the correcting,
  // and because the two that matter — this line, this function — are the ones a
  // reader reaches for after they have understood something.
  const line = target.line;
  if (line) {
    items.push({
      label: line.text ? "Edit the comment on this line…" : "Comment this line…",
      title: why || line.text || "a note above this line, kept in the decompiler",
      disabled: Boolean(why),
      run: () => commentDecompLine(fn, line),
    });
    if (line.text) {
      items.push({
        label: "Remove the comment on this line",
        title: why || line.text,
        disabled: Boolean(why),
        // Not "empty the box": the editor treats an empty entry as a cancel, so
        // that a value is never written because a key was leant on. Removing is
        // therefore its own item rather than a thing you can do by deleting.
        run: () => sendDecompEdit("decomp.comment", {
          kind: "line", function: fn.entry, address: line.addr, name: fn.name, value: "",
        }).catch(reportError),
      });
    }
  }
  const onFunction = decomp.functionComment();
  items.push({
    label: onFunction ? `Edit the comment on ${fn.name}…` : `Comment the function ${fn.name}…`,
    title: why || onFunction || "a note above the function, kept in the decompiler",
    disabled: Boolean(why),
    run: () => commentDecompFunction(fn, onFunction, target.at),
  });
  if (onFunction) {
    items.push({
      label: `Remove the comment on ${fn.name}`,
      title: why || onFunction,
      disabled: Boolean(why),
      run: () => sendDecompEdit("decomp.comment", {
        kind: "function", function: fn.entry, name: fn.name, value: "",
      }).catch(reportError),
    });
  }

  // Undoing a whole run, offered only when there is one worth undoing. An agent
  // writes forty annotations in a burst, and forty presses of Ctrl+Shift+Z is
  // not an undo.
  const run = st?.undo;
  if (run && run.count > 1) {
    items.push({
      label: run.author === "agent"
        ? `Undo the agent's last ${run.count} edits`
        : `Undo the last ${run.count} edits`,
      title: "reverses them one at a time, newest first, in the decompiler",
      disabled: Boolean(why),
      run: () => send("decomp.undo", {
        run: run.id, stopSeq: store.get("session.stopSeq"),
      }).then(applyDecompEdit).catch(reportError),
    });
  }
  return items;
}

// sourceNote says where a name came from, when that is worth saying. "Inferred"
// covers an agent and Ghidra's own analysers alike, because the decompiler does
// not distinguish them and this must not claim to.
function sourceNote(source) {
  if (source === "inferred") return " — the current name was inferred, not typed by you";
  if (source === "symbol") return " — the current name came from the binary's symbol table";
  return "";
}

// Renaming a recovered frame, from the call stack.
//
// This is where FUN_00401154 is most often first seen — a stripped binary's
// stack is a column of them — and it is the one place where the name and the
// question "what is this function?" appear together.
ui.stack.addEventListener("contextmenu", (ev) => {
  const row = ev.target.closest(".list-row");
  if (!row) return;
  const found = stack.recoveredAt(Number(row.dataset.level));
  // Only a recovered name. A frame gdb named came from a symbol table, and
  // offering to rename it would be offering something that cannot work.
  if (!found?.entry) return;
  ev.preventDefault();
  hover.hide();

  const st = store.get("session.decomp");
  const why = st?.editable ? "" :
    "this Ghidra project is yours; gdb-wui only edits the one it imported itself";
  showContextMenu(ev.clientX, ev.clientY, found.name, [{
    label: `Rename ${found.name}…`,
    title: why || found.signature || "the decompiler's name for this function",
    disabled: Boolean(why),
    run: () => editDecomp({
      type: "decomp.rename",
      cell: row.querySelector(".list-main"),
      value: found.name,
      title: `a new name for ${found.name}`,
      payload: { kind: "function", function: found.entry, name: found.name },
    }),
  }]);
});

// The four edits. Each opens the same in-place editor the value panels use and
// sends one request; the server answers with the whole function decompiled
// again, which is what gets painted.

function renameDecompSymbol(sym, fn, kind) {
  editDecomp({
    type: "decomp.rename",
    over: sym.rect,
    value: sym.name,
    title: `a new name for ${sym.name}`,
    payload: {
      kind,
      function: fn.entry,
      symbol: sym.id,
      name: sym.name,
      address: sym.addr,
    },
  });
}

function retypeDecompSymbol(sym, fn) {
  editDecomp({
    type: "decomp.retype",
    over: sym.rect,
    // The current type, so correcting `undefined8` to `long` is an edit rather
    // than a retype from memory.
    value: sym.type,
    title: `a C type for ${sym.name}`,
    payload: {
      kind: "variable",
      function: fn.entry,
      symbol: sym.id,
      name: sym.name,
    },
  });
}

function renameDecompFunction(fn, at) {
  editDecomp({
    type: "decomp.rename",
    over: at,
    value: fn.name,
    title: `a new name for ${fn.name}`,
    payload: { kind: "function", function: fn.entry, name: fn.name },
  });
}

function retypeDecompFunction(fn, at) {
  editDecomp({
    type: "decomp.retype",
    over: at,
    // Ghidra's own rendering of the prototype, which is both the thing being
    // corrected and a demonstration of the syntax expected.
    value: fn.signature,
    title: "a C prototype: return type, name and parameters",
    payload: { kind: "function", function: fn.entry, name: fn.name },
  });
}

// Commenting. Not a correction of what the decompiler guessed but a note about
// what it means, which is the other half of reading a binary — and the half
// that has nowhere else to go: there is no source file to write it in.

function commentDecompLine(fn, line) {
  editDecomp({
    type: "decomp.comment",
    over: line.rect,
    value: line.text,
    title: "a note above this line — what it does, or what you have worked out",
    payload: { kind: "line", function: fn.entry, address: line.addr, name: fn.name },
  });
}

function commentDecompFunction(fn, current, at) {
  editDecomp({
    type: "decomp.comment",
    over: at,
    value: current,
    title: `a note above ${fn.name}`,
    payload: { kind: "function", function: fn.entry, name: fn.name },
  });
}

function editDecomp({ type, cell = ui.decomp, over, value, title, payload }) {
  editCell({
    cell,
    over,
    value,
    title,
    commit: (typed) => sendDecompEdit(type, { ...payload, value: typed }),
    onError: (err) => setStatus(err.message ?? String(err), true),
  });
}

// sendDecompEdit is the one request every decompiler edit makes. The rejection
// is left to the caller: an editor keeps the box open with the text in it,
// while a menu item has nothing to keep and only reports.
function sendDecompEdit(type, payload) {
  return send(type, { ...payload, stopSeq: store.get("session.stopSeq") })
    .then(applyDecompEdit);
}

// applyDecompEdit repaints from the server's answer.
//
// Everything here is a refetch rather than a patch, and it has to be: the reply
// renumbers the symbol ids the pane is holding, and a new prototype changes how
// callers decompile, so the only safe assumption is that anything showing a
// decompiler name is now out of date.
function applyDecompEdit(out) {
  // The reply already says what one undo would reverse, so the tab that made
  // the edit needs no round trip to know.
  const status = store.get("session.decomp");
  if (status) store.set("session.decomp", { ...status, undo: out?.run ?? null });
  // Only if the pane is showing the function that was edited. Renaming a frame
  // from the call stack edits a function the pane may not be on, and painting
  // it there would navigate somebody away from what they were reading.
  const shown = decomp.shown();
  if (out?.function && (!shown || shown.entry === out.function.entry)) {
    decomp.set(out.function);
    updateCenterMeta();
  }
  // The call stack shows these names, and so does the symbol list.
  nameUnknownFrames();
  symbols.refresh();
  if (out?.warning) {
    setStatus(`${out.did} — ${out.warning}`, true);
  } else if (out?.did) {
    // The undo hint rides on the message rather than living in a menu,
    // because the moment someone wants it is the moment they have just
    // renamed the wrong thing.
    setStatus(out.canUndo ? `${out.did} — Ctrl+Shift+Z undoes it` : out.did);
  }
  return out;
}

// The journal is the server's, not this tab's: two windows on one session
// share it, and a page that reloaded has no idea what is on it. So this asks
// and lets the answer — including "nothing to undo" — come back from there.
function undoDecompEdit() {
  send("decomp.undo", { stopSeq: store.get("session.stopSeq") })
    .then(applyDecompEdit)
    .catch((err) => setStatus(err.message ?? String(err), true));
}

// LOWEST_PLAUSIBLE_ADDRESS keeps "show what it points to" off values that are
// plainly not pointers. The first page is never mapped on any system this runs
// on, so a value below it is a small integer wearing a hex hat — offering to
// follow `3` would be noise on every int in the program.
const LOWEST_PLAUSIBLE_ADDRESS = 0x1000;

function evaluateForMenu(expr) {
  // Nothing to evaluate: the pointer was over a name the decompiler owns and
  // gdb has never heard of, which is a menu about editing rather than about
  // values.
  if (!expr) return Promise.resolve(null);
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
      updateCenterMeta();
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
      updateCenterMeta();
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
      cancelEdit();
      source.clearExecLine();
      stack.clear();
      variables.clear();
      break;
    case "exited":
      execBusy = false;
      // The session the prompt was guarding has ended on its own.
      hideConfirm();
      hover.hide();
      cancelEdit();
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
    case "valueWritten":
      applyValueWritten(msg.payload);
      break;
    case "symbolsInvalidated":
      symbols.refresh();
      break;
    case "decompEdited":
      // Another tab — or this one — changed a name. Everything showing one is
      // now stale, including the function on screen, because a new prototype
      // changes how its callers decompile too.
      nameUnknownFrames();
      symbols.refresh();
      // What one undo would now reverse. Cheap next to the decompile below, and
      // it is how a tab that did not make the edit learns that an agent has
      // just written twenty annotations it could offer to take back.
      refreshDecompStatus();
      // The function this pane is showing, not the one the program counter is
      // in: someone renaming a symbol has usually navigated somewhere on
      // purpose, and refetching around the pc would take them back. In the tab
      // that made the edit this repaints what the reply already painted, which
      // is one decompile of waste per edit and the price of the other tabs
      // being right.
      {
        const shown = decomp.shown();
        // Quietly: the pane is already showing this function, and an error
        // about a refresh nobody asked for is worse than leaving it be.
        if (shown && centre.isVisible("decomp")) fetchDecomp(shown.entry).catch(() => {});
      }
      break;
    case "decompLog":
      log.decomp(msg.payload?.text ?? "", msg.payload?.level, msg.payload?.millis);
      break;
    case "decompChanged":
      // The stack was drawn before the decompiler had anything to say. Ask
      // again now it has: this is the moment a column of "?? ()" can stop
      // being one, and nothing else would trigger it until the next stop.
      nameUnknownFrames();
      refreshDecompStatus().then((st) => {
        if (st?.state === "ready" && centre.isVisible("decomp")) {
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
    "session.server": hello.server ?? "",
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
  nameUnknownFrames();
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
  // The cell an editor is sitting over now holds a value from a different
  // stop. Committing what was typed against the old one would write it
  // somewhere the user was not looking.
  cancelEdit();
  store.patch({
    "session.runState": stopped.runState,
    "session.stopSeq": stopped.stopSeq,
    "session.lastStopReason": describeStop(stopped),
    "selection.thread": stopped.threadId,
    "selection.frame": 0,
  });

  stack.set(stopped.frames ?? [], 0);
  nameUnknownFrames();
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
      if (centre.focused() === "source") {
        setStatus(`Stopped at ${frame.address}${where} with no source — showing disassembly.`);
        showCenter("disasm");
      } else {
        setStatus(`Stopped at ${frame.address}${where}`);
      }
    }
  }
}

// applyValueWritten reacts to a write — this browser's or another one's.
//
// It refreshes everything rather than the panel that was edited, because a
// write does not stay where it was made: assigning a local changes the bytes
// the hex view is showing, and writing a byte changes a variable in the tree.
// There is no subset that is safe to leave alone, and a write is a deliberate
// act a person just performed, so the cost of re-reading is invisible.
function applyValueWritten(payload) {
  if (!payload) return;
  setStatus(payload.value
    ? `${payload.detail} = ${payload.value}`
    : `wrote ${payload.detail}`);

  memory.invalidate();
  registers.refetch();

  const stopSeq = store.get("session.stopSeq");
  send("vars.locals", {
    thread: store.get("selection.thread"),
    frame: store.get("selection.frame"),
    stopSeq,
  })
    .then((out) => {
      variables.setLocals(out.variables ?? [], out.stopSeq);
      // The locals list is only the top level. An expanded struct holds its
      // own values, and a write into one of them — through the hex view, or
      // through a pointer — would otherwise show the old number.
      return variables.refreshOpen();
    })
    .catch(() => {
      // The program has moved on since the write — a stop event is on its way
      // and will repaint everything anyway.
    });
  send("watch.list", {})
    .then((out) => variables.setWatches(out.watches ?? [], out.stopSeq))
    .catch(() => {});
}

// nameUnknownFrames fills in the frames gdb has no symbol for.
//
// gdb reports "??" for every frame of a stripped binary, and the decompiler
// knows what is there. Asked rather than pushed, for the reason mem.symbols is:
// it is a handful of addresses per stop, and asking on the stop path would put
// a Ghidra round trip in front of the stack appearing at all.
//
// The decompiler's state is not checked first. The server answers an empty
// list when it has nothing, and asking is also what starts it — so a user who
// configured -ghidra and never opened the Decompiled tab still gets a named
// stack.
function nameUnknownFrames() {
  const addresses = stack.unnamed();
  if (!addresses.length) return;
  send("decomp.names", { addresses, stopSeq: store.get("session.stopSeq") })
    .then((out) => stack.setNames(out.names ?? []))
    .catch(() => {
      // A stack that stays as gdb reported it is the status quo, not a
      // failure worth a message on every stop.
    });
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
  if (sel.frames?.length) {
    stack.set(sel.frames, sel.frame);
    nameUnknownFrames();
  }
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

// The go-to box.
//
// One box for four views, acting on the focused one. What a place *is* differs
// per view — the source view wants a file and a line, the disassembly an
// address — so the server resolves the target once and each view is given the
// part of the answer it needs.
//
// Any gdb expression is accepted, because that is what a user has in their
// head: "&cfg", "$sp", "buf+16", as well as "walk" and "globals.c:65".
ui.goto.addEventListener("keydown", (ev) => {
  if (ev.key !== "Enter") return;
  ev.preventDefault();
  gotoTarget(ui.goto.value);
});

function gotoTarget(raw) {
  const target = raw.trim();
  if (!target) return;
  const view = centre.focused();

  // ":65" is a line in the file already on screen, and a plain FILE:LINE with
  // the source view focused is a file and a line and nothing else. Both are
  // answered here rather than by the server: they need no address, so they
  // work before a program has been loaded at all.
  const here = /^:(\d+)$/.exec(target);
  if (here && view !== "source") {
    // A bare line number is a line in the file on screen, and only one view
    // has one. gdb would answer this with "No symbol \":65\"", which describes
    // the wrong problem.
    setStatus(`:${here[1]} is a line in a file, so it needs the source view.`, true);
    return;
  }
  if (view === "source" && here) {
    if (!source.path) {
      setStatus(`No file is open, so there is no line ${here[1]} to go to.`, true);
      return;
    }
    source.open(source.path, { line: Number(here[1]) })
      .then(() => setStatus(`${source.path}:${here[1]}`))
      .catch(reportError);
    return;
  }
  const fileLine = /^(.*[^:]):(\d+)$/.exec(target);
  if (view === "source" && fileLine) {
    source.open(fileLine[1], { line: Number(fileLine[2]) })
      .then(() => setStatus(`${fileLine[1]}:${fileLine[2]}`))
      .catch(() => setStatus(`${fileLine[1]} is not a file in this project.`, true));
    return;
  }

  send("goto.locate", {
    target,
    thread: store.get("selection.thread"),
    frame: store.get("selection.frame"),
    stopSeq: store.get("session.stopSeq"),
  })
    .then((loc) => goToLocation(view, loc))
    .catch((err) => setStatus(err?.message ?? String(err), true));
}

// goToLocation sends a resolved place to the view that asked for it.
//
// The focused view is honoured rather than second-guessed: a target it cannot
// show says so and changes nothing, because silently switching views would
// take the reader away from what they were reading. What was typed stays in
// the box, so focusing another view and pressing Enter again is the fix.
function goToLocation(view, loc) {
  switch (view) {
    case "source": {
      if (loc.source?.available) {
        source.open(loc.source.path, { line: loc.source.line })
          .then(() => setStatus(describeLocation(loc)))
          .catch(reportError);
        return;
      }
      // A path gdb knows but this machine does not: offer the local files that
      // share its basename, which is the same bar a stop with unresolved
      // source puts up.
      if (loc.source?.candidates?.length) {
        showLocate(loc.source);
        return;
      }
      setStatus(loc.address
        ? `${loc.target} has no source line. The disassembly and the memory `
          + `view can both show ${loc.address}.`
        : `${loc.target} has no source line.`, true);
      return;
    }
    case "disasm":
      if (!hasCode(loc)) return;
      // The pin holds what to re-fetch if the tab switch triggers one. An
      // address is safe here where one from the symbol pane would not be: this
      // one came from gdb a moment ago and is therefore the *runtime* address,
      // and the pin is dropped at the next stop anyway.
      disasmPin = gotoSpec(loc);
      disasmPinExplicit = true;
      fetchPinned().then((ok) => {
        if (ok) setStatus(describeLocation(loc));
      });
      return;
    case "decomp":
      if (!hasCode(loc)) return;
      showDecompAt(loc);
      return;
    case "memory":
      showMemoryAt(loc);
      return;
  }
}

// gotoSpec is what to hand a view that wants an address.
//
// The resolved address where there is one, because the views that are not the
// source view cannot take "globals.c:65" and gdb has already done the work.
function gotoSpec(loc) {
  return loc.address || loc.target;
}

// hasCode reports whether there is anything for an instruction-level view to
// show, and says so when there is not. A line that generated no code is a real
// place with no address, and the alternative is passing "globals.c:65" to gdb
// and surfacing its parse error, which describes the wrong problem.
function hasCode(loc) {
  if (loc.address) return true;
  setStatus(`${loc.target} generated no code, so there are no instructions `
    + "to show. The source view can go there.", true);
  return false;
}

// GOTO_HINT is the placeholder per focused view. Each says what that view
// will do, because "0x401136" means a line to one of them and a byte to
// another.
const GOTO_HINT = {
  source: "go to  walk · file.c:65",
  disasm: "go to  walk · 0x401136",
  decomp: "go to  walk · 0x401136",
  memory: "go to  &head · 0x404040",
};

function describeLocation(loc) {
  const parts = [loc.target];
  const where = [];
  if (loc.func && loc.func !== loc.target) where.push(loc.func);
  if (loc.source?.path) where.push(`${loc.source.path}:${loc.source.line}`);
  if (loc.address) where.push(loc.address);
  if (where.length) parts.push(where.join(" · "));
  return parts.join(" — ");
}

function showDecompAt(loc) {
  fetchDecomp(gotoSpec(loc))
    .then(() => setStatus(describeLocation(loc)))
    .catch((err) => {
      // Loud, unlike the background refresh that shares this fetch: someone
      // typed a target and is owed an answer about it.
      decomp.message(decompMessage(err), "src-empty");
      updateCenterMeta();
      setStatus(err?.message ?? String(err), true);
    });
}

function showMemoryAt(loc) {
  send("mem.read", {
    address: gotoSpec(loc),
    count: 16,
    stopSeq: store.get("session.stopSeq"),
  })
    .then((res) => {
      memory.show(res.addr, { expr: loc.target, seq: store.get("session.stopSeq") });
      updateCenterMeta();
      setStatus(res.unreadable
        ? `${loc.target} → 0x${res.addr.toString(16)} is not readable`
        : describeLocation(loc));
    })
    .catch((err) => setStatus(err?.message ?? String(err), true));
}

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
  if (centre.focused() !== "decomp") return null;
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

// showCenter brings a view up programmatically.
//
// A view already on screen is focused where it is rather than moved into the
// focused slot, so following a pointer into the disassembly while the
// decompiled C is beside it does not throw the decompiled C away.
function showCenter(name) {
  centre.show(name);
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
    // A disabled item stays on the menu with the reason in its title. Removing
    // it would leave no way to discover that the thing is possible at all.
    b.disabled = Boolean(item.disabled);
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
  ui.ctxmenu.querySelector(".ctxmenu-item:not([disabled])")?.focus();
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
    F7: () => centre.toggleSplit(),
    "Shift+F7": () => centre.toggleOrientation(),
    // Ctrl+Shift, because that and the function keys are the only chords the
    // keymap takes out of a terminal, and the console is where the focus
    // often is when you decide to go somewhere.
    "Ctrl+Shift+G": () => {
      ui.goto.focus();
      ui.goto.select();
    },
    // Undoing a rename in the decompiler, not in gdb: gdb has no undo and
    // nothing else here does either, so the chord is unambiguous.
    "Ctrl+Shift+Z": () => undoDecompEdit(),
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
  if (!centre.isVisible("disasm")) return;
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

// updateCenterMeta labels each slot with what its own view is showing. Split,
// there are two headers and one shared line cannot say which is which.
function updateCenterMeta() {
  for (const name of centre.visible()) {
    const slot = centre.slotOf(name);
    const path = slot === "b" ? ui.sourcePathB : ui.sourcePath;
    const meta = slot === "b" ? ui.sourceMetaB : ui.sourceMeta;
    if (name === "source") {
      // The source view writes its own header as files load, so it is given
      // the elements rather than asked for the text.
      source.setLabels(path, meta);
      continue;
    }
    // The source path means nothing in the other views, and "No file open"
    // beside a screenful of instructions reads as a broken panel.
    path.textContent = "";
    meta.textContent =
      name === "disasm" ? disasm.summary()
        : name === "decomp" ? decomp.summary()
          : name === "memory" ? memory.summary()
            : "";
  }
}

// fetchDecomp puts one particular function in the pane, named by an address or
// a symbol. It resolves when the pane has it and rejects with the server's
// error, leaving the caller to decide how loud to be about either.
//
// Not refreshDecomp, for two reasons. That one follows the *selected frame*,
// ignoring the address it is given except to decide whether to fetch at all —
// right when the program moved, wrong when it did not. And it skips the fetch
// entirely when the address is already on screen, which is exactly wrong after
// an edit, where the text is the thing that has to be read again.
function fetchDecomp(target) {
  return send("decomp.function", {
    target,
    thread: store.get("selection.thread"),
    frame: store.get("selection.frame"),
    stopSeq: store.get("session.stopSeq"),
  })
    .then((out) => {
      decomp.set(out);
      decomp.setBreakpoints(breakpoints.all());
      updateCenterMeta();
      return out;
    });
}

// refreshDecomp keeps the decompiled view in step with the program counter.
//
// Same three cases as the disassembly, cheapest first: hidden, so do nothing;
// the pc is already in the function on screen, so move the marker; otherwise
// fetch. The middle case matters more here than there — a decompiled function
// is one round trip to Ghidra, and stepping within one should not pay it.
function refreshDecomp(pc) {
  if (!centre.isVisible("decomp")) return;
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
        updateCenterMeta();
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
    const alreadyShowing = centre.isVisible("disasm");
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
    // The same rule localNodes applies on the server, for the same reason:
    // --simple-values prints a value for exactly the types gdb will let you
    // assign to, which is also what expandable is derived from.
    editable: !v.expandable && !v.optimizedOut,
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
//
// A tab acts on the focused slot. Which slot that is comes from where you last
// clicked, so with two views up the tabs replace the one you were looking at.
for (const tab of document.querySelectorAll(".tab[data-center]")) {
  tab.addEventListener("click", () => centre.assign(centre.focusedSlot(), tab.dataset.center));
}

ui.splitBtn.addEventListener("click", () => centre.toggleSplit());
ui.splitOrientBtn.addEventListener("click", () => centre.toggleOrientation());

// onCentreChange runs whenever the slots or the focus change, and is the one
// place that reacts to it: the tab markers, the address box, and a fetch for
// any view that has just come on screen.
function onCentreChange({ visible, split }) {
  hover.hide();

  const focusedName = centre.focused();
  for (const tab of document.querySelectorAll(".tab[data-center]")) {
    const name = tab.dataset.center;
    tab.classList.toggle("is-active", name === focusedName);
    // The other slot's view is marked too, more faintly. Without it the tabs
    // claim only one view is showing while two are.
    tab.classList.toggle("is-secondary", visible.includes(name) && name !== focusedName);
  }

  ui.splitBtn.setAttribute("aria-pressed", String(split !== "off"));
  ui.splitBtn.title = split === "off"
    ? "Show two views side by side (F7)"
    : "Show one view (F7)";
  ui.splitOrientBtn.classList.toggle("is-hidden", split === "off");
  ui.splitOrientBtn.textContent = split === "y" ? "⫿" : "⊟";
  ui.splitOrientBtn.title = split === "y"
    ? "Put the two views side by side (Shift+F7)"
    : "Stack the two views (Shift+F7)";

  // Only the splitter for the current orientation is in the grid.
  el("split-center-x").classList.toggle("is-hidden", split !== "x");
  el("split-center-y").classList.toggle("is-hidden", split !== "y");

  // The placeholder names what the focused view will do with what you type.
  // Same box, four destinations, and no way to tell them apart otherwise.
  ui.goto.placeholder = GOTO_HINT[focusedName] ?? GOTO_HINT.source;
  if (!centre.isVisible("source")) {
    // Somewhere harmless to write while the source view is off screen.
    // Without it, a file loading in the background would put its name in a
    // header belonging to the disassembly.
    source.setLabels(offscreenPath, offscreenMeta);
  }

  const frame = stack.frameAt(store.get("selection.frame"));
  if (visible.includes("disasm")) refreshDisasm(frame?.address);
  if (visible.includes("decomp")) {
    refreshDecompStatus();
    refreshDecomp(frame?.address);
  }
  updateCenterMeta();
  // The memory view is the only one that shows nothing at all until it is
  // told where to look, so it is the only one worth taking focus for.
  if (focusedName === "memory") ui.goto.focus();
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

// A watch is typed into the same in-place editor as every other value in the
// window, rather than into window.prompt. The dialog was the odd one out in
// more than looks: it takes the keyboard away from the page, so nothing else
// answers while it is up, and a rejected expression closed it and lost what
// had been typed. Here a bad expression keeps the box open with the text in
// it, which is where it can be corrected.
el("btn-add-watch").addEventListener("click", () => {
  const button = el("btn-add-watch");
  const box = button.getBoundingClientRect();
  // Across the panel head rather than over the button. `head.next->value` is a
  // normal watch and the button is a few characters wide; the tabs it covers
  // are not wanted mid-expression anyway.
  const head = button.parentElement.getBoundingClientRect();
  editCell({
    cell: button,
    over: { left: head.left + 3, top: box.top, width: head.width - 6, height: box.height },
    title: "an expression to watch, Enter to add",
    commit: (expr) => send("watch.add", { expr })
      .then((out) => variables.setWatches(out.watches, out.stopSeq)),
    onError: reportError,
  });
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
// Now that the panels exist, apply the restored split and let onCentreChange
// fetch whatever came back on screen with it.
centre.sync();

// About. The version and the gdb it is driving are filled in when the box is
// opened rather than kept in sync, because it is the only thing that shows
// them and it is shut almost all of the time.
function showAbout() {
  ui.aboutVersion.textContent = store.get("session.server") || "dev";
  ui.aboutGdb.textContent = store.get("session.gdbVersion") || "not started";
  ui.about.classList.remove("is-hidden");
  ui.aboutClose.focus();
}

function hideAbout() {
  ui.about.classList.add("is-hidden");
}

ui.aboutOpen.addEventListener("click", showAbout);
ui.aboutClose.addEventListener("click", hideAbout);
// The backdrop, but not the box itself, so a click that misses a link does not
// shut what it was aiming at.
ui.about.addEventListener("click", (ev) => {
  if (ev.target === ui.about) hideAbout();
});
ui.about.addEventListener("keydown", (ev) => {
  if (ev.key === "Escape") hideAbout();
});

initTheme({ toggle: el("btn-theme") });
document.addEventListener("gdb-wui:theme", () => {
  // The terminals were built with the old palette, so they have to be told.
  gdbConsole.retheme();
  inferiorTerm.retheme();
});

conn.connect();
tree.start();
