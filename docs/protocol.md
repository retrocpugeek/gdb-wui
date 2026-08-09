# The gdb-wui protocol

This is the contract between the Go server and the browser. It is enforced by a
test: every request type and error code named in `internal/wire` must appear
here, and every type listed here must be answered by the server rather than
rejected as unsupported. A change to one without the other fails the build.

Protocol version: **1**.

## Transports

There are two.

**WebSocket** carries everything stateful: commands, their replies, and pushed
events. There is one connection per client. This gives inferior stdin a
low-latency ordered byte channel, which a POST per keystroke would not, and
gives the idle-exit lifecycle a client identity to track.

**HTTP GET** carries bulk reads: source text and directory listings, with ETags.
Sending these over HTTP keeps a 2 MB file from queueing ahead of stepping
traffic and inferior output on the socket.

Both use the same session cookie and pass through the same authorization gate.
See [Security](#security).

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

Two rules a client has to be written around:

- **Exec responses are acknowledgements, not completions.** `-exec-continue`
  returns as soon as gdb accepts the command; the stop arrives later as an
  event. A client that awaits a step and then reads state will read stale state.
- **An unknown `type` returns an `unsupported` error rather than closing the
  connection.** A newer frontend talking to an older server degrades instead of
  disconnecting. The same applies to malformed JSON and to unexpected binary
  frames.

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
| `bp.setAddress` | `{location, temporary?, condition?}` | [`Breakpoint`](#breakpoints-1) | `location` is an address or a function name. The only way to break in a stripped binary. |
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
| `vars.assign` | `{path, id?, expr?, value, thread?, frame?, stopSeq?}` | `{path, id, value, stopSeq}` | Writes a variable. `value` is a gdb expression. The reply carries the value read back, not the one sent. |
| `regs.names` | — | `{names}` | Cached per program. **Empty entries are preserved.** |
| `regs.values` | `{thread?, format?, stopSeq?}` | `{stopSeq, threadId, format, registers}` | `format` is one of `x d o t N r z`, default `x`. |
| `regs.write` | `{number, value, thread?, format?, stopSeq?}` | `{stopSeq, threadId, format, register}` | Writes one register, by number. Refused for a register gdb has not named. |
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
| `exec.stepLine` | `{lines?, bodyStart?, bodyEnd?, over?, thread?, stopSeq?}` | [`ExecAck`](#execack) | Step until the pc reaches a different decompiled line. For views with no line table. |
| `mem.read` | `{address, offset?, count, stopSeq?}` | [`Memory`](#memory) | `address` is any gdb expression. Capped at 64 KiB per read. |
| `mem.symbols` | `{addresses, stopSeq?}` | `{symbols}` | Which symbol each address falls in. Capped at 128 per request. |
| `mem.write` | `{address, offset?, dataHex, stopSeq?}` | `{stopSeq, addr, count}` | `dataHex` is two hex digits per byte. Capped at 4 KiB. Empty and odd-length are refused. |
| `eval.expr` | `{expr, thread?, frame?, stopSeq?}` | `{expr, value, addr}` | `addr` is set when the value looks like an address. Also the hover evaluator, which is why the client debounces it. |
| `goto.locate` | `{target, thread?, frame?, stopSeq?}` | [`GotoLocation`](#gotolocation) | Where a symbol, address, expression or `FILE:LINE` is. Needs a program, not a running one. |
| `symbols.list` | `{filter?, kind?, limit?}` | [`SymbolsList`](#symbols) | Allowed while the inferior runs: the symbol table is a property of the file. |
| `symbols.load` | `{path, mode?, offset?}` | `{path, mode, available}` | Symbols without an exec file. `mode` is `replace` or `add`. |
| `decomp.status` | `{}` | [`DecompStatus`](#decompilation) | Answered even with no program loaded: it is how a client learns the feature exists. |
| `decomp.function` | `{target?, thread?, frame?, stopSeq?}` | [`DecompFunction`](#decompilation) | `target` is a name or any address inside a function; empty follows the selected frame. |

`exec.pause` is the one request that does not queue behind the others. The
server's actor loop is usually blocked in a gdb round-trip when a user presses
Pause, so a queued request would arrive only after whatever it was meant to
interrupt had finished. It goes straight to gdb as `-exec-interrupt`, which is
also why `mi-async on` is part of the startup handshake.

`exec.kill` is not a passthrough. `-exec-kill` is not an MI3 command: gdb 17.1
answers `^error,msg="Undefined MI command:
exec-kill",code="undefined-command"`. The server implements it with
`-interpreter-exec console "kill"` instead.

#### ExecAck

```jsonc
{"runState": "running", "stopSeq": 4}
```

This is an acknowledgement rather than a completion. The stop that follows
arrives as a [`stopped`](#stopped) event.

Every exec request may carry `stopSeq`, which is the stop the client believed it
was acting on. If it does not match the server's current stop, the request is
refused with `busy` rather than applied to state that has since moved on. Send
`0`, or omit it, to opt out; that is what a toolbar button does. The same
mechanism covers a double-clicked step, a panel refreshing against a superseded
stop, and a variable tree built from a frame that no longer exists.

`bp.setFunction` is reserved rather than implemented, because `bp.setAddress`
accepts a function name as well as an address.

### Reserved

These names are reserved so that adding them later does not require renaming
anything. Requesting one now returns `unsupported`.

`exe.unload` · `exec.until` `exec.return` ·
`bp.setFunction` `bp.setWatch` `bp.setCondition`
`bp.setIgnoreCount` · `vars.setFormat`

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
| `valueWritten` | A variable, register or byte was written by hand. | `{stopSeq, what, detail, value}` |
| `console` | gdb wrote console or log output. | `{text, stream}` |
| `inferiorOutput` | The debuggee wrote to its terminal. | `{dataB64}` |
| `threadsChanged` | Threads appeared or disappeared. | [`ThreadsList`](#threads) |
| `symbolsInvalidated` | The cached symbol table belongs to a program that is no longer loaded. | `{}` |
| `remoteChanged` | A remote target was connected or disconnected. | `{connected, address?}` |
| `decompChanged` | The decompiler started, died, or now holds a different program. | `{}` |
| `decompLog` | One line of decompiler activity, for the log pane. | `{text, level?, millis?}` |
| `mi` | Raw MI traffic, only with `-mi-log`. | `{direction, text}` |
| `gdbDead` | The gdb process exited unexpectedly. | `{reason, stderr}` |
| `error` | An asynchronous failure with no request to attach it to. | [`Error`](#errors) |
| `shuttingDown` | The server is going away. | `{}` |

The `hello` event is pushed on every connection, unconditionally. The server is
authoritative, so a client's startup path and its recovery path are the same
code, and reconnect, page reload and a second browser tab need no special
handling.

Unknown events must be ignored by clients, so a newer server can add one.

`console` carries a `stream` of `console`, `log`, `target` or `inferior`. Until
M5 gives the debuggee its own pty, its stdout is interleaved into gdb's and
arrives here tagged `inferior` — the server recovers it from lines that are not
valid MI rather than discarding them.

### Hello

The full snapshot. A client repaints from this alone and needs to ask for
nothing else, so a reload behaves the same as a first load.

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

A single event carrying everything the UI needs in order to repaint.

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

Threads, the stack and frame-0 locals are gathered eagerly and sent together,
because fetching them separately would cost four or five round-trips per
single-step. Registers, disassembly and memory are not included; those panels
fetch what they need when they are visible, passing `stopSeq`.

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
binary reports `func="??"` with no file at all, so `address` is the only field a
frame always has, and clients must render such frames rather than skip them.
When a path could not be located inside the project — a libc frame, or a
build-time path that does not exist on this machine — `available` is false and
`gdbPath` holds what gdb reported, so that the UI can offer to locate it.

Arguments come from a second command: `-stack-list-frames` does not return them.

### Variable

```jsonc
{"name": "cfg", "type": "struct config", "expandable": true}
```

`value` is absent for aggregates, because the server asks with
`--simple-values`. That absence is what marks a row expandable, and it is also
why a 100,000-element array costs nothing until it is opened: none of it was
fetched.

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

Clients must key on `path`, not on `id`. The varobj behind a row is deleted and
recreated on every re-run and on LRU eviction, whereas the path survives, so the
user's expansion state survives stepping.

`expandable` comes from the absence of `value` rather than from a type guess.
The server asks gdb with `--simple-values`, which omits the value for
aggregates, so a 100,000-element array costs nothing until it is opened. For the
same reason `vars.locals` creates no varobjs at all; one is created when a row
is expanded.

`optimizedOut` is derived from `value == "<optimized out>"`. At `-O2` this is
normal rather than an error, and should be shown as what it is.

Expansion pages 200 children at a time. `hasMore` says whether there are more
and `numChild` is the total, so a client can show "200 of 4096". `char
buf[1<<20]` is a real declaration, and fetching it whole would be a 40 MB
message.

### Watches

```jsonc
{"stopSeq": 4, "watches": [ /* VarNode, path "watch:1" */ ]}
```

Watches are floating varobjs, created with `@`, so they follow the current frame
rather than staying pinned to the frame that was selected when the expression
was typed. The expressions are stored independently of the varobjs behind them:
a re-run deletes every varobj, and the watches are recreated at the next stop.

### Registers

```jsonc
{"number": 0, "name": "rax", "value": "0x1", "changed": true}
```

Registers are identified by number rather than by name. gdb's name list contains
empty strings at stable indices, so position in the list is the only reliable
identity, and `regs.names` preserves the blanks. `changed` comes from gdb's
`-data-list-changed-registers` rather than from a diff computed here.

### The console

`console.exec` runs a line as if it had been typed at gdb's prompt, so anything
the UI does not model can still be done. It is allowed while the program runs,
because refusing it would leave no way out of a state the UI has no button
for.

A typed command can change anything: `b main.c:12`, `next` and `thread 2` are
all ordinary things to type. The server therefore resyncs afterwards and reports
what it re-read in `resynced`, so that the breakpoint mirror and the selection
do not drift.

A gdb error, such as a typo or an unknown command, arrives as a `console` event
and the request still succeeds, because mistyping at a console is normal and
should not raise a dialog.

`console.complete` forwards to gdb's `-complete`. The frontend therefore carries
no command table of its own and cannot drift from the debugger it is driving,
including commands added by a user's Python extensions.

### The inferior's terminal

The program being debugged gets its own pty, set with `-inferior-tty-set` before
the first run. This gives three things a pipe does not: the program can be typed
into, its output is separated from gdb's rather than interleaved into the MI
stream as unparseable lines, and libc line-buffers rather than block-buffers, so
a prompt written without a trailing newline appears.

`inferiorOutput` and `inferior.stdin` carry base64, because the bytes are
arbitrary. A program may emit invalid UTF-8 or raw control sequences, and JSON
strings cannot carry those losslessly.

Send `\r` for Enter, which is what a terminal sends; the line discipline turns it
into a newline. Echo is on, so typed characters come back as output and the
client does not have to render local echo itself.

`inferior.stdin` and `inferior.resize` bypass the command queue. The actor loop
is often blocked in a gdb round-trip, and a keystroke that waits for it appears
to the user as a hang. Neither request touches session state, so there is
nothing to serialise.

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

`-data-disassemble` returns two different shapes, and both are handled: a flat
list, and instructions grouped under `src_and_asm_line` when gdb can attribute
them to source. The caller does not choose between them. The server always asks
for mode 5, and gdb groups the output only when there is debug info, so a
stripped binary yields the flat form with `hasSource` false. Clients must render
those instructions with no line and no file.

`disasm.function` uses gdb's `-a` option, which asks for the function containing
an address. It is capability-gated on the `data-disassemble-a-option` feature,
and falls back to a window around the program counter of 64 bytes back and 256
forward. The backward part is a guess, because x86 instructions are
variable-length and there is no way to know where the previous one began; in
practice gdb resynchronises quickly.

Replies are capped at 4000 instructions with `truncated` set.

To stop a stripped binary, use `exec.run {stopAtEntry: true}`, which runs gdb's
`starti`. `stopAtMain` does not work on one: `--start` sets a temporary
breakpoint on `main`, a stripped binary has no such symbol, and the program runs
to completion instead.

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

`address` accepts any gdb expression, such as `&cfg`, `$sp` or `buf+16`, and is
resolved on the server. A plain address is parsed locally, so paging through a
region already on screen costs no extra round-trip. `offset` shifts a read
without re-evaluating the expression.

The reply is a list of ranges rather than one buffer, because a region can be
partly unmapped and the gap has to be visible. The viewer renders bytes it does
not have as `??` rather than as zeros, which would look like data.

`unreadable` is an ordinary answer rather than an error. gdb fails the whole read
when any part of the range is unmapped, verified against 17.1, so a viewer has
to read in chunks and mark the ones that fail.

The viewer computes rows rather than storing them: row N is `base + N*16`. Only
the bytes for visible rows are fetched, into a sparse cache of 4 KiB chunks with
an LRU bound, so a region gigabytes wide costs nothing. The cache is dropped at
every stop, because memory is what changes while a program runs.

### GotoLocation

`goto.locate` answers "where is this?" for the go-to box, which acts on
whichever centre view has focus.

```json
{
  "target": "structs.c:42",
  "address": "0x555555555337",
  "addr": 93824992235831,
  "func": "main",
  "source": {"available": true, "path": "structs.c", "line": 42}
}
```

`target` is a symbol, an address, any gdb expression, or `FILE:LINE`.

One resolver rather than one per view, because the views want different facts
about the same place: the source view needs a file and a line, the disassembly
needs an address, and neither is derivable from the other without gdb. It also
has to be gdb that answers — `-symbol-info-*` reports link-time addresses, so a
client resolving names against the symbol table would be wrong about every one
of them once a position-independent executable is running and relocated.

Every field but `target` is optional, and a partial answer is ordinary rather
than a failure:

- A stripped binary's symbol has an address and no `source`.
- A line that generated no code — a declaration, a blank line — has a `source`
  and no address. Returning a neighbouring line's address instead would put the
  disassembly somewhere that was not asked for and give no sign of it.
- A stack or heap address has neither `func` nor `source`.

`FILE:LINE` has no MI command behind it. `-data-disassemble -f FILE -l LINE`
starts at the *containing function's* entry rather than at the line, so the
address is found by looking through the grouped output for the line rather than
by trusting where gdb began. `-n -1` asks for the whole function, which bounds
the work by the function's size and guarantees the line is inside the reply.
The alternative, `info line FILE:N`, returns an English sentence naming only
the file's basename, and resolving a source path into the project needs the
full one.

### Symbolising addresses

`mem.symbols` answers "what is this address called", so a hex dump reads as
`cfg+0x10` rather than as a bare number.

```json
{"symbols": [{"addr": "0x5555555551f9", "name": "inspect+16"}]}
```

An address in no symbol is omitted rather than returned empty, which is the
ordinary case for the stack and the heap. An empty reply is therefore a
meaningful answer rather than a failure.

This is implemented by evaluating `(void*)ADDR`, because gdb annotates a pointer
with its symbol when it prints one: `0x5555555551f9 <inspect+16>`. That needs no
console command, and it stays correct across relocation and across shared
libraries, each of which has its own load bias. A table built from
`-symbol-info-*` would not, because those addresses are link-time.

The client sends the addresses it is showing rather than a range. The memory
view is virtual over a 4 KiB chunk, so symbolising a whole chunk would mean 256
lookups for a screenful of forty rows.

`eval.expr`'s `addr` is what lets a client offer to follow a value. It is set
whenever the value is address-shaped, including for a register, and is not a
claim that the value is a pointer: an `int` holding 3 yields `addr: 3`. A client
that offers to follow a value should ignore anything below one page, which is
never mapped.

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

Matching on the basename alone is not enough, because most projects have several
files called `util.c` and picking the wrong one shows the wrong code with line
numbers that look plausible. A tie is therefore treated as a refusal, and the
unresolved [`SourceRef`](#frame) carries `candidates` so that the UI can ask.

On a clear match the server gives gdb the prefix with `substitute-path`, once per
mapping. That fixes every later frame in the same tree, and also `list`,
`info line` and anything typed at the console. Rewriting paths per file in the
UI would not, because gdb goes on reporting the originals.

`path.substitute` accepts either two prefixes or the pair of files that should
match. The "locate this file" affordance knows the files rather than the
prefixes, so the server derives them.

`SourceRef.stale` is set when the source is newer than the binary. The code
shown is real and the line numbers are what have drifted, so saying so is more
useful than leaving the reader to work it out.

### Restarting gdb

`session.restart` is refused while gdb is healthy, and is the only request that
works when it is dead. Restarting is never automatic: gdb dying means something
went wrong — a crash, an OOM kill, an external `kill -9` — and starting another
silently would hide that. The program is re-loaded and breakpoints are recreated
from the mirror, because those are the user's work. Run state is not restored,
because it cannot be.

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

Breakpoint state is event-driven. `-break-insert -f` can return
`addr="<PENDING>"`, with the real address arriving later in a
`=breakpoint-modified`, so a client must not treat the creation reply as final.

The mirror hides temporary breakpoints the server did not create. `-exec-run
--start` injects one at `main`, and showing it would give the user a marker they
cannot delete.

### Decompilation

Recovered C shown beside a live session, for a binary with no source. Ghidra
produces it, supervised as a separate process in the same way gdb is: nothing is
linked and nothing is vendored. The feature is optional. With no `-ghidra` and no
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

`state` is `off`, `starting`, `ready` or `failed`. Clients do observe `starting`:
opening an existing project takes seconds, and importing and analysing a binary
takes minutes.

`mismatch` is set when the decompiler's program is not the binary gdb loaded,
compared by sha256. This is a warning rather than a refusal, because a stripped
and an unstripped link of the same program share every address and the
decompilation is often still correct. It has to be visible, because reading one
build while debugging another produces answers that look right.

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

`addrs` is a set rather than a range. A decompiled line's addresses are often
disjoint, and consecutive lines interleave — a loop's init, increment and test
sit either side of its body — so a min/max range would claim instructions
belonging to another line.

Every address has `bias` already applied, so it can be compared directly with
`stopped`, the disassembly and everything else on the wire. `biasFrom` names the
symbol the bias was established from, by resolving that symbol through gdb and
subtracting Ghidra's address for it. Image bases are not used, because that
arithmetic is correct for a non-PIE and wrong for everything else. An empty
`biasFrom` means no shared symbol was found, which is the ordinary case for a
stripped image where Ghidra's names are `FUN_<address>`. In that case `bias` is
zero and the addresses are link-time, which a client should state rather than
leave implied.

`pcLine` is the line the program counter is on, resolved on the server so that
clients do not each have to implement the two rules below.

The tie-break: in optimised code about one address in five is claimed by two
lines, and the answer is the lowest line number that claims it.
`pcLineAmbiguous` reports when that happened. It is the same imprecision as
stepping `-O2` code with DWARF.

The fallback: many addresses are claimed by no line at all. Prologues, register
spills and epilogues belong to no expression, and stepping lands on them often —
on a hello-world, stepping off a function's last statement lands on `0x1248`
when the nearest mapped addresses are `0x1243` and `0x124d`. Reporting "no line"
there is accurate but makes the marker disappear mid-step, so the nearest
preceding line is used instead and flagged `pcLineApprox`. A client should draw
that differently, because "the program is here" and "the program is somewhere
after here" are different claims.

`decompLog` is not behind a flag, unlike the raw MI stream, because its volume
is one line per operation — start, import, ready, and one per decompile.
Without it, a pane that says "starting" for a minute gives no way to tell
whether anything is happening. Ghidra's own output is filtered on the way
through: its `REPORT:` milestones and its complaints go to the browser, while
the JVM banner, each analyzer's timing and the log4j noise stay in the server's
log, where `-v` finds them.

`level` is `info`, `warn` or `error`. `millis` is the duration of an operation
that finished. It is a separate field rather than part of `text` so that a
client can format durations itself.

`expr` uses gdb's type vocabulary rather than Ghidra's. `undefined4`, `uint` and
the name of a struct Ghidra invented all fail to parse: measured, `p *(config *
*)($rbp - 0x58)` answers `No symbol "config" in current context`. A pointer to
something unnameable therefore becomes `void *`, and anything else unnameable
becomes an unsigned integer of the right width. Both lose the type and keep the
value.

`storage` is `stack`, `register` or `none`, and the three behave differently.
`stack` is readable anywhere in the frame. `register` is readable only near
`pc`, because in optimised code the decompiler packs several variables into one
register, so a value read elsewhere will be wrong. `none` is a decompiler
temporary that exists nowhere in the machine and can never show a value; it is
reported rather than omitted, so that the row is visibly blank rather than
missing.

`expr` is a gdb expression formed from Ghidra's frame base, which is the stack
pointer at function entry, using a per-ABI rule established by measurement:
`$rbp + pointerSize` on x86-64 with a frame pointer, and `$sp + frame.size` on
MIPS64. An architecture with no established rule gets no expression rather than
a guess. See [docs/decompilation.md](decompilation.md).

`bp.setAddress` passes a name to gdb verbatim, and wraps only a bare address in
`*`. This matters because gdb skips the prologue for a named function — `break
process_packet` on a MIPS firmware stops at entry+24, past the register spills —
and `*name` would defeat that and stop on the first instruction instead.

`exec.stepLine` exists because gdb's `next` and `step` need a line table.
Without one, gdb's step range is the whole function, so stepping over in a
binary with no debug info runs to the function's exit. Measured on a
symbols-but-no-DWARF build: `break main` then `next` lands at `0x7ffff7c2a601`,
inside libc, having returned out of `main`.

It does what gdb does internally when it has a line table — single-step until
the pc reaches a different line — using Ghidra's map instead of DWARF's.

The rule is "until the pc resolves to a different line", and simpler rules do not
work. A line's address set is sparse — it holds the addresses its tokens carry,
not every instruction between them — so stepping until the pc leaves the set
ends at the
first unlisted instruction, usually the second one. A line's *span* is no good
either: a loop header's addresses wrap around the body, so its span covers the
whole loop and stepping out of it would step out of the loop. Resolving "which
line" reuses the pc marker's rule, fallback included, so an instruction between
a line's tokens maps back to that line and the walk continues.

The client sends the whole map because it already holds it, which saves the
server decompiling the function again on every step. At stepping speeds this is
a few kilobytes.

The intermediate stops are not broadcast. Ten instructions inside one decompiled
line are one step, and emitting ten `stopped` events would repaint the stack,
the locals and the registers ten times. The walk also ends on any stop that is
not `end-stepping-range`, such as a breakpoint or a signal, and on leaving the
frame it started in, so stepping over the last statement of a function stops on
return rather than continuing into the caller's addresses.

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

`busy` needs a note. gdb rejects most commands while the program runs, with
messages such as `Selected thread is running.` The server gates those requests
and returns `busy` immediately rather than forwarding them, which turns an
undocumented gdb behaviour into a documented one. The requests allowed while
running are `exec.pause`, `exec.kill`, `inferior.*`, `console.exec` and
`session.*`.

## HTTP endpoints

Both require the session cookie and both are subject to the checks in
[Security](#security).

### `GET /api/tree?path=<root-relative>`

Lists one directory level rather than walking recursively. On a large repository
a full walk is slow to produce, large to send, and mostly unread.

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

Directories sort first, then entries sort by name. `.git` and `node_modules` are
skipped. Listings are capped at 5000 entries and set `truncated` when the cap is
reached, so that a directory listing 5000 of its 9000 files says so.

A symlink is reported as a link rather than being followed or hidden. Opening
one that resolves inside the root works; one that escapes it is refused.

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

The NUL check exists so that the UI can offer a hex viewer rather than render an
ELF file as text.

## Security

gdb-wui runs arbitrary binaries with the user's full privileges, which is what a
debugger does. Sandboxing the program being debugged is not a goal. What is in
scope: other local users and processes reaching the port, hostile web pages the
user visits, and path traversal.

Binding to `127.0.0.1` is not sufficient on its own. Any page in the user's
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

The file permission is the authentication. That is sufficient here because only
the same uid can read the file, and the same uid is already fully trusted: it
can run anything as the user, which is what gdb-wui does anyway. What the scheme
protects against is the case in the threat model — another local user or an
unprivileged process reaching the loopback port. The session cookie is not
accepted on this route, so a compromised browser tab cannot mint fresh
credentials for itself.

**One gate, applied to every route including the WebSocket upgrade.**
Authorization runs before `websocket.Accept`, because Accept writes the 101
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

The list holds two kinds of symbol, which behave differently:

- **`debug: true`** — from DWARF. Carries `gdbPath` and `line`, and also `file`
  when the source resolves inside the project. Only these can be jumped to in
  the source view.
- **`debug` absent** — from the ELF symbol table. Carries only `address`. gdb
  knows where such a symbol is but not what it is, so it can be located but not
  evaluated. A function goes to the disassembly, and a variable goes to the
  memory viewer, because disassembling data produces output that looks like
  code.

`mem.read` and `disasm.function` both accept a symbol name where they accept
an address. Resolution tries the expression, then `&(expression)` — a typeless
symbol fails the first (`'LogType' has unknown type; cast it to its declared
type`) and succeeds at the second.

gdb makes the split. The server issues `-symbol-info-functions` and
`-symbol-info-variables`, each with `--include-nondebug`, and gdb sorts ELF
symbols into the code and data lists. That is where `kind` comes from for a
stripped binary, which has no debug info to consult.

`matched` counts what the filter selected before `limit` was applied, and
`available` is the size of the whole table, so a client can show "200 of 4096"
rather than presenting a truncated list as a complete one.

Addresses are trimmed of the zero padding gdb applies
(`0x0000000000001060` → `0x1060`) to match every other panel.

The cache is dropped, and `symbolsInvalidated` emitted, when a program is
loaded, when gdb is restarted, when a shared library is loaded or unloaded, and
when a console command that changes symbols is typed: `file`, `symbol-file`,
`add-symbol-file`, `core-file`, `load` or `remove-symbol-file`. The last case
matters, because the remote-target workflow loads symbols by typing `file …`
rather than through `exe.load`.

`=library-loaded` arrives dozens of times per run, so it only marks the cache
stale. The single `symbolsInvalidated` is sent just before the `stopped` event
that ends the run, so by the time a client knows the program has stopped, the
symbol table it is about to query is the new one.

### Loading symbols without a program

`exe.load` issues `-file-exec-and-symbols`, which says two things at once: these
are the symbols, and this is the program to run. Those coincide only when gdb
starts the program itself. Against a stub that cannot load an ELF, or a process
someone else started, the code is already in the target's memory and only the
first is true; declaring an exec file would also leave the UI offering to run a
second, local copy.

`symbols.load` says only the first:

| mode | command | for |
|---|---|---|
| `replace` (default) | `-file-symbol-file <path>` | an image that runs where it was linked |
| `add` | `add-symbol-file <path> [-o <offset>]` | an image that does not — the usual bare-metal case |

`offset` is a string rather than a number, because a 64-bit address does not
survive JSON's float64 and `"0x8000"` is how such an offset is normally
written. It is meaningful only with `add`, since `symbol-file` has nowhere to
put one. `add-symbol-file` has no MI form, so it goes through
`-interpreter-exec console`, the same route as `starti`.

`path` is root-relative and checked in the same way as `exe.load`'s: resolved
through the project root, and required to start with the ELF magic. Both are
allowed while the program runs, because telling gdb what addresses mean is
configuration, and it is what someone does after attaching to a running target.

`symbols.load` does not set the architecture. Only `file`, through `exe.load`,
does that, by reading the ELF header; `symbol-file` and `add-symbol-file` both
leave gdb on the host's architecture. `target remote` needs the architecture in
order to parse the stub's register reply, so a foreign target must have its ELF
loaded — or `set architecture` and `set endian` typed — before connecting.

Shared libraries for an attached process are not covered here. They need
`set sysroot`, using `target:` when the stub does file transfer and a local copy
otherwise, which is a console command like any other.

## Remote targets

`hello` carries `remote: {connected, address}` when gdb is attached to a target
this server did not start, and `remoteChanged` fires when that changes.

There is no `target.connect` request. To connect, send `console.exec` with
`target remote <address>`; to disconnect, send `console.exec` with `disconnect`.
These are the commands a user would type, so the console shows what ran and
shows gdb's own error text if a connection is refused. The UI's connect button
is a shortcut for typing rather than a separate mechanism, which keeps one
source of truth for a connection that a console command can also make or break.

The server recognises those commands by reading the command text. There is no MI
query for "am I attached to something I did not start" that avoids parsing
console output. A `target remote` is believed only if gdb accepted it, because
reporting `connected` after a refused connection would make shutdown try to
detach from something it never attached to. A `detach` or `disconnect` is
believed either way, since whatever went wrong, the connection is no longer in a
state to act on.

This matters beyond the indicator: shutting down detaches from a remote target
rather than killing it, because killing a target you connected to would destroy
someone else's session.
