---
title: Flags
layout: default
parent: Reference
nav_order: 1
---

# Command-line flags

`./gdb-wui -h` prints these too. A test fails if a flag exists in the code and
not on this page, so the list cannot quietly fall behind.

## What to serve, and what to debug

| Flag | What it does |
|---|---|
| `-project DIR` | The directory to browse. Nothing outside it is served. Default `.` |
| `-exe PATH` | Load a program at startup, relative to `-project`. |
| `-gdb PATH` | Which gdb to run. Default `gdb`; use `gdb-multiarch` for a foreign architecture. |
| `-no-gdb` | Browse the project without starting a debugger. |

## Where to listen

| Flag | What it does |
|---|---|
| `-addr ADDR` | Listen address; must be loopback. Default `127.0.0.1:0` — a free port. |
| `-listen-anywhere` | Permit a non-loopback address. Read the warning on the [home page](../index.md) first. |
| `-open` | Open a browser at the URL. Default true; `-open=false` to suppress. |
| `-print-url` | Print a fresh login link for an already-running server and exit. |
| `-idle-exit DUR` | Exit after this long with no browser connected. `0` disables. |

## Decompilation

All optional; without Ghidra none of them matter. See
[decompilation](../features/decompilation.md).

| Flag | What it does |
|---|---|
| `-ghidra DIR` | Ghidra installation. Defaults to `$GHIDRA_INSTALL_DIR`, then the usual locations. |
| `-ghidra-project PATH` | An existing Ghidra project to read, **opened read-only** — your names and types, never written to. |
| `-ghidra-program NAME` | Which program inside that project. Required with `-ghidra-project`: a real project holds several. |
| `-decomp-dir DIR` | Where to cache projects gdb-wui creates itself. Default `<project>/gdb-wui-decomp`. |

{: .warning }
> `-decomp-dir` must not contain a path element beginning with a dot. Ghidra
> refuses those outright — see
> [Troubleshooting](../troubleshooting.md#path-element-starting-with--is-not-permitted).

## Diagnostics

| Flag | What it does |
|---|---|
| `-mi-log` | Stream raw GDB/MI traffic to the browser's [log pane](../features/console.md#log). |
| `-v` | Verbose logging on the server's stderr. |
| `-assets-dir DIR` | Serve the frontend from disk instead of the binary. Reload is the whole dev loop. |
| `-version` | Print the version and exit. |
