---
title: Remote targets
layout: default
parent: Features
nav_order: 12
---

# Remote targets

gdb-wui can debug anything that speaks the GDB remote protocol: a gdbserver, an
emulator's stub, or a board on the end of a probe.

![Attached to a gdbserver](../images/remote.png)

The address box and the **connect** and **disconnect** buttons are in the
console's tab bar, with a pill showing whether gdb is attached. The buttons run
`target remote <address>` and `disconnect`, so the console below shows what ran,
and shows gdb's own error text if the stub refuses.

Disconnecting uses `disconnect` rather than `detach`, because `detach` resumes
the target and someone inspecting a stopped machine usually does not want that.

## Load the program before connecting

![The architecture warning](../images/remote-warning.png)

Load the program first, then connect. gdb-wui asks for confirmation if you try
it the other way round.

`target remote` asks the stub for its registers immediately, and reading that
reply requires knowing the architecture. If gdb has the wrong architecture it
misparses the reply — a MIPS64 target read as x86-64 reports a meaningless
program counter, and can disturb the target enough to end the session.

Only loading the program sets the architecture, because only `file` reads it
from the ELF header. Measured against gdb 17.1 with a MIPS64 image: `file` gives
`mips:octeon/big`, while `symbol-file` and `add-symbol-file` both leave gdb at
the host's `i386`. Loading symbols is therefore not enough, even though the
[Symbols pane](symbols.md) fills with correct names.

## Debugging another architecture

To debug binaries for another architecture, install a suitable gdb (e.g.
`gdb-multiarch`) on your host machine, and use the `-gdb` argument when starting
gdb-wui to use it:

```sh
sudo apt install gdb-multiarch
./gdb-wui -project . -gdb gdb-multiarch -exe firmware
```

## Worked example

A local gdbserver behaves the same way as anything remote, so it is a
convenient thing to try first:

```sh
gcc -g -O0 -no-pie -o /tmp/tour/globals testdata/fixtures/globals.c
gdbserver 127.0.0.1:41234 /tmp/tour/globals &
./gdb-wui -project /tmp/tour
```

Click `globals` in the file tree to load it, enter `127.0.0.1:41234` in the
address box, and press **connect**. The pill changes to
`remote 127.0.0.1:41234`, and the console shows gdb's account of the attach.

To see the guard, try it in the wrong order: connect before loading anything and
gdb-wui shows the warning above instead of connecting with the wrong
architecture.

For qemu in user mode the shape is the same:

```sh
qemu-aarch64 -g 1234 -L /path/to/sysroot ./yourbinary &
./gdb-wui -project . -gdb gdb-multiarch -exe yourbinary
```

## What remote targets do not do

- **gdb-wui does not launch anything.** It does not start gdbserver, run qemu or
  flash a board. Start the far end yourself, then connect to it.
- **It does not detect the target's architecture.** The architecture comes from
  the ELF you load, which is why the order matters.
- **It does not attach to a local pid**, and does not open core dumps.
- **`extended-remote`, `target sim` and similar are not on the buttons.** Type
  them at the [console](console.md); the pill follows, because it reflects gdb's
  state rather than which button was pressed.

{: .note }
> If the far end is **qiling**, a successful single-step is reported as
> `SIGTERM` on AArch64 and x86. This is a bug in qiling's stub rather than a
> problem with your program; see
> [Troubleshooting](../troubleshooting.md#program-received-signal-sigterm-on-every-single-step).
