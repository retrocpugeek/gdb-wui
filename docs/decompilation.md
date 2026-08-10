# Decompilation

For a binary with no source, gdb-wui can show Ghidra's recovered C beside the
live session, with the program counter marked on it. gdb still does the
debugging; this adds a view rather than a second debugger.

The feature is optional. Without `-ghidra` nothing else changes, and the
Decompiled tab says what it needs.

## Using it

```sh
gdb-wui -project DIR -ghidra /opt/ghidra_12.1.2_PUBLIC
```

gdb-wui runs Ghidra as a separate process, in the same way it runs gdb: nothing
is linked and nothing is vendored. It analyses the loaded executable once,
caches the result under `<project>/gdb-wui-decomp` keyed on the binary's sha256,
and keeps a resident decompiler for the session, so decompiling a function takes
100-200 ms rather than the 3.5 s a fresh `analyzeHeadless` would take.

The cache directory is visible rather than hidden because Ghidra refuses any
path element beginning with a dot, anywhere in a project's location. Measured on
12.1.2: `.../x/.gdbwui/ghidra` and `.../x/.hidden/sub/ghidra` are both rejected
with "Path element starting with '.' is not permitted", while the same tree
without dots imports normally. This also rules out the conventional cache
locations, since `$XDG_CACHE_HOME` is `~/.cache` and `$XDG_STATE_HOME` is
`~/.local/state`. If the project itself is under a dotted directory, gdb-wui
falls back to a temporary directory and reports that it has done so.

To use your own Ghidra project instead, with your names, types and comments,
point gdb-wui at it. The project is opened read-only and never written to:

```sh
gdb-wui -ghidra /opt/ghidra_12.1.2_PUBLIC \
        -ghidra-project ~/ghidra-projects/fw/fw.gpr \
        -ghidra-program firmware.elf
```

`-ghidra-program` is required in this case because a project usually holds
several programs, and in Ghidra's Debugger workflow a number of traces as well.
`analyzeHeadless` with no `-process` pattern would process all of them.

Use `-decomp-dir` to move the cache, which a read-only or network-mounted
project needs.

### What the pane does

When the program stops, the pane decompiles the function it is in and marks the
line. The gutter sets breakpoints on lines that have addresses, and hovering a
local or a global reads its value.

Stepping works here, which it does not otherwise: gdb's own stepping needs a
line table, and without one its step range is the whole function, so stepping
over runs to the function's exit. While this tab is showing, a step moves to the
next decompiled line instead.

The **Log** tab shows what the decompiler is doing: what it imported, how long
analysis took, one line per function with its timing, and Ghidra's own
messages.

## Producing a sidecar by hand

```sh
analyzeHeadless /tmp/proj decomp -import ./firmware.elf \
    -scriptPath internal/ghidra/scripts -postScript ExportDecomp.java out.json \
    -deleteProject
```

The Ghidra-side sources are in `internal/ghidra/scripts` because they are
embedded into the binary with `go:embed`, so a built gdb-wui carries its own
decompiler glue and does not need the repository checked out. They are ordinary
Ghidra scripts and can be run by hand, as above.

The script takes an optional second argument, a regular expression; only
functions whose names match are decompiled. On an image with thousands of
functions this is the difference between a quick look and a batch job.

This is the batch counterpart of what the server does on demand. Both emit the
same schema from the same `DecompJson` helper, so the format described below
applies to either.

## Reading one

`scripts/ghidra/show-decomp.py` prints a sidecar in a form you can look at:

```sh
scripts/ghidra/show-decomp.py out.json      # functions, most ambiguous first
show-decomp.py out.json FUN_001028c0        # one function, annotated
show-decomp.py out.json main --bias 0x555555554000
```

`--bias` shifts every address into the running program's coordinates, so that
the output lines up with what gdb shows. Lines marked `!` share an address with
another line. Variables are printed as expressions you can paste into gdb.

