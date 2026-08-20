# gdb findings

Behaviours of GDB and its MI interface that this project established by
measurement. They are recorded because each took time to work out, and several
are counter-intuitive enough to be worth rediscovering only once. Test comments
cite them by number, as in "is finding 3 end to end", so the numbering is stable
and new entries are appended rather than inserted.

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
17. **`--all-values` does not stringify char arrays.** `char name[16]` shows no
    value under `--simple-values`, and switching to `--all-values` does not
    help: gdb renders such a child as the literal `"[16]"`, which looks like a
    value but is not. A `char *` already shows its string under
    `--simple-values`. `--simple-values` is therefore kept, and the cost is that
    a string reads as an expandable array of chars.
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
    before gdb knows the architecture is the same mistake that ended a Qiling
    session earlier in this project, through a 312-versus-576-byte `g` packet;
    it fails destructively rather than with an error. The UI therefore warns
    about the ordering. What makes it easy to get wrong is that the symbols pane
    looks as though loading symbols did the job.

24. **An event must not be broadcast before the snapshot describes it.**
    `serve()` publishes the snapshot only *after* `dispatch()` returns, so a
    handler that broadcast from inside announced a change the snapshot did not
    yet carry. A client acting on the event — or a second browser connecting in
    that window and being handed `hello` — could be told a program had loaded
    and simultaneously given a snapshot saying none had.

    It reproduced about one run in fifty, so it appeared as a CI flake
    (`snapshot exePath = ""`) rather than as a bug, and a test asserting from
    the test goroutine cannot catch it reliably. The deterministic test samples
    the snapshot inside the broadcast, which is the only place the ordering is
    observable.

    The fix routes every broadcast through `emit`, which publishes first.
    Building a snapshot reads everything the actor owns, so routing the terminal
    pump through it tripped the race detector immediately, because that runs on
    its own goroutine. `emitOffActor` exists for the two genuinely off-actor
    events — inferior output and gdb dying — whose payloads carry no session
    state. A source-level test checks that nothing calls `Broadcast` directly.

25. **MI has no register-write command.** `-data-list-register-values` has no
    counterpart: gdb 17.1 answers `-data-write-register-values` with
    `^error,msg="Undefined MI command: data-write-register-values"`. Writing a
    register goes through `-gdb-set var $rax = …` instead.

    `-gdb-set` does take the general `--thread` option, which is what makes a
    per-thread register write possible: `-gdb-set --thread 1 var $rbx = 0x5678`
    is accepted. That is not obvious from its documentation, which describes it
    as a pass-through to the CLI `set`.

26. **`-data-write-memory-bytes ""` succeeds and writes nothing.** An empty hex
    string is `^done`, so a UI that lets a value cell be committed empty would
    report a write that never happened. The other malformed inputs gdb does
    catch: `"f"` and `"abc"` give *"Hex-encoded 'f' must represent an integral
    number of addressable memory units"*, and `"zz"` gives *"Invalid argument"*.

    So does `"0xff"` — the prefix is rejected, with the same unhelpful
    *"Invalid argument"*. The hex view writes addresses with a `0x` prefix, so
    a user typing one into a byte cell is following the screen; gdb-wui strips
    it before the command goes out.

27. **`-var-assign` refuses aggregates, and `-var-show-attributes` says so in
    advance.** Assigning to a struct or an array gives *"-var-assign: Variable
    object is not editable"*. `-var-show-attributes` answers `attr="editable"`
    or `attr="noneditable"` — note that the second contains the first as a
    substring, so the answer has to be compared rather than searched.

    For *children* the answer comes free: `-var-list-children --simple-values`
    omits `value` for exactly the aggregate types, which is the same signal the
    tree already uses to decide whether a row can be expanded. Only roots — the
    watches — need the extra round trip.

28. **`-var-assign` returns the value that landed, not the one sent.** Assigning
    `321` to a `char` replies `^done,value="65 'A'"`. Echoing the input back to
    the UI would hide the truncation until the next stop.

