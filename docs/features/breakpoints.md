---
title: Breakpoints
layout: default
parent: Features
nav_order: 1
---

# Breakpoints

Click a line number. That is the whole of the common case.

![A gutter marker and the Breakpoints pane](../images/breakpoints.png)

The dot in the gutter and the row in the **Breakpoints** pane are one
breakpoint seen twice — the pane is a mirror of what gdb holds, not a separate
list. Clicking the gutter again removes it. The checkbox in the pane disables a
breakpoint without deleting it, and the `×` deletes it.

The gutters in the [disassembly](disassembly.md) and
[decompiled](decompilation.md) views work the same way, except that those set a
breakpoint on an **address** rather than a line.

## By name, when there is no source

Right-click a function in the [Symbols pane](symbols.md).

![The Symbols pane's right-click menu](../images/symbol-menu.png)

## Breaking by name is not breaking at the address

They land in different places, and the difference matters more than it sounds.

Given a name, gdb **skips the prologue** — it uses the line table, or failing
that a heuristic, to find the first instruction after the frame is set up. On a
MIPS firmware, `break process_packet` stops at entry+24, past the register
spills.

Given an address, gdb stops exactly there. At a function's entry that is
*before* the frame exists and *before* any argument has been stored anywhere
you can read, so the Variables pane will show you nonsense and be right to.

Which you want depends on the question:

| You want | Use |
|---|---|
| To inspect arguments and locals | The name. Let gdb skip the prologue. |
| To see the prologue itself, or to catch a jump into the middle of a function | The address. |

## Pending breakpoints

A breakpoint on a file or symbol gdb does not yet know about is accepted and
marked pending; it resolves when a shared library loads or a program is loaded.
The Breakpoints pane shows it differently so you are not left believing an
unresolved breakpoint will be hit.

## Worked example

```sh
gcc -g -O0 -no-pie -o /tmp/tour/globals testdata/fixtures/globals.c
./gdb-wui -project /tmp/tour -exe globals
```

Click line 49. Then set the same breakpoint both other ways at the gdb console
and compare:

```
(gdb) break walk
Breakpoint 1 at 0x401162: file globals.c, line 44.
(gdb) break *walk
Breakpoint 2 at 0x401156: file globals.c, line 43.
```

Twelve bytes and one line apart. Breakpoint 2 is on the opening brace, before
`out` has been stored anywhere; breakpoint 1 is on the first statement, where
the parameter can be read. Both appear in the pane, and the addresses beside
them are the evidence.

## What it will not do

- **Watchpoints, catchpoints and tracepoints.** gdb has all three; gdb-wui has
  no UI for them. They work perfectly well typed into the
  [console](console.md), and any breakpoint made that way appears in the pane
  like the rest.
- **Conditions and hit counts** are not exposed in the UI either. `condition`
  and `ignore` at the console apply to a breakpoint set by clicking.
