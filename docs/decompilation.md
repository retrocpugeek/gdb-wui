# Decompilation sidecars

A prototype. Nothing in the server or the frontend reads this yet; what exists
is the exporter and the format, so the shape can be judged against real
binaries before any UI is built on it.

The idea: for a binary with no source, run Ghidra's decompiler once and keep
its output beside the program, so gdb-wui can show recovered C next to the
disassembly with the program counter marked on it. gdb is still in charge —
this adds a view, not a debugger.

## Producing one

```sh
analyzeHeadless /tmp/proj decomp -import ./firmware.elf \
    -scriptPath scripts/ghidra -postScript ExportDecomp.java out.json \
    -deleteProject
```

An optional second script argument is a regular expression; only functions whose
names match are decompiled. On an image with thousands of functions that is the
difference between a prototype and a batch job.

gdb-wui would spawn this exactly as it spawns gdb — a separate process, no
linking, nothing vendored. Ghidra is Apache-2.0, so unlike gdb there is no
licence pressure here; the rule is kept anyway because Ghidra is a gigabyte with
a JRE inside it and must stay an optional dependency.

## The format

```json
{
  "schema": 1,
  "generator": { "tool": "ghidra", "version": "12.1.2", "script": "ExportDecomp" },
  "program": {
    "name": "structs",
    "path": "/home/…/build/structs",
    "format": "Executable and Linking Format (ELF)",
    "sha256": "fcebdac5…",
    "languageId": "x86:LE:64:default",
    "compilerSpec": "gcc",
    "pointerSize": 8,
    "imageBase": "0x100000"
  },
  "functions": [
    {
      "name": "inspect",
      "entry": "0x1011e9",
      "bodyStart": "0x1011e9",
      "bodyEnd": "0x10125e",
      "signature": "void inspect(config * cfg)",
      "frame": { "size": 96, "localSize": 96, "paramOffset": 0,
                 "returnAddressOffset": 0, "growsNegative": true },
      "variables": [
        { "name": "buf", "type": "char[64]", "size": 64, "param": false,
          "pc": null, "storage": { "kind": "stack", "offset": -88 } },
        { "name": "cfg", "type": "config *", "size": 8, "param": true,
          "pc": "0x1011e8", "storage": { "kind": "register", "register": "RDI" } },
        { "name": "lVar1", "type": "long", "size": 8, "param": false,
          "pc": "0x1011f9", "storage": { "kind": "unique" } }
      ],
      "lineCount": 20,
      "text": "void inspect(config *cfg)\n\n{\n…",
      "lines": [
        { "n": 10, "addrs": ["0x1011f9"] },
        { "n": 11, "addrs": ["0x10120c", "0x101212", "0x10121d", "0x101237"] },
        { "n": 12, "addrs": ["0x10123c", "0x101243"] }
      ]
    }
  ]
}
```

Every address is a hex string. A 64-bit address does not survive JSON's
float64, and this document is for firmware.

`schema` is checked, not assumed: a cached sidecar outlives the code that wrote
it, and a consumer that guesses at unknown fields is a consumer that renders the
wrong thing rather than saying it cannot.

### `lines`

`n` is 1-based into `text`. Only lines carrying addresses appear; braces,
declarations and blank lines are omitted rather than padded with nulls.

`text` is rebuilt from the very `ClangLine` objects that `lines` indexes, so
line *n* of the string is line *n* of the map by construction. Emitting
`getC()` and separately walking the token tree would be two renderings of one
function, and any disagreement puts the highlight on the wrong line — which
looks like a decompiler bug rather than an export bug.

**`addrs` is a set, not a range, and that is the whole design.** A decompiled
line's addresses are routinely disjoint. Measured on `build/nodebug`, a
stripped `for` loop:

```
  10 | for (local_10 = 0; local_10 < 5; local_10 = local_10 + 1) {   0x117a 0x1190 0x1198
  11 |     iVar1 = FUN_00101149(local_10);                           0x1188
  12 |     local_c = local_c + iVar1;                                0x118d
```

The loop's init, increment and test are at `0x117a`, `0x1190` and `0x1198`,
with the body's `0x1188` and `0x118d` *between* them. A min/max range for line
10 would swallow lines 11 and 12 whole.

### `variables`

Three storage kinds come out of the decompiler and they are not equally useful:

| kind | meaning |
|---|---|
| `stack` | A real location, once the frame base is reconciled — see below. |
| `register` | A real location, but only near `pc`: the register gets reused. |
| `unique` | A decompiler temporary. It exists **nowhere in the machine** and can never be shown. |
| `other` | Anything else, recorded in Ghidra's own spelling rather than dropped. |

`unique` is why this has to be recorded rather than inferred from the name.
In `inspect`, `lVar1` and `local_58` look alike in the C text; one of them has
a location and the other never will. A UI that shows blanks for some variables
is honest. One that quietly omits them is not.

## The two things a consumer must get right

### Relocation

Ghidra's addresses are link-time. gdb's are not. Ghidra loaded `build/structs`
at `imageBase` `0x100000`; gdb ran it at `0x555555554000`.

Do not compute the bias from `imageBase`. Take any function present in both,
ask gdb for its address, and subtract:

```
bias = gdb_address(sym) - sidecar_entry(sym)
```

For that binary the answer is `0x555555454000`, and it is the same arithmetic
the symbols pane already avoids by jumping to names instead of addresses.

