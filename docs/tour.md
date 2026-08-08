---
title: A first session
layout: default
nav_order: 3
---

# A first session

About five minutes, against a program in this repository, ending with you
looking at a linked list's bytes in memory and knowing which byte is which.

Every screenshot on this site is taken from this program, so what you see here
is what you will see.

## Build something to debug

`testdata/fixtures/globals.c` is a small program with named globals — a counter,
a string, an array of doubles, a static, and a two-node linked list.

```sh
mkdir -p /tmp/tour && cp testdata/fixtures/globals.c /tmp/tour/
gcc -g -O0 -no-pie -o /tmp/tour/globals /tmp/tour/globals.c
```

`-no-pie` so the addresses below are the ones you will see. Under a
position-independent executable everything is relocated at load and the numbers
differ every run — which is normal, and which gdb-wui handles, but it makes a
tutorial hard to follow.

## Start

```sh
go build ./cmd/gdb-wui
./gdb-wui -project /tmp/tour -exe globals
```

A URL appears on stdout and a browser opens at it. If it does not — over SSH, in
a container, with no desktop session — paste the URL yourself. That path is the
supported one, not a fallback.

The status bar at the bottom should read **connected** and name your gdb.

## Break, and run

Click **globals.c** in the file tree. Click the line number **49**, the
`out->visited++;` inside `walk`. A red dot appears in the gutter and the same
breakpoint shows up in the **Breakpoints** pane on the right — one breakpoint,
two views of it.

![A gutter marker and the Breakpoints pane](images/breakpoints.png)

Press **Run** in the toolbar, or `Ctrl+F5`.

[![The whole window, stopped at a breakpoint](images/overview.png)](images/overview.png)

Everything filled in at once:

- The green bar is the program counter, on line 49.
- **Locals** has `out` and `n`, the parameter and the cursor into the list.
- The **Call stack** has two frames — `walk` called from `main` — and clicking
  `#1 main()` moves a *blue* bar to the calling line without moving the green
  one. Inspecting a caller never hides where the program actually stopped.
- The **gdb console** shows the stop exactly as gdb reported it. That console is
  a real one: anything you can type at gdb, you can type there.

## Read a value without typing anything

Rest the pointer on `n->name` on line 51.

![The value tooltip over a struct field](images/hover.png)

The whole path is read, not the word under the pointer: you get the `name` field
of the node `n` points at, not something called `name` out of context.

{: .note }
> Only names, fields and subscripts are evaluated. `f(x)` is not, deliberately —
> gdb would answer by *calling f*, which is not a thing a mouse should do by
> accident.

## Follow it into memory

Right-click the same token.

![The memory context menu, showing both addresses](images/hover-menu.png)

Two different questions, kept apart, each naming the address it would show:

- **Show where it is stored** — where the pointer variable itself lives.
- **Show what it points to** — where it points, `0x40200d` here.

Take the second. The **Memory** tab opens at that address. Now type `&counter`
into the address box at the top and press Enter — it takes an expression, not
just a number.

![The hex view with its symbol column](images/memory.png)

The right-hand column is the part worth having. Each row says which symbol it
falls in, so `banner+16` rather than a bare number, and the rows that belong to
nothing — padding, and later the stack and the heap — are honestly blank rather
than guessed at.

You can read the program's data structures straight off it: `counter` is `07`,
`banner` spells `gdb-wui` in the ASCII column, `hidden_total` is the
`0x4142434445464748` from the source with `9a` in the low byte because `main`
has already added to it, and `head` at `0x4040d0` holds `01` then a pointer to
`0x40200d` — the string `"head"` you hovered a moment ago.

## Step

`F10` steps over, `F11` steps into, `Shift+F11` runs to the end of the frame.
Hold `F10` and the line marker walks down the loop; the Locals pane and the
tooltip both follow, and the tooltip disappears the instant the program moves so
it can never show you a value from the previous stop.

## Where next

- The same program with `gcc -g` removed, to see what the
  [Symbols pane](features/symbols.md) and the
  [Decompiled tab](features/decompilation.md) are for.
- [Remote targets](features/remote.md), if what you are debugging is not on this
  machine or not this architecture.
- [The keyboard map](reference/keys.md).
