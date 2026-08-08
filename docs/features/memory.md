---
title: Memory
layout: default
parent: Features
nav_order: 6
---

# Memory

![The hex view with its symbol column](../images/memory.png)

Sixteen bytes a row: address, hex, ASCII, and **the symbol the row falls in**.

That last column is the part worth having. `banner+16` tells you what you are
looking at; `0x404070` does not. It is resolved through gdb, so it knows
whatever gdb knows — which on a stripped binary is still the ELF symbol table,
and needs no debug info at all.

Rows that belong to no symbol are **blank**. The stack and the heap are mostly
blank, and that is the truth rather than an omission.

## The address box takes an expression

Not just a number. `&head`, `$sp`, `cfg->items`, `0x404040` — anything gdb can
evaluate to an address. The bar beside it shows what your expression resolved
to, so a typo is visible rather than mysterious.

## Getting here from something you were reading

Usually you do not type an address at all. [Hover a variable](variables.md),
right-click, and take *Show where it is stored* or *Show what it points to*.
The [Symbols pane](symbols.md) will also jump a data symbol straight here.

## Unreadable bytes are `??`

A page that is not mapped reads as `??` rather than as zeros. Zeros are a
value; unmapped is not, and a viewer that showed one as the other would be
lying at exactly the moment you were trying to find out whether a pointer was
any good.

## Worked example

```sh
gcc -g -O0 -no-pie -o /tmp/tour/globals testdata/fixtures/globals.c
./gdb-wui -project /tmp/tour -exe globals
```

Break on line 49, run, open **Memory** and enter `&counter`. Everything the
program declares is in the next few rows, and you can read the data structures
straight off:

- `counter` is `07`.
- `banner` spells `gdb-wui` in the ASCII column.
- `hidden_total` is `9a 47 46 45 44 43 42 41` — the `0x4142434445464748` from
  the source, little-endian, with the low byte already changed because `main`
  has run its loop.
- `head` at `0x4040d0` holds `01`, then a pointer to `0x40200d`, which is the
  string `"head"`.

Now enter `$sp`. Same viewer, and the symbol column is empty the whole way
down — the stack belongs to no symbol.

## What it will not do

- **No writing.** Read-only. `set {int}0x404040 = 9` at the
  [console](console.md) if you mean it.
- **No search.** No "find this byte pattern".
- **No watchpoints on a region.** `watch` at the console; there is no UI.
- **No structure overlay.** It shows bytes. Use the
  [Variables pane](variables.md) for a typed view of the same memory.
