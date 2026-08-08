---
title: Disassembly
layout: default
parent: Features
nav_order: 7
---

# Disassembly

![Disassembly with the program counter marked](../images/disassembly.png)

The function containing the program counter, following it as you step. Address,
offset from the function's start, opcode bytes, and the instruction — with
registers, immediates and symbols coloured differently so the operands are
scannable.

The `▸` in the gutter is the program counter. The dot beside it is a breakpoint,
and clicking the gutter sets one **by address**, which is not the same as
setting one by line — see [breakpoints](breakpoints.md).

Faint rules separate the instructions belonging to one source line from the
next, when there is a line table to say so. Without one the listing is
continuous, which is itself information.

gdb annotates what it can: `# 0x4040d0 <head>` on the `lea` above is gdb
resolving the rip-relative operand to a symbol. Hovering a register reads it;
hovering the `<head>` annotation reads the symbol. See
[hover](variables.md).

## Following the pc, unless you asked otherwise

Normally the view follows the program counter. Double-clicking a symbol in the
[Symbols pane](symbols.md) pins it there instead, so switching to this tab to
look at a function does not immediately jerk back to wherever the pc is. The pin
is dropped at the next stop, because a stop means the pc moved and following it
is once again what you want.

## Worked example

A binary with nothing to go on:

```sh
gcc -O0 -no-pie -o /tmp/tour/nodebug testdata/fixtures/nodebug.c && strip /tmp/tour/nodebug
./gdb-wui -project /tmp/tour -exe nodebug
```

No source and none of the program's own symbols. `strip` removes `.symtab` but
leaves the *dynamic* symbol table, so the Symbols pane will still list the
library stubs — `printf@plt` and a handful of others — and nothing that was
written in this program. There is no `main` to break on.

Press **Run→entry**: the entry point is the one address you know exists. The
disassembly is then the only view with anything in it, and `Alt+F11` steps one
instruction at a time.

That is the floor of what this tool does, and it is where
[decompilation](decompilation.md) starts being worth the 884 MB.

## What it will not do

- **No editing or patching.** No assembling, no writing bytes.
- **No control-flow graph.** A linear listing only.
- **No cross-references.** "Who calls this" is not a question this pane can
  answer; Ghidra's is, if you have it open beside this.
- **Intel syntax** only by telling gdb: `set disassembly-flavor intel` at the
  [console](console.md), which this pane will then use.
