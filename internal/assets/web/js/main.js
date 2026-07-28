// Wiring. Everything here is deliberately dull: panels own their DOM, the
// store owns state, and this file only introduces them to each other.

import { createStore } from "./core/store.js";
import { createConnection } from "./core/ws.js";
import { createTree } from "./panels/tree.js";
import { createSource } from "./panels/source.js";

const el = {
  tree: document.getElementById("tree"),
  source: document.getElementById("source"),
  sourcePath: document.getElementById("source-path"),
  sourceMeta: document.getElementById("source-meta"),
  conn: document.getElementById("conn"),
  runState: document.getElementById("run-state"),
  projectRoot: document.getElementById("project-root"),
  statusMessage: document.getElementById("status-message"),
};

const store = createStore({
  connection: "connecting",
  session: { projectRoot: "", runState: "noProgram", stopSeq: 0 },
  source: { path: null },
});

function setStatus(message, isError = false) {
  el.statusMessage.textContent = message;
  el.statusMessage.dataset.state = isError ? "closed" : "";
}

const source = createSource({
  element: el.source,
  pathLabel: el.sourcePath,
  metaLabel: el.sourceMeta,
  onGutterClick(path, line) {
    // Breakpoints arrive in M3. Until then the click is acknowledged rather
    // than silently ignored, so the affordance is visibly not-yet-wired
    // instead of appearing broken.
    setStatus(`${path}:${line} — breakpoints arrive in M3`);
  },
});
source.clear();

const tree = createTree({
  element: el.tree,
  onOpenFile(path) {
    store.set("source.path", path);
    source.open(path);
  },
  onError(message) {
    setStatus(message, true);
  },
});

const conn = createConnection({
  onStatus(state) {
    store.set("connection", state);
  },
  onEvent(msg) {
    switch (msg.event) {
      case "hello":
        store.patch({
          "session.projectRoot": msg.payload.projectRoot,
          "session.runState": msg.payload.runState,
          "session.stopSeq": msg.payload.stopSeq,
        });
        break;
      case "shuttingDown":
        setStatus("server is shutting down");
        break;
      case "error":
        setStatus(msg.payload?.message || "server error", true);
        break;
      default:
        // Unknown events are ignored on purpose: a newer server must be able
        // to add one without breaking a cached frontend.
        break;
    }
  },
});

store.subscribe("connection", (state) => {
  el.conn.dataset.state = state.connection;
  el.conn.textContent = { connecting: "connecting…", open: "connected", closed: "disconnected" }[
    state.connection
  ] ?? state.connection;
});

store.subscribe("session", (state) => {
  el.projectRoot.textContent = state.session.projectRoot;
  el.projectRoot.title = state.session.projectRoot;
  el.runState.dataset.state = state.session.runState;
  el.runState.textContent = {
    noProgram: "no program",
    stopped: "stopped",
    running: "running",
    exited: "exited",
  }[state.session.runState] ?? state.session.runState;
});

conn.connect();
tree.start();
