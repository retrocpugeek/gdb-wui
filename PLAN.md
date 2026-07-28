# gdb-wui — a web UI for GDB

## Context

`/home/user/repo` is empty (one empty commit on `master`). This plan bootstraps the
whole project from nothing.

The need: GDB's own interfaces are a bare console or the cramped `tui` mode. Neither
shows source, disassembly, registers, the call stack, and thread state at once, and
neither lets you click a gutter to set a breakpoint. `gdb-wui` puts a multi-panel
debugger UI in the browser while leaving GDB in charge — the server translates
between GDB/MI and a WebSocket; it reimplements no debugger logic.

Outcome: run `gdb-wui --project /path/to/repo`, get a browser tab with the project's
file tree, pick an executable, click a line to break, and step through it with source,
disassembly, locals, registers, threads, and a raw GDB console all visible.

## Decisions (confirmed with the user)

| Decision | Choice |
|---|---|
| Backend | **Go** — drives gdb, browses the repository, loads binaries |
| Frontend | **Zero-build vanilla JS** — ES modules + CSS, no npm/bundler in the dev loop |
| Session model | **Single local session** — one gdb per invocation, bind 127.0.0.1, token in URL |
| v1 features | Core debug loop; variables & stack; registers, disassembly, memory; raw GDB console |
| Threading | **All-stop + thread switching** (`mi-async on` so Pause works). Non-stop is out of scope. |
| Source resolution | **Auto-map against the browsed repo** (longest trailing path suffix), then teach gdb via `substitute-path` |
| Debuggees | **C/C++ (`-g`)** and **stripped binaries** (disassembly-first). Rust/Go out of scope. |

## Environment (verified here, not assumed)

- Go **1.24.7** (`/usr/local/go/bin/go`), `proxy.golang.org` reachable. `embed.FS` and
  `os.Root` both available (`os.OpenRoot` confirmed in `/usr/local/go/src/os/root.go`).
- gdb **15.1**, `--interpreter=mi3`. `-list-features` includes `thread-info`,
  `data-read-memory-bytes`, `breakpoint-notifications`, `data-disassemble-a-option`,
  `simple-values-ref-types`, `pending-breakpoints`, `exec-run-start-option`.
- gcc 13.3 / clang 18.1.3 / make / cmake — fixtures compile with `-g`.
- Chromium at `/opt/pw-browsers` (`PLAYWRIGHT_BROWSERS_PATH` set). **Never** run
  `playwright install`.
- Deps resolve: `github.com/coder/websocket` v1.8.15, `github.com/creack/pty` v1.1.24.

### MI findings that drive the design (all reproduced against gdb 15.1)

1. **`mi-async` and `non-stop` both default to `off`.** `-gdb-show mi-async` → `"off"`.
   So `-exec-interrupt` does not work out of the box — `mi-async on` belongs in the
   startup handshake, not a later toggle.
2. **gdb rejects most commands while the inferior runs.** Verified: `-exec-continue` →
   `^error,msg="Cannot execute this command while the selected thread is running."`;
   `-stack-list-frames` and `-data-list-register-values` → `^error,msg="Selected thread
   is running."`; but `-thread-info` succeeds and reports `state="running"` with no
   `frame`. A run-state gate is structural, not a refinement.
3. **Inferior stdout interleaves into gdb's stdout, mixed with MI records.** With pipes,
   `/bin/echo GARBAGE_LINE_IN_MI_STREAM` produced a bare unparseable line between
   `^running` and `=thread-group-exited`. Two consequences: a separate inferior pty via
   `-inferior-tty-set` is **mandatory**, and the parser must tolerate garbage lines
   rather than erroring. (`@` target-stream records are for remote targets — they do not
   appear natively, so there is no cheap path here.)
4. **Build-time source paths don't resolve.** A real stop reported
   `fullname="./time/../sysdeps/unix/sysv/linux/clock_nanosleep.c"` plus
   `&"warning: ... No such file or directory"`.
5. **No-debug-info is a live path.** `/bin/ls` → `~"(No debugging symbols found)"`,
   frames with `func="??"`, `-file-list-exec-source-files` → `files=[]`,
   `-break-insert main` → `^error`. `file`/`line`/`fullname` must be **optional types**;
   `addr`/`at` are the only guaranteed frame identity.
6. **Pending breakpoints mutate.** `-break-insert -f` → `addr="<PENDING>"`, then a
   `=breakpoint-modified` supplies the real address. Breakpoint state must be event-driven.
7. **`-data-list-register-names` contains empty strings at stable indices.** Registers
   are identified by **number**, never name.
8. **The value grammar needs a real parser.** Captured shapes include repeated keys
   inside lists (`stack=[frame={…},frame={…}]`, `body=[bkpt={…},bkpt={…}]`), nested
   tuples in lists (`ranges=[{from=…,to=…}]`), and C-escaped strings.
   `encoding/json` cannot read any of it, and a `map[string]any` parser silently drops
   the duplicate keys.
9. **Single MI records get large** — a 200 KB `-data-read-memory-bytes` produced a
   16 KB single line, and `-var-list-children` on a big array goes far higher. Use
   `bufio.Reader.ReadString('\n')`, **not** `bufio.Scanner` (64 KiB default token cap
   fails in a way that looks like a hang).
