---
title: Troubleshooting
layout: default
nav_order: 6
---

# Troubleshooting

Errors you may see, and what they mean. Most are gdb's or Ghidra's own messages,
passed through unchanged, so they sometimes need translating.

## Path element starting with '.' is not permitted

```
ghidra: Abort due to Headless analyzer error: Path element starting with '.' is not permitted
```

Ghidra rejects any path containing an element that begins with a dot, not only
the last one. `~/.cache/gdb-wui/ghidra` is rejected, and so is
`/home/you/.config/x/visible/ghidra`.

This rules out the conventional per-user cache locations, since
`$XDG_CACHE_HOME` is `~/.cache` and `$XDG_STATE_HOME` is `~/.local/state`, which
is why gdb-wui caches analysis in `<project>/gdb-wui-decomp` instead.

You will see this error if your project directory is itself under a dotted
directory, because then nothing inside it can hold the cache either. To fix it,
put the cache somewhere else:

```sh
./gdb-wui -project ~/.local/src/firmware -decomp-dir /var/tmp/gdb-wui-decomp
```

gdb-wui checks the path itself and falls back to a temporary directory rather
than failing, because Ghidra's own message names neither the path nor the
offending element, and only appears after a JVM has started.

## gdb does not know that architecture

A stock `gdb` supports only the architecture it was built for. Pointed at a MIPS
or AArch64 binary it will not disassemble, the registers will be wrong or
missing, and connecting to a stub produces errors that look like the stub's
fault.

To fix this, install a suitable gdb and select it with `-gdb`:

```sh
sudo apt install gdb-multiarch
./gdb-wui -project . -gdb gdb-multiarch -exe firmware
```

Note that loading the ELF is what sets the architecture. Loading only symbols,
with `symbol-file` or `add-symbol-file`, does not — which is why the Symbols
pane's **+ load** button says so, and why loading symbols against the wrong
architecture leaves you with correct-looking names over disassembly that is not
meaningful.

## 'LogType' has unknown type; cast it to its declared type

gdb knows where the symbol is but not what type it is. This is normal for a
release build: no DWARF, but an intact ELF symbol table.

To read the value, cast it:

```
(gdb) p LogType
'LogType' has unknown type; cast it to its declared type
(gdb) p (int)LogType
$1 = 7
(gdb) p (char *)&LogBuffer
$2 = 0x404060 <LogBuffer> "ready"
```

Take the *address* of anything that is not a scalar. `p (char *)LogBuffer` casts
the array's first eight bytes to a pointer and dereferences those, which gives
`Cannot access memory at address 0x7964616572` — the characters of `ready` read
as an address.

Hovering such a symbol shows its address rather than a value, for the same
reason. The [memory viewer](features/memory.md) needs no type at all and will
name the row for you.

## No function at 0x111b900

The address is not in the program the decompiler holds. In a running process
that usually means it is in a shared library, in the dynamic loader, or in an
emulator's own mapping rather than in your binary.

The message names the program Ghidra does hold, so you can see the mismatch. If
the address should be in your binary, the load bias is wrong: check that the
program shown in the Decompiled tab is the one you are running.

## Everything decompiles to FUN_00401136

Ghidra found no symbol table, so it named every function after its address. The
decompiled C is still correct — the recovered logic does not depend on names —
but nothing is named.

This is what a fully stripped binary looks like. To confirm:

```sh
readelf -S ./yourbinary | grep symtab   # no output means stripped
```

If you have an unstripped copy or a separate symbol file, load it with the
Symbols pane's **+ load**, using *Add symbols…* with an offset if the image does
not run where it was linked.

## globals.c is newer than the program — line numbers may be wrong

The source file on disk has changed since the binary was built, so the line
table points at lines that have moved. Rebuild the program.

This message is worth acting on rather than dismissing: it explains a breakpoint
that lands a couple of lines away from where you clicked.

## Program received signal SIGTERM, on every single step

If your target is [qiling](https://github.com/qilingframework/qiling), this is a
bug in its gdb stub rather than anything to do with your program. `handle_s`,
the handler for a hardware single-step, decides what to report from `emu_state`,
which is set to `STOPPED` after every `uc.emu_start()`. That means "not
currently inside emu_start" rather than "the program terminated", so the handler
reports `SIGTERM` for every successful step.

The step itself works; only the reply is wrong.

You will see this on **AArch64 and x86** but not on **MIPS**, and the difference
is in gdb rather than in the emulator. MIPS has no hardware single-step, so gdb
emulates one: it works out the address of the next instruction, sets a
breakpoint there, and sends `vCont;c`. That goes through the stub's continue
handler, which classifies the stop correctly. On AArch64, gdb sends `vCont;s`
and reaches the broken handler.

## The login link expired

The login link is single-use and lasts 60 seconds. To get another without losing
your gdb session or breakpoints:

```sh
./gdb-wui -print-url
```

## The Decompiled tab says "Ghidra is starting"

Import and analysis take seconds for a small program and minutes for firmware.
The **Log** tab shows Ghidra's progress line by line, which is why that log is
not behind a flag: without it, a slow start looks the same as a stuck one.

If it never finishes, the log will say why in Ghidra's own words.

## The call stack is all `?? ()`

gdb has no symbol for the functions of a stripped binary, so it says so. To get
names, run gdb-wui with `-ghidra`: the decompiler knows what is there, and the
frames are named once it has analysed the binary. See
[naming the call stack](features/decompilation.md#naming-the-call-stack).

Those names are the decompiler's, not symbols, and are shown in italics to say
so. Frames in libc keep gdb's own names.

## Which version am I running?

Click **?** in the toolbar.

![The About box](images/about.png)

The box reports the gdb-wui build and the gdb behind it. Both are worth quoting
in a bug report, because most of what gdb-wui shows comes from gdb and its
answers differ between versions.

The version comes from the server rather than the page, so a browser tab left
open across a restart reports the server it is actually talking to.
