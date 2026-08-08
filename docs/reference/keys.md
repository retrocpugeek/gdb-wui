---
title: Keyboard
layout: default
parent: Reference
nav_order: 2
---

# Keyboard

| Key | What it does |
|---|---|
| `F5` | Continue |
| `Ctrl+F5` | Run |
| `Ctrl+Shift+F5` | Run to `main` |
| `F6` | Pause |
| `F9` | Toggle a breakpoint on the current line |
| `F10` | Step over |
| `F11` | Step into |
| `Shift+F11` | Step out |
| `Alt+F10` | Step over one instruction |
| `Alt+F11` | Step one instruction |
| `Escape` | Close a context menu |

## Inside a terminal panel

The [gdb console and the Program terminal](../features/console.md) are real
terminals, so almost nothing is intercepted there: **only the function keys and
`Ctrl+Shift+…`**.

`Ctrl+C`, `Ctrl+D`, `Ctrl+Z`, Tab and the arrow keys all reach the program or
gdb. That is deliberate — a debugger whose UI ate `Ctrl+C` in the terminal
would be unusable for the programs most worth debugging.

In the gdb console specifically, Tab completes and `↑` recalls history, because
those are gdb's, not ours.

## Holding a key down

`F10` yields at most one step per completed stop. Holding it walks the marker
down the code at the speed the program can actually be stepped, rather than
queueing a hundred steps that carry on after you let go.
