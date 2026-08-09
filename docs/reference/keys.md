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
| `Ctrl+Shift+G` | Go to a symbol, address or line in the focused view |
| `F7` | Split the centre view, or unsplit it |
| `Shift+F7` | Switch between side by side and stacked |
| `Ctrl+Shift+Z` | Undo the last [decompiler rename or retype](../features/decompilation.md#renaming-what-the-decompiler-guessed) |
| `Escape` | Close a context menu, or the About box |

## While editing a value

Double-clicking a value opens a box over it. See
[changing values](../features/editing.md).

| Key | What it does |
|---|---|
| `Enter` | Write the value through gdb |
| `Escape` | Leave the value alone |

The function keys still work while the box is open, so `F10` steps — and the box
closes when the program moves, since the value under it is a value from a stop
that has passed.

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