29. **`-var-list-children` answers from gdb's varobj cache.** Re-listing a
    struct's children after its bytes have changed underneath returns the old
    values; only `-var-update` re-reads them. Between stops nothing issues one,
    so writing memory through `-data-write-memory-bytes` left an expanded
    struct in the UI showing numbers the program no longer held.

    The consequence is that a *write* has to refresh varobjs for the same
    reason a stop does. It cannot reuse the stop's refresh unchanged, though,
    because that clears every change mark first — which would erase the mark on
    the value the user had just written.

30. **`-data-disassemble -f FILE -l LINE` does not start at that line.** It
    starts at the entry of the function *containing* it: `-f globals.c -l 65
    -n 200 -- 5` came back with its first group labelled `line="57"`, main's
    opening brace, and reached line 65 some way in. So the address of a line is
    found by looking through the grouped output for that line, not by taking
    the first instruction returned. `-n -1` asks for the whole function, which
    bounds the work by the function's size and guarantees the line is inside
    the reply.

    MI has no other route. `-symbol-info-line` does not exist — gdb 17.1
    answers `^error,msg="Undefined MI command: symbol-info-line"` — and
    `info line FILE:N` is an English sentence: *Line 65 of "globals.c" starts
    at address 0x401251 <main+123> and ends at 0x401286 <main+176>.* That is
    parseable here, because gdb is started with `LC_ALL=C` on purpose, but it
    names only the file's basename, and resolving a source path into the
    project needs the full one that `-data-disassemble` reports as `fullname`.

    Two neighbouring answers are worth knowing. A line that generated no code
    gives *Line 55 ... is at address 0x4011d6 <main> but contains no code* —
    the address is the *next* line's, so treating it as line 55's would be
    wrong. And a line past the end of the file is `^done` with a console
    warning rather than an error, so "no address found" has to be an ordinary
    outcome rather than a failure.
31. **A resident Ghidra script cannot save until it hands back the
    framework's transaction.** `analyzeHeadless` opens a transaction named
    after the script and holds it for the whole run, so every save inside one
    fails with `IOException: Unable to lock due to active transaction`. For an
    ordinary script that never matters — the framework commits when it returns
    — but the decompilation sidecar never returns, so nothing it changed could
    ever reach the disk.

    `FlatProgramAPI.end(true)` releases it and `start()` opens another. The
    sidecar calls `end(true)` before its serve loop and `start()` on the way
    out, and only then does `Program.save` succeed. Measured at 6 ms for a
    small program; the edit itself is 4–6 ms.

32. **`-readOnly` does not protect a Ghidra project.** Under it,
    `DomainFile.isReadOnly()` is still false, a script can rename a function,
    and `save` succeeds — the new name is there on the next open. The flag only
    stops `analyzeHeadless` itself saving when a script finishes.

    So the comment that used to sit on the spawn arguments — "gdb-wui reads it
    and must never write to it" — described an intention with no mechanism
    behind it. What protects a project the user named with `-ghidra-project` is
    that gdb-wui does not pass `writable` to the sidecar and the sidecar
    refuses every edit without it.

33. **Saving a program clears Ghidra's undo stack.** `canUndo` is true after a
    committed transaction, with `getUndoName` reporting the transaction's own
    description, and false immediately after `Program.save`. Since an unsaved
    rename lives only inside the sidecar process, saving per edit is not
    optional — so Ghidra's undo cannot be the undo a user sees. gdb-wui keeps a
    journal of inverse edits instead, which also survives a restart of the
    decompiler.

34. **A `HighSymbol` id is stable until the function is edited, and then
    everything in it renumbers.** Two consecutive decompilations of an
    unchanged function give identical ids. After renaming one symbol, an
    untouched neighbour moved from `4611734396939010063` to `…051`.

    Two consequences. An edit has to answer with the whole function decompiled
    again, because every id the client is holding has just gone stale; and a
    stale id must be *refused* rather than applied to whatever now holds it,
    since renaming the wrong variable is worse than renaming nothing.

    The second one is easy to under-implement, because "stale" suggests an id
    that resolves to nothing and the renumbering says otherwise: the ids stay
    dense, so a client's old id lands on a *neighbour*. An edit resolved by id
    with a name beside it therefore has to be resolved by the name, which is
    what the caller pointed at and the only key that does not quietly become
    somebody else. The id is worth keeping for a caller with no name to send,
    and worth ignoring the moment there is one.

    Ids are also large — a decompiler-only symbol's is around 4.6e18 — so they
    cross the wire as strings. A JavaScript number cannot hold one exactly.

