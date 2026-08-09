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
| `-decomp-dir DIR` | Where to cache projects gdb-wui creates itself. Default `<project>/gdb-wui-decomp`. |

{: .warning }
> `-decomp-dir` must not contain a path element beginning with a dot, because
> Ghidra rejects those. See
> [Troubleshooting](../troubleshooting.md#path-element-starting-with--is-not-permitted).

## Choosing settings

| Flag | What it does |
|---|---|
| `-config PATH` | Read settings from this file instead of searching for one. Fails if it does not exist. |
| `-no-config` | Ignore any [config file](config.md). |

`-version` and `-print-url` are also excluded from a config file, because they
are actions rather than settings.

## Diagnostics

| Flag | What it does |
|---|---|
| `-mi-log` | Streams raw GDB/MI traffic to the browser's [log pane](../features/console.md#log). |
| `-v` | Verbose logging on the server's stderr. |
| `-assets-dir DIR` | Serves the frontend from disk instead of from the binary, so a reload picks up changes. |
| `-version` | Prints the version and exits. |
