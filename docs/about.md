---
title: About
layout: default
nav_order: 7
---

# About

gdb-wui — a fast and friendly interface to GDB.

It shows source, disassembly, variables, registers, memory, threads and a gdb
console in a browser tab, while GDB itself does the debugging. gdb-wui adds no
debugger of its own: everything on the screen is an answer gdb gave, and
anything gdb-wui has no button for can be typed at
[the console](features/console.md).

The same information is in the window itself, under **?** in the toolbar.

![The About box](images/about.png)

## Version

The box reports the gdb-wui build and the version of gdb behind it. Both belong
in a bug report — most of what you see comes from gdb, and its answers differ
between versions.

The version comes from the server rather than from the page, so a browser tab
left open across a restart reports the server it is actually talking to. See
[which version am I running](troubleshooting.md#which-version-am-i-running).

## Licence

Copyright © 2026 retrocpugeek. Licensed under the
[Apache License, Version 2.0](https://github.com/retrocpugeek/gdb-wui/blob/master/LICENSE).

gdb is GPLv3, and gdb-wui stays clear of it by construction: gdb is spawned as a
separate process and spoken to over the documented GDB/MI protocol. No libgdb is
linked, no gdb source is embedded, and no gdb binary is shipped.

The browser side vendors [xterm.js](https://xtermjs.org/) for the two terminals,
under the MIT licence, with every file hash-verified on each test run; see
[VENDOR.md](https://github.com/retrocpugeek/gdb-wui/blob/master/internal/assets/web/vendor/VENDOR.md).
Nothing else is vendored — there is no npm, no bundler and no webfont.

[Decompilation](features/decompilation.md) is optional and is done by a
[Ghidra](https://ghidra-sre.org/) you install and point gdb-wui at; no part of
Ghidra is distributed here.

## Source and issues

- [Source on GitHub](https://github.com/retrocpugeek/gdb-wui)
- [Issues](https://github.com/retrocpugeek/gdb-wui/issues)

A bug report is most useful with the two versions from the About box, what you
expected, and what happened instead. If gdb and gdb-wui appear to disagree,
`-mi-log` records the whole conversation with gdb; see
[the Log tab](features/console.md#log).