35. **Ghidra accepts two functions with the same name.** `Function.setName` to
    a name already in use throws nothing; the program ends up with two symbols
    answering to it. Nothing may therefore look a function up by name — the
    address is the key — and a UI that renames one is the only thing in a
    position to mention that the name is now ambiguous.
    `SymbolTable.getGlobalSymbols(name)` is the check.

36. **`CParserUtils.parseSignature` answers a bad prototype with null**, even
    through the overload that declares `ParseException`. `wibble *wobble(qux)`
    parses to `null` rather than throwing, so an unchecked caller reports
    success and changes nothing — the one outcome worse than failing.

    Applying a signature is also *not* a rename by default:
    `ApplyFunctionSignatureCmd` uses `RENAME_IF_DEFAULT`, which applies the
    types and quietly drops the name unless the function is still called
    `FUN_something`. gdb-wui passes `RENAME`, because the user typed a whole
    prototype and obeying half of it is worse than refusing it.

37. **`DataTypeParser` rejects an unknown type properly**, unlike the prototype
    parser above: `struct not_a_type_at_all` raises
    `InvalidDataTypeException: Unrecognized data type of "struct
    not_a_type_at_all"`. That message is the most informative thing available
    and is passed through to the user unchanged.

38. **`HighFunctionDBUtil.updateDBVariable` creates the database variable a
    decompiler local does not have.** Ghidra's local symbol map mixes symbols
    that exist in the program database with ones the decompiler invented for
    this decompilation — `getSymbol()` is null for the second kind — and only
    this call can rename or retype either. It is what turns the second kind
    into the first: after renaming one, it came back with a small database id
    like the rest.

39. **The decompiler prints two kinds of comment and stores five.** Ghidra's
    listing holds PRE, POST, EOL, PLATE and REPEATABLE comments, but
    `DecompileOptions` defaults display to PRE only — `commentPREInclude` is
    true, `commentPLATEInclude`, `commentPOSTInclude` and `commentEOLInclude`
    are all false — plus the entry point's PLATE comment, which
    `commentHeadInclude` prints as the function's header comment.

    So a comment written anywhere else is stored correctly and shown nowhere:
    an edit that appears to do nothing. gdb-wui writes those two only, and
    refuses a PRE comment on an address outside the function whose text is on
    screen for the same reason — it would be a note the writer never sees
    again.

    A comment token carries the address of the *statement it annotates* rather
    than of any code generated for it. That is why the line map throws those
    addresses away — counted, they would put the program counter on a comment —
    and why the same address is worth reporting separately: it is the only way
    back from a comment on the page to the thing it is about, and so the only
    way a right-click on one can edit it.

40. **Ghidra can say who named something, and it survives.** A name carries a
    `SourceType` — `USER_DEFINED`, `ANALYSIS`, `IMPORTED` or `DEFAULT` — and
    writing one as `ANALYSIS` records "something worked this out" rather than
    "somebody said so". Probed on 12.1.2: a function renamed that way, and a
    local renamed through `HighFunctionDBUtil.updateDBVariable` with the same
    source type, both read back as `ANALYSIS` in a fresh process, and **a
    re-run of full analysis over the project changed neither**. That last part
    was the risk worth measuring: analysis-sourced names are lower priority
    than user ones, and a name that quietly evaporated on the user's next
    Ghidra session would be worse than one never marked.

    The reverse mapping is not exact and must not be presented as one. Ghidra's
    own analysers also produce `ANALYSIS` names — a demangler's, for one — so
    the protocol calls it `inferred` rather than "an agent named this".

    A comment has no source type at all; the listing stores text and nothing
    else. Authorship therefore rides beside it as a bookmark — type `Note`,
    category `gdb-wui/agent` — which survives the save, the reopen and the
    re-analysis alongside the comment, and leaves the comment text exactly as
    it was typed. A marker inside the text would have to be parsed off on the
    way back and would be noise to anyone reading the project in Ghidra.

    Two consequences for the code. The bookmark goes on and comes off with the
    comment, so a person who rewrites an agent's note takes it over. And
    `HighSymbol.getName()` still answers with the *old* name after
    `updateDBVariable` — the object is stale, and only the re-decompilation is
    the truth, which the edit path already returns.