10. **`-complete` exists in MI3**: `-complete "info thr"` →
    `^done,completion="info threads",matches=[…]`. Console tab-completion works over MI,
    so gdb itself never needs a pty.
11. **`-exec-run --start` injects a temporary breakpoint** visible in `-break-list`
    (`disp="del"`). The breakpoint mirror must filter breakpoints we didn't create.
12. **`=library-loaded` floods** (one per shared object) — suppress by default.

### Corrections found while implementing (gdb 17.1)

All twelve findings above were re-verified against gdb 17.1 during M1 and hold.
Three things the plan got wrong or left out, each discovered by a test:

13. **There is no `-exec-kill`.** `^error,msg="Undefined MI command:
    exec-kill",code="undefined-command"`. The `exec.kill` message is therefore a
    semantic command implemented with `-interpreter-exec console "kill"`, not a
    passthrough. (The lifecycle section already assumed console `kill`; the
    message-group list did not.)
14. **`-stack-list-frames` does not return arguments.** A stack panel showing
    `main(argc=1, argv=0x…)` needs a second command,
    `-stack-list-arguments --simple-values`, merged by frame level. It is one
    extra round-trip per stop, still inside the single fat `stopped` event.
15. **gdb reports an exit twice, and the code is on the first one.**
    `=thread-group-exited,exit-code="0"` arrives *before*
    `*stopped,reason="exited-normally"`, and only the notification carries the
    code — in octal. The two must be merged, or the UI sees a codeless exit
    followed by a redundant second event.

## Architecture

Module `github.com/retrocpugeek/gdb-wui`. Exactly two non-stdlib deps:
`coder/websocket` (no transitive deps, explicit `OriginPatterns`) and `creack/pty`.

```
cmd/gdb-wui/main.go          flags, wiring, listener, browser launch, signals
cmd/mi-repl/main.go          dev tool: MI on stdin -> canonical JSON  (also the M1 demo)
internal/mi/                 MI codec + process supervisor. ZERO domain knowledge.
internal/ptyio/              pty alloc, hold slave, read/write, EIO handling
internal/debugger/           domain layer; owns all state; single actor goroutine
internal/wire/               pure DTOs + type-name constants; imports nothing
internal/srcfs/              os.Root browser, basename index, source resolution
internal/hub/                websocket accept, per-conn pumps, dispatch, fan-out
internal/httpapi/            mux, auth/origin middleware, /api/tree /api/file
internal/assets/             embed.FS + dev-dir switch;  assets/web/** lives HERE
internal/gdbfake/            scripted fake gdb for deterministic tests
internal/testutil/           tool gates, fixture compilation, WS test client
testdata/fixtures/           hello.c threads.c structs.c interactive.c nodebug.c opt.c
docs/protocol.md             the frontend contract; test-enforced
```

Dependency direction: `mi ← debugger → wire`; `hub → debugger, wire`;
`httpapi → hub, srcfs, assets`. Note `//go:embed` cannot reach parent directories, so
the frontend tree must live at **`internal/assets/web/`** — there is no root-level `web/`.

### The MI layer (`internal/mi`) — the crux

**Spawn gdb with pipes, not a pty** (a pty echoes our commands back into the MI stream
and re-enables readline behaviour we just disabled). `SysProcAttr{Setpgid:true}` so
`Kill(-pgid, SIGKILL)` reaps gdb *and* the inferior; stderr into a 64-line ring buffer
for crash diagnostics.

Startup handshake (each awaited): `-gdb-set mi-async on` (finding 1), `non-stop off`,
`confirm off`, `pagination off`, `height 0`, `width 0`, `breakpoint pending on`,
`startup-with-shell off` (deterministic pid/pgid), `print object on`,
`print elements 200`, `print repeats 10`, `filename-display absolute`,
`-enable-pretty-printing`, `-list-features`. Child env scrubbed, `LC_ALL=C` (MI
messages are translatable), `--nx` always.

**Parser: hand-rolled** (~250 lines). The one candidate library parses into loose maps,
which structurally cannot represent finding 8. Unify tuple and list as an *ordered*
slice of optionally-named results — the only lossless representation:

```go
type Result struct{ Name string; Value Value }   // Name=="" for anonymous list elements
type Value struct {
    Kind  Kind      // KindConst | KindTuple | KindList
    Str   string    // already unescaped
    Items []Result  // tuple and list alike; order and duplicates preserved
}
func (r Results) All(name string) []Value   // v.All("frame"), v.All("bkpt")
func (v Value) U64(name string) (uint64, bool)  // ParseUint(s,0,64): "0x00000000004af4a0"
func (v Value) MarshalJSON() ([]byte, error)    // canonical JSON -> free console/raw passthrough
```

Record types: `RecResult`, `RecExec` (`*`), `RecNotify` (`=`), `RecStatus` (`+`),
`RecConsole` (`~`), `RecTarget` (`@`), `RecLog` (`&`), `RecPrompt`, **`RecGarbage`**.
`RecGarbage` is required by finding 3, not defensive padding: never drop it (surface as
console output), never panic. Strip a leading `"(gdb) "` prefix before classifying — gdb
sometimes emits the prompt without a trailing newline, gluing it to the next record.
Unescape `\n \r \t \" \\ \a \b \f \v` and octal `\NNN`, then sanitize to valid UTF-8
before anything can reach `encoding/json`.

