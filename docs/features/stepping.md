---
title: Running and stepping
layout: default
parent: Features
nav_order: 2
---

# Running and stepping

Three ways to start a program, and six ways to move it.

## Starting

| Button | Key | What it does |
|---|---|---|
| **Run** | `Ctrl+F5` | Run to the first breakpoint. If there are none, the program runs to completion. |
| **Run→main** | `Ctrl+Shift+F5` | Run and stop at `main`, whether or not you put a breakpoint there. |
| **Run→entry** | | Stop at the very first instruction, before the C runtime has started. |

**Run→entry** is the one worth knowing about. On a stripped binary there is no
`main` to break on and often no symbol to break on at all — the entry point is
the only place you know exists, and stopping there is the only way in. From
that stop the [disassembly](disassembly.md) and
[Symbols pane](symbols.md) become useful; before it you have nothing.

## Moving

| Button | Key | gdb |
|---|---|---|
| ▶ | `F5` | `continue` |
| ⏸ | `F6` | interrupt |
| ⤼ | `F10` | `next` — step over a call |
| ↳ | `F11` | `step` — step into it |
| ↰ | `Shift+F11` | `finish` — run to the end of this frame |
| ↓i | `Alt+F11` | `stepi` — one instruction |
| ⤼i | `Alt+F10` | `nexti` — one instruction, over a call |
| ■ | | `kill` |

Buttons disable themselves when gdb would refuse the command anyway: nothing is
steppable while the program is running, and nothing is continuable before it
starts. That mirrors gdb rather than guessing — if a button is grey, typing the
command at the console would have produced an error.

## Stepping without a line table

`F10` and `F11` need debug info. Without it, gdb's step range is the *whole
function*, so a step over runs to the function's exit — which looks like the
button is broken.

With the [Decompiled tab](decompilation.md) showing, stepping walks to the next
**decompiled** line instead, using Ghidra's address map in place of the missing
one. It is the only way to step by anything smaller than a function in a
stripped binary short of stepping instruction by instruction.

## Worked example

```sh
gcc -g -O0 -no-pie -o /tmp/tour/globals testdata/fixtures/globals.c
./gdb-wui -project /tmp/tour -exe globals
```

Break on line 64 (`walk(&s);`) and press Run. Now press `F10` — you land on line
65, having run all of `walk`. Restart, and press `F11` instead: you land on
line 44, inside `walk`. `Shift+F11` from there runs the rest of `walk` and puts
you back on line 65.

Hold `F10` down. Steps do not queue — one key repeat yields at most one step per
completed stop, so releasing the key leaves you where you can see, not several
stops further on.

## What it will not do

- **Reverse debugging.** No `reverse-step`, no `rr` integration.
- **Non-stop mode.** Everything stops together and continues together; you
  cannot run one thread while another sits still. See [threads](threads.md).
- **Follow-fork or multiple inferiors.** One program at a time.
- **Run until a condition**, other than by putting a condition on a breakpoint
  at the console.