41. **A `DAT_` label is not in the symbol table.** `SymbolTable.getDefinedSymbols()`
    returns nothing for a global whose name Ghidra generated. Those names are
    *dynamic*: Ghidra composes `DAT_001a08de` on demand for an address something
    references, and stores nothing until somebody renames it. Enumerating the
    symbol table therefore finds every global that has already been named and
    none that has not — precisely backwards for a stripped binary, where the
    generated names are the only names there are. Measured: a fixture with one
    global answered with an empty list.

    What creates the name is a reference, so references are what to walk.
    `ReferenceManager.getReferenceDestinationIterator` gives the addresses, and
    `SymbolTable.getPrimarySymbol(addr)` composes the label for each — the same
    name that appears in the decompiled text, which is the one a reader will try
    to look up. The symbol table is still worth a second pass afterwards, for
    data nothing points at directly.

    Two filters are needed and both matter. An address inside a function body is
    a `LAB_` jump target, and there are far more of those than there are
    globals; letting them through buries the twenty names somebody wants under
    two thousand nobody does. And a `FUNCTION` symbol is not data — it has its
    own list, and a merged pane would show every function twice.

42. **An unanalysed byte is one undefined item, whatever follows it, and typing
    it renames the label.** Two things about `Listing.getDataAt`, both of which
    decide what a symbol column may claim.

    `getLength()` answers 1 for undefined bytes. That is a fact about how Ghidra
    represents an address nobody has looked at, not about the program: busybox's
    `applet_names` is a 1954-byte table and reads as one undefined byte until
    somebody types it. So a length is only worth reporting when `isDefined()`,
    and the honest value otherwise is zero — "this address, and nothing about
    what follows it" — which is a different claim from a length of one and the
    only one available. `getDataAt` also answers *nothing* for an address in the
    middle of a defined array, so a label generated inside one comes back with
    no extent: an index that searched every label for what contains an address
    would find that one, see it covers nothing, and stop short of the object it
    sits inside. Only the labels that have an extent belong in that search.

    And applying a type regenerates the label. `DAT_00104000` typed as
    `char[16]` comes back as `s__00104000`, because the generated name describes
    the data and the data has changed. Anything holding the old name across a
    retype is holding a name the program no longer uses — the same staleness as
    finding 34, from the other direction. Verified by
    `TestTheMemoryColumnStopsWhereTheTypeStops`, which reads the name back
    rather than assuming it survived the edit.

    A related refusal: Ghidra will not define data that does not fit its memory
    block. `char[16]` at the last address of a block answers "Insufficent memory
    at address 00102000 (length: 16 bytes)" — spelling Ghidra's, and an ordinary
    outcome rather than a failure worth reporting as one.

43. **`disconnect` kills a process gdb attached to; `detach` is the only way
    out.** Measured against gdb 17.1, attaching to a sleeping process with
    `ptrace_scope=1` and `PR_SET_PTRACER_ANY`.

    Three things, of which the third is the dangerous one. Attaching reports
    itself in MI exactly like a run does — `=thread-group-started,pid="…"`
    followed by a `*stopped` with a frame — so the stack, the threads and the
    pid the terminal needs all arrive by the ordinary path. gdb also reads the
    program out of `/proc/<pid>/exe` by itself, symbols and architecture
    together, so none of the load-order care a remote stub needs applies here.

    Quitting is safe: `-gdb-exit` while attached detaches, and the process lives
    on. An explicit `kill` is not, which is what makes it worth saying — the
    teardown in `mi.doClose` sends one, so a session that does not know it is
    attached ends a program it did not start.

    `disconnect` is not the gentle option it is for a stub. Against a native
    target it answers `A program is being debugged already.  Kill it? (y or n)
    [answered Y; input not from terminal]`, and the process is gone. `detach`
    and `-target-detach` both leave it running, and they are the only commands
    that do.

