// A tiny observable store.
//
// Both of the following exist for the step hot path, where one gdb stop touches
// half a dozen slices and a held-down F10 delivers stops faster than the
// browser can paint:
//
//   1. Writes record dirty dotted paths and notification is deferred to a
//      microtask, so one event that touches six slices notifies once.
//   2. Subscribers match by path prefix, so a panel can watch "source.execLine"
//      and do an O(1) class toggle instead of rebuilding on every "source.*".
//
// Nothing renders inside a WebSocket handler. Panels subscribe, mark themselves
// dirty, and the render pass runs in one requestAnimationFrame.

export function createStore(initial = {}) {
  const state = structuredClone(initial);
  const subscribers = new Set();
  let dirty = new Set();
  let scheduled = false;

  function get(path) {
    if (!path) return state;
    let node = state;
    for (const key of path.split(".")) {
      if (node == null) return undefined;
      node = node[key];
    }
    return node;
  }

  function set(path, value) {
    const keys = path.split(".");
    let node = state;
    for (let i = 0; i < keys.length - 1; i++) {
      const key = keys[i];
      if (node[key] == null || typeof node[key] !== "object") node[key] = {};
      node = node[key];
    }
    const last = keys[keys.length - 1];
    if (node[last] === value) return false;
    node[last] = value;
    markDirty(path);
    return true;
  }

  // patch applies several updates as one notification. Callers use it for
  // anything derived from a single event, which is most things.
  function patch(updates) {
    let changed = false;
    for (const [path, value] of Object.entries(updates)) {
      if (set(path, value)) changed = true;
    }
    return changed;
  }

  function markDirty(path) {
    dirty.add(path);
    if (scheduled) return;
    scheduled = true;
    queueMicrotask(flush);
  }

  function flush() {
    scheduled = false;
    const paths = dirty;
    dirty = new Set();
    if (paths.size === 0) return;
    for (const sub of subscribers) {
      if (sub.paths.some((p) => matches(paths, p))) {
        try {
          sub.callback(state, paths);
        } catch (err) {
          // One panel throwing must not stop the others from updating; a
          // half-rendered UI that keeps working beats a frozen one.
          console.error("subscriber failed", err);
        }
      }
    }
  }

  // A subscription to "source" fires for "source.path" and for "source"
  // itself, but not for "sources".
  function matches(dirtyPaths, watched) {
    for (const p of dirtyPaths) {
      if (p === watched) return true;
      if (p.startsWith(watched + ".")) return true;
      if (watched.startsWith(p + ".")) return true;
    }
    return false;
  }

  function subscribe(paths, callback) {
    const sub = { paths: Array.isArray(paths) ? paths : [paths], callback };
    subscribers.add(sub);
    return () => subscribers.delete(sub);
  }

  return { get, set, patch, subscribe, state };
}
