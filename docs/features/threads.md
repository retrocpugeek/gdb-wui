---
title: Threads and the stack
layout: default
parent: Features
nav_order: 10
---

# Threads and the call stack

![The thread list and the call stack](../images/threads.png)

Every thread gdb knows about, with the frame each one is sitting in. Clicking a
thread switches to it, and the call stack, Variables, Registers and source
markers all follow — the reply to a thread switch carries that thread's stack,
so nothing is left showing the previous thread's frames while it catches up.

Clicking a **frame** selects it without moving the program counter. See
[source](source.md#two-markers-because-there-are-two-questions) for what that
looks like, because it is the distinction most worth understanding: everything
you read afterwards comes from the frame you selected.

## All-stop, and only all-stop

When one thread stops, they all stop. When you continue, they all continue.

That is gdb's `all-stop` mode, and it is the only one supported here. Non-stop
mode — running some threads while others sit still — is a real gdb feature that
this UI does not model, and pretending to would be worse than not offering it.

## Worked example

```sh
gcc -g -O0 -no-pie -pthread -o /tmp/tour/threads testdata/fixtures/threads.c
./gdb-wui -project /tmp/tour -exe threads
```

Break on line 30 — inside the worker loop — and Run. The main thread plus three
workers appear. Click between them: `#1` is in `pthread_join` or the barrier,
the workers are in `worker`, and each shows its own stack.

Press ⏸ instead of setting a breakpoint and you interrupt wherever they happen
to be, which for this program is usually inside libc's `nanosleep`. The frame
is still real, and the call stack shows where it came from.

## What it will not do

- **No non-stop mode**, as above.
- **No per-thread breakpoints** in the UI. `break … thread N` at the
  [console](console.md).
- **No thread naming or filtering.** The list is everything gdb reports.
- **No frame filters** or Python frame decorators.