44. **Ghidra's `frame.size` is not what the prologue did to the stack
    pointer.** Measured on ARM32, AArch64, PowerPC, PowerPC64 and x86-64 builds
    of the same source, with gcc 15.2.0 for each and Ghidra 12.1.2.

    The frame size is derived from the variables Ghidra found: it reaches from
    the frame base down to the lowest slot something references, and stops
    there. `accumulate` opens with `push {r7}` and `sub sp,#20` — 24 bytes —
    and reports a frame size of 20, because nothing touches the word the push
    saved. Treating that as the prologue's effect puts every local four bytes
    low, onto its neighbour, which prints a number rather than an error. The
    same function built for x86-64 reports 36 against a real depth of 8: a leaf
    at `-O0` keeps its locals in the red zone, so `push %rbp` is all that moves
    the stack pointer.

    It gets worse on exactly the binaries the decompiled view is for. Stripped
    of its debug information, the 4128-byte `bigframe` reports a frame size of
    **13**, because with no names for its locals there is less for the size to
    be derived from. The stack depth is unchanged at -4128: `sub sp` is `sub
    sp` whether or not anything describes it.

    What does carry it is Ghidra's own stack analysis,
    `ghidra.app.cmd.function.CallDepthChangeInfo`, whose `getSPDepth` gives the
    depth at any address in the function. Sampled across the body it agrees
    with the instruction stream on every function checked, and with gdb's CFA
    on a live ARM inferior: "Previous frame's sp" is `$sp - spDepth` exactly.
    It costs a symbolic evaluation of the whole function — 465 ms against 3.5 s
    to decompile glibc's 2020-instruction `__printf_buffer`, under a
    millisecond for anything of ordinary size — so it is computed only for a
    function that has something on the stack to apply it to.

    PowerPC puts numbers on how far apart the two can be. 32-bit `accumulate`
    allocates 48 bytes in one `stwu r1,-48(r1)` and reports a `frame.size` of
    56. The 64-bit build of the same function allocates 80 and reports 132,
    with a `localSize` of 192 — not an understatement of the frame but a
    different quantity, and there is no arithmetic on either that recovers 80.

    It also produced the first positive stack offsets: both PowerPC ABIs keep
    the parameter save area in the caller's frame, so a spilled argument sits
    *above* the frame base at `+48` while the locals are below it. Nothing in
    the rule needed changing, but an implementation that assumed stack offsets
    are negative would have been wrong there and nowhere else.

    The depth also survives the prologues that defeat a simpler reading. On
    AArch64 `bigframe` opens with `stp x29, x30, [sp, #-16]!` — a pre-indexed
    store, which moves the stack pointer as a side effect — and then
    `sub sp, sp, x13`, subtracting a *register* whose value the analysis has to
    have propagated. The depth is -4144, which is right, against a `frame.size`
    of 4132.

    The x86-64 rule did not survive either, and it failed the same way one
    level up: it read `$rbp + 8`, which names the entry stack pointer only
    where there is a frame pointer to name it with. `-O0` always has one, every
    binary it was measured on was `-O0`, and `-O2` omits it. Measured on gcc
    15's `-O2` output, the addresses landed 192 bytes away, inside the caller's
    frame — mapped memory holding somebody's live data. The depth is right
    there, and right in the two x86 cases the old rule could not express at
    all: a leaf's red-zone locals below `$rsp`, and 32-bit x86, where emitting
    `$rbp` got `void` out of gdb and showed nothing.

    The MIPS rule did not survive. It read `ghidra_offset + frame.size`, and
    the firmware it was established on is the one binary where that is right:
    `process_packet`'s eleven register spills reach the bottom of its frame, so
    the size and the depth are both 352 there. gcc's ordinary output leaves
    slots nobody touches, and then they part — 20 against 16 on 32-bit
    `accumulate`, 36 against 48 on the 64-bit build of it — which put every
    local one slot away from where it lives. Checked against a live inferior:
    gdb agrees with the depth. Nothing was going to catch that except a second
    binary, built differently, on the same architecture.