### Stack offsets

A `stack` offset is relative to **Ghidra's frame base**, not to any register
gdb knows. On x86-64 SysV with a frame pointer, measured on two functions in
two different binaries:

```
rbp_offset = ghidra_offset + 8
```

`inspect`: `buf` at Ghidra `-0x58` is `-0x50(%rbp)` in the instruction stream.
`FUN_00101167`: `local_10` at Ghidra `-16` is `-0x8(%rbp)`. The 8 is the saved
frame pointer — Ghidra's base is the entry stack pointer, which points at the
return address, and `push %rbp` moves `%rbp` eight bytes below it.

Verified against a live inferior, on the **stripped** binary, third time round
the loop:

```
ghidra local_10 Stack[-16] via $rbp+(-16+8) = 2      ground truth -0x8(%rbp) = 2
ghidra local_c  Stack[-12] via $rbp+(-12+8) = 8      ground truth -0x4(%rbp) = 8
```

This constant is **not** portable. It follows from the ABI's return-address
convention and from the function actually having a frame pointer, and it must
be established per architecture by measurement — a link register on ARM means
no return address on the stack at all. `frame.returnAddressOffset` and
`frame.growsNegative` are exported so a consumer has the inputs rather than a
hardcoded eight.

## How good is the mapping?

Checked against gdb's own DWARF line table for `build/structs`, which is built
with `-g` so the truth is known:

```
  mapped lines         36
  address collisions   1
  agree with DWARF     22/22  (100% sit inside one source line)
```

Every decompiled line that has DWARF coverage falls inside exactly one source
line. Ghidra's `puts(buf)` statement spans `0x123c–0x1243`; DWARF assigns
`structs.c:23` to `0x123c–0x1247`. They agree on the boundary.

### Collisions, and why `-O0` fixtures flatter them

An address can belong to two decompiled lines, so "which line is the PC on" is
not always a single answer. A consumer must pick deterministically.

Two token kinds are excluded from `addrs` because their address is a
*reference*, not code generated for that line:

- **Comments.** Ghidra's `/* WARNING: Subroutine does not return */` carries
  the address of the call it annotates. Five of the six original collisions in
  `build/structs` were a comment and its own statement. A comment is not code
  and must never be marked as executing.
- **Labels.** Every `goto LAB_00102942` line carries the *label's* address, so
  a label with four `goto`s reaching it was claimed by five lines.

On `build/structs` that leaves one collision, and it is genuine: in
`FUN_00101020`, `(*(code *)0x0)();` and `return;` really do share `0x1026`.

**But `-O0` fixtures are not the interesting case.** Measured on `/usr/bin/gzip`
— 97 KiB, 72 functions, ordinary optimised C:

| | collisions | of addresses | adjacent | distant |
|---|---|---|---|---|
| comments excluded only | 2129 | 29.2% | 1336 | 793 |
| labels excluded too | 1614 | 22.4% | 1205 | 409 |

So on real code roughly **one address in five is claimed by two decompiled
lines**, and no amount of token filtering will remove that — optimised code
genuinely has instructions belonging to more than one expression.

It is much less alarming than the number suggests. 75% of what remains is
between *adjacent* lines: one statement that the pretty-printer wrapped, where
either choice is right. The distant cases are dominated by shared control flow.

A consumer should pick deterministically — preferring the line for which the
address is the minimum of its set, then the lowest line number — and should
show the ambiguity rather than hide it. This is the same class of imprecision
as stepping through `-O2` code with DWARF, and it is honest to present it that
way rather than implying the decompiled line is where the program "is".

## Limits worth stating before building on this

- **Decompiled C is a model, not the truth.** It can be wrong. It belongs
  beside the disassembly, never instead of it.
- **`-g` flatters the output.** `build/structs` decompiles to `config *cfg`
  only because Ghidra's DWARF analyser imported the names and types. The
  stripped case — the actual use case — gives `param_1` and `local_58`. The
  address mapping is unaffected; the readability is much worse, which is
  precisely the argument for showing live values in the pane.
- **Analysis is not interactive.** It can never sit on the stop path. The
  sidecar is a cache, keyed on `sha256` and the Ghidra version — a cache keyed
  on a path would happily serve a stale decompilation of a rebuilt binary.
  Measured end to end, including JVM startup and auto-analysis:

  | binary | size | functions | decompile | total |
  |---|---|---|---|---|
  | `build/structs` | 16 KiB | 9 | 31 ms | 6.5 s |
  | `/usr/bin/gzip` | 97 KiB | 72 | 3.8 s | 12.1 s |

  That is ~50 ms per function, so a few thousand functions is minutes, not
  hours. The fixed ~5 s of JVM startup and analysis is why one function per
  invocation is not viable: export in bulk, as this does, or keep a Ghidra
  process alive.
- **The sidecar is not small.** `/usr/bin/gzip` produces 819 KiB of JSON from a
  97 KiB binary — roughly 8× the input, most of it the address sets. It
  compresses well and it is a cache, but it is not something to hold in memory
  per connected browser.
- **Decompiling is the cost, not analysis.** Ghidra's own `ParallelDecompiler`
  exists; this script is deliberately serial and single purpose.
- **83% of variables have a usable location.** On `/usr/bin/gzip`: 630
  register, 163 stack, 143 `unique`, 24 other. So roughly one variable in six
  in a decompiled function can never show a value, and the UI has to say so.
