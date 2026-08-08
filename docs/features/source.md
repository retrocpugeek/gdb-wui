---
title: Source and files
layout: default
parent: Features
nav_order: 3
---

# Source and files

The file tree shows `-project` and nothing outside it. That is a hard boundary,
not a default: paths are resolved against the project root and anything that
escapes it is refused.

`ELF` badges mark the executables.

![The file tree's ELF menu](../images/files.png)

Right-click one for the three things you can do with it, which are genuinely
three different operations:

| Menu entry | gdb | What it means |
|---|---|---|
| **Load program** | `file` | The program to run. **The only one that sets the architecture.** |
| **Replace symbols** | `symbol-file` | New symbols for the program already loaded. |
| **Add symbols…** | `add-symbol-file` | More symbols, at an offset you give. |

A plain left-click on an ELF loads it as the program. If one is already being
debugged you are asked first — loading a program replaces the inferior, and a
stray click on the wrong row would otherwise throw away a live session.

{: .warning }
> Loading *symbols* does not set the architecture. Only `file` reads it from the
> ELF header. Measured against gdb 17.1 with a MIPS64 image: `file` gives
> `mips:octeon/big`, while `symbol-file` and `add-symbol-file` both leave gdb at
> the host's `i386`. The Symbols pane looking correct is exactly the trap — see
> [remote targets](remote.md).

## Two markers, because there are two questions

![An outer frame selected, with the pc marker left alone](../images/stack.png)

The **green** bar is the program counter: where the program actually is. The
**blue** bar is the line of an outer frame you have selected in the call stack.

Conflating them is a real bug in other debuggers: clicking a caller moves the
"executing here" marker onto code that is not executing, and you lose sight of
where the program really stopped. Here, selecting frame `#1 main()` moves the
blue bar to line 64 and leaves the green one on 49.

Every value you read — the Variables pane, a [hover](variables.md) tooltip — is
read from the **selected** frame, so the two markers also tell you which frame
the numbers belong to.

## When the source is not where gdb says

A binary built elsewhere carries the build machine's paths. When gdb names a
file that is not there, a bar appears offering the files in your project whose
names match, and choosing one tells gdb about the substitution for every file
under that directory rather than just the one.

## What it will not do

- **No editing.** It is a viewer. There is no save.
- **No syntax highlighting.** Each rendered line is a single text node, which is
  what lets the hover evaluator map a pixel to a character without a reverse
  mapping. Highlighting would need that to be rebuilt.
- **No serving files outside `-project`.** Including through symlinks.
- **Very large files are refused** rather than rendered slowly.
