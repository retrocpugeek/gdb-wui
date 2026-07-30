# gdb-wui

A web UI for GDB. Source, disassembly, variables, registers, memory, threads and
a real gdb console in a browser tab — with GDB itself still in charge.

```sh
go build ./cmd/gdb-wui
./gdb-wui -project /path/to/your/repo
```

It prints a URL and opens a browser at it. Pick an executable from the file tree,
click a line number to set a breakpoint, and step.

> ## ⚠ It runs your programs as you
>
> gdb-wui starts arbitrary binaries with your full privileges. That is what a
> debugger does, and sandboxing the debuggee is **not** a goal.
>
> It listens on loopback only, and refuses a non-loopback address unless you pass
> `-listen-anywhere`. **Never expose it to a network you do not control.** Anyone
> who can reach the port can run programs as you.
>
> Binding loopback is not by itself enough — any web page you visit can `fetch` a
> loopback URL — so access needs a single-use login link, and requests are
> checked against DNS rebinding three separate ways. See
> [docs/protocol.md](docs/protocol.md#security).

## Why

GDB's own interfaces are a bare console or the cramped `tui` mode. Neither shows
source, disassembly, registers, the call stack and thread state at once, and
neither lets you click a gutter to set a breakpoint.

gdb-wui is a translator, not a debugger: it speaks GDB/MI to a real gdb process
and a small JSON protocol to the browser. It reimplements no debugger logic, so
what you get is gdb's behaviour with a better view of it.

## Requirements

- **Linux, x86-64.** The pty and process-group handling do not port for free.
- **gdb ≥ 10** with the `mi3` interpreter (17.1 is what it is developed against).
- **Go ≥ 1.24** to build. Two dependencies, no npm, no bundler.

## Getting a link

The login link is single-use and expires after 60 seconds — it ends up in argv,
where `ps` can read it, and in browser history. For another:

```sh
./gdb-wui -print-url
```

That mints a fresh one against the *running* server, so your gdb session and
breakpoints survive.

## Usage

| Flag | What it does |
|---|---|
| `-project DIR` | The directory to browse. Nothing outside it is served. |
| `-exe PATH` | Load a program at startup, relative to `-project`. |
| `-addr ADDR` | Listen address; must be loopback (default `127.0.0.1:0`). |
| `-no-gdb` | Browse the project without starting a debugger. |
| `-assets-dir DIR` | Serve the frontend from disk — reload is the whole dev loop. |
| `-mi-log` | Stream raw MI traffic to the browser's log pane. |
| `-idle-exit DUR` | Exit after this long with no browser connected. |
| `-print-url` | Print a fresh login link for a running server and exit. |

Keys: **F5** continue, **F6** pause, **F9** toggle breakpoint, **F10** step over,
**F11** step into, **Shift+F11** step out, **Alt+F10/F11** instruction step,
**Ctrl+F5** run, **Ctrl+Shift+F5** run to `main`.

Inside a terminal panel only function keys and `Ctrl+Shift+…` are intercepted, so
`Ctrl+C`, `Ctrl+D`, Tab and the arrows reach your program.

## Supported

| Works | Not supported |
|---|---|
| C and C++ with `-g` | Rust, Go, or any other language |
| Stripped binaries, disassembly-first | Remote targets and `gdbserver` |
| Multiple threads, all-stop, thread switching | Non-stop mode, per-thread run control |
| Breakpoints by source line, conditions | Watchpoints, catchpoints, tracepoints |
| Locals, nested structs, watch expressions | Editing values, register writes, memory writes |
| Registers, disassembly, memory (read-only) | Reverse debugging, `rr` |
| The gdb console, with tab completion | Core dumps, attach-to-pid |
| A program with its own terminal | Full terminal emulation for curses programs |
| Several browser tabs on one session | Multi-user, auth beyond loopback, TLS |
| | Follow-fork, multi-inferior |
| | Windows, macOS |

A program built elsewhere reports source paths that do not exist here. gdb-wui
matches them against your tree by longest trailing path component and teaches gdb
the prefix; when the match is ambiguous it asks rather than guessing, because
showing the wrong file with plausible line numbers is worse than showing none.

## Architecture

Five layers, each ignorant of the ones above it:

- `internal/mi` — GDB/MI codec and process supervisor. No domain knowledge.
- `internal/debugger` — all session state, behind a single actor goroutine.
- `internal/hub` + `internal/httpapi` — the WebSocket protocol and HTTP surface.
- `internal/srcfs` — the project, browsed through `os.Root` so nothing escapes.
- `internal/assets/web` — zero-build ES modules; the only vendored code is
  xterm.js, hash-verified in
  [VENDOR.md](internal/assets/web/vendor/VENDOR.md).

The protocol is documented in [docs/protocol.md](docs/protocol.md), and a test
fails if that document and the code disagree.

## Development

```sh
make test              # go vet + race tests + frontend checks
make test-integration  # the same, plus tests against a real gdb
make run               # serve this repo with assets from disk
```

## Licence

Apache-2.0. See [LICENSE](LICENSE).

gdb is GPLv3, but gdb-wui only *spawns* it as a separate process and speaks a
documented protocol to it, so there is no derivative-work obligation. The project
rule that keeps it that way: **never link libgdb, never embed gdb source, never
ship a gdb binary.**
