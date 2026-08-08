---
title: Remote targets
layout: default
parent: Features
nav_order: 12
---

# Remote targets

A gdbserver, an emulator's stub, a board on the end of a probe. Anything that
speaks the GDB remote protocol.

![Attached to a gdbserver](../images/remote.png)

The address box and **connect** / **disconnect** sit in the console's tab bar,
with a pill showing whether gdb is attached. Those buttons run `target remote
<address>` and `disconnect` — the same commands you would type, so the console
below shows exactly what ran, and gdb's own error text when a stub refuses.

*disconnect*, not *detach*: detach resumes the target, and someone who connected
to look at a stopped machine rarely wants it to run on.

## Load the program first

![The architecture warning](../images/remote-warning.png)

This is the one ordering mistake that matters, and gdb-wui asks before letting
you make it.

`target remote` immediately asks the stub for its registers, and how to read
that reply depends on the architecture. Get it wrong and gdb misparses
everything — a MIPS64 target read as x86-64 reports a nonsense pc, and can upset
the far end badly enough to end the session.

**Only loading the program sets the architecture**, because only `file` reads it
out of the ELF header. Measured against gdb 17.1 with a MIPS64 image: `file`
gives `mips:octeon/big`, while `symbol-file` and `add-symbol-file` both leave
gdb at the host's `i386`. Loading *symbols* is exactly the trap — the
[Symbols pane](symbols.md) fills with correct-looking names and the
architecture is still wrong.

So: click the ELF in the file tree, then connect.

## A foreign architecture needs a foreign gdb

A stock `gdb` knows one architecture. For anything else:

```sh
sudo apt install gdb-multiarch
./gdb-wui -project . -gdb gdb-multiarch -exe firmware
```

## Worked example

A local gdbserver stands in for anything remote — from gdb's side they are the
same protocol:

```sh
gcc -g -O0 -no-pie -o /tmp/tour/globals testdata/fixtures/globals.c
gdbserver 127.0.0.1:41234 /tmp/tour/globals &
./gdb-wui -project /tmp/tour
```

Click `globals` in the file tree to load it, put `127.0.0.1:41234` in the
address box, press **connect**. The pill turns to `remote 127.0.0.1:41234` and
the console shows gdb's own account of the attach.

Try it in the wrong order to see the guard: connect before loading anything and
you get the warning above rather than a silently mis-parsed session.

For qemu user-mode, the shape is the same:

```sh
qemu-aarch64 -g 1234 -L /path/to/sysroot ./yourbinary &
./gdb-wui -project . -gdb gdb-multiarch -exe yourbinary
```

## What it will not do

- **It will not launch anything for you.** No spawning gdbserver, no starting
  qemu, no flashing a board. You start the far end; gdb-wui connects to it.
- **No auto-detecting the target's architecture.** It comes from the ELF you
  load, which is why the ordering matters.
- **No `attach` to a running local pid**, and no core dumps.
- **`extended-remote`, `target sim` and the rest** are not wired to the buttons.
  Type them at the [console](console.md); the pill follows, because it reflects
  gdb's state rather than which button was pressed.

{: .note }
> If you are using **qiling** as the far end, a successful single-step is
> reported as `SIGTERM` on AArch64 and x86 — a bug in its stub, not in your
> program. See
> [Troubleshooting](../troubleshooting.md#program-received-signal-sigterm-on-every-single-step).
