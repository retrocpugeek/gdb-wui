---
title: Install
layout: default
nav_order: 2
---

# Install

Two dependencies, no npm, no bundler. The frontend is vendored ES modules served
straight out of the binary.

```sh
git clone https://github.com/retrocpugeek/gdb-wui
cd gdb-wui
go build ./cmd/gdb-wui
```

That produces a single `gdb-wui` executable with the whole frontend embedded in
it. There is no build step for the web assets and no separate install.

## Requirements

| | |
|---|---|
| **Linux, x86-64** | The pty and process-group handling do not port for free. Windows and macOS are not supported. |
| **gdb ≥ 10** | It needs the `mi3` interpreter. Developed against 17.1. |
| **Go ≥ 1.24** | To build. Not needed to run. |
| **gcc** | Only to build the example programs used throughout this documentation. |

The architecture you are debugging matters more than the one you are on. A stock
`gdb` only knows its host architecture; for a MIPS firmware or an AArch64 binary
you want `gdb-multiarch`:

```sh
sudo apt install gdb-multiarch
./gdb-wui -project . -gdb gdb-multiarch
```

Getting this wrong produces a confusing failure rather than a clear one — see
[Troubleshooting](troubleshooting.md#gdb-does-not-know-that-architecture).

## Ghidra, optional

Only the [Decompiled tab](features/decompilation.md) uses it. Without Ghidra
nothing else changes and no warning is printed until you open that tab.

It is an 884 MB install and needs a system JDK 21+. Unpack the release
[from ghidra-sre.org](https://ghidra-sre.org/) somewhere and either set
`GHIDRA_INSTALL_DIR` or pass `-ghidra`:

```sh
./gdb-wui -project . -exe firmware -ghidra ~/ghidra_12.1.2_PUBLIC
```

`~/ghidra`, `/opt/ghidra`, `/usr/share/ghidra` and the version-stamped
directories the official zip unpacks to are all found without a flag.

{: .warning }
> Ghidra refuses any path with an element beginning with a dot — not just the
> last one. That rules out `~/.cache` and every other conventional cache
> location, which is why the analysis cache defaults to the visible
> `<project>/gdb-wui-decomp`. If your *project* lives under a dotted directory,
> pass `-decomp-dir` somewhere that does not.

## Running it

```sh
./gdb-wui -project /path/to/your/repo -exe build/myprogram
```

`-project` is the only directory served; nothing outside it is readable over
HTTP. `-exe` is optional and relative to it.

The link printed on stdout is **single-use and expires after 60 seconds** — it
ends up in `argv`, where `ps` can read it, and in browser history. For another,
without disturbing your session:

```sh
./gdb-wui -print-url
```

Every flag is on the [flags page](reference/flags.md).