Ghidra is Apache-2.0, so unlike gdb there is no licensing reason to keep it at
arm's length. It is kept at arm's length anyway, because it is an 884 MB install
that also needs a system JDK 21 or later, and both have to remain optional.

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
      "source": "IMPORTED",
      "frame": { "size": 96, "localSize": 96, "paramOffset": 0,
                 "returnAddressOffset": 0, "growsNegative": true },
      "variables": [
        { "name": "buf", "id": "57", "source": "DEFAULT",
          "type": "char[64]", "size": 64, "param": false,
          "pc": null, "storage": { "kind": "stack", "offset": -88 } },
        { "name": "cfg", "id": "58", "type": "config *", "size": 8, "param": true,
          "pc": "0x1011e8", "storage": { "kind": "register", "register": "RDI" } },
        { "name": "lVar1", "id": "4611715705241337865", "type": "long", "size": 8,
          "param": false, "pc": "0x1011f9", "storage": { "kind": "unique" } }
      ],
      "lineCount": 20,
      "text": "void inspect(config *cfg)\n\n{\n…",
      "lines": [
        { "n": 10, "addrs": ["0x1011f9"] },
        { "n": 11, "addrs": ["0x10120c", "0x101212", "0x10121d", "0x101237"] },
        { "n": 12, "addrs": ["0x10123c", "0x101243"] }
      ],
      "commentLines": [ { "n": 9, "addr": "0x1011f9" } ],
      "comments": [ { "addr": "0x1011f9", "kind": "pre", "author": "",
                      "text": "cfg->count is a byte count, not an item count" } ]
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

### `commentLines` and `comments`

Two views of the same comments, and they are not interchangeable.

`author` on a comment is `agent` when a Ghidra bookmark at that address marks it
as one, and empty when a person wrote it. A comment has no source type of its
own — the listing stores text and nothing else — so the bookmark is the only
record there is, and it goes on and comes off with the comment it marks.

`comments` is what is stored in the program's listing, as typed. Two kinds are
exported, because two are what the decompiler displays with its default
options: `pre`, printed above the statement its address generated, and `plate`
on the entry point, printed as the function's header comment. A comment stored
anywhere else would be an edit that appears to do nothing.

`commentLines` is where those comments ended up in `text`: the lines that are
wholly comment, with the address each annotates. It comes from the token markup
rather than from the text, because the text cannot be trusted to say — a
decompiled `puts("/* x */")` defeats any prefix test, and a comment longer than
the print width is wrapped across several lines of which only the first would
match. `addr` is absent on a decompiler warning, which belongs to no address.

A comment line appears in `commentLines` and **not** in `lines`. A comment
token carries the address of the statement it annotates rather than of code
generated for it, so counting it in the line map would put the program counter
on a comment.

Both are needed by an editor: the rendering is wrapped and decorated, so what
was typed cannot be recovered from it, and the stored text says nothing about
where it appears on the page.

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

`source` is where the name came from, in Ghidra's own vocabulary:
`USER_DEFINED`, `ANALYSIS`, `IMPORTED` or `DEFAULT`. Empty on a variable means
there is no database symbol at all — the decompiler invented that one for this
decompilation — which is a different thing from a name nobody has touched, and
the two must not be shown alike.

It is exported rather than reduced here because the reduction is lossy and
belongs to the consumer: `ANALYSIS` covers an agent's guess and Ghidra's own
demangler alike, and this document should not decide which of those a reader is
told about. See finding 40.

`id` is Ghidra's symbol id, and it is what addresses a variable for a rename or
a retype. It is a string rather than a number because a symbol the decompiler
invented — one with no database entry, `lVar1` above — gets an id around 4.6e18,
which a JavaScript number cannot hold exactly. Nothing does arithmetic on it.

An id is stable only while the function is unchanged: editing one symbol
renumbers the others. A consumer holding ids across an edit must refresh them,
and a producer asked to act on a stale one must refuse rather than guess.

### `globals`

A separate list, from a separate map. `getLocalSymbolMap()` holds the frame and
nothing else, so a function full of counters and flags — `cnt_drop_malformed`
and its twenty-five siblings in `process_packet` — yields nothing addressable
for any of them from `variables` alone.

They are the readable half of the picture: a fixed address is valid at every
pc, unlike a register, and needs no frame, unlike a stack slot. They are
addressed by number rather than by name, because in a stripped image Ghidra
calls one `DAT_<address>` and gdb has never heard of it.

A symbol in Ghidra's synthetic `EXTERNAL` block is **not** exported. An
undefined symbol resolved from a shared library — `__stack_chk_guard` in any
dynamically linked binary — is parked past the end of the image, and biasing
that address yields a plausible pointer into nothing. Measured on an AArch64
busybox: Ghidra `0x1c9638` against LOAD segments ending at `0xc8938`, which gdb
answers with "Cannot access memory".

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

A stripped image has no such symbol, which is the case the decompiled view exists
for. Measured on a buildroot busybox: all 372 of its function symbols are
undefined imports and none is defined, so gdb and Ghidra share no name. The
anchor in that case is the entry point, which always exists. Its link-time value
is in the ELF header, which gdb-wui reads itself, and its runtime value is what
gdb prints for `info files`. Neither needs a symbol table.

