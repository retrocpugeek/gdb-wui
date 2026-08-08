---
title: Symbols
layout: default
parent: Features
nav_order: 8
---

# Symbols

![The symbol list, filtered, on a binary with no debug info](../images/symbols.png)

Every function and global the loaded program has, from whatever gdb knows —
debug info if there is any, the ELF symbol table if not. On a release build
that table is the only map you have, and this pane is how you read it.

The filter is a substring match. The `fn` and `var` sigils say which is which,
the `all` dropdown narrows to one kind, and **dimmed rows are the ones with no
debug info** — so you can see at a glance how much of the program you can
actually reason about.

Double-click a symbol to jump:

| Symbol | Goes to |
|---|---|
| A function with debug info | its source line |
| A function with only an address | the [disassembly](disassembly.md) |
| A variable with only an address | the [memory viewer](memory.md) |

Right-click for **Set breakpoint** and **Go to**.

## Loading more symbols

**+ load** takes a symbol file for the program already running.

![The load-symbols bar, with an offset](../images/symbols-load.png)

*replace* is `symbol-file` — the symbols you have are wrong or missing and these
are better. *add* is `add-symbol-file`, and reveals an **offset** field.

The offset is for an image that does not run where it was linked: a firmware
blob a bootloader relocated, a module mapped at a base decided at load time.
Give the difference and every symbol lines up again. Get it wrong and every
symbol is wrong by a constant, which is easy to spot — names appear, but they
name the wrong things.

{: .warning }
> **Loading symbols does not set the architecture.** Only loading the *program*
> does, because only `file` reads the ELF header. A symbols-only load against
> the wrong architecture leaves you with plausible names over nonsense
> disassembly, which is worse than no names at all. Click the ELF in the file
> tree first. See [remote targets](remote.md).

## Worked example

The same program, with the debug info removed but the symbol table left alone —
which is what a release build looks like:

```sh
gcc -g -O0 -no-pie -o /tmp/tour/globals-nodebug testdata/fixtures/globals.c
objcopy --strip-debug /tmp/tour/globals-nodebug
./gdb-wui -project /tmp/tour -exe globals-nodebug
```

`walk`, `main`, `counter`, `banner`, `head`, `hidden_total` — all still there,
all dimmed, because gdb knows where each one is and nothing about what it is.
Right-click `walk` and set a breakpoint; it works exactly as it would with debug
info.

Then try to read one at the console and see the other half of the trade:

```
(gdb) p counter
'counter' has unknown type; cast it to its declared type
(gdb) p (int)counter
$1 = 7
```

The [memory viewer](memory.md) needs no type at all, which is why it is the
better tool here — and its symbol column is populated from this same table.

## What it will not do

- **Functions and variables only.** Types, macros, files and gdb's other symbol
  domains are not listed. `info types` at the [console](console.md).
- **No demangling control.** Names are as gdb reports them, which means gdb's
  demangler settings apply.
- **No search by address.** Type the address into the
  [memory viewer](memory.md) or the disassembly instead.
