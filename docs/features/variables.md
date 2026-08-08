---
title: Variables and hover
layout: default
parent: Features
nav_order: 4
---

# Variables and hover

![An expanded struct and a watch expression](../images/variables.png)

The Variables pane shows the locals and arguments of the selected frame, which
can be expanded to any depth. To add an expression that is re-read at every stop
and kept across stops, click **+ watch**.

Values that changed at the last stop are marked, so when stepping through a loop
you can see which field moved without comparing against memory yourself.

## Reading a value by hovering

To read a value, rest the pointer on it for 300 ms.

![The value tooltip over a struct field](../images/hover.png)

gdb-wui reads the whole expression, not the word under the pointer: hovering
`name` in `cfg.items[2].name` gives you that field rather than something else
called `name`. The evaluator walks `.`, `->` and `[…]` outwards from the
character you are on.

In the [disassembly](disassembly.md), hovering reads registers instead — `%rax`
on x86, or a bare `r0` or `sp` on other architectures — and the symbol in an
annotation such as `<add+4>`.

![A register's value, and the same number in the other base](../images/hover-register.png)

Integers are also shown in the other base, so that a value like
140737488347136 is shown as `0x7fffffffe000` as well.

Values are read from the frame selected in the call stack. The tooltip closes as
soon as the program moves, so it cannot show a value from the previous stop.

{: .warning }
> Only names, fields and subscripts are evaluated; `f(x)` is not. gdb would
> answer `f(x)` by calling `f` in the program being debugged, with whatever side
> effects that has, so the expression parser stops at a `(`.

## Showing where a value lives

To see the memory behind a value, right-click it.

![The memory context menu, showing both addresses](../images/hover-menu.png)

The menu offers two things and names the address each would show:

- **Show where it is stored** — the address of the variable itself.
- **Show what it points to** — the address the variable holds.

A pointer has both, and they are different addresses. A plain `int` has only the
first. A variable held in a register has only the second, and the first entry is
omitted rather than shown with a wrong address.

This also works for registers, which is what makes them useful here: `%rbp`, or
a `char *` in `$x0`, leads directly to the bytes. Values below the first page
are not offered as pointers, because that page is never mapped.

## Worked example

```sh
gcc -g -O0 -no-pie -o /tmp/tour/globals testdata/fixtures/globals.c
./gdb-wui -project /tmp/tour -exe globals
```

Break on line 65 and run. Expand `s` in the Variables pane to see `visited 2`,
`total 3` and `last 0x402008 "tail"`. Add a watch on `hidden_total`, then step:
it is a global, so it stays readable in every frame.

Now hover `s.last` on line 66 to see the string, right-click it, and choose
**Show what it points to** to open the [memory viewer](memory.md) on the
characters themselves.

## What variables and hover do not do

- **They do not edit values.** There is no `set var`, no register write and no
  memory write in the UI. `set` at the [console](console.md) works.
- **They do not call functions**, as above.
- **They do not recover optimised-out values.** At `-O2` a local may not exist
  anywhere; the pane and the tooltip both show `<optimized out>` rather than a
  plausible wrong value.
- **They add no pretty-printers.** Values are formatted by gdb, so any gdb
  Python pretty-printers you have configured do apply.