Reading it is the one place gdb-wui parses gdb's prose output.
`-file-list-shared-libraries` omits the main executable, and no MI command
reports a section address, so `info files` is parsed instead. Its console output
is captured at the MI handler rather than from the actor's queue, because `send`
blocks the actor until the reply arrives, and a command whose answer is its
console output could not otherwise be read.

### An address in another module

The most common reason a lookup fails is not a wrong bias but that the program
counter is somewhere the decompiler does not have; stopping in the dynamic
loader is enough to cause it. The consumer knows the image's extent from the ELF
and should say so, because Ghidra's own answer names a translated address the
user never saw:

```
no function 0x111b900
0x00007d3aecfae900 is not inside busybox, which is the only program the
decompiler has. It is in a shared library or the dynamic loader.
```

Not every target needs a bias. `vwfw-linux_64` is a statically linked `EXEC`
rather than a PIE, and Ghidra loaded it at its true `0x120000000`, so the bias
is zero. Firmware is often the simpler case and a desktop hello-world the harder
one.

### Stack offsets

A `stack` offset is relative to Ghidra's frame base rather than to any register
gdb knows. On x86-64 SysV with a frame pointer, measured on two functions in two
different binaries:

```
rbp_offset = ghidra_offset + 8
```

AArch64 has no rule established, so its stack variables get no expression. This
is a measured result rather than an omission: `bb_full_fd_action` in busybox
opens with `stp x19, x20, [sp, #-96]!` and then `sub sp, sp, #4112`, a 4208-byte
frame, while Ghidra reports `frame.size` as 104. The MIPS rule does not
transfer, and nothing else derivable from the sidecar alone does either.

gdb's own CFA does work. `info frame` reports "Previous frame's sp", which
equals Ghidra's frame base exactly and is correct even mid-prologue. It is
reachable over MI as `$sp` evaluated in the caller's frame, and it generalises:
`bl` and `jal` push nothing and `call` pushes 8, so
`entry_sp = caller_sp - callPush` covers x86-64, MIPS64 and AArch64 with one
constant per ISA. This was verified on all three. Adopting it would make
expressions frame-dependent rather than static, which changes what `expr`
promises, so it is recorded here rather than implemented halfway.

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

This constant is **not** portable, and the general rule is simpler than it
looks. Ghidra's frame base is always **the stack pointer at function entry**,
so the address is always:

```
address = entry_sp + ghidra_offset
```

All that changes per ABI is how `entry_sp` is recovered from a register gdb
has. That has to be measured per architecture.

#### MIPS64, measured

Established on `vwfw-linux_64.symbols`, a 2 MB statically linked big-endian
MIPS64 Octeon firmware:

```
sp_offset = ghidra_offset + frame.size
```

`process_packet` opens with `daddiu sp,sp,-352`, which is Ghidra's
`frame.size` exactly, and there is no frame pointer: `jal` leaves the return
address in `$ra` and touches no memory, so the whole frame is that one
instruction. All 16 of its stack variables land on offsets the instruction
stream really uses — ten as a direct `N(sp)` and six as `daddiu rX,sp,N` for an
array base. The prologue's eleven register spills at `264(sp)`…`344(sp)` map to
Ghidra `-88`…`-8`, with `ra` at `-8`, which is the same frame base seen from
the other end.

#### A correction

An earlier draft said `frame.returnAddressOffset` gives a consumer "the inputs
rather than a hardcoded eight". It does not. Ghidra reports
`returnAddressOffset: 0` for **both** x86-64 and MIPS64, so it does not
distinguish the two conventions at all, even though one pushes a return address
onto the stack at the call and the other does not.

What actually carries the information is `frame.size` together with knowing
which register the ABI leaves usable — and that last part is knowledge about
the architecture, not a field in the sidecar. `scripts/ghidra/show-decomp.py`
holds the two rules established so far and refuses to guess for anything else.

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

### Collisions

An address can belong to two decompiled lines, so "which line is the program
counter on" does not always have a single answer, and a consumer has to choose
deterministically.

Two kinds of token are excluded from `addrs`, because their address is a
reference rather than code generated for that line:

- **Comments.** Ghidra's `/* WARNING: Subroutine does not return */` carries
  the address of the call it annotates. Five of the six original collisions in
  `build/structs` were a comment and its own statement. A comment is not code
  and must never be marked as executing.
- **Labels.** Every `goto LAB_00102942` line carries the *label's* address, so
  a label with four `goto`s reaching it was claimed by five lines.

On `build/structs` that leaves one collision, and it is genuine: in
`FUN_00101020`, `(*(code *)0x0)();` and `return;` really do share `0x1026`.