**Correlation**: `atomic.Uint64` token written as `%d%s\n`; `pending map[uint64]chan
*Record`; a cap-1 `sendSem` serializes command→reply (GDB/MI is not concurrency-safe).
`SendUnlocked` bypasses the semaphore for `-exec-interrupt` and `-gdb-exit`, which must
work while a console `shell sleep 60` holds it. Context cancellation removes the pending
entry *and* leaves a tombstone so a late reply isn't delivered to a reused channel.
Token-0 result records are orphans (replies to console-originated activity) → route to
events, don't error.

**`(gdb) ` is not a completion delimiter — the matching `^` result token is.**

**Fan-out**: reader goroutine parses onto a cap-4096 channel; one dispatch goroutine
invokes a single `Handler`. One handler, not a broadcaster — `debugger` owns state and
`hub` owns per-connection fan-out. The queue **blocks rather than drops**: dropping a
`*stopped` desynchronizes the UI permanently, whereas blocking backpressures gdb
(correct), with a watchdog log if it lasts >1s.

**Inferior pty** (`internal/ptyio`): open a pair, `-inferior-tty-set <slave>` before
`-exec-run`. **Keep the slave fd open for the session** or `ptm` reads return `EIO` the
moment the inferior exits. Inferior bytes are arbitrary → **base64** in
`inferiorOutput`. Send `\r` for Enter (`ICRNL`); default `ECHO` gives terminal echo for
free; `ONLCR` means the frontend must handle bare CR. Treat `EIO`/`EAGAIN` as "closed".
A pty (not a pipe) is also what keeps libc line-buffered, so `printf` without a newline
appears immediately.

**Lifecycle**: graceful close = interrupt if running → `console "kill"` → `-gdb-exit` →
`Wait` with deadline → `Kill(-pgid, SIGKILL)`; `cmd.WaitDelay = 3s`. `^exit` means gdb
*accepted* exit, **not** that it's gone — only `Wait()` returning means that (teardown
records arrive after `^exit`). On EOF/crash: close `dead`, fail all pending, emit
`gdbDead` with the stderr ring buffer. No auto-restart — offer an explicit one.

### Session state (`internal/debugger`)

A **single actor goroutine** `select`s over browser requests and MI events, so all state
access is lock-free and event ordering is deterministic and reproducible in tests. Two
paths deliberately bypass it: `exec.pause` (must work *while* the loop blocks on a gdb
round-trip) and `inferior.stdin` (straight to the pty master).

- **Run-state gate**: while `Running`, state queries return `{"code":"busy"}` immediately
  instead of forwarding. This turns finding 2's cryptic errors into a documented
  contract. Allowed while running: `exec.pause`, `exec.kill`, `inferior.*`,
  `console.exec`, `session.*`.
- **`stopSeq`** increments on every `*stopped`. Every request carries it; every response
  echoes it; stale responses are dropped. This one mechanism solves varobj staleness,
  race-on-refresh, and double-click-step together.
- **Never rely on gdb's selected thread/frame for programmatic commands** — pass
  `--thread N --frame M` explicitly. *Also* issue `-thread-select`/`-stack-select-frame`
  on selection change, for the benefit of raw console commands the user types.
- **Breakpoint mirror** reconciled from `-break-list` + `=breakpoint-*`, filtering
  breakpoints we didn't create (finding 11).

**Varobjs** — the three designs disagreed; this is the resolution:

- **No varobj for the flat locals list.** `-stack-list-variables --simple-values` returns
  `value` only for simple types, so *absence of `value` is the "expandable" signal* —
  exactly what the tree needs, and it's also the 100k-array defense. Change highlighting
  comes from diffing the previous cached result for the same frame identity.
- **Varobjs only for user-expanded subtrees and watch expressions**, and they **persist
  across stops** (refreshed with one `-var-update --all-values *`). Persisting is what
  keeps node ids stable so the frontend's expansion state survives stepping, which is
  the single most important usability property of that panel.
- Leaks are contained by a **512-root LRU** with real `-var-delete` on eviction,
  `in_scope="false"` / `type_changed="true"` handling, delete-all on re-run/`exe.load`,
  and a test asserting the registry is empty after `exec.run`.
- Roots get explicit names (`r17`) so deletion is deterministic; children keep
  gdb-assigned names (deleting a root deletes its children). Locals bind to a frame
  (`-var-create r17 * --thread T --frame F expr`); watches are floating (`@`) so they
  follow the current frame.
- Frame identity for cache reuse is `{threadID, frameLevel, funcName, stackDepth}` —
  **not** `frame.addr`, which is the PC and changes while stepping inside one frame.
- Child paging via `-var-list-children … NAME 0 200` honoring `has_more`, so
  `char buf[1<<20]` doesn't become a 40 MB message.

### Wire protocol

**WebSocket + JSON for everything stateful; HTTP GET for bulk reads.** Inferior stdin
needs a low-latency ordered client→server byte channel (POST-per-keystroke is a
round-trip each); one connection is one client identity, which the idle-exit lifecycle
needs. But **source text and the file tree go over HTTP** with ETags, so a 2 MB file
fetch doesn't sit in front of latency-sensitive stepping traffic and inferior output.

