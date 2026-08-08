---
title: Home
layout: default
nav_order: 1
---

# gdb-wui

A web UI for GDB. Source, disassembly, variables, registers, memory, threads and
a real gdb console in a browser tab — with GDB itself still in charge.

```sh
go build ./cmd/gdb-wui
./gdb-wui -project /path/to/your/repo
```

It prints a URL and opens a browser at it. Pick an executable from the file tree,
click a line number to set a breakpoint, and step.

[![The whole window, stopped at a breakpoint](images/overview.png)](images/overview.png)

{: .warning }
> **It runs your programs as you.** gdb-wui starts arbitrary binaries with your
> full privileges. That is what a debugger does, and sandboxing the debuggee is
> **not** a goal.
>
> It listens on loopback only, and refuses a non-loopback address unless you
> pass `-listen-anywhere`. **Never expose it to a network you do not control.**
> Anyone who can reach the port can run programs as you.
>
> Binding loopback is not by itself enough — any web page you visit can `fetch`
> a loopback URL — so access needs a single-use login link, and requests are
> checked against DNS rebinding three separate ways. The details are in
> [the protocol document](https://github.com/retrocpugeek/gdb-wui/blob/master/docs/protocol.md#security).

## Why

GDB's own interfaces are a bare console or the cramped `tui` mode. Neither shows
source, disassembly, registers, the call stack and thread state at once, and
neither lets you click a gutter to set a breakpoint.

gdb-wui is a translator, not a debugger: it speaks GDB/MI to a real gdb process
and a small JSON protocol to the browser. It reimplements no debugger logic, so
what you get is gdb's behaviour with a better view of it — including gdb's error
messages, unedited, when something will not work.

That is also the honest limit. If gdb cannot do it, neither can this.

## Where to start

- **[Install](install.md)** — what you need, and what is optional.
- **[A first session](tour.md)** — load, break, step, inspect, in about five
  minutes, against a program in this repository.
- **[Features](features/index.md)** — one page each, with what it will not do.
- **[Troubleshooting](troubleshooting.md)** — the errors we actually hit, and
  what they mean.

## What it is good at

The ordinary case — C or C++ built with `-g`, stepping through source — works
and is not the interesting part. These are:

- **A binary with no source at all.** The Symbols pane reads the ELF symbol
  table, the disassembly is a first-class view rather than a fallback, and with
  Ghidra installed the [Decompiled tab](features/decompilation.md) shows
  recovered C with the program counter marked in it.
- **A foreign architecture.** Point `-gdb` at `gdb-multiarch` and
  [connect to a stub](features/remote.md) — a gdbserver, qemu, an emulator, a
  board on a probe. Developed against MIPS64 and AArch64 as well as x86-64.
- **An image that does not run where it was linked.** Load symbols at an offset
  and the addresses line up again.
- **Knowing what you are looking at.** The memory viewer names the symbol each
  row falls in; hovering a variable reads the whole path, not the word under the
  pointer.
