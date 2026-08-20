---
title: Debugging a kernel
layout: default
parent: Features
nav_order: 14
---

# Debugging a kernel

A kernel is a [remote target](remote.md) like any other, with three differences
that matter: there is no process to start or restart, the debugger stops the
whole machine rather than one thread of it, and the image is usually far larger
and far less annotated than a program. Everything on this page is about those
three.

The examples use a MIPS64 Linux kernel under qemu, because qemu's own gdb stub
is the least trouble: it debugs the emulated machine, so it needs nothing of
the kernel and it is there from the reset vector. [kgdb](#kgdb-on-real-hardware)
is the equivalent on hardware.

## Starting the machine with a debugger attached

`-s` opens a gdb stub on `tcp::1234`, and `-S` holds the processor at the reset
vector until something connects. Together they mean nothing at all has run when
you get your first prompt, so an early-boot breakpoint is as easy as any other:

```sh
qemu-system-mips64 -M malta -kernel vmlinux \
    -drive file=rootfs.ext2,format=raw \
    -append "rootwait root=/dev/sda rcupdate.rcu_cpu_stall_suppress=1" \
    -nographic -s -S
```

Then point gdb-wui at the same image and connect to the stub from the
[address box](remote.md):

```sh
./gdb-wui -gdb gdb-multiarch -project . -exe vmlinux
```

The gdb has to know the target's architecture. A distribution `gdb` built for
your own machine will not talk to a MIPS64 stub; `gdb-multiarch`, or a
cross-gdb from a toolchain, will.

{: .note }
> `rcupdate.rcu_cpu_stall_suppress=1` is not cosmetic. The kernel measures
> wall-clock time, and every second spent sitting at a breakpoint is a second
> it did not run, so it concludes something has wedged and fills the console
> with RCU stall traces. Check which detectors your kernel actually builds
> before adding more: `nowatchdog` and friends do nothing if
> `CONFIG_SOFTLOCKUP_DETECTOR` is not set, and an unrecognised parameter is
> passed to init rather than rejected.

## Build the kernel with debug information

Without `CONFIG_DEBUG_INFO` a kernel carries function names and nothing else:
no line numbers, no types, no arguments. gdb will break on `start_kernel` and
tell you nothing about it, and the [source view](source.md) has nothing to
show. It is a kernel configuration option rather than a gdb-wui one:

```
CONFIG_DEBUG_INFO_DWARF_TOOLCHAIN_DEFAULT=y
CONFIG_GDB_SCRIPTS=y
```

`CONFIG_GDB_SCRIPTS` adds the kernel's own `lx-*` commands, which work in the
[console](console.md) like any others.

If rebuilding is not an option, the [Decompiled tab](decompilation.md) is the
view that still works — see below.

## A kernel is too large for Ghidra's analysis

Nothing is needed from you here: gdb-wui measures the executable sections and,
past 4 MB, imports without Ghidra's analysis and disassembles each function as
it is opened. The Log tab says what it chose.

It has to, because Ghidra's auto-analysis walks the whole image and past a few
megabytes of code it stops finishing — a 12 MB MIPS64 `vmlinux`, 6.9 MB of it
code, exhausts `analyzeHeadless`'s 2 GB heap and the import fails.

For a kernel built normally that costs very little, because the ELF symbol
table already names every function — all 22,143 of them in the image above —
and the decompiler recovers C without help from the analysis.

## A stripped kernel

Strip the image and that stops being true: with no symbol table there are no
functions to list and a program counter resolves to nothing. Two ways back.

**Its own symbol table.** `CONFIG_KALLSYMS` puts the kernel's symbol table
inside the image as ordinary data, so the kernel can name its own functions in
an oops trace, and stripping the ELF does not touch it. A stripped kernel can
therefore still tell you what all of its functions are called — 22,563 of them
in the image above. From a machine running that kernel:

```sh
cat /proc/kallsyms > kallsyms.txt
./gdb-wui -gdb gdb-multiarch -exe vmlinux -ghidra-symbols kallsyms.txt
```

`-ghidra-symbols` takes `address [type] name` lines, which is the format both
`/proc/kallsyms` and `nm` produce, and creates a function for each one before
serving. Names arrive, and every later step behaves as it would on an
unstripped image.

The addresses have to be the ones Ghidra loaded, so a kernel built with KASLR
needs the table from a boot with `nokaslr`, or an offset applied first.

**Or let Ghidra find them.** With no symbols from anywhere,
`-ghidra-analysis=lean` runs the analyzers that discover functions and leaves
out the ones that cost the memory. On the stripped kernel above that is 89
seconds and 1.28 GB, and it finds 12,955 functions: 97% of them start exactly
where a real function starts, but they are only 57% of the real ones, and each
is named `FUN_` and its address. Pattern matching can find where a function is;
it cannot recover what it was called.

`auto` picks between these: `none` for an image whose symbols say where the
functions are, `lean` for a stripped one.

## kgdb, on real hardware

qemu's stub debugs the machine from outside it. On hardware the equivalent is
`kgdb`, which is part of the kernel being debugged:

```
CONFIG_KGDB=y
CONFIG_KGDB_SERIAL_CONSOLE=y
CONFIG_MAGIC_SYSRQ=y
```

Boot with `kgdboc=ttyS0,115200` to say which port it listens on, and either
`kgdbwait` to stop early in boot and wait, or SysRq-G later to break in.
gdb-wui then connects to that serial line the same way it connects to
anything else.

Three things are different from the qemu case, and all three follow from kgdb
living inside the kernel it is debugging:

- **It cannot debug what it is not yet.** kgdb starts when the kernel starts
  it, so the early boot before that point is out of reach. qemu's stub is there
  from the reset vector.
- **It shares the serial port with the console** unless the board has a second
  one, and a console writing to the port a debugger is speaking on is a problem
  for both. A second UART is the usual answer; the alternatives depend on what
  the board and the kernel version offer.
- **A wedged kernel cannot answer.** kgdb runs on the CPU it stopped; a lockup
  with interrupts off, or a fault inside kgdb, leaves nothing listening. The
  qemu stub is unaffected by anything the guest does.

## What does not work as it does for a program

- **There is no `run`.** The kernel is already there. [Stepping](stepping.md),
  breakpoints and [memory](memory.md) work normally; starting and restarting
  do not apply, and neither does the program's exit.
- **Stepping stops the machine.** Every other CPU is frozen while you are at a
  prompt, so timings are meaningless and anything the kernel does with real
  time will notice. The stall parameter above is one consequence; a network
  connection dropping is another.
- **A breakpoint in kernel text may need hardware support.** A software
  breakpoint patches the instruction, and `CONFIG_STRICT_KERNEL_RWX` makes that
  text read-only; `hbreak` uses the processor's debug facilities instead, and
  there are only as many of those as the processor has. The kernel above
  reports "this architecture does not have kernel memory protection" at boot,
  and ordinary breakpoints in it work.
- **Modules are not in the image.** A module is loaded at an address the
  `vmlinux` knows nothing about, so its frames stay unnamed until you tell gdb
  where it went with `add-symbol-file`.
- **Userspace is not in the image either.** A stack that leaves the kernel
  leaves what gdb-wui can name.