```jsonc
{"id":17,"type":"exec.step","payload":{"thread":1}}                    // req
{"id":17,"ok":true,"type":"exec.step","payload":{"runState":"running"}} // res
{"id":17,"ok":false,"type":"exec.step","error":{"code":"busy","message":"…"}}
{"event":"stopped","seq":412,"payload":{…}}                            // push
```

`seq` is server-monotonic so the frontend detects gaps and tests assert ordering.
Error codes are a closed set: `bad_request unsupported not_ready busy gdb_error
gdb_dead timeout path_denied not_found too_large internal`. Unknown `type` →
`unsupported`, never a connection close. WS read limit 1 MiB.

**Exec responses are acknowledgements, not completions** — `-exec-continue` returns
`^running` immediately; the stop arrives as an event. Documented so the frontend is
event-driven by construction.

**Semantic discrete command types, not raw MI.** Raw MI from the browser would
desynchronize the varobj registry, breakpoint mirror, and run-state gate. The escape
hatch is `console.exec {line}` → `-interpreter-exec console "…"`, streaming `~`/`&`
as `console` events, **followed by a server resync** (`-break-list`, `-thread-info`,
re-read selection) because `b main.c:12` / `next` / `thread 2` from the console change
state behind us. `console.complete {prefix}` → `-complete` (finding 10) gives Tab
completion. A `mi.exec` type exists only behind `-unsafe-raw-mi`, off by default.

Message groups: `session.*`, `exe.load/unload`, `exec.*` (run/continue/pause/step/next/
stepi/nexti/finish/until/return/kill), `bp.*` (setSource/setFunction/setAddress/
setWatch/delete/setEnabled/setCondition/setIgnoreCount/list), `threads.list`,
`thread.select`, `stack.list`, `frame.select`, `vars.locals/expand/setFormat/assign`,
`eval.expr`, `watch.*`, `regs.names/values`, `disasm.function/range`, `mem.read`,
`console.exec/complete`, `inferior.stdin/signal/resize`, `path.substitute/addDir/list`.

Events: `hello stopped running exited exeLoaded threadCreated threadExited
selectionChanged breakpointsChanged varsInvalidated console inferiorOutput progress
gdbDead shuttingDown error`. `libraryLoaded` exists but is suppressed by default
(finding 12). `hello` is pushed on connect with a **full snapshot** (gdb version,
features, runState, stopSeq, threads, breakpoints, watches, selection) so reconnect,
page reload, and a second tab all work for free — this is a day-one decision, a rewrite
if deferred.

**Fat `stopped` event.** On each stop the server eagerly gathers `-thread-info`,
`-stack-list-frames --thread T 0 63`, and frame-0 locals, resolves source paths, and
emits **one** event. This removes 4–5 round-trips per single-step — the difference
between stepping feeling instant and laggy. Registers, disassembly, and memory are
**not** eager; those panels pull lazily and pass `stopSeq`.

Enumerate stop reasons with passthrough for unknown: `breakpoint-hit`,
`watchpoint-trigger`, `watchpoint-scope`, `function-finished` (carries `return-value`),
`end-stepping-range`, `location-reached`, `signal-received`, `solib-event`,
`exited-normally`, `exited` (`exit-code`), `exited-signalled`.

Two browser tabs: because the server is authoritative and snapshots on connect,
**all tabs are writers**. Last command wins; the run-state gate makes a concurrent
`-exec-continue` safe. Cheaper and more useful than a takeover lock.

### Repo browsing, binary loading, source resolution (`internal/srcfs`)

**Containment: `os.Root`, chosen over `filepath.Abs` + prefix check.** It resolves
per-component with `openat`, so `..` cannot escape *and* a symlink inside the root
pointing at `/etc/shadow` is rejected — which is exactly what defeats a
`strings.HasPrefix` check, along with TOCTOU races. Every API path is a root-relative
slash path validated with `fs.ValidPath` before touching `root.Open`. **No manual
`filepath.Join` with user input anywhere** — a lint-able invariant.

- `GET /api/tree?path=&depth=1` — one level at a time, lazily (a recursive monorepo walk
  is slow and huge). Skip `.git`/`node_modules`, cap 5 000 entries with `truncated:true`.
- `GET /api/file?path=` — `text/plain`, ETag `size-mtime-inode`, 2 MiB cap → 413,
  NUL-byte sniff → 415 so the UI offers the hex viewer instead of rendering garbage.
- `exe.load` over WS (stateful): validate regular file + `\x7fELF` magic → fast clear
  error instead of a cryptic gdb one; then `-file-exec-and-symbols`, `-exec-arguments`,
  `-environment-cd`, `-data-list-register-names`.

**Source resolution**, per reported frame, in order:
1. `fullname` exists and resolves in-root → use it.
2. Match against a lazily built basename index by **longest trailing path-component
   count** (basename alone silently picks the wrong `util.c`). On a clear winner, use it
   **and install `-gdb-set substitute-path <from> <to>`** — fixing it in gdb once makes
   every future frame, plus `list`, `info line`, and console commands correct. Per-file
   mapping in the UI is a losing game.
3. Otherwise `{sourceAvailable:false, gdbPath:…}` → the UI shows disassembly plus a
   "locate source" affordance wired to `path.substitute`/`path.addDir`.