45. **A negative assertion read without a barrier passes for ever.** The
    session broadcasts from the actor's goroutine, so a test that counts events
    straight after an action counts whatever had arrived by then. Read too
    early, a positive assertion fails intermittently — unpleasant, but it
    announces itself, which is how finding 24 was found, and how a cross-arch
    test that read the console the moment `console.exec` returned was found:
    on a loaded runner what had arrived was nothing at all. Read too early, a
    negative assertion *passes*, and passes every run: the event that should
    have failed it had not been broadcast yet. The suite counts it as coverage
    and it can never fail.

    `TestRemoteChangedOnlyOnChange` was the case here. Its barrier was real —
    two `mustDo` round-trips through a single-threaded actor — but it was a
    fact about the lines above the assertion rather than anything the assertion
    said, and deleting either call as dead setup would have left it passing and
    worthless. It is now `h.never`, which brackets the action with barriers of
    its own.

    The barrier is `session.ping`. Requests arrive on one channel and the actor
    takes them in order, so a reply to a later request proves the earlier
    handlers ran to completion and emitted whatever they were going to emit.
    That is the whole guarantee, and it stops at gdb's out-of-band records:
    `run()` selects between a waiting request and a waiting record and Go picks
    at random, so stops and inferior output have no barrier, only a wait.
    `TestEventAssertionsWaitOrBarrier` keeps `rec.all` and `rec.count` inside
    the helpers that wait or barrier, by parsing the test files. Like the check
    in finding 24 it reads the source, because the mistake it prevents shows up
    as a test that passes.

46. **The decompiler does not need the analysis; everything derived from the
    listing does.** A 12 MB MIPS64 `vmlinux`, 6.9 MB of it code, cannot be
    analysed at all: `analyzeHeadless` hard-codes a 2 GB heap, and
    `MipsAddressAnalyzer` exhausts it across 22,702 functions. The log is worth
    knowing by sight, because it reports success and failure together —
    `Analysis succeeded for file`, then `Can't checkpoint with locked buffers`,
    then `Import failed`. Only the check for `REPORT: Import succeeded` caught
    it; the exit code is 0.

    Imported with `-noanalysis` instead, the same kernel takes 6.8 seconds and
    597 MB, and the ELF symbol table alone gives 22,143 named functions with
    correct entry points. Decompiling one of them takes 86 ms — from a listing
    holding zero instructions. The decompiler follows flow and translates bytes
    itself; auto-analysis buys cross-references, parameter types and typed
    strings, none of which it consults.

    What breaks is quieter. `spDepth` builds a `CallDepthChangeInfo` by walking
    the function's instructions, and an unanalysed body is one byte long, so
    the depth comes back null and the frame rule has nothing to turn a stack
    offset into an address with. Every stack local shows no value, and the
    decompiled C — the thing a reader would check — is perfect. It reads
    exactly like the documented "a frame whose depth Ghidra cannot settle on".
    The fix is to disassemble the one function being opened (41 ms, plus 6 ms
    to recompute its body), bounded by the next function's entry so following
    flow cannot wander off across the image. The second, related trap: with
    every body one byte long, `getFunctionContaining` answers null for any
    address that is not an entry point, so a program counter one instruction
    into a function finds nothing at all.

    Both are tested by asking for an interior address of a function nothing has
    touched yet, on a program imported with `AnalysisNone`, and asserting a
    stack depth comes back. Asking for the entry point, or asking twice,
    exercises neither.
