---
title: Flags
layout: default
parent: Reference
nav_order: 1
---

# Command-line flags

`./gdb-wui -h` prints the same list. Any of these except the four in
[Choosing settings](#choosing-settings) can also be set in a
[config file](config.md), using the flag's name without the dash.

## What to serve, and what to debug

| Flag | What it does |
|---|---|
| `-project DIR` | The directory to browse. No file outside it is served. Default `.` |
| `-exe PATH` | Loads a program at startup, relative to `-project`. |
| `-gdb PATH` | Selects the gdb executable. Default `gdb`; use a suitable gdb (e.g. `gdb-multiarch`) for another architecture. |
| `-no-gdb` | Browses the project without starting a debugger. |

## Where to listen

| Flag | What it does |
|---|---|
| `-addr ADDR` | Listen address; must be loopback. Default `127.0.0.1:0`, which picks a free port. |
| `-listen-anywhere` | Permits a non-loopback address. Read the warning on the [home page](../index.md) first. |
| `-open` | Opens a browser at the URL. Default true; pass `-open=false` to suppress it. |
| `-print-url` | Prints a new login link for an already-running server, then exits. |
| `-idle-exit DUR` | Exits after this long with no browser connected. `0` disables it. |

## Decompilation

These are all optional, and do nothing without Ghidra. See
[decompilation](../features/decompilation.md).

| Flag | What it does |
|---|---|
| `-ghidra DIR` | The Ghidra installation. Defaults to `$GHIDRA_INSTALL_DIR`, then the usual locations. |
| `-ghidra-project PATH` | An existing Ghidra project to read, opened read-only, so your names and types are used but never written to. |
| `-ghidra-program NAME` | Which program inside that project to use. Required with `-ghidra-project`, because a project usually holds several. |
| `-ghidra-analysis MODE` | How much of the binary Ghidra analyses at import: `auto`, `full`, `lean` or `none`. Past 4 MB of code the analysis cannot finish, so `auto` takes `none` for an image whose symbols say where the functions are and `lean` — the analyzers that find functions, without the ones that cost the memory — for a stripped one. See [images too big to analyse](../features/decompilation.md#images-too-big-to-analyse). |
| `-ghidra-symbols FILE` | A file of `address [type] name` lines naming functions the binary does not name itself, as `nm` and `/proc/kallsyms` write them. See [a stripped kernel](../features/kernel.md#a-stripped-kernel). |
| `-ghidra-binary FILE` | The file for Ghidra to reverse, when it is not the program gdb loaded. The way in for a target gdb will not take a file for at all, such as an emulator booting a raw kernel image. See [a kernel image gdb will not load](../features/kernel.md#decompiling-a-kernel-image-gdb-will-not-load). |
| `-ghidra-processor ID` | A Ghidra language ID, as `ARM:LE:32:v7`, saying what the bytes are. Required with `-ghidra-base`, since a raw image says nothing about itself. |
| `-ghidra-base ADDR` | The address a raw `-ghidra-binary` is loaded at. It imports the file through Ghidra's binary loader, and it is the whole of the mapping between the debugger's addresses and the decompiler's — nothing checks it and nothing corrects it. |
| `-decomp-dir DIR` | Where to cache projects gdb-wui creates itself. Default `<project>/gdb-wui-decomp`. |

{: .warning }
> `-decomp-dir` must not contain a path element beginning with a dot, because
> Ghidra rejects those. See
> [Troubleshooting](../troubleshooting.md#path-element-starting-with--is-not-permitted).

## Agents

`-mcp` joins an already-running server rather than starting one, and serves
[MCP](../features/agent.md) on stdio. Three flags, because reading a binary,
writing into your Ghidra project and running your program are three different
things to agree to.

| Flag | What it does |
|---|---|
| `-mcp` | Serve MCP on stdio for a running gdb-wui. Read-only: decompile, disassemble, read the stack, memory, registers and expressions. |
| `-mcp-annotate` | With `-mcp`: also rename, retype and comment in the decompiler. Marked as an agent's, and undoable a run at a time. |
| `-mcp-run` | With `-mcp`: also set breakpoints and run, step and pause the program. |

{: .warning }
> `-mcp-run` lets an agent run your program with your privileges, on its own
> initiative. That is what a debugger does, and it is why it is separate from
> reading.

With several servers running, `-addr` chooses which one to join.

## Choosing settings

| Flag | What it does |
|---|---|
| `-config PATH` | Read settings from this file instead of searching for one. Fails if it does not exist. |
| `-no-config` | Ignore any [config file](config.md). |
| `-save-config` | Write the current settings to `./gdb-wui.json` and exit. `-save-config=PATH` writes somewhere else. |

`-version`, `-print-url`, `-mcp` and `-save-config` are also excluded from a
config file, because they are actions rather than settings.

## Diagnostics

| Flag | What it does |
|---|---|
| `-mi-log` | Streams raw GDB/MI traffic to the browser's [log pane](../features/console.md#log). |
| `-v` | Verbose logging on the server's stderr. |
| `-assets-dir DIR` | Serves the frontend from disk instead of from the binary, so a reload picks up changes. |
| `-version` | Prints the version and exits. |
