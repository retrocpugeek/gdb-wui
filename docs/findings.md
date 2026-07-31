# gdb findings

Behaviours of GDB and its MI interface that this project had to discover by
measurement, kept because each one cost real time to establish and several are
counter-intuitive enough to be rediscovered the hard way. Test comments cite
them by number — "is finding 3 end to end" — so the numbering is stable and
new entries are appended rather than inserted.

Findings 1-12 were reproduced against gdb 15.1 and re-verified against 17.1.
Everything from 13 onward was found by a failing test or a bug report while
implementing, and each says which gdb it was measured on.

### MI findings that drive the design (all reproduced against gdb 15.1)

1. **`mi-async` and `non-stop` both default to `off`.** `-gdb-show mi-async` → `"off"`.
   So `-exec-interrupt` does not work out of the box — `mi-async on` belongs in the
   startup handshake, not a later toggle.
2. **gdb rejects most commands while the inferior runs.** Verified: `-exec-continue` →
   `^error,msg="Cannot execute this command while the selected thread is running."`;
   `-stack-list-frames` and `-data-list-register-values` → `^error,msg="Selected thread
   is running."`; but `-thread-info` succeeds and reports `state="running"` with no
   `frame`. A run-state gate is structural, not a refinement.
3. **Inferior stdout interleaves into gdb's stdout, mixed with MI records.** With pipes,
   `/bin/echo GARBAGE_LINE_IN_MI_STREAM` produced a bare unparseable line between
   `^running` and `=thread-group-exited`. Two consequences: a separate inferior pty via
   `-inferior-tty-set` is **mandatory**, and the parser must tolerate garbage lines
   rather than erroring. (`@` target-stream records are for remote targets — they do not
   appear natively, so there is no cheap path here.)
4. **Build-time source paths don't resolve.** A real stop reported
   `fullname="./time/../sysdeps/unix/sysv/linux/clock_nanosleep.c"` plus
   `&"warning: ... No such file or directory"`.
5. **No-debug-info is a live path.** `/bin/ls` → `~"(No debugging symbols found)"`,
   frames with `func="??"`, `-file-list-exec-source-files` → `files=[]`,
   `-break-insert main` → `^error`. `file`/`line`/`fullname` must be **optional types**;
   `addr`/`at` are the only guaranteed frame identity.
6. **Pending breakpoints mutate.** `-break-insert -f` → `addr="<PENDING>"`, then a
   `=breakpoint-modified` supplies the real address. Breakpoint state must be event-driven.
7. **`-data-list-register-names` contains empty strings at stable indices.** Registers
   are identified by **number**, never name.
8. **The value grammar needs a real parser.** Captured shapes include repeated keys
   inside lists (`stack=[frame={…},frame={…}]`, `body=[bkpt={…},bkpt={…}]`), nested
   tuples in lists (`ranges=[{from=…,to=…}]`), and C-escaped strings.
   `encoding/json` cannot read any of it, and a `map[string]any` parser silently drops
   the duplicate keys.
9. **Single MI records get large** — a 200 KB `-data-read-memory-bytes` produced a
   16 KB single line, and `-var-list-children` on a big array goes far higher. Use
   `bufio.Reader.ReadString('\n')`, **not** `bufio.Scanner` (64 KiB default token cap
   fails in a way that looks like a hang).
10. **`-complete` exists in MI3**: `-complete "info thr"` →
    `^done,completion="info threads",matches=[…]`. Console tab-completion works over MI,
    so gdb itself never needs a pty.
11. **`-exec-run --start` injects a temporary breakpoint** visible in `-break-list`
    (`disp="del"`). The breakpoint mirror must filter breakpoints we didn't create.
12. **`=library-loaded` floods** (one per shared object) — suppress by default.

### Corrections found while implementing (gdb 17.1)

All twelve findings above were re-verified against gdb 17.1 during M1 and hold.
What follows is what the plan got wrong or left out, each found by a test while
implementing:

13. **There is no `-exec-kill`.** `^error,msg="Undefined MI command:
    exec-kill",code="undefined-command"`. The `exec.kill` message is therefore a
    semantic command implemented with `-interpreter-exec console "kill"`, not a
    passthrough. (The lifecycle section already assumed console `kill`; the
    message-group list did not.)
14. **`-stack-list-frames` does not return arguments.** A stack panel showing
    `main(argc=1, argv=0x…)` needs a second command,
    `-stack-list-arguments --simple-values`, merged by frame level. It is one
    extra round-trip per stop, still inside the single fat `stopped` event.
15. **gdb reports an exit twice, and the code is on the first one.**
    `=thread-group-exited,exit-code="0"` arrives *before*
    `*stopped,reason="exited-normally"`, and only the notification carries the
    code — in octal. The two must be merged, or the UI sees a codeless exit
    followed by a redundant second event.
16. **`-var-create` does not take `--thread`/`--frame` where the plan puts
    them.** The varobj section writes
    `-var-create r17 * --thread T --frame F expr`; gdb 17.1 answers
    `^error,msg="-var-create: Usage: NAME FRAME EXPRESSION."`, having read the
    options as part of the expression. MI general options come *before* a
    command's positional arguments:
    `-var-create --thread T --frame F r17 * expr`.
