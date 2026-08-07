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

Everything below needs a debugger session except the `session.*` group; with
`-no-gdb` the rest return `unsupported`.

| Type | Payload | Response | Notes |
|---|---|---|---|
| `session.hello` | — | [`Hello`](#hello) | The snapshot, on demand. |
| `session.info` | — | [`Hello`](#hello) | Alias of `session.hello`. |
| `session.ping` | — | `{"pong": true}` | Liveness check. |
| `session.restart` | — | `{restarted, exePath, breakpointsRestored}` | **Only** when gdb has died; refused while it is healthy. |
| `path.substitute` | `{from, to}` or `{gdbPath, path}` | [`PathList`](#source-paths) | Allowed whenever gdb is alive. |
| `path.addDir` | `{dir}` | [`PathList`](#source-paths) | `dir` is root-relative. |
| `path.list` | — | [`PathList`](#source-paths) | |
| `exe.load` | `{path, args?}` | `{path, runState}` | Root-relative path; refused unless the file starts with the ELF magic. |
| `exec.run` | `{stopAtMain?, stopAtEntry?}` | [`ExecAck`](#execack) | `stopAtMain` uses `-exec-run --start`; `stopAtEntry` uses gdb's `starti`. |
| `exec.continue` | `{thread?, stopSeq?}` | [`ExecAck`](#execack) | |
| `exec.step` | `{thread?, stopSeq?}` | [`ExecAck`](#execack) | Step into. |
| `exec.next` | `{thread?, stopSeq?}` | [`ExecAck`](#execack) | Step over. |
| `exec.finish` | `{thread?, frame?, stopSeq?}` | [`ExecAck`](#execack) | Refused on the outermost frame. |
| `exec.pause` | — | `{paused}` | Allowed while running. |
| `exec.kill` | — | [`ExecAck`](#execack) | Allowed while running. |
| `bp.setSource` | `{path, line, temporary?, condition?}` | [`Breakpoint`](#breakpoints-1) | |
| `bp.delete` | `{number}` | [`BreakpointList`](#breakpoints-1) | |
| `bp.setEnabled` | `{number, enabled}` | [`BreakpointList`](#breakpoints-1) | |
| `bp.list` | — | [`BreakpointList`](#breakpoints-1) | Re-reads from gdb. Allowed while running. |
| `stack.list` | `{thread?, low?, high?, stopSeq?}` | `{stopSeq, threadId, frames}` | Capped at 64 frames. |
| `frame.select` | `{thread?, frame, stopSeq?}` | [`Selection`](#selection) | Also emits `selectionChanged`. |
| `vars.locals` | `{thread?, frame?, stopSeq?}` | `{stopSeq, threadId, frame, variables}` | [`VarNode`](#varnode) rows, no varobjs created. |
| `vars.expand` | `{path, id?, expr?, thread?, frame?, from?, to?, stopSeq?}` | `{path, id, children, hasMore, numChild}` | Creates a varobj on first expansion. Pages 200 children at a time. |
| `watch.add` | `{expr}` | [`WatchList`](#watches) | Floating varobj; a gdb error is returned as `gdb_error`. |
| `watch.remove` | `{path}` | [`WatchList`](#watches) | Allowed while running. |
| `watch.list` | — | [`WatchList`](#watches) | Allowed while running. |
| `regs.names` | — | `{names}` | Cached per program. **Empty entries are preserved.** |
| `regs.values` | `{thread?, format?, stopSeq?}` | `{stopSeq, threadId, format, registers}` | `format` is one of `x d o t N r z`, default `x`. |
| `console.exec` | `{line}` | `{resynced, runState, stopSeq}` | Allowed while running. A gdb error is shown as console output, not a failed request. |
| `console.complete` | `{prefix}` | `{completion, matches, truncated}` | gdb does the completion. |
| `inferior.stdin` | `{dataB64}` | `{written}` | Bypasses the command queue. Allowed while running. |
| `inferior.signal` | `{signal}` | `{sent}` | Name such as `INT`. Sent to the process group. |
| `inferior.resize` | `{rows, cols}` | `{resized}` | Bypasses the command queue. |
| `threads.list` | `{stopSeq?}` | [`ThreadsList`](#threads) | |
| `thread.select` | `{thread, stopSeq?}` | [`Selection`](#selection) | Also sets gdb's own selection, for the console's benefit. |
| `disasm.function` | `{address?, thread?, frame?, stopSeq?}` | [`Disassembly`](#disassembly) | The function containing the address, or a window around it. |
| `disasm.range` | `{start, end, stopSeq?}` | [`Disassembly`](#disassembly) | Capped at 1 MiB of address space. |
| `exec.stepi` | `{thread?, stopSeq?}` | [`ExecAck`](#execack) | One instruction. |
| `exec.nexti` | `{thread?, stopSeq?}` | [`ExecAck`](#execack) | One instruction, over calls. |
| `mem.read` | `{address, offset?, count, stopSeq?}` | [`Memory`](#memory) | `address` is any gdb expression. Capped at 64 KiB per read. |
| `eval.expr` | `{expr, thread?, frame?, stopSeq?}` | `{expr, value, addr}` | `addr` is set when the value looks like an address. Also the hover evaluator, which is why the client debounces it. |
| `symbols.list` | `{filter?, kind?, limit?}` | [`SymbolsList`](#symbols) | Allowed while the inferior runs: the symbol table is a property of the file. |
| `symbols.load` | `{path, mode?, offset?}` | `{path, mode, available}` | Symbols without an exec file. `mode` is `replace` or `add`. |
| `decomp.status` | `{}` | [`DecompStatus`](#decompilation) | Answered even with no program loaded: it is how a client learns the feature exists. |
| `decomp.function` | `{target?, thread?, frame?, stopSeq?}` | [`DecompFunction`](#decompilation) | `target` is a name or any address inside a function; empty follows the selected frame. |

`exec.pause` is the one request that does not queue behind the others. The
server's actor loop is frequently blocked in a gdb round-trip, and that is
exactly when a user presses Pause — routing it through the queue would mean the
button works only when it is not needed. It goes straight to gdb as
`-exec-interrupt`, which is also why `mi-async on` is in the startup handshake.

`exec.kill` is a semantic name, not a passthrough. `-exec-kill` is **not** an
MI3 command — gdb 17.1 answers `^error,msg="Undefined MI command:
exec-kill",code="undefined-command"` — so the server implements it with
`-interpreter-exec console "kill"`.

#### ExecAck

```jsonc
{"runState": "running", "stopSeq": 4}
```

An acknowledgement, never a completion. The stop that follows arrives as a
[`stopped`](#stopped) event.

Every exec request may carry `stopSeq`: the stop the client believed it was
acting on. If it does not match the server's current stop, the request is
refused with `busy` rather than applied to state that has moved on. Sending `0`
(or omitting it) opts out, which is what a toolbar button does. This one
mechanism covers a double-clicked step, a panel refreshing against a stop that
has been superseded, and — from M4 — a variable tree built from a frame that no
longer exists.

### Reserved

These names are fixed now so the frontend and the docs do not have to be renamed
later. Requesting one today returns `unsupported`.

`exe.unload` · `exec.until` `exec.return` ·
`bp.setFunction` `bp.setAddress` `bp.setWatch` `bp.setCondition`
`bp.setIgnoreCount` · `vars.setFormat` `vars.assign`

## Events

| Event | When | Payload |
|---|---|---|
| `hello` | Immediately on connect, before anything is requested. | [`Hello`](#hello) |
| `stopped` | The inferior stopped. | [`Stopped`](#stopped) |
| `running` | The inferior resumed. | `{threadId, runState}` |
| `exited` | The inferior finished. | `{exitCode?, signal?, runState}` |
| `exeLoaded` | A program was loaded. | `{path, runState}` |
| `breakpointsChanged` | The breakpoint mirror changed. | `{breakpoints}` |
| `selectionChanged` | The selected thread or frame changed. | [`Selection`](#selection) |
| `varsInvalidated` | Every variable node the client holds is dead. | `{}` |
| `watchesChanged` | The watch list or its values changed. | [`WatchList`](#watches) |
| `console` | gdb wrote console or log output. | `{text, stream}` |
| `inferiorOutput` | The debuggee wrote to its terminal. | `{dataB64}` |
| `threadsChanged` | Threads appeared or disappeared. | [`ThreadsList`](#threads) |
| `symbolsInvalidated` | The cached symbol table belongs to a program that is no longer loaded. | `{}` |
| `remoteChanged` | A remote target was connected or disconnected. | `{connected, address?}` |
| `decompChanged` | The decompiler started, died, or now holds a different program. | `{}` |
| `mi` | Raw MI traffic, only with `-mi-log`. | `{direction, text}` |
| `gdbDead` | The gdb process exited unexpectedly. | `{reason, stderr}` |
| `error` | An asynchronous failure with no request to attach it to. | [`Error`](#errors) |
| `shuttingDown` | The server is going away. | `{}` |

The `hello` event is pushed unconditionally on every connection. This is the
single decision that makes reconnect, page reload and a second browser tab all
work without special cases: the server is authoritative, and a client's startup
path is identical to its recovery path.

Unknown events must be ignored by clients, so a newer server can add one.

`console` carries a `stream` of `console`, `log`, `target` or `inferior`. Until
M5 gives the debuggee its own pty, its stdout is interleaved into gdb's and
arrives here tagged `inferior` — the server recovers it from lines that are not
valid MI rather than discarding them.

### Hello

The full snapshot. A client repaints entirely from this and asks for nothing
else, which is what makes a reload indistinguishable from a first load.

```jsonc
{
  "protocol": 1,
  "server": "dev",
  "projectRoot": "/home/user/project",  // absolute, display only
  "gdbVersion": "GNU gdb (Ubuntu 17.1-2ubuntu1) 17.1",
  "features": ["thread-info", "…"],     // gdb's -list-features
  "runState": "stopped",                // noProgram | stopped | running | exited
  "stopSeq": 4,                         // increments on every stop
  "exePath": "build/hello",             // root-relative, absent if none
  "breakpoints": [ /* Breakpoint */ ],
  "threads":     [ /* Thread */ ],
  "frames":      [ /* Frame */ ],       // present only when stopped
  "locals":      [ /* Variable */ ],    // the selected frame's
  "selection":   { /* Selection */ },
  "lastStopReason": "breakpoint-hit"
}
```

Every path elsewhere in the protocol is **root-relative** with forward slashes
and no leading slash. `projectRoot` is the sole exception and is display-only.

### Stopped

One fat event carrying everything the UI needs to repaint.

```jsonc
{
  "stopSeq": 4,
  "reason": "breakpoint-hit",
  "breakpointNumber": 1,
  "signal": "SIGINT", "signalMeaning": "Interrupt",  // signal-received only
  "returnValue": "42",                               // function-finished only
  "threadId": 1,
  "threads": [ /* Thread */ ],
  "frames":  [ /* Frame */ ],
  "locals":  [ /* Variable */ ],
  "runState": "stopped"
}
```

Threads, the stack and frame-0 locals are gathered eagerly and sent together
because fetching them separately costs four or five round-trips per single-step,
and stepping is the thing users do most. Registers, disassembly and memory are
deliberately **not** here: those panels pull lazily and pass `stopSeq`.

`reason` is passed through verbatim, including values not listed here.
Recognised: `breakpoint-hit`, `watchpoint-trigger`, `watchpoint-scope`,
`function-finished`, `end-stepping-range`, `location-reached`,
`signal-received`, `solib-event`, `exited-normally`, `exited`,
`exited-signalled`.

Note that gdb reports an exit **twice**: `=thread-group-exited` carries the exit
code, and the `*stopped` that follows carries the reason but not the code. The
server merges them, so a client sees one `exited` event with both.

### Frame

```jsonc
{
  "level": 0,
  "address": "0x0000555555555157",
  "func": "add",
  "args": [{"name": "a", "value": "0"}],
  "source": {"available": true, "path": "hello.c", "line": 5}
}
```

Every part of `source` is optional, and `available` may be false. A stripped
binary reports `func="??"` with no file at all, so **`address` is the only
guaranteed frame identity** — a client must render such frames rather than skip
them. When a path could not be located inside the project (a libc frame, or a
build-time path that does not exist on this machine) `available` is false and
`gdbPath` holds what gdb said, so the UI can offer to locate it.

Arguments come from a second command: `-stack-list-frames` does not return them.

### Variable

```jsonc
{"name": "cfg", "type": "struct config", "expandable": true}
```

`value` is **absent** for aggregates, because the server asks with
`--simple-values`. That absence *is* the expandable signal, and it is also the
defence against a 100k-element array: nothing was fetched.

### Selection

```jsonc
{"threadId": 1, "frame": 1, "stopSeq": 4, "locals": [], "source": {}}
```

### VarNode

One row of the variables tree.

```jsonc
{
  "path": "local:cfg.items[0].name",  // stable identity — the client keys on this
  "id": "r17.items.0.name",           // gdb varobj name, absent until created
  "name": "name",
  "expr": "cfg.items[0].name",
  "type": "char [16]",
  "value": "\"item-0\"",               // absent for aggregates
  "numChild": 16,
  "expandable": true,
  "hasMore": false,
  "inScope": true,
  "changed": true,                    // differs from the previous stop
  "arg": false,                       // a function argument
  "optimizedOut": false
}
```

**Clients key on `path`, never on `id`.** The varobj behind a row is deleted and
recreated on every re-run and on LRU eviction; the path survives that, so the
user's expansion state survives stepping — which is exactly when they care.

**`expandable` comes from the absence of `value`,** not from a type guess. The
server asks gdb with `--simple-values`, which omits the value for aggregates
precisely so a 100k-element array costs nothing until somebody opens it. The
same rule makes `vars.locals` free: it creates no varobjs at all, and one is
created only when a row is expanded.

`optimizedOut` is derived from `value == "<optimized out>"`. At `-O2` this is
normal, not an error, and should be rendered as what it is rather than hidden.

Expansion pages 200 children at a time; `hasMore` says there are more, and
`numChild` is the total, so a UI can say "200 of 4096". `char buf[1<<20]` is a
real declaration and fetching it whole would be a 40 MB message.

### Watches

```jsonc
{"stopSeq": 4, "watches": [ /* VarNode, path "watch:1" */ ]}
```

Watches are **floating** varobjs, created with `@`, so they follow the current
frame rather than being pinned to whichever one was selected when the
expression was typed. The expressions are kept independently of the varobjs
behind them: a re-run deletes every varobj, and the watches are recreated at the
next stop, so the panel survives.

### Registers

```jsonc
{"number": 0, "name": "rax", "value": "0x1", "changed": true}
```

**Registers are identified by number, never by name.** gdb's name list contains
empty strings at stable indices, so position in the list is the only reliable
identity and `regs.names` preserves the blanks. `changed` comes from gdb's own
`-data-list-changed-registers` rather than a diff computed here.

### The console

`console.exec` runs a line as if typed at gdb's prompt. It is the escape hatch
that keeps the semantic command set honest: anything the UI does not model, gdb
still can. It is allowed while the program runs, because refusing it would
remove the only way out of a state the UI has no button for.

The cost is that a typed command can change anything — `b main.c:12`, `next`
and `thread 2` are all ordinary things to type — so the server **resyncs**
afterwards and reports what it re-read in `resynced`. Without that the
breakpoint mirror and the selection would drift quietly out of true.

A gdb error (a typo, an unknown command) arrives as a `console` event and the
request still succeeds. Mistyping at a console is normal and should not raise a
dialog.

`console.complete` forwards to gdb's `-complete`, so the frontend carries no
command table and cannot drift from the debugger it is driving — including
commands added by a user's Python extensions.

### The inferior's terminal

The debuggee gets its own pty, set with `-inferior-tty-set` before the first
run. That buys three things a pipe cannot: the program can be typed into, its
output is separated from gdb's rather than interleaved into the MI stream as
unparseable lines, and libc line-buffers instead of block-buffering — so a
prompt written without a trailing newline actually appears.

`inferiorOutput` and `inferior.stdin` carry **base64**, because the bytes are
arbitrary: a debuggee may emit invalid UTF-8 or raw control sequences, and JSON
strings cannot hold those losslessly.

Send `\r` for Enter — that is what a terminal sends, and the line discipline
turns it into a newline. Echo is on, so typed characters come back as output
and the UI does not have to render local echo itself.

`inferior.stdin` and `inferior.resize` bypass the command queue. The server's
actor loop is frequently blocked in a gdb round-trip, and a keystroke that
waits for it is a keystroke the user experiences as a hang; neither touches
session state, so there is nothing to serialise.

### Threads

```jsonc
{"stopSeq": 4, "selected": 1, "threads": [
  {"id": 1, "targetId": "Thread 0x7ffff7d68b00 (LWP 126406)",
   "name": "worker", "state": "stopped", "core": "3", "frame": { /* Frame */ }}
]}
```

`state` is `running` or `stopped`. While the program runs, `-thread-info` is the
one query gdb still answers, and it reports `state: "running"` with no frame.

### Disassembly

```jsonc
{
  "stopSeq": 4,
  "func": "main",
  "start": "0x0000555555555167",
  "end":   "0x00005555555551c4",
  "pc":    "0x000055555555517a",
  "hasSource": true,
  "truncated": false,
  "instructions": [
    {"address": "0x000055555555517a", "addr": 93824992235898,
     "func": "main", "offset": 19,
     "opcodes": "c7 45 fc 00 00 00 00",
     "text": "movl   $0x0,-0x4(%rbp)",
     "line": 12, "source": { /* SourceRef */ }}
  ]
}
```

`-data-disassemble` returns two different shapes and both are handled: a flat
list, and instructions grouped under `src_and_asm_line` when gdb can attribute
them to source. Which arrives is not a choice the caller makes — the server
always asks for mode 5, and gdb groups only if there is debug info. **A stripped
binary yields the flat form, and `hasSource` is false.** That is not a
degraded path to be tolerated; it is the case instruction-level debugging exists
for, and a client must render those instructions with no line and no file.

`disasm.function` uses gdb's `-a` option, which asks for "the function
containing this address". It is capability-gated on the
`data-disassemble-a-option` feature and falls back to a window around the PC —
64 bytes back, 256 forward. Backwards is a guess, because x86 instructions are
variable-length and there is no way to know where the previous one began; gdb
resynchronises quickly in practice.

Replies are capped at 4000 instructions with `truncated` set.

**Stopping a stripped binary** needs `exec.run {stopAtEntry: true}`, which runs
gdb's `starti`. `stopAtMain` cannot work: `--start` sets a temporary breakpoint
on `main`, and a stripped binary has no such symbol, so the program runs to
completion instead. Without `starti` there is no way to stop it at all.

### Memory

```jsonc
{
  "stopSeq": 4,
  "requested": "&cfg",
  "addr": 140737488349088,
  "count": 32,
  "unreadable": false,
  "ranges": [
    {"start": "0x00007fffffffe9b0", "addr": 140737488349104,
     "dataHex": "030000008d000000109055555555000"}
  ]
}
```

`address` is **any gdb expression** — `&cfg`, `$sp`, `buf+16` — resolved
server-side, because that is what a user has in their head rather than a hex
number they would have to look up first. A plain address is parsed locally, so
paging through a region already on screen costs no extra round-trip. `offset`
shifts a read without re-evaluating the expression.

**Ranges, not one buffer.** A region can be partly unmapped, and the gap has to
be visible: the viewer renders bytes it does not have as `??` rather than as
zeros, which would look exactly like data.

`unreadable` is an ordinary answer, not an error. gdb fails the *whole* read
when any of the range is unmapped — verified against 17.1 — so a viewer must
read in chunks and mark the failing ones, which is what pointing a hex viewer at
an unmapped page is for in the first place.

The viewer computes rows rather than storing them: row N is `base + N*16`. That
is what makes a gigabyte-wide region free — only the bytes for visible rows are
ever fetched, into a sparse cache of 4 KiB chunks with an LRU bound. The cache
is dropped on every stop, because memory is precisely the thing that changes
while a program runs.

### Source paths

```jsonc
{
  "substitutions": [{"from": "/build/agent/work", "to": "/home/user/project"}],
  "directories": ["src"],
  "indexed": 412,
  "indexTruncated": false
}
```

A program built anywhere but this machine records paths that do not exist
locally. Resolution tries, in order: the path as reported; then the project's
basename index, matched by **longest trailing path-component count**.

Basename alone is not enough. Any real project has several files called
`util.c`, and picking the wrong one shows the wrong code with line numbers that
look right — worse than showing nothing, because nothing is obviously nothing.
**A tie is therefore a refusal**, and the unresolved [`SourceRef`](#frame)
carries `candidates` so the UI can ask.

On a clear match the server tells gdb the *prefix* with `substitute-path`, once
per mapping. That fixes every later frame in that tree at the source, plus
`list`, `info line` and anything typed at the console. Rewriting paths per file
in the UI is a losing game: gdb keeps reporting the originals.

`path.substitute` accepts either two prefixes or the pair of files that should
match — the "locate this file" affordance knows the files, not the prefixes, so
the server derives them.

`SourceRef.stale` is set when the source is newer than the binary. The code
shown is real; the line numbers are what have drifted, and saying so beats
letting someone chase the discrepancy.

### Restarting gdb

`session.restart` is refused while gdb is healthy and is the only request that
works when it is dead. Restarting is **never automatic**: gdb dying means
something went wrong — a crash, an OOM kill, an external `kill -9` — and
silently starting another would hide that while discarding the user's state. The
program is re-loaded and breakpoints re-created from the mirror, because those
are the user's work; run state is not, because it cannot be.

### Breakpoints

```jsonc
{
  "number": 1,
  "enabled": true,
  "pending": false,
  "address": "0x0000555555555157",
  "func": "main",
  "path": "hello.c",        // root-relative when resolvable
  "gdbPath": "",            // what gdb said, when it was not
  "line": 12,
  "condition": "", "hitCount": 0, "temporary": false
}
```

`bp.list`, `bp.delete` and `bp.setEnabled` return `{"breakpoints": [...]}`.

Breakpoint state is **event-driven**: `-break-insert -f` can return
`addr="<PENDING>"` and the real address arrives later in a
`=breakpoint-modified`. A client must not assume the creation reply is final.

The mirror hides temporary breakpoints the server did not create. `-exec-run
--start` injects one at `main`, and a marker the user cannot delete because they
never made it is worse than no marker.

### Decompilation

Recovered C beside a live session, for a binary with no source. The producer is
Ghidra, supervised as a separate process exactly as gdb is — no linking,
nothing vendored. The feature is optional: with no `-ghidra` and no
`GHIDRA_INSTALL_DIR`, `decomp.status` reports `off` and nothing else changes.

`decomp.status`:

```json
{ "state": "ready",
  "ghidraVersion": "12.1.2",
  "functionCount": 1415,
  "program": { "name": "vwfw-linux_64.symbols", "sha256": "27763cc2…",
               "languageId": "MIPS:BE:64:default", "imageBase": "0x120000000",
               "pointerSize": 8 } }
```

`state` is `off`, `starting`, `ready` or `failed`. `starting` is a state a
client genuinely observes: opening an existing project is seconds, importing
and analysing a binary is minutes.

`mismatch` is set when the decompiler's program is not the binary gdb loaded,
compared by sha256. A warning rather than a refusal — a stripped and an
unstripped link of one program share every address, so the decompilation is
often still correct — but reading one build while debugging another is a
confidently wrong answer and has to be visible.

`decomp.function` returns the recovered text with a line map:

```json
{ "name": "process_packet", "entry": "0x120007ee0",
  "text": "void process_packet(…)\n{\n…",
  "lines": [ {"n": 26, "addrs": ["0x1200068d5"]},
             {"n": 28, "addrs": ["0x1200068e2", "0x1200068e5"]} ],
  "vars":  [ {"name": "local_70", "type": "undefined1 *", "storage": "stack",
              "expr": "*(undefined1 * *)($sp + 0xf0)"} ],
  "bias": 0, "biasFrom": "main", "pcLine": 28 }
```

**`addrs` is a set, not a range.** A decompiled line's addresses are routinely
disjoint and consecutive lines interleave — a loop's init, increment and test
sit either side of its body — so a min/max range would claim instructions
belonging to a different line.

**Every address has `bias` already applied**, so it is directly comparable with
`stopped`, the disassembly and everything else on the wire. `biasFrom` names
the symbol the bias was established from, by resolving it through gdb and
subtracting Ghidra's address for it. Image bases are *not* used for this: that
arithmetic is right for a non-PIE and silently wrong for everything else. An
empty `biasFrom` means no shared symbol was found — the ordinary case for a
stripped image, where Ghidra's names are `FUN_<address>` and gdb has never
heard of them — and then `bias` is zero and the addresses are link-time, which
a client must say rather than imply otherwise.

`pcLine` is the line the program counter is on, resolved server-side so every
client does not reimplement the tie-break: on optimised code about one address
in five is claimed by two lines, and the rule is the lowest line number that
claims it. `pcLineAmbiguous` reports when that happened, because it is the same
imprecision as stepping `-O2` code with DWARF and hiding it would be a lie.

`storage` is `stack`, `register` or `none`, and the three are not
interchangeable. `stack` is readable anywhere in the frame. `register` is
readable only near `pc` — in optimised code the decompiler packs many variables
into one register, so a value read elsewhere is confidently wrong. `none` is a
decompiler temporary that exists nowhere in the machine and can never show a
value; it is reported rather than omitted, because a blank row is honest and a
missing one is not.

`expr` is a gdb expression, formed from Ghidra's frame base — the stack pointer
at function entry — using a per-ABI rule established by measurement:
`$rbp + pointerSize` on x86-64 with a frame pointer, `$sp + frame.size` on
MIPS64. An architecture with no established rule gets no expression rather than
a guess. See [docs/decompilation.md](decompilation.md).

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

**Getting another link.** The bootstrap token is single-use and short-lived, so
"the link expired" needs an answer that is not "restart the server". A running
server records its address and a *mint secret* in a run file under
`$XDG_RUNTIME_DIR/gdb-wui/`, mode 0600, and `POST /api/bootstrap-url` with that
secret in an `X-Gdb-Wui-Mint` header issues a fresh token, invalidating the
previous one. `gdb-wui -print-url` is the client for it.

The file permission *is* the authentication, and that is sufficient here: only
the same uid can read it, and the same uid is already fully trusted — it can run
anything as the user, which is what gdb-wui does for a living. What the scheme
protects against is the case the threat model actually names: another local user
or an unprivileged process reaching the loopback port. The session cookie is
deliberately **not** accepted here, so a compromised browser tab cannot mint
fresh credentials for itself.

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

## Symbols

`symbols.list` answers from a table read once per program and cached, so the
filter box costs a message rather than a gdb round trip. `filter` is a
case-insensitive substring match on the name, `kind` is `function` or
`variable`, and `limit` defaults to 500 and is capped at 5000.

```json
{
  "symbols": [
    {"name": "main", "kind": "function", "type": "int (int, char **)",
     "file": "src/hello.c", "gdbPath": "/build/src/hello.c", "line": 9,
     "debug": true},
    {"name": "_start", "kind": "function", "address": "0x1060"}
  ],
  "matched": 2,
  "available": 148
}
```

Two populations share the list and they are not interchangeable:

- **`debug: true`** — from DWARF. Carries `gdbPath` and `line`, and `file` as
  well when the source resolves inside the project. This is the only kind that
  can be jumped to in the source view.
- **`debug` absent** — from the ELF symbol table. Carries only `address`. gdb
  knows where such a symbol is but not what it is, so it cannot be evaluated,
  only located. A function goes to the disassembly; a variable goes to the
  memory viewer, because disassembling data produces plausible nonsense.

`mem.read` and `disasm.function` both accept a symbol name where they accept
an address. Resolution tries the expression, then `&(expression)` — a typeless
symbol fails the first (`'LogType' has unknown type; cast it to its declared
type`) and succeeds at the second.

The split between the two comes from gdb itself: the server issues
`-symbol-info-functions` and `-symbol-info-variables`, each with
`--include-nondebug`, and gdb sorts ELF symbols into the code and data lists
for us. That is where `kind` comes from for a stripped binary, which has no
debug info to ask.

`matched` counts what the filter selected before `limit` was applied, and
`available` is the whole table, so a client can render "200 of 4096" rather
than presenting a truncated list as the complete answer.

Addresses are trimmed of the zero padding gdb applies
(`0x0000000000001060` → `0x1060`) to match every other panel.

The cache is dropped, and `symbolsInvalidated` emitted, when a program is
loaded, when gdb is restarted, when a shared library is loaded or unloaded, and
when a console command that changes symbols is typed — `file`, `symbol-file`, `add-symbol-file`, `core-file`, `load`,
`remove-symbol-file`. That last case is not an afterthought: the remote-target
workflow loads symbols by typing `file …`, never through `exe.load`.

`=library-loaded` arrives dozens of times per run, so it only marks the cache
stale; the single `symbolsInvalidated` goes out just before the `stopped` event
that ends the run. By the time a client knows the program has stopped, the
symbol table it is about to query is already the new one.

### Loading symbols without a program

`exe.load` issues `-file-exec-and-symbols`, which says two things at once:
these are the symbols, *and* this is the program to run. They only coincide
when gdb starts the program itself. Against a stub that cannot load an ELF, or
a process someone else started, the code is already in the target's memory and
only the first is true — and declaring an exec file leaves the UI offering to
Run a second, local copy.

`symbols.load` says only the first:

| mode | command | for |
|---|---|---|
| `replace` (default) | `-file-symbol-file <path>` | an image that runs where it was linked |
| `add` | `add-symbol-file <path> [-o <offset>]` | an image that does not — the usual bare-metal case |

`offset` is a string, not a number: a 64-bit address does not survive JSON's
float64, and `"0x8000"` is how people write one. It is only meaningful with
`add`, because `symbol-file` has nowhere to put one. `add-symbol-file` has no
MI form, so it goes through `-interpreter-exec console`, the same route
`starti` takes.

`path` is root-relative and checked exactly like `exe.load`'s: through the
project root, and it must start with the ELF magic. Both are allowed while the
inferior runs — telling gdb what addresses mean is configuration, and it is
precisely what someone does after attaching to a running target.

**`symbols.load` does not set the architecture, and cannot.** Only `file`
(`exe.load`) does, by reading the ELF header; `symbol-file` and
`add-symbol-file` both leave gdb on the host's. Since `target remote` needs the
architecture to parse the stub's register reply, a foreign target must have its
ELF loaded — or `set architecture`/`set endian` typed — *before* connecting.

Not covered here: shared libraries for an attached process need
`set sysroot` (`target:` when the stub does file transfer, otherwise a local
copy), which is a console command like any other.

## Remote targets

`hello` carries `remote: {connected, address}` when gdb is attached to a target
this server did not start, and `remoteChanged` fires when that changes.

There is no `target.connect` request. Connecting is `console.exec` with
`target remote <address>`, and disconnecting is `console.exec` with
`disconnect` — the same commands the user would type, so the console shows
exactly what was run and gdb's own error text when a connection is refused.
The UI's connect button is a shortcut for typing, not a separate mechanism,
which is what keeps one source of truth for a connection that a console
command can also make or break.

The server recognises those commands by reading the command text. There is no
MI query for "am I attached to something I did not start" that does not involve
parsing console output, which would be worse. A `target remote` is believed
only if gdb accepted it: a refused connection that still reported `connected`
would be worse than no indicator, and would make shutdown try to detach from
something never attached. A `detach` or `disconnect` is believed either way —
whatever went wrong, the connection is no longer in a state to act on.

Why it matters beyond display: shutting down **detaches** from a remote target
rather than killing it, because killing something you merely connected to
destroys somebody else's session.
