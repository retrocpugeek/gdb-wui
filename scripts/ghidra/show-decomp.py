#!/usr/bin/env python3
"""Eyeball a decompilation sidecar produced by ExportDecomp.java.

    show-decomp.py out.json                    # list functions, worst first
    show-decomp.py out.json FUN_001028c0       # one function, annotated
    show-decomp.py out.json main --bias 0x555555554000

Without a function it lists what is in the sidecar, sorted by how ambiguous the
address map is, so the awkward functions are the ones you look at first.

With a function it prints the recovered C with each line's addresses beside it,
marks lines that share an address with another line, and turns each variable's
storage into an expression you can paste into gdb.

--bias shifts every address into the running program's coordinates. Get it from
gdb with `info proc mappings` (the first mapping of your binary), or by
subtracting: bias = gdb's address for a function - the sidecar's `entry` for it.
Without it, addresses are shown as Ghidra recorded them.
"""
import argparse
import json
import sys
from collections import defaultdict

p = argparse.ArgumentParser()
p.add_argument("sidecar")
p.add_argument("function", nargs="?")
p.add_argument("--bias", default=None,
               help="runtime load bias, e.g. 0x555555554000")
p.add_argument("--limit", type=int, default=25, help="functions to list")
args = p.parse_args()

doc = json.load(open(args.sidecar))
if doc.get("schema") != 1:
    sys.exit(f"unknown schema {doc.get('schema')}; this reader knows 1")

bias = int(args.bias, 16) if args.bias else 0
image_base = int(doc["program"]["imageBase"], 16)
psize = doc["program"]["pointerSize"]
lang = doc["program"]["languageId"]


def rt(addr):
    """Ghidra address -> the address gdb would show. Takes hex text or an int."""
    n = int(addr, 16) if isinstance(addr, str) else addr
    return n - image_base + bias if bias else n


def owners(fn):
    """address -> the decompiled lines claiming it."""
    m = defaultdict(list)
    for e in fn["lines"]:
        for a in e["addrs"]:
            m[int(a, 16)].append(e["n"])
    return m


if not args.function:
    rows = []
    for fn in doc["functions"]:
        m = owners(fn)
        shared = sum(1 for ls in m.values() if len(ls) > 1)
        rows.append((shared, len(m), len(fn["lines"]), fn["name"], fn["entry"]))
    rows.sort(reverse=True)
    print(f"{doc['program']['name']}  {lang}  "
          f"{len(doc['functions'])} functions  imageBase {doc['program']['imageBase']}")
    print(f"\n{'function':<28} {'entry':>12} {'lines':>6} {'addrs':>6} {'shared':>7}")
    for shared, naddr, nlines, name, entry in rows[:args.limit]:
        print(f"{name:<28} {entry:>12} {nlines:>6} {naddr:>6} {shared:>7}")
    if len(rows) > args.limit:
        print(f"... {len(rows) - args.limit} more")
    print("\n'shared' = addresses claimed by more than one decompiled line.")
    sys.exit(0)

match = [f for f in doc["functions"]
         if f["name"] == args.function or f["entry"] == args.function]
if not match:
    sys.exit(f"no function {args.function!r}; run without a name to list them")
fn = match[0]
m = owners(fn)

print(f"=== {fn['name']}  {fn['signature']}")
print(f"    entry {fn['entry']}" + (f"  ->  {rt(fn['entry']):#x} at bias {bias:#x}" if bias else ""))
print(f"    frame size={fn['frame']['size']} returnAddressOffset="
      f"{fn['frame']['returnAddressOffset']} growsNegative={fn['frame']['growsNegative']}")
print()

by_line = {e["n"]: e["addrs"] for e in fn["lines"]}
for i, text in enumerate(fn["text"].split("\n"), 1):
    addrs = by_line.get(i, [])
    shared = any(len(m[int(a, 16)]) > 1 for a in addrs)
    shown = " ".join(f"{rt(a):#x}" for a in addrs)
    mark = "!" if shared else " "
    if not text.strip() and not addrs:
        continue
    print(f"{i:>4}{mark}| {text:<64} {shown}")

if any(len(ls) > 1 for ls in m.values()):
    print("\n  ! = this line shares an address with another line. Which are:")
    for a in sorted(m):
        if len(m[a]) > 1:
            print(f"      {rt(a):#x} claimed by lines {m[a]}")

print("\nvariables:")
x86 = lang.startswith("x86")
for v in fn["variables"]:
    st = v["storage"]
    kind = st["kind"]
    if kind == "stack":
        if x86:
            # Verified on x86-64 SysV with a frame pointer: Ghidra's frame base
            # is the entry stack pointer, which points at the return address,
            # and `push %rbp` puts %rbp one pointer below it. NOT portable —
            # see docs/decompilation.md.
            off = st["offset"] + psize
            sign = "+" if off >= 0 else "-"
            expr = f"*({v['type'] or 'long'} *)($rbp {sign} {abs(off):#x})"
        else:
            expr = f"<stack {st['offset']}: frame-base rule unverified for {lang}>"
    elif kind == "register":
        expr = f"${st['register'].lower()}"
        if v["pc"]:
            expr += f"   (only near {rt(v['pc']):#x}; the register is reused)"
    elif kind == "unique":
        expr = "— decompiler temporary, exists nowhere in the machine"
    else:
        expr = f"— {st.get('text', kind)}"
    flag = "param" if v["param"] else "     "
    print(f"  {v['name']:<14} {v['type']:<16} {flag}  {expr}")
