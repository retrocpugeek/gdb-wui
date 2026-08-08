---
title: Variables and hover
layout: default
parent: Features
nav_order: 4
---

# Variables and hover

![An expanded struct and a watch expression](../images/variables.png)

Locals and arguments for the selected frame, expandable to any depth. **+ watch**
adds an expression that is re-read at every stop and kept across them.

A value that changed at the last stop is marked, so stepping through a loop
shows you which field moved rather than making you compare against memory.

## Hover

Rest the pointer on something for 300 ms.

![The value tooltip over a struct field](../images/hover.png)

**The whole path is read, not the word under the pointer.** Point at `name` in
`cfg.items[2].name` and you get that field, not something called `name` out of
context. The evaluator walks `.`, `->` and `[…]` outwards from the character you
are on.

In the [disassembly](disassembly.md) it reads registers instead — `%rax` on
x86, a bare `r0` or `sp` elsewhere — and the symbol in a `<add+4>` annotation.

![A register's value, and the same number in the other base](../images/hover-register.png)

Integers are shown in the other base alongside, because a stack pointer as
140737488347136 is a number and as `0x7fffffffe000` is an address you recognise.

Values come from the frame selected in the call stack, and the tooltip goes the
instant the program moves, so it can never show a value from the previous stop.

{: .warning }
> **Only names, fields and subscripts are evaluated. `f(x)` is not.**
>
> This is not a limitation, it is the point. gdb would answer `f(x)` by
> *calling f* — in the process being debugged, with its side effects — which is
> not a thing a mouse drifting across source should be able to do. The parser
> refuses to cross a `(`.

## Following a value into memory

Right-click the same token.

![The memory context menu, showing both addresses](../images/hover-menu.png)

Two questions, kept apart, each naming the address it would show:

- **Show where it is stored** — the variable's own address.
- **Show what it points to** — the address it holds.

A pointer has both, and they are different. A plain `int` has only the first. A
register-resident variable has only the second — and the entry for the first is
absent rather than lying.

Following a value is what makes a register useful here: `%rbp`, or a `char *` in
`$x0`, leads straight to the bytes. A value below the first page is not offered
as a pointer, since that page is never mapped.

## Worked example

```sh
gcc -g -O0 -no-pie -o /tmp/tour/globals testdata/fixtures/globals.c
./gdb-wui -project /tmp/tour -exe globals
```

Break on line 65 and run. Expand `s` in the Variables pane — `visited 2`,
`total 3`, `last 0x402008 "tail"`. Add a watch on `hidden_total` and step: it
is a global, so it stays readable in every frame, which is exactly what a watch
is for.

Now hover `s.last` on line 66. You get the string. Right-click it and take
*Show what it points to* — the [memory viewer](memory.md) opens on the
characters themselves.

## What it will not do

- **No editing values.** Reading only, everywhere: no `set var`, no register
  writes, no memory writes. `set` at the [console](console.md) still works.
- **No calling functions**, as above.
- **`<optimized out>` stays `<optimized out>`.** At `-O2` a local may not exist
  anywhere; the pane says so rather than inventing a plausible number. So does
  the tooltip.
- **No pretty-printers.** Values are gdb's own formatting, which does mean
  gdb's Python pretty-printers apply if you have them configured.
