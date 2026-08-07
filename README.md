# gdb-wui

A web UI for GDB. Source, disassembly, variables, registers, memory, threads and
a real gdb console in a browser tab — with GDB itself still in charge.

```sh
go build ./cmd/gdb-wui
./gdb-wui -project /path/to/your/repo
```

It prints a URL and opens a browser at it. Pick an executable from the file tree,
click a line number to set a breakpoint, and step.

Clicking another ELF while a program is being debugged asks first — loading one
replaces the inferior, and a stray click on the wrong row would otherwise throw
away a live session.

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
  A stock `gdb` only knows the host architecture; for a foreign target install
  `gdb-multiarch` and point `-gdb` at it.
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
| `-listen-anywhere` | Permit a non-loopback address. Read the warning above first. |
| `-gdb PATH` | Which gdb to run (default `gdb`). Use `gdb-multiarch` for a foreign architecture. |
| `-no-gdb` | Browse the project without starting a debugger. |
| `-open` | Open a browser at the URL (default true; `-open=false` to suppress). |
| `-assets-dir DIR` | Serve the frontend from disk — reload is the whole dev loop. |
| `-mi-log` | Stream raw MI traffic to the browser's log pane. |
| `-idle-exit DUR` | Exit after this long with no browser connected. |
| `-print-url` | Print a fresh login link for a running server and exit. |
| `-ghidra DIR` | Ghidra installation, to enable decompilation. Defaults to `$GHIDRA_INSTALL_DIR`, then the usual locations. |
| `-ghidra-project PATH` | An existing Ghidra project to read, **opened read-only** — your names and types, never written to. |
| `-ghidra-program NAME` | Which program inside that project. Required with `-ghidra-project`: a real project holds several. |
| `-decomp-dir DIR` | Where to cache projects gdb-wui creates itself (default `<project>/.gdb-wui/ghidra`). |

The **Symbols** pane under the file tree lists the loaded program's functions
and globals. Type in the filter box to narrow it, and double-click a symbol to
jump: to the source line if it has debug info, to the disassembly if it is a
function with only an address, or to the memory viewer if it is a variable with
only an address. `fn` and `var` sigils say which is which, and dimmed rows are
the ones with no debug info. It works on a stripped binary, where the ELF symbol
table is the only map you have.

**Right-click an ELF** in the file tree for the three things you can do with
one: *Load program* (`file` — the program to run, and the only thing that sets
the architecture), *Replace symbols* (`symbol-file`), and *Add symbols…*
(`add-symbol-file` with an offset, for an image that does not run where it was
linked).

In the source view the green bar is the program counter and a blue one marks an
outer frame you have selected in the call stack, so inspecting a caller never
hides where the program actually stopped.

**Rest the pointer on something to see what it holds.** In the source view that
is a variable, and the whole path is read, not just the word: point at `name`
in `cfg.items[2].name` and you get that field, not a field name out of context.
In the disassembly it is a register — `%rax` on x86, a bare `r0` or `sp`
elsewhere — or the symbol in a `<add+4>` annotation. Integers are shown in the
other base alongside, because a stack pointer as 140737488347136 is a number
and as `0x7fffffffe000` is an address you recognise. Values are read from the
frame selected in the call stack, and the tooltip goes as soon as the program
moves, so it can never show you a value from the previous stop.

Only names, fields and subscripts are evaluated. `f(x)` is not, and that is
deliberate: gdb would answer by *calling f*, which is not a thing a mouse
should do by accident.

Keys: **F5** continue, **F6** pause, **F9** toggle breakpoint, **F10** step over,
**F11** step into, **Shift+F11** step out, **Alt+F10/F11** instruction step,
**Ctrl+F5** run, **Ctrl+Shift+F5** run to `main`.

Inside a terminal panel only function keys and `Ctrl+Shift+…` are intercepted, so
`Ctrl+C`, `Ctrl+D`, Tab and the arrows reach your program.

## Supported

