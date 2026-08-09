---
title: Home
layout: default
nav_order: 1
---

# gdb-wui

gdb-wui is a web UI for GDB. It shows source, disassembly, variables, registers,
memory, threads and a gdb console in a browser tab, while GDB itself does the
debugging.

```sh
go build ./cmd/gdb-wui
./gdb-wui -project /path/to/your/repo
```

gdb-wui prints a URL and opens a browser at it. To start debugging, pick an
executable from the file tree, click a line number to set a breakpoint, and
step.

[![The whole window, stopped at a breakpoint](images/overview.png)](images/overview.png)

{: .warning }
> **gdb-wui runs your programs with your privileges.** That is what a debugger
> does; sandboxing the program being debugged is not a goal.
>
> gdb-wui listens on loopback only, and refuses a non-loopback address unless
> you pass `-listen-anywhere`. Do not expose it to a network you do not control:
> anyone who can reach the port can run programs as you.
>
> Binding to loopback is not sufficient on its own, because any web page you
> visit can `fetch` a loopback URL. Access therefore needs a single-use login
> link, and requests are checked against DNS rebinding in three ways. These are
> described in
> [the protocol document](https://github.com/retrocpugeek/gdb-wui/blob/master/docs/protocol.md#security).

## Why

GDB's own interfaces are a console or the `tui` mode. Neither shows source,
disassembly, registers, the call stack and thread state at the same time, and
neither lets you click a gutter to set a breakpoint.

gdb-wui translates rather than debugs: it speaks GDB/MI to a real gdb process,
and a small JSON protocol to the browser. It implements no debugger logic of its
own, so you get gdb's behaviour — including gdb's error messages, unedited, when
something does not work.

This also sets the limit of what gdb-wui can do. If gdb cannot do something,
neither can gdb-wui.

## Where to start

- **[Install](install.md)** — what you need, and what is optional.
- **[A first session](tour.md)** — load a program, break, step and inspect it, in
  about five minutes, using a program in this repository.
- **[Features](features/index.md)** — one page per feature, each listing what it
  does not do.
- **[Troubleshooting](troubleshooting.md)** — errors you may see, and what they
  mean.

## What gdb-wui is good at

Stepping through C or C++ built with `-g` works, and is not the interesting
part. These are:

- **Binaries with no source.** The Symbols pane reads the ELF symbol table, the
  disassembly is a first-class view rather than a fallback, and if Ghidra is
  installed the [Decompiled tab](features/decompilation.md) shows recovered C
  with the program counter marked in it. Ghidra also
  [names the call stack](features/decompilation.md#naming-the-call-stack),
  which gdb otherwise reports as a column of `?? ()`.
- **Other architectures.** Use `-gdb` to select a suitable gdb, then
  [connect to a stub](features/remote.md) — a gdbserver, qemu, an emulator, or a
  board on a probe.

  gdb-wui holds no architecture-specific knowledge of its own. Registers,
  disassembly, frames and memory all come from gdb and are passed through as
  reported, so any architecture the gdb you select supports should work. MIPS64,
  AArch64 and x86-64 are the ones it is developed and tested against.

  The one exception is the [Decompiled tab](features/decompilation.md), which
  needs a per-ABI rule to turn Ghidra's stack offsets into expressions gdb can
  evaluate. Where no rule has been established, stack variables show no value;
  everything else in that tab still works.
- **Images that do not run where they were linked.** Load symbols at an offset
  and the addresses line up again.
- **Identifying what you are looking at.** The memory viewer names the symbol
  each row falls in, and hovering a variable reads the whole expression rather
  than the word under the pointer.
- **Trying a different value.** Double-click a variable, a register or a byte of
  memory to [change it](features/editing.md) and carry on from there.

## About

The **?** button in the toolbar shows the version of gdb-wui you are running,
the gdb it is driving, and where the licence and this documentation live.