Also `-environment-directory <projectRoot>` at load. For legitimate out-of-root source
(libc, `/usr/include`), a prefix check gives a bad answer; use a **capability set**
instead — `knownSourcePaths` populated *only* from paths gdb itself reported, served by
`GET /api/gdbsource?path=<abs>`, disable-able with `-no-system-source`. If a source
file's mtime exceeds the binary's, return `stale:true` so the UI can warn that line
numbers are lying.

### Security

Threat model stated plainly: gdb-wui runs arbitrary binaries with the user's full
privileges — that *is* the product. Sandboxing the debuggee is **not** a goal. In scope:
other local users/processes reaching the loopback port, hostile web pages the user
visits, and path traversal. Binding 127.0.0.1 is **not** sufficient on its own: any page
can `fetch` loopback, which for this service means RCE.

- Listen `127.0.0.1:0`; `-addr` refuses non-loopback unless `-listen-anywhere` (loud warning).
- **Bootstrap token → session cookie.** Two 32-byte `crypto/rand` secrets. The bootstrap
  token is single-use, 60 s TTL, and is the only one that ever appears in a URL —
  because the URL goes to `xdg-open`, so it lands in argv (world-readable via `ps`),
  browser history, and `Referer`. `GET /?t=<bootstrap>` validates constant-time,
  invalidates, sets `Set-Cookie: gdbwui=…; HttpOnly; SameSite=Strict`, and 303s to `/`.
  The session token never appears in a URL or argv.
- One `authMiddleware` wraps **every** route including the **WebSocket upgrade, before
  `websocket.Accept`** — the easy one to forget. Cookies (not headers) are required
  because the browser `WebSocket` API cannot set headers.
- **Anti-rebinding, three layers, each covering a different corner:** `Host` must exactly
  match `127.0.0.1:PORT`/`localhost:PORT`/`[::1]:PORT` (a rebound page arrives with
  `Host: evil.example`); `Origin`, required on `/ws` and non-GETs, must match; and
  `Sec-Fetch-Site: cross-site` → reject. Under rebinding the page's origin stays
  `http://evil.com`, so the cookie isn't sent *and* Origin exposes it. Set
  `OriginPatterns` on `websocket.Accept` too, so behaviour never depends on a library default.
- No CORS headers ever. `Content-Type: application/json` required on POSTs.
  `nosniff`, `Referrer-Policy: no-referrer`, `Cross-Origin-Resource-Policy: same-origin`,
  and a CSP of `default-src 'none'; script-src 'self'; style-src 'self' 'unsafe-inline';
  img-src 'self' data:; connect-src 'self' ws://127.0.0.1:PORT; frame-ancestors 'none'`.
  (`'unsafe-inline'` for styles only, because xterm injects a `<style>` element at
  runtime — verify in M4 and drop it if it turns out to be unnecessary.)
- `ReadHeaderTimeout: 5s`, `MaxHeaderBytes: 1<<16`, `MaxBytesReader` on bodies.
  Log only the first 4 chars of any token. `xdg-open` with fixed argv, never a shell;
  always print the URL to stdout as the primary path.

### Frontend (`internal/assets/web/`)

Serving: `//go:embed all:web` (`all:` so `_`/`.`-prefixed vendored files are included);
`-assets-dir` swaps in `os.OpenRoot(dir).FS()` so editing JS/CSS needs only a reload —
that's the zero-build dev loop. `mime.AddExtensionType(".mjs","text/javascript")` in
`init()`, or browsers refuse the modules. `embed.FS` has no mtimes, so hash the tree at
startup for a strong ETag; dev mode sends `no-store`. No SPA catch-all — real file paths.

**Vendored deps** (`web/vendor/<name>-<version>/`, byte-identical, each with `LICENSE`):

| File | Size | Note |
|---|---|---|
| `@xterm/xterm@6.0.0` `lib/xterm.mjs` + `css/xterm.css` | 344,970 B + 7,112 B | **Verified**: zero bare imports, ends `export{Dl as Terminal}`. Don't vendor the 1.7 MB sourcemap. |
| `@xterm/addon-fit@0.11.0` `lib/addon-fit.mjs` | 1,967 B | Hand-rolling it means depending on the same private internals with no maintenance. |
| `@highlightjs/cdn-assets@11.11.1` `es/core.min.js` + `es/languages/{c,cpp}.min.js` | 20,445 + 4,177 + 6,040 B | **Trap, verified**: the plain `highlight.js` package's `es/core.js` is a 204-byte Node dual-package wrapper importing CJS — it will not load in a browser. Must use `cdn-assets`. |

`VENDOR.md` records package, version, tarball URL, sha256, license, and a curl+tar
refetch recipe (no npm). A Go test recomputes every hash and fails on mismatch — that is
the entire supply-chain story for a zero-build repo, in ~40 lines. **Nothing else is
vendored**: no docking lib (CSS grid + ~70 lines of pointer events), no icon font
(inline SVG sprite), no webfont, no util libs.

Layout: CSS grid whose track sizes are custom properties that splitters mutate;
regions `toolbar / left (file tree) / center (tabs: source | disasm) / right (vars|watch|
registers over stack|threads|breakpoints) / bottom (gdb console | inferior | memory |
state) / status`. Tabs, not free docking — a debugger user wants panels reachable in one
keystroke, not arbitrarily arrangeable. Every panel root gets
`overflow:hidden; min-width:0; min-height:0` (the grid footgun that otherwise lets long
lines blow the layout out) plus `contain: layout paint`.

