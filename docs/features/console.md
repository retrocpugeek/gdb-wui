---
title: Console, terminal and log
layout: default
parent: Features
nav_order: 11
---

# Console, terminal and log

Three tabs along the bottom, and they are three genuinely different streams.

## gdb console

![A gdb command and its answer](../images/console.png)

A real gdb console. Anything you can type at gdb, you can type here — including
everything this UI has no buttons for: `watch`, `condition`, `set var`,
`info proc mappings`, your own `define`d commands.

Tab completes, `↑` recalls history. Commands typed here are not second-class:
set a breakpoint with `break` and it appears in the
[Breakpoints pane](breakpoints.md), because the pane mirrors gdb rather than
tracking what the UI did.

It works while the program is running, too. It is the escape hatch, and gating
it would remove the only way out of a state the UI does not model.

## Program

![Typing into the debuggee](../images/terminal.png)

The debuggee's own terminal — a **pty**, not a pipe, which is the whole point.

`printf("name? ")` has no trailing newline. On a pipe libc block-buffers and the
prompt sits invisible in the buffer until something flushes it; on a tty libc
line-buffers and it appears at once. So the program behaves the way it does in
your shell, and typing here reaches its stdin.

Inside a terminal panel only the function keys and `Ctrl+Shift+…` are
intercepted, so `Ctrl+C`, `Ctrl+D`, Tab and the arrows all reach your program
rather than the UI.

## Log

![The raw GDB/MI traffic under -mi-log](../images/log.png)

Diagnostics about gdb-wui itself, in three kinds:

- **The decompiler's activity**, always. One line per operation with timings —
  see [decompilation](decompilation.md).
- **gdb's own log stream**, the messages that are not answers to anything.
- **Raw GDB/MI traffic**, only under `-mi-log`. Both directions, exactly as it
  went over the wire. That is behind a flag because it is every line of a
  conversation rather than one line per operation.

`-mi-log` is the thing to reach for when the UI and gdb disagree: it shows what
was actually asked and actually answered, which settles it.

## Worked example

Break anywhere in the tour's program, then at the console:

```
(gdb) info frame
(gdb) p ratios
(gdb) watch counter
```

The watchpoint is one of the things with no UI here — and it still shows up as a
row in the Breakpoints pane, labelled `counter` with no address, because gdb
announces it with the same `=breakpoint-created` it uses for everything else and
the pane mirrors gdb rather than tracking what the UI did. You can disable and
delete it from there like the rest.

That is the general shape of this: the console is not a lesser path.

## What it will not do

- **No full terminal emulation.** Line-oriented programs are fine; a curses
  application will not render correctly.
- **No console scripting or macros** beyond gdb's own.
- **No log filtering or search** in the UI. The pane keeps a bounded number of
  lines and drops the oldest.