`-O0` fixtures understate the problem. Measured on `/usr/bin/gzip`, which is
97 KiB, 72 functions of ordinary optimised C:

| | collisions | of addresses | adjacent | distant |
|---|---|---|---|---|
| comments excluded only | 2129 | 29.2% | 1336 | 793 |
| labels excluded too | 1614 | 22.4% | 1205 | 409 |

On real code, then, roughly one address in five is claimed by two decompiled
lines, and token filtering cannot remove that: optimised code genuinely has
instructions belonging to more than one expression.

The number matters less than it appears. 75% of what remains is between adjacent
lines — one statement that the pretty-printer wrapped, where either choice is
correct. The distant cases are mostly shared control flow.

A consumer should choose deterministically, preferring the line for which the
address is the minimum of its set and then the lowest line number, and should
show the ambiguity rather than hide it. This is the same class of imprecision as
stepping through `-O2` code with DWARF.

## Limits to know before building on this

- **Decompiled C is a model rather than the truth.** It can be wrong, and
  belongs beside the disassembly rather than in place of it.
- **`-g` makes the output look better than it will be.** `build/structs`
  decompiles to `config *cfg` only because Ghidra's DWARF analyser imported the
  names and types. The stripped case, which is the one this feature is for,
  gives `param_1` and `local_58`. The address mapping is unaffected; only
  readability suffers, which is the argument for showing live values in the
  pane.
- **Analysis is not interactive.** It can never sit on the stop path. The
  sidecar is a cache, keyed on `sha256` and the Ghidra version — a cache keyed
  on a path would happily serve a stale decompilation of a rebuilt binary.
  Measured end to end, including JVM startup and auto-analysis:

  | binary | size | functions | decompile | total |
  |---|---|---|---|---|
  | `build/structs` | 16 KiB | 9 | 31 ms | 6.5 s |
  | `/usr/bin/gzip` | 97 KiB | 72 | 3.8 s | 12.1 s |
  | `vwfw-linux_64` (MIPS64, static) | 2.0 MiB | 21 of 1703 | 0.5 s | 68 s |

  Analysis rather than decompilation dominates on a large image. The firmware's
  68 seconds is almost all auto-analysis, and decompiling 21 named functions
  from it took half a second. Decompiling costs about 50 ms per function, so
  even all 1703 would take under two minutes. Because of that fixed cost, one
  function per invocation is not viable: either export in bulk, as this script
  does, or keep a Ghidra process alive.
- **The sidecar is large.** `/usr/bin/gzip` produces 819 KiB of JSON from a
  97 KiB binary, roughly eight times the input, most of it the address sets. It
  compresses well and it is a cache, but it should not be held in memory per
  connected browser.
- **Decompiling costs more than analysis for repeated work.** Ghidra provides a
  `ParallelDecompiler`; this script is serial and single-purpose.
- **Import and serve cannot be one invocation.** `analyzeHeadless` writes an
  imported program to the project only after the postScript returns, and the
  resident server never returns, because it is the server. Doing both at once
  analyses the binary, serves it and discards it, leaving an empty project for
  the next run. They have to be two invocations.
- **Stepping is reconstructed rather than recovered.** `exec.stepLine`
  single-steps until the pc reaches a different line, which is what gdb does
  with a real line table. It cannot know about inlining, or about statements the
  decompiler merged, so where DWARF exists gdb's own stepping is better and is
  what the source view uses.
- **Fewer variables are readable than the storage kinds suggest.**
  On `/usr/bin/gzip`, of 960 variables:

  | | | |
  |---|---|---|
  | `stack` | 17% | readable anywhere in the frame |
  | `register` | 66% | readable **only near one pc** — the register is reused |
  | `unique` / other | 17% | never readable |

  The firmware splits almost identically — 71% register, 18% stack, 11%
  `unique` across its 21 functions — so this is not an artefact of one binary.

  The middle row is where the trap is. Counting it as "has a location" gives
  83%, but in optimised code the decompiler packs many variables into one
  register: `FUN_001028c0` maps eight of its locals onto `$rax`, and one
  function maps ten. Reading `$rax` for `pcVar5` while stopped anywhere except
  `0x1028f4` gives a wrong answer that looks correct.

  A decompiled pane should therefore show a value for a register variable only
  when the pc is within its live range, and blank it otherwise. `pc` is exported
  for this purpose. The live range itself is not, and obtaining it would mean
  asking the decompiler for the HighVariable's p-code cover rather than a single
  address. That is the main remaining work in a live decompiled view.