State: one observable store; writes only via `update`/`patch`, each recording dirty
dotted paths and bumping a per-slice version; **notification deferred to a
`queueMicrotask`** so one `stopped` event touching six slices notifies once; rendering is
a **single rAF pass** over visible dirty panels — nothing renders inside a WebSocket
handler, which is what makes held-down-F10 survivable. `subscribe(paths, cb)` matches by
path prefix, so the source panel does a full rebuild on `source.path` but on
`source.execLine` alone just moves one CSS class (O(1) — the step hot path).
**Staleness guard**: every request captures `(stopSeq, threadId, frameLevel)` and one
`guard()` helper drops mismatched responses. Hidden panels never issue gdb round-trips;
they refresh in `onShow()`.

Source view — **hand-written virtualization**: fixed row height, pooled row nodes,
`translateY` inside a sizer. One node per line is 100k+ nodes for a 20k-line file;
`content-visibility` still creates all of them and drifts the scrollbar. Each row is a
2-column grid with the gutter cell `position:sticky; left:0`, giving a frozen gutter with
horizontal code scroll in pure CSS and gutter click targets aligned by construction. No
soft wrap (fixed `--line-h` is load-bearing; verify it at boot against a measured probe
and warn on mismatch). Decorations are class toggles, never re-renders.
`revealLine` has a jitter guard (skip if already in the middle 60%) so stepping doesn't
bounce the view. Known limitation, stated rather than faked: native selection breaks
past the window — provide Copy line / Copy `file:line` / raw-file link.

Highlighting composes with per-line DOM via a **whole-file tokenize + span-repair
split**: hljs v11 removed the public continuation parameter, so per-line calls
mis-colour block comments and raw strings. Highlight once, then walk the HTML
maintaining a stack of open classes and at each `\n` close and re-open them — output is a
`string[]` index-aligned with source lines, each line independently `innerHTML`-able,
which is exactly what virtualization needs. Run it in a **module Web Worker** importing
the same vendored ESM (zero build needed — the killer argument for the ESM builds);
plaintext renders instantly and the worker result upgrades the visible window. Skip above
~200k lines with a "highlighting off" pill.

Disassembly and hex memory reuse the same virtual list. **Hand-roll the disasm
tokenizer** (~40 lines, 4 token classes): hljs's `x86asm` grammar targets assembler
*source* and mis-parses objdump-style columns for 19 KB. Hex rows are *computed*
(`base + r*bytesPerRow`), so a 1 GB region is free; bytes live in a sparse LRU of 4 KB
chunks, one coalesced `mem.read` per render pass, missing bytes render `??`.

Variables tree: **flattened visible-row array + keyed reconcile**, on the virtual list
from day one (arrays of structs *will* produce thousands of rows). Expansion is keyed by
**stable expression path** (`local/cfg->items[3].name`), not varobj id, so it survives
even if a subtree is recreated; re-expansion after a frame change is breadth-first with
capped concurrency, all `stopSeq`-guarded.

Console: **`line` mode over MI** — a ~120-line local line editor over xterm with history,
plus Tab completion via `-complete` (finding 10). The alternative "give gdb a pty"
proposal is rejected: it conflicts with MI-over-pipes and reintroduces command echo.
Two **separate** terminals (gdb console, inferior) — interleaving them is the most
confusing thing in existing web debuggers.

Keyboard: one capture-phase dispatcher keyed by focus context. F5 continue, F6 interrupt,
F10 next, F11 step, Shift+F11 finish, Alt+F10/F11 nexti/stepi, F9 toggle breakpoint,
Ctrl+G goto, Ctrl+P quick-open, Alt+↑/↓ frame, Ctrl+Alt+↑/↓ thread. **Composition rule
with terminals**: inside `.xterm` or an input, only function keys and `Ctrl+Shift+*` are
intercepted — `Ctrl+C`, `Ctrl+D`, arrows, and Tab reach the terminal, because Ctrl+C to
the inferior is a real debugging need. Escape leaves the terminal; the status bar always
shows `focus:<context>`. Exec actions require `stopped` and no in-flight exec request, so
holding F10 yields at most one step per completed stop.

Styling: `tokens.css` is the only file with colour literals (a Go test greps for `#hex`
outside it); `layout.css` owns geometry and `panels.css` appearance; light theme
overrides only token names. `hljs-tokens.css` maps the ~15 classes the C/C++ grammars
emit onto the same `--tok-*` variables, so source, disasm, and hex colours agree; xterm's
theme is built at boot from `getComputedStyle` on those tokens.

## Milestones

Risk front-loaded; each is demonstrable in a minute and reviewable as one PR.

**M1 — MI core, no server.** `internal/mi` parser + client + `gdbfake` tests +
`cmd/mi-repl`. *Done:* `go test ./internal/mi/...` green including the fuzz seed corpus;
`mi-repl` prints canonical JSON for real gdb output piped through it.

**M2 — Server skeleton + security + assets + repo browsing.** Listener, bootstrap→cookie
flow, Host/Origin/Sec-Fetch middleware, `embed.FS` + `-assets-dir`, `/api/tree`,
`/api/file`, `/ws` with the envelope and a real `hello`. *Done:* the browser renders a
file tree and file contents; the security matrix test passes; `curl` with a wrong `Host`
gets 403.

