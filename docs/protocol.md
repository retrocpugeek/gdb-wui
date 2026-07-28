# The gdb-wui protocol

This is the contract between the Go server and the browser. It is enforced by a
test: every request type and error code named in `internal/wire` must appear
here, and every type listed here must be answered by the server rather than
rejected as unsupported. A change to one without the other fails the build.

Protocol version: **1**.

## Transports, and why there are two

**WebSocket** carries everything stateful: commands, their replies, and pushed
events. It is one connection per client, which is what makes inferior stdin a
low-latency ordered byte channel (a POST per keystroke would be a round-trip
each) and what gives the idle-exit lifecycle a client identity to track.

**HTTP GET** carries bulk reads: source text and directory listings. They go
over HTTP with ETags so that fetching a 2 MB file does not sit in front of
latency-sensitive stepping traffic and inferior output on the socket.

Both are authenticated by the same session cookie and pass through the same
authorization gate. See [Security](#security).

## Envelope

Requests carry a client-chosen `id`, echoed on the response.

```jsonc
// request
{"id": 17, "type": "exec.step", "payload": {"thread": 1}}

// success
{"id": 17, "ok": true, "type": "exec.step", "payload": {"runState": "running"}}

// failure
{"id": 17, "ok": false, "type": "exec.step",
 "error": {"code": "busy", "message": "the inferior is running"}}

// event (unsolicited)
{"event": "stopped", "seq": 412, "payload": {}}
```

`seq` is server-monotonic across every event on every connection, so a client
can detect that it missed something and tests can assert ordering.

Two rules that shape the frontend:

- **Exec responses are acknowledgements, not completions.** `-exec-continue`
  returns as soon as gdb accepts it; the stop arrives later as an event. A
  client that awaits a step and then reads state will read stale state.
- **An unknown `type` returns an `unsupported` error, never a closed
  connection.** A newer frontend against an older server degrades; it does not
  disconnect. The same applies to malformed JSON and unexpected binary frames.

## Requests

### Implemented

| Type | Payload | Response | Notes |
|---|---|---|---|
| `session.hello` | — | [`Hello`](#hello) | The snapshot, on demand. |
| `session.info` | — | [`Hello`](#hello) | Alias of `session.hello`. |
| `session.ping` | — | `{"pong": true}` | Liveness check. |

### Reserved

These names are fixed now so the frontend and the docs do not have to be
renamed later. Requesting one today returns `unsupported`.

`exe.load` `exe.unload` · `exec.run` `exec.continue` `exec.pause` `exec.step`
`exec.next` `exec.stepi` `exec.nexti` `exec.finish` `exec.until` `exec.return`
`exec.kill` · `bp.setSource` `bp.setFunction` `bp.setAddress` `bp.setWatch`
`bp.delete` `bp.setEnabled` `bp.setCondition` `bp.setIgnoreCount` `bp.list` ·
`threads.list` `thread.select` · `stack.list` `frame.select` · `vars.locals`
`vars.expand` `vars.setFormat` `vars.assign` · `eval.expr` · `watch.add`
`watch.remove` `watch.list` · `regs.names` `regs.values` · `disasm.function`
`disasm.range` · `mem.read` · `console.exec` `console.complete` ·
`inferior.stdin` `inferior.signal` `inferior.resize` · `path.substitute`
`path.addDir` `path.list`

Note: `exec.kill` is a semantic name, not a passthrough. `-exec-kill` is **not**
an MI3 command — gdb 17.1 answers `^error,msg="Undefined MI command:
exec-kill",code="undefined-command"` — so the server implements it with
`-interpreter-exec console "kill"`.

## Events

| Event | When | Payload |
|---|---|---|
| `hello` | Immediately on connect, before anything is requested. | [`Hello`](#hello) |
| `console` | gdb wrote console or log output. | `{"text": "…"}` |
| `error` | An asynchronous failure with no request to attach it to. | [`Error`](#errors) |
| `shuttingDown` | The server is going away. | `{}` |

The `hello` event is pushed unconditionally on every connection. This is the
single decision that makes reconnect, page reload and a second browser tab all
work without special cases: the server is authoritative, and a client's startup
path is identical to its recovery path.

Unknown events must be ignored by clients, so a newer server can add one.

### Hello

```jsonc
{
  "protocol": 1,
  "server": "dev",
  "projectRoot": "/home/user/project",  // absolute, for display only
  "gdbVersion": "",                     // empty until a session exists (M3)
  "features": [],                       // gdb's -list-features (M3)
  "runState": "noProgram",              // noProgram | stopped | running | exited
  "stopSeq": 0                          // increments on every stop
}
```

Every path elsewhere in the protocol is **root-relative** with forward slashes
and no leading slash. `projectRoot` is the sole exception and is display-only.

## Errors

`code` is drawn from a closed set. `message` is for humans and must not be
parsed.

| Code | Meaning |
|---|---|
| `bad_request` | Malformed payload or invalid field. |
| `unsupported` | Unknown request type. |
| `not_ready` | No program is loaded. |
| `busy` | The inferior is running and this request needs it stopped. |
| `gdb_error` | gdb replied `^error`. |
| `gdb_dead` | The gdb process is gone. |
| `timeout` | gdb did not answer in time. |
| `path_denied` | The path escaped the project root. |
| `not_found` | No such thing. |
| `too_large` | A hard cap was exceeded. |
| `internal` | A server bug. |

`busy` deserves a note: gdb rejects most commands while the inferior runs, with
messages like `Selected thread is running.` The server gates those requests and
returns `busy` immediately rather than forwarding them, which turns an
undocumented gdb behaviour into a contract. Allowed while running: `exec.pause`,
`exec.kill`, `inferior.*`, `console.exec`, `session.*`.

## HTTP endpoints

Both require the session cookie and both are subject to the checks in
[Security](#security).

### `GET /api/tree?path=<root-relative>`

Lists one directory level. One level, not a recursive walk: on a large
repository a full walk is slow to produce, large to send, and mostly unread.

```jsonc
{
  "path": "src",
  "entries": [
    {"name": "deep", "path": "src/deep", "dir": true},
    {"name": "util.c", "path": "src/util.c", "dir": false, "size": 812},
    {"name": "link", "path": "src/link", "dir": false, "symlink": true}
  ],
  "truncated": false
}
```

Directories sort first, then by name. `.git` and `node_modules` are skipped.
Listings are capped at 5000 entries and set `truncated` when they hit it —
surfaced rather than silently dropped, because a directory that quietly lists
5000 of its 9000 files is worse than one that admits it.

A symlink is reported as a link rather than followed or hidden. Opening one that
resolves inside the root works; one that escapes is refused.

### `GET /api/file?path=<root-relative>`

Returns the file as `text/plain; charset=utf-8` with a strong `ETag`.
Conditional requests with `If-None-Match` return `304`.

| Condition | Status | Code |
|---|---|---|
| Missing `path` | 400 | `bad_request` |
| Outside the root | 403 | `path_denied` |
| Not found | 404 | `not_found` |
| A directory | 400 | `bad_request` |
| Over 2 MiB | 413 | `too_large` |
| Contains NUL bytes | 415 | `bad_request` |

The NUL sniff exists so the UI can offer a hex viewer rather than render an ELF
file as text.

## Security

gdb-wui runs arbitrary binaries with the user's full privileges — that is what a
debugger is. Sandboxing the debuggee is not a goal. In scope: other local users
and processes reaching the port, hostile web pages the user visits, and path
traversal.

Binding `127.0.0.1` is **not** sufficient on its own. Any page in the user's
browser can `fetch` a loopback URL, and for this service a successful
cross-origin request means arbitrary code execution.

**Bootstrap token → session cookie.** Two 32-byte random secrets. The bootstrap
token is single-use with a 60-second TTL and is the only one that appears in a
URL — because that URL is handed to `xdg-open`, so it lands in argv (readable by
any local user via `ps`), in browser history, and potentially in a `Referer`.
`GET /?t=<bootstrap>` validates it in constant time, burns it, sets
`Set-Cookie: gdbwui=…; HttpOnly; SameSite=Strict`, and redirects (303) to `/`.
The session token never appears in a URL or in argv.

**One gate, applied to every route including the WebSocket upgrade.**
Authorization runs *before* `websocket.Accept`, because Accept writes the 101
response and it cannot be retracted afterwards.

**Anti-rebinding, three independent layers:**

1. `Host` must exactly match one of `127.0.0.1:PORT`, `localhost:PORT`,
   `[::1]:PORT`. A rebound page arrives on loopback but says
   `Host: evil.example`.
2. `Origin`, required on `/ws` and on non-GET requests, must match. Under
   rebinding the page's origin stays `http://evil.example`, so the browser both
   withholds the `SameSite=Strict` cookie and announces the true origin here.
3. `Sec-Fetch-Site: cross-site` is rejected — a second read of the same fact
   that does not depend on `Origin` being sent.

No CORS headers are ever set. Responses carry `nosniff`,
`Referrer-Policy: no-referrer`, `Cross-Origin-Resource-Policy: same-origin`, and
a CSP of `default-src 'none'` with `script-src 'self'`, `frame-ancestors 'none'`
and `connect-src` naming the WebSocket origins explicitly.

`style-src` currently includes `'unsafe-inline'` because xterm.js injects a
`<style>` element at runtime. That arrives in M5; if it proves unnecessary, the
allowance is dropped.
