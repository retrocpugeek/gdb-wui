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
terminals, so gdb-wui intercepts only the function keys and `Ctrl+Shift+…`
there.

`Ctrl+C`, `Ctrl+D`, `Ctrl+Z`, Tab and the arrow keys all reach the program or
gdb, which is necessary for debugging programs that use them.

In the gdb console, Tab completes and `↑` recalls history; both are gdb's own
features.

## Holding a key down

`F10` produces at most one step per completed stop. Holding it walks the marker
through the code at the speed the program can be stepped, rather than queueing
steps that continue after you release the key.