| Works | Not supported |
|---|---|
| C and C++ with `-g` | Rust, Go, or any other language |
| Stripped binaries, disassembly-first | Launching `gdbserver` or an emulator for you |
| Multiple threads, all-stop, thread switching | Non-stop mode, per-thread run control |
| Breakpoints by source line, conditions | Watchpoints, catchpoints, tracepoints |
| Locals, nested structs, watch expressions | Editing values, register writes, memory writes |
| Hover a variable or register for its value | Hovering a call — it would run the function |
| Registers, disassembly, memory (read-only) | Reverse debugging, `rr` |
| A searchable symbol list, functions and globals | Types, macros, and other symbol domains |
| The gdb console, with tab completion | Core dumps, attach-to-pid |
| A program with its own terminal | Full terminal emulation for curses programs |
| Several browser tabs on one session | Multi-user, auth beyond loopback, TLS |
| Remote targets: connect, disconnect, symbols-only loading | Auto-detecting a foreign target's architecture |
| | Follow-fork, multi-inferior |
| | Windows, macOS |

## Remote targets

A gdbserver, an emulator's stub, a board on the end of a probe. The console's
tab bar has an address box with **connect** and **disconnect** buttons and a
pill showing whether gdb is attached. Those buttons run `target remote
<address>` and `disconnect` — the same commands you would type, so the console
shows exactly what ran, and gdb's own error text when a stub refuses.

Three things still go through the console, because they have no UI:
`set architecture`, `set endian`, and `set sysroot`. Everything else — loading
the program, loading symbols, connecting — has a control.

Start with a gdb that knows the architecture and a project containing the
symbols:

```sh
gdb-wui -gdb gdb-multiarch -project ~/where/the/symbols/are
```

```
set architecture mips:isa64r2
set endian big
file /path/to/symbols           ← or click the ELF in the file tree
target remote 127.0.0.1:9999    ← or use the connect button
```

**Load the ELF before you connect.** `target remote` immediately reads the
stub's registers, and how to interpret that reply depends on the architecture.
Connect first and gdb assumes *this* machine's architecture, misparses
everything, and can disrupt the far end badly enough to end the session. Only
`file` — clicking the ELF in the file tree — establishes it, by reading the ELF
header. Measured with gdb 17.1 on a MIPS64 image:

| command | architecture | endianness |
|---|---|---|
| `file <elf>` — the file tree | `mips:octeon` | big |
| `symbol-file <elf>` — **+ load**, replace | `i386` | little |
| `add-symbol-file <elf>` — **+ load**, add | `i386` | little |

So **loading symbols is not enough**, which is the trap: the symbols pane looks
like it did the job. `set architecture` and `set endian` at the console work
too, and stick. gdb-wui warns before connecting with no program loaded.

Dropping the exec file afterwards does not help either — `exec-file` with no
argument reverts the architecture to the host's while leaving the endianness
where it was, which is worse than both.

**+ load** in the Symbols pane is for the other case: a target that already
describes itself, where you want symbols without declaring a program to run.
`replace` suits an image that runs where it was linked; `add` takes an offset
for one that does not. Attaching to a running process may also need
`set sysroot` so gdb can find shared libraries — `target:` if the stub does
file transfer, a local copy otherwise.

Connecting emits a stop, so the stack, disassembly, registers and stepping all
light up. Continue, pause, step and `stepi` work as usual. **Run**, **Run→main**
and **Run→entry** do not apply — the program is already running under the stub —
though they stay clickable, and pressing one asks gdb to start the program over,
which is rarely what you want against something you merely connected to.
Shutting gdb-wui down **detaches** rather than killing, so the far end survives;
note that detaching resumes it, because that is what the remote protocol's
detach does.

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
[docs/findings.md](docs/findings.md) records the GDB behaviours this had to
establish by measurement; test comments cite them by number.

[docs/decompilation.md](docs/decompilation.md) covers decompilation: gdb-wui
supervises a resident Ghidra the same way it supervises gdb, and serves
recovered C with a map back to real addresses. There is no pane yet — the
protocol is there and the browser does not use it — and the whole feature is
optional: without `-ghidra` nothing changes.

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