**M3 — The vertical slice: one line, one breakpoint, end to end.** `exe.load`,
`exec.run/continue/step/next/finish`, `bp.*` + gutter markers, `stack.list`, the fat
`stopped` event, the run-state gate, `stopSeq`, source-resolution seam, the virtualized
source view, and a raw MI log pane. *Done:* click a gutter line, run, hit the breakpoint
with the line highlighted, F10 through it, run to exit; **and reload the browser
mid-session to get the same stopped state back** (proves the authoritative-server
snapshot). *This is the architecture proof — if it takes more than a week, cut M7 and
the inferior-pty half of M5 immediately rather than discovering the overrun at the end.*

**M4 — Values: watch first, then locals tree.** Watch expressions (cheap, and the escape
hatch that rescues you when locals disappoint on `-O2` code), then flat locals via
`--simple-values`, then on-demand expansion with the varobj registry, LRU, and
`expandedKeys` re-expansion. Registers with change highlighting. *Done:* expand a nested
struct in `structs.c`, watch `argv[1]`, see `<optimized out>` rendered honestly in an
`-O2` fixture, and confirm the varobj registry is empty after a re-run.

**M5 — Console + inferior I/O + threads.** `console.exec` with `~`/`&` streaming,
`-complete` tab completion, post-command resync; then the pty for inferior stdin/stdout;
`exec.pause`; thread list and switching with per-thread stacks. *Done:* type into
`interactive.c` from the browser and see line-buffered output; Ctrl-C a spinning loop and
land on a real frame; switch among three threads in `threads.c` and see distinct stacks.

**M6 — Machine level.** Disassembly (capability-gated `-data-disassemble -a`, hand-rolled
tokenizer, PC marker, lazy window extension, src+asm mode), `stepi`/`nexti`, source↔disasm
sync. *Done:* full instruction-level debugging of the **stripped** fixture, where it is
the only available view.

**M7 — Memory viewer.** Ship the minimal version first: an expression → one window,
read-only, refreshed on stop, holes as `??`. Extend to scrolling chunks only if M3–M6
came in on time. *Cuttable — this is the first thing to drop if over budget.*

**M8 — Hardening and hygiene.** Source resolution for real (substitute-path + suffix
matching + "locate file" UI), no-debug-info degradation audited across every panel,
big-value/big-file guards, backpressure limits, splitters + persisted layout, reconnect
resync, idle-exit, `gdbDead` + restart, light theme, ARIA roles, README/LICENSE/CI.

Natural stopping points: **M3** is already a usable tool; **M4** is ~90% of what people
use a debugger GUI for.

## Testing

- **`internal/mi` — thorough, table-driven, over real captured MI.** Commit the corpus
  already captured during planning: the escaped-quote `^error`, `BreakpointTable` with
  `body=[bkpt={…},bkpt={…}]`, `stack=[frame={…},…]`, `register-names` with empty entries,
  both `-data-disassemble` shapes, `memory=[{…contents="…"}]`, empty lists,
  `&"warning: 78\t…"` (tab + octal), `ranges=[{from=…,to=…}]`, the literal
  `GARBAGE_LINE_IN_MI_STREAM` case, and a `(gdb) `-glued-to-next-record case. Golden JSON
  via `Value.MarshalJSON` with a `-update` flag. Plus `FuzzParseRecord` (`go test -fuzz`,
  stdlib, ~30 lines — a hand-written parser over C escaping is exactly what fuzzing is
  for), a parse→serialize→parse round-trip, and a 4 MiB single-line test (finding 9).
- **`internal/gdbfake` — the highest-value test investment, and easy to skip.** A scripted
  fake speaking MI over pipes makes the state machine, command gate, and reconnect logic
  deterministic in milliseconds, and reaches what real gdb won't reproduce on demand:
  `*stopped` arriving between a command write and its reply, gdb dying mid-command with a
  partial line, orphan token-0 results, a 100k-element response, and the exact
  `"Selected thread is running."` error. Make `mi.Options` take `Stdin io.Writer, Stdout
  io.Reader` so no process is needed. Transcript files (`>` input, `<` output) keep
  dialogs readable. Also add a `-mock` server flag replaying recorded sessions, so the
  frontend can be developed and tested with no gdb at all.
- **`internal/srcfs`** — real symlink/traversal fixtures in `t.TempDir()`: in-root symlink
  to `/etc/passwd` (must fail), `..`, absolute path, `%2e%2e` post-decode; plus
  suffix-matching tests using the captured `./time/../sysdeps/…` shape.
- **`internal/httpapi`** — a table-driven security matrix, the highest-value single test in
  the repo: no credential → 401; cookie → 200; bootstrap reuse → 401; `Host: evil.com` →
  403; `Origin: http://evil.com` → 403; `Sec-Fetch-Site: cross-site` → 403; `/ws` upgrade
  with no cookie → 401.
- **Integration, `//go:build integration`** — 8–10 tests over fixtures compiled at test
  time (`hello.c`, `threads.c`, `structs.c`, `interactive.c`, `nodebug.c` stripped,
  `opt.c` at `-O2`). Hermeticity: always `--nx` (a contributor's `.gitignore`-invisible
  `~/.gdbinit` would otherwise break tests and get blamed on us), `HOME=t.TempDir()`,
  `LC_ALL=C`, `t.Context()` with a 30 s timeout, `t.Cleanup` killing the *process group*,
  and a `RequireTools(t,"gdb","gcc")` gate that also checks the version floor and
  `-list-features` and **skips** rather than fails. Parallel behind a semaphore of 4
  (ptys are finite). A `-update-transcripts` flag records real MI back into the parser
  corpus, closing the loop.
