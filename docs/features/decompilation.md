---
title: Decompilation
layout: default
parent: Features
nav_order: 9
---

# Decompilation

A fourth centre tab, for a binary with no source. It shows Ghidra's recovered C
for the function you are stopped in, **with the program counter marked in it**.

![Recovered C for a stripped binary, with the pc marked](../images/decompiled.png)

That is `walk` from the tour's program with its debug info removed. The
parameter is `param_1` and the local is `local_10`, because DWARF is gone and
those names went with it — but `&head` is still `&head`, because the ELF symbol
table survived and Ghidra reads it too. On a *fully* stripped binary every
function comes back as `FUN_00401156`.

It needs `-ghidra`. See [Install](../install.md#ghidra-optional).

## Stepping works here, and does not otherwise

gdb's own stepping needs a line table. Without one its step range is the whole
function, so `F10` runs to the function's **exit** — which looks like the button
is broken.

With this tab showing, a step walks to the next *decompiled* line, using
Ghidra's address map in place of the missing one. That is the difference between
a decompiler you read and a decompiler you debug with.

The gutter sets breakpoints too, by address.

## It says where it is guessing

This is a model of the program, not its source, and the pane distinguishes the
two kinds of claim:

- An exact match — some decompiled line claims the pc's address — is drawn
  **filled**.
- No line claims it, and this is the nearest one below: drawn as an **outline**,
  and the header says `pc between lines`. Prologues, epilogues and spills belong
  to no expression, so this is normal rather than a failure.
- More than one line claims the address: marked ambiguous. Optimised code merges
  statements and the map cannot separate them again.

A local the decompiler invented that lives nowhere in the machine shows **no
value at all** rather than a plausible wrong one. Two thirds of recovered locals
live in a register that is only correct near one pc; a global at a fixed address
is valid at every pc, which is why globals are the readable ones.

## Watching it work

![The decompiler's activity in the log](../images/decompiled-log.png)

The **Log** tab carries what Ghidra is doing: what it imported, how long
analysis took, one line per decompiled function with its timing, and Ghidra's
own complaints. Unlike the raw MI stream this is not behind a flag — it is one
line per operation, and without it a slow start is indistinguishable from a
stuck one.

Import and analysis are genuinely slow: seconds for a small program, minutes for
firmware. The result is cached under `<project>/gdb-wui-decomp`, keyed on the
binary's SHA-256, so the second run is immediate.

## Using your own Ghidra project

If you have already named functions and laid out structures, use that work:

```sh
./gdb-wui -project . -exe firmware \
  -ghidra-project ~/ghidra-projects/firmware-re.gpr \
  -ghidra-program firmware
```

**Opened read-only.** Your names and types come through; nothing is written
back. `-ghidra-program` is required because a real project holds several
programs.

## Worked example

```sh
gcc -g -O0 -no-pie -o /tmp/tour/globals-nodebug testdata/fixtures/globals.c
objcopy --strip-debug /tmp/tour/globals-nodebug
./gdb-wui -project /tmp/tour -exe globals-nodebug -ghidra ~/ghidra
```

Filter the [Symbols pane](symbols.md) to `walk`, right-click, **Set breakpoint**,
Run. Open **Decompiled** — the first time, the Log tab shows the import and
analysis; after that it is instant.

You land in the prologue, so the header says `pc between lines`. Press `F10`
twice and the marker becomes a solid highlight on `local_10 = &head;`. Now
compare with the [source](../tour.md): that is `struct node *n = &head;`.

## What it will not do

- **No editing Ghidra's names or types from here.** It is a reader. Rename in
  Ghidra and reload.
- **No decompiling a function you are not stopped in**, except by breaking in it
  first.
- **It is not the source.** Recovered C compiles to the same behaviour, not to
  the same text: loop shapes change, variables merge, and types are inferred.
  Read it as a model.
- **No Ghidra, no tab.** Nothing else changes, and no warning appears until you
  open it.
