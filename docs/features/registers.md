---
title: Registers
layout: default
parent: Features
nav_order: 5
---

# Registers

![The register list](../images/registers.png)

Whatever gdb reports for the target's architecture, in gdb's own order, read
from the frame selected in the call stack. A register that changed at the last
stop is marked, which is what makes single-stepping through a prologue readable.

The number beside each name is gdb's register number, and it is the real
identity: gdb's list has gaps at stable indices, and a register with no name is
not a bug.

Registers are also where [hover](variables.md) is most useful — in the
[disassembly](disassembly.md), pointing at `%rax` reads it, and right-clicking
offers to open the [memory viewer](memory.md) at what it points to. For a
register holding a pointer that is usually the question you actually have.

## Worked example

Break anywhere and switch to the Registers tab, then step one instruction at a
time with `Alt+F11`. Watch `rip` advance and the marks appear on whatever the
instruction touched. On a foreign target under
[gdb-multiarch](remote.md) the same pane shows that architecture's registers,
because the list comes from gdb rather than from anything here.

## What it will not do

- **No writing.** There is no way to set a register from the UI. `set $rax = 1`
  at the [console](console.md) works and the pane will show the result.
- **No per-register format switching** in the UI. `p/d $rax` at the console.
- **No register groups or filtering.** The list is flat and complete; on an
  architecture with several hundred registers it is a long list.