- **Frontend.** `node --check` over every `web/js/**/*.js` in CI — zero dependencies,
  ~1 s, and it catches the classic zero-build failure where one typo yields a blank page
  and green CI. Then an in-browser `web/test/index.html` + ~30-line `tinytest.js` for the
  pure logic that actually breaks (store dirty-path matching and microtask batching, the
  span-repair line splitter against golden fixtures, virtual-list index math, varobj
  flatten/re-expand, hex arithmetic, keymap dispatch). A Go test asserts every `<script
  src>`/`<link href>` in `index.html` resolves inside `embed.FS`, and another greps import
  direction (`core/*` must not import `panels/*`).
- **Playwright: one spec, in `e2e/` with its own `package.json`, not a PR gate.** It's the
  only thing that proves a real browser reaches "breakpoint hit, line highlighted", and
  Chromium is already installed — but gating PRs on it means every contributor needs npm
  and every flake blocks merges, which is a bad trade for a project whose selling point is
  `go build` and done. `PLAYWRIGHT_BROWSERS_PATH=/opt/pw-browsers`; never `playwright
  install`. CI skips it when `node_modules` is absent.
- Everything under `-race`. A goroutine-leak check polls `runtime.NumGoroutine` after
  `Close` rather than adding `goleak`. A docs-honesty test asserts every dispatch-table
  `type` appears in `docs/protocol.md`.

## Explicitly out of scope for v1

State these in the README so the plan is honest rather than aspirational: remote
targets/`gdbserver`, core dumps, attach-to-pid, **non-stop mode and per-thread run
control**, reverse debugging/`rr`, non-C/C++ debuggees (no Rust/Go), TUI/curses debuggees
and full terminal emulation, multi-user/auth/TLS, follow-fork and multi-inferior,
catchpoints/tracepoints/hardware watchpoints, **all writing** (no source editing, no
register writes, no `-var-assign` beyond the locals inline edit, `mem.write` deferred),
session persistence, and Windows/macOS (the pty and `/proc` assumptions won't port free).

## Repo hygiene

- **README**, one page: what it is; **the security warning in the first screenful** (runs
  arbitrary binaries, loopback only, never expose); quickstart; a supported/not-supported
  table lifted from the section above (this prevents issues you'd otherwise triage);
  requirements (gdb ≥ 10 with `mi3`, Linux/x86-64); a five-line architecture note. Skip
  badges and a roadmap.
- **LICENSE: Apache-2.0.** Worth one README paragraph: gdb is GPLv3, but gdb-wui only
  *spawns* it as a separate process and speaks a documented protocol, so there's no
  derivative-work obligation. Project rule: never link libgdb, never embed gdb source,
  never ship a gdb binary.
- **CI**, one workflow on `ubuntu-latest`: `gofmt -l` + `go vet`; `golangci-lint` (already
  at `/usr/local/bin/golangci-lint`) with a minimal linter set (`govet staticcheck
  ineffassign unused errcheck` — no `gocyclo`, no `lll`); `go test -race ./...`; then
  `apt-get install -y gdb` and `go test -tags integration -race ./...` (don't rely on gdb
  being preinstalled — it's seconds and makes the workflow self-documenting; runners have
  `ptrace_scope=1`, which permits tracing a *child*, so `-exec-run` works while
  attach-to-pid wouldn't, another reason it's out of scope); `node --check` over
  `web/js/**`; the `VENDOR.md` hash test. Fuzzing on a nightly schedule only.
- `Makefile` with `build test test-integration lint fixtures run vendor-verify`.
- `.gitignore`: `/gdb-wui`, `*.test`, `coverage.out`, compiled fixtures, `/e2e/node_modules`,
  `.mi-log*`. Not a 200-line boilerplate file. `CONTRIBUTING.md` deferred until there's a
  second contributor.

## Verification

1. `go test -race ./...` — parser, fake-gdb state machine, srcfs containment, security matrix.
2. `go test -tags integration -race ./...` — the real gdb, all six fixtures.
3. `go run ./cmd/gdb-wui --project . --dev` and walk the **M3 demo script** by hand:
   file tree → pick `hello` → click gutter line → Run → breakpoint hits with the line
   highlighted → F10 → Continue → "exited (0)" → **reload the page and confirm the same
   stopped state returns**.
4. Adversarial pass, in the app: the **stripped** fixture (source pane must offer
   disassembly, not look broken); the `-O2` fixture (`<optimized out>` shown honestly,
   non-monotonic line jumps tolerated); `interactive.c` (type into it, see unbuffered
   output); a spinning loop (Interrupt lands on a real frame, with the pending state
   visible if it's slow); `kill -9` the gdb process externally (UI reports `gdb-dead`, not
   a spinner); a 20k-line source file (scrolls smoothly, gutter stays aligned); two
   browser tabs at once.
5. `curl` the security matrix by hand: no cookie, wrong `Host`, cross-site `Origin`,
   bootstrap-token reuse — all rejected.
6. `make vendor-verify` and `node --check` over the frontend modules.
