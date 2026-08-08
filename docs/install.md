---
title: Install
layout: default
nav_order: 2
---

# Install

Building gdb-wui needs Go and nothing else. There is no npm, no bundler and no
build step for the web assets — the frontend is vendored ES modules embedded in
the binary.

```sh
git clone https://github.com/retrocpugeek/gdb-wui
cd gdb-wui
go build ./cmd/gdb-wui
```

This produces a single `gdb-wui` executable.

## Requirements

| | |
|---|---|
| **Linux, x86-64** | Windows and macOS are not supported. The pty and process-group handling are Linux-specific. |
| **gdb ≥ 10** | The `mi3` interpreter is required. gdb-wui is developed against 17.1. |
| **Go ≥ 1.24** | To build. Not needed to run. |
| **gcc** | Only to build the example programs used in this documentation. |

## Debugging another architecture

To debug binaries for another architecture (using an emulator or a remote gdb
server), install a suitable gdb (e.g. `gdb-multiarch`) on your host machine, and
use the `-gdb` argument when starting gdb-wui to use it.

```sh
sudo apt install gdb-multiarch
./gdb-wui -project . -gdb gdb-multiarch
```

A stock `gdb` supports only the architecture it was built for. If you point it
at a binary for another one, it will not disassemble correctly and the registers
will be wrong. See
[Troubleshooting](troubleshooting.md#gdb-does-not-know-that-architecture).

## Installing Ghidra, optional

Ghidra is used only by the [Decompiled tab](features/decompilation.md). If you
do not install it, everything else works as normal and no warning appears until
you open that tab.

Ghidra is an 884 MB install and needs a system JDK 21 or later. Download a
release from [ghidra-sre.org](https://ghidra-sre.org/), unpack it, and either
set `GHIDRA_INSTALL_DIR` or pass the `-ghidra` argument:

```sh
./gdb-wui -project . -exe firmware -ghidra ~/ghidra_12.1.2_PUBLIC
```

gdb-wui also looks in `~/ghidra`, `/opt/ghidra`, `/usr/share/ghidra` and the
version-stamped directories the official zip unpacks to, so if Ghidra is in one
of those you do not need the argument.

{: .warning }
> Ghidra rejects any path containing an element that begins with a dot, not just
> the last one. This rules out `~/.cache` and other conventional cache
> locations, so gdb-wui defaults its analysis cache to
> `<project>/gdb-wui-decomp`. If your project directory is itself under a dotted
> directory, use `-decomp-dir` to put the cache somewhere else.

## Running gdb-wui

```sh
./gdb-wui -project /path/to/your/repo -exe build/myprogram
```

`-project` is the directory gdb-wui serves; no file outside it is readable over
HTTP. `-exe` is optional, and is relative to `-project`.

gdb-wui prints a login link on stdout and opens a browser at it. The link is
single-use and expires after 60 seconds, because it appears in `argv` where `ps`
can read it, and in browser history.

To get a new link without disturbing your session, run:

```sh
./gdb-wui -print-url
```

All arguments are listed on the [flags page](reference/flags.md).
