// The WebSocket client: request/response correlation and event dispatch.
//
// The server is authoritative and pushes a full snapshot in its hello, so
// reconnecting is not a special case — it is the same code path as the first
// connection. That is what makes a page reload, a dropped connection and a
// second browser tab all behave identically.

export function createConnection({ onEvent, onStatus }) {
  let socket = null;
  let nextId = 1;
  const pending = new Map();
  let reconnectDelay = 250;
  let closedByUs = false;

  function url() {
    const scheme = location.protocol === "https:" ? "wss:" : "ws:";
    return `${scheme}//${location.host}/ws`;
  }

  function connect() {
    closedByUs = false;
    onStatus("connecting");
    socket = new WebSocket(url());

    socket.addEventListener("open", () => {
      reconnectDelay = 250;
      onStatus("open");
    });

    socket.addEventListener("message", (ev) => {
      let msg;
      try {
        msg = JSON.parse(ev.data);
      } catch (err) {
        console.error("unparseable frame", err, ev.data);
        return;
      }
      if (msg.event !== undefined) {
        onEvent(msg);
        return;
      }
      const entry = pending.get(msg.id);
      if (!entry) {
        console.warn("response for an unknown request", msg.id);
        return;
      }
      pending.delete(msg.id);
      if (msg.ok) entry.resolve(msg.payload);
      else entry.reject(Object.assign(new Error(msg.error?.message || "request failed"),
        { code: msg.error?.code || "internal" }));
    });

    socket.addEventListener("close", () => {
      onStatus("closed");
      for (const [, entry] of pending) {
        entry.reject(Object.assign(new Error("connection closed"), { code: "gdb_dead" }));
      }
      pending.clear();
      if (closedByUs) return;
      // Bounded backoff. The server is on loopback, so a failure is either a
      // restart (back in a moment) or a shutdown (never), and hammering it
      // helps neither.
      setTimeout(connect, reconnectDelay);
      reconnectDelay = Math.min(reconnectDelay * 2, 5000);
    });

    socket.addEventListener("error", () => {
      // "close" always follows; reporting both would just double the noise.
    });
  }

  function send(type, payload) {
    return new Promise((resolve, reject) => {
      if (!socket || socket.readyState !== WebSocket.OPEN) {
        reject(Object.assign(new Error("not connected"), { code: "gdb_dead" }));
        return;
      }
      const id = nextId++;
      pending.set(id, { resolve, reject });
      socket.send(JSON.stringify({ id, type, payload }));
    });
  }

  function close() {
    closedByUs = true;
    socket?.close();
  }

  return { connect, send, close };
}