17. **`--all-values` does not stringify char arrays.** On finding that
    `char name[16]` shows no value under `--simple-values`, the temptation is
    to switch. It does not help: gdb renders such a child as the literal
    `"[16]"`, which looks like a value and is not. A `char *` already shows its
    string under `--simple-values`. The plan's choice stands, and the cost — a
    string reads as an openable array of chars — is a real limitation rather
    than an oversight.
18. **Varobj children of a pointer are the pointee's fields.** `struct item
    *items` expands straight to `id`/`name`/`weight`; gdb dereferences for you.
    Numeric children appear only for genuine arrays, which is the only place
    the `[n]` path form is needed.

### Found later, by tests and by use

19. **`-symbol-info-*` addresses are link-time until the program runs.**
    The `nondebug` entries come from the ELF symbol table, so before the
    process exists every address for a position-independent executable is
    the unrelocated one: `_start` reads `0x1060` while the running process
    has it at `0x555555555060`. Re-reading the table after the program
    starts gives relocated addresses instead. So a *cached* table is
    dangerous in a way a fresh one is not, and the danger is asymmetric:
    disassembling a link-time address succeeds *before* the run, reading
    from the exec file, and fails with "Cannot access memory" *after* — the
    opposite of the intuition that a live process makes more memory
    readable. The symbol pane therefore jumps by *name* and lets gdb apply
    the load bias, which is correct whatever the cache holds. Verified by
    `TestDisassembleBySymbolNameFollowsRelocation`, which fails with the
    resolution removed.

20. **`-symbol-info-functions` and `-symbol-info-variables` partition the
    nondebug symbols between them.** Each reports its own `nondebug` list —
    ELF symbols gdb classifies as code and as data respectively — with no
    overlap. That split is the only source of the function-vs-variable
    distinction for a stripped binary, which has no debug info to ask.
    Without `--include-nondebug` neither command returns them at all, and a
    stripped binary yields a bare `symbols={}`.

21. **A symbol with no debug info cannot be evaluated, only located.** For a
    binary built without `-g`, `-data-evaluate-expression LogType` fails with
    `'LogType' has unknown type; cast it to its declared type`, and
    `-data-disassemble -a LogType` fails the same way — gdb knows where the
    symbol is but not what it is, and both of those ask for its value.
    `-data-evaluate-expression &(LogType)` succeeds and yields
    `0x4010 <LogType>`. Every path that turns a name into an address
    therefore falls back to address-of. This is the normal case for a release
    firmware image, not an edge case. Verified by
    `TestResolveMinimalSymbolAddress`.

22. **Data symbols belong in the memory view, not the disassembler.**
    Disassembling a variable produces plausible-looking nonsense. The symbol
    pane routes by kind: a variable without debug info opens the memory
    viewer at `&(name)`, and the address-of matters even for a variable that
    *does* have a type — a bare `LogType` holding 7 resolves to the address
    7, which is a readable-looking answer to the wrong question.

23. **Only `file` sets the architecture; loading symbols does not.** Measured
    with gdb 17.1 against a MIPS64 big-endian image: `file` leaves the
    architecture at `mips:octeon` and endianness big, while `symbol-file` and
    `add-symbol-file` both leave them at the host's `i386`/little. `exec-file`
    with no argument, to drop the exec file afterwards, reverts the
    architecture to the host's but leaves the endianness — a half-state worse
    than either. `set architecture` and `set endian` do stick.

    This matters because `target remote` immediately reads the stub's
    registers, and the register layout is architecture-dependent. Connecting
    before gdb knows the architecture is the same mistake that killed a Qiling
    session earlier in this project via a 312-vs-576-byte `g` packet: it fails
    destructively, not politely. So the ordering rule is real and the UI warns
    about it — and the trap is specifically that the symbols pane *looks* like
    it did the job.

24. **An event must not be broadcast before the snapshot describes it.**
    `serve()` publishes the snapshot only *after* `dispatch()` returns, so a
    handler that broadcast from inside announced a change the snapshot did not
    yet carry. A client acting on the event — or a second browser connecting in
    that window and being handed `hello` — could be told a program had loaded
    and simultaneously given a snapshot saying none had.

    It reproduced about one run in fifty, which is why it surfaced as a CI
    flake (`snapshot exePath = ""`) rather than a bug, and why a test asserting
    from the test goroutine cannot catch it reliably. The deterministic test
    samples the snapshot *inside* the broadcast, which is the only vantage
    point where the ordering is observable.

    Fixed by routing every broadcast through `emit`, which publishes first.
    The catch is that building a snapshot reads everything the actor owns:
    routing the terminal pump through it tripped the race detector at once,
    because that runs on its own goroutine. Hence `emitOffActor` for the two
    genuinely off-actor events — inferior output and gdb dying — whose payloads
    carry no session state. A source-level test enforces that nothing calls
    `Broadcast` directly.
