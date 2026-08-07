// A Chrome DevTools Protocol client, in about a hundred lines.
//
// Node 22 ships a global WebSocket, which is the only reason this can exist
// without a dependency. That matters here more than it would elsewhere: the
// repository's claim is `go build` and done, with a vendored, unbundled
// frontend, and a screenshot tool that dragged in a browser-automation
// framework and its four hundred transitive packages would quietly make that
// claim false.
//
// CDP is a JSON-RPC stream over one WebSocket. Requests carry an id and are
// answered by id; everything without an id is an event. Attaching to a page
// with flatten:true multiplexes that page's traffic over the same socket,
// tagged with a sessionId — which is why every send takes one.

/** connect opens a CDP connection to a WebSocket URL. */
export async function connect(url) {
  const ws = new WebSocket(url);
  const pending = new Map();
  const listeners = new Map();
  let nextId = 1;
  let closed = null;

  await new Promise((resolve, reject) => {
    ws.addEventListener("open", resolve, { once: true });
    ws.addEventListener("error", () => reject(new Error(`cannot connect to ${url}`)), {
      once: true,
    });
  });

  ws.addEventListener("message", (ev) => {
    const msg = JSON.parse(ev.data);
    if (msg.id !== undefined) {
      const waiter = pending.get(msg.id);
      if (!waiter) return;
      pending.delete(msg.id);
      if (msg.error) {
        waiter.reject(new Error(`${waiter.method}: ${msg.error.message}`));
      } else {
        waiter.resolve(msg.result);
      }
      return;
    }
    for (const fn of listeners.get(msg.method) ?? []) fn(msg.params, msg.sessionId);
  });

  ws.addEventListener("close", () => {
    closed = new Error("the browser connection closed");
    // Rejecting rather than leaving them hanging: a browser that dies mid-run
    // must fail the run, not hang it until the outer timeout.
    for (const [, waiter] of pending) waiter.reject(closed);
    pending.clear();
  });

  return {
    send(method, params = {}, sessionId) {
      if (closed) return Promise.reject(closed);
      const id = nextId++;
      const message = { id, method, params };
      if (sessionId) message.sessionId = sessionId;
      return new Promise((resolve, reject) => {
        pending.set(id, { resolve, reject, method });
        ws.send(JSON.stringify(message));
      });
    },

    on(method, fn) {
      if (!listeners.has(method)) listeners.set(method, []);
      listeners.get(method).push(fn);
      return () => {
        const list = listeners.get(method) ?? [];
        const at = list.indexOf(fn);
        if (at >= 0) list.splice(at, 1);
      };
    },

    /** once resolves with the next event of this name, or rejects on timeout. */
    once(method, { timeout = 30_000 } = {}) {
      return new Promise((resolve, reject) => {
        const timer = setTimeout(() => {
          off();
          reject(new Error(`timed out after ${timeout}ms waiting for ${method}`));
        }, timeout);
        const off = this.on(method, (params) => {
          clearTimeout(timer);
          off();
          resolve(params);
        });
      });
    },

    close() {
      try {
        ws.close();
      } catch {
        // Already gone. Closing twice is not an error worth reporting.
      }
    },
  };
}
