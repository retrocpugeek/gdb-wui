---
title: Troubleshooting
layout: default
nav_order: 6
---

# Troubleshooting

Errors we have actually hit, and what each one means. Most of them are gdb's or
Ghidra's own words, passed through unedited — which is deliberate, but does mean
you sometimes need a translation.

## Path element starting with '.' is not permitted

```
ghidra: Abort due to Headless analyzer error: Path element starting with '.' is not permitted
```

Ghidra refuses **any** path element beginning with a dot, not merely the last
one. `~/.cache/gdb-wui/ghidra` fails, and so does
`/home/you/.config/x/visible/ghidra`.

That rules out every conventional per-user cache location, since
`$XDG_CACHE_HOME` is `~/.cache` and `$XDG_STATE_HOME` is `~/.local/state`, which
is why the analysis cache lives in the visible `<project>/gdb-wui-decomp`.

You will see this if your *project* is under a dotted directory, because then
nothing inside it can hold the cache either. Point the cache somewhere else:

```sh
./gdb-wui -project ~/.local/src/firmware -decomp-dir /var/tmp/gdb-wui-decomp
```

gdb-wui checks the path itself and falls back to a temporary directory rather
than failing, because Ghidra's own message names neither the path nor the
offending element, and arrives only after a JVM has started.

## gdb does not know that architecture

A stock `gdb` is built for one architecture — the host's. Point it at a MIPS or
AArch64 binary and it will not disassemble, the registers will be wrong or
absent, and connecting to a stub produces errors that look like the stub's fault.

```sh
sudo apt install gdb-multiarch
./gdb-wui -project . -gdb gdb-multiarch -exe firmware
```

Loading the ELF is what sets the architecture. Loading only *symbols*
(`symbol-file`, `add-symbol-file`) does not — which is why the Symbols pane's
**+ load** button says so, and why a symbols-only load against the wrong
architecture leaves you with plausible names over nonsense disassembly.

## 'LogType' has unknown type; cast it to its declared type

gdb knows where the symbol is and nothing about what it is. This is the normal
state of a release build: no DWARF, but an intact ELF symbol table.

Cast it, and gdb will read it:

```
(gdb) p LogType
'LogType' has unknown type; cast it to its declared type
(gdb) p (int)LogType
$1 = 7
(gdb) p (char *)&LogBuffer
$2 = 0x404060 <LogBuffer> "ready"
```

Take the *address* for anything that is not a scalar. `p (char *)LogBuffer`
casts the array's first eight bytes to a pointer and dereferences those, which
gives `Cannot access memory at address 0x7964616572` — the characters of
`ready` read as an address.

Hovering such a symbol shows the address rather than a value, for the same
reason. The [memory viewer](features/memory.md) is the better tool here — it
needs no type at all, and it will name the row for you.

## No function at 0x111b900

The address is not in the program the decompiler has. On a running process that
usually means it is in a shared library, the dynamic loader, or an emulator's own
mapping — not in your binary.

The message names the program Ghidra *does* hold so you can see the mismatch. If
the address really should be yours, the load bias is wrong: check that the
program in the Decompiled tab is the one you are running.

## Everything decompiles to FUN_00401136

Ghidra found no symbol table, so it named every function after its address. The
decompiled C is still correct — it is the recovered logic either way — but
nothing is named.

This is what a fully stripped binary looks like. Confirm with:

```sh
readelf -S ./yourbinary | grep symtab   # nothing = stripped
```

If you have a separate unstripped copy or a symbol file, load it from the
Symbols pane's **+ load**, and use *Add symbols…* with an offset if the image
does not run where it was linked.

## globals.c is newer than the program — line numbers may be wrong

Exactly what it says: the source on disk has changed since the binary was built,
so the line table points at lines that have moved. Rebuild.

Worth trusting rather than dismissing — it is the explanation for a breakpoint
that lands two lines from where you clicked.

## Program received signal SIGTERM, on every single step

If your target is [qiling](https://github.com/qilingframework/qiling), this is a
bug in its gdb stub rather than anything about your program. `handle_s` — the
handler for a hardware single-step — decides what to report from
`emu_state`, which is set to `STOPPED` after *every* `uc.emu_start()` and so
means "not currently inside emu_start", not "the program terminated". It
therefore reports `SIGTERM` for every successful step.

The step itself works; only the reply is wrong.

You will see it on **AArch64 and x86** but not on **MIPS**, and the difference is
not the emulator. MIPS has no hardware single-step, so gdb emulates one: it
works out the next instruction's address, plants a breakpoint there and sends
`vCont;c`. That goes through the stub's *continue* handler, which classifies the
stop correctly. On AArch64 gdb sends `vCont;s` and reaches the broken handler.

## The link expired

The login link is single-use and lasts 60 seconds. Mint another against the
running server, keeping your gdb session and breakpoints:

```sh
./gdb-wui -print-url
```

## The Decompiled tab says "Ghidra is starting"

Import and analysis are genuinely slow: seconds for a hello-world, minutes for
firmware. The **Log** tab carries Ghidra's progress line by line, which is the
whole reason that log is not behind a flag — without it a slow start is
indistinguishable from a stuck one.

If it never finishes, the log will say why in Ghidra's own words.
