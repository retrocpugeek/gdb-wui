#!/usr/bin/env python3
"""Regenerate internal/mi/testdata/records.mi by driving a real gdb.

The parser's corpus is real captured output, not hand-written examples, because
hand-written MI is how you end up with a parser that agrees with your idea of
gdb rather than with gdb. Run this after a gdb upgrade and read the diff:

    python3 scripts/capture-mi.py
    go test ./internal/mi/ -run TestCorpusGolden -update
    git diff internal/mi/testdata/

Records are written byte-identical to what gdb emitted, which means a few
fields are inherently unstable between runs: pid, core, and stack addresses
such as argv's value. Expect those in the diff and ignore them; what matters is
whether any record changed *shape*.

Requires gcc and gdb on PATH. Nothing else.
"""

import os
import re
import subprocess
import sys
import shutil
import tempfile
import threading
import time

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
FIXTURES = os.path.join(ROOT, "testdata", "fixtures")
OUT = os.path.join(ROOT, "internal", "mi", "testdata", "records.mi")


def compile_fixture(tmp, name, *cflags):
    out = os.path.join(tmp, name)
    src = os.path.join(FIXTURES, name + ".c")
    subprocess.run(["gcc", *cflags, "-o", out, src], check=True)
    return out


def capture(binary, commands, home):
    """Run gdb on binary, send commands, return its raw stdout lines."""
    env = dict(os.environ, LC_ALL="C", LANG="C", HOME=home)
    argv = ["gdb", "--nx", "-q", "--interpreter=mi3"]
    if binary:
        argv.append(binary)

    p = subprocess.Popen(argv, stdin=subprocess.PIPE, stdout=subprocess.PIPE,
                         stderr=subprocess.DEVNULL, env=env, bufsize=0,
                         start_new_session=True)
    out = []

    def reader():
        for line in iter(p.stdout.readline, b""):
            out.append(line.decode("utf-8", "replace").rstrip("\n"))

    t = threading.Thread(target=reader, daemon=True)
    t.start()

    time.sleep(0.4)
    for c in commands:
        if c.startswith("#sleep"):
            time.sleep(float(c.split()[1]))
            continue
        try:
            p.stdin.write((c + "\n").encode())
            p.stdin.flush()
        except BrokenPipeError:
            break
        time.sleep(0.35)
    time.sleep(0.5)
    try:
        p.stdin.write(b"-gdb-exit\n")
        p.stdin.flush()
    except BrokenPipeError:
        pass
    try:
        p.wait(timeout=5)
    except subprocess.TimeoutExpired:
        p.kill()
    t.join(timeout=2)
    return out


# (corpus-entry name, regex selecting the record) — first match wins.
WANTED = [
    ("result-done-bare",            r"^\^done$"),
    ("result-done-value",           r'^\^done,value="off"$'),
    ("result-features",             r"^\^done,features=\["),
    ("result-error-simple",         r'^\^error,msg="The program is not being run\."'),
    ("result-error-escaped-quotes", r'^\^error,msg="Function \\"main\\" not defined\."'),
    ("result-error-code",           r'code="undefined-command"'),
    ("result-error-running",        r'^\^error,msg="Selected thread is running\."'),
    ("result-running",              r"^\^running$"),
    ("result-exit",                 r"^\^exit$"),
    ("result-bkpt",                 r"^\^done,bkpt=\{"),
    ("result-breakpoint-table",     r"^\^done,BreakpointTable=\{"),
    ("result-stack-frames",         r"^\^done,stack=\[frame="),
    ("result-variables",            r"^\^done,variables=\["),
    ("result-thread-info-running",  r'^\^done,threads=\[.*state="running"'),
    ("result-register-names",       r"^\^done,register-names=\["),
    ("result-asm-flat",             r"^\^done,asm_insns=\[\{"),
    ("result-asm-src-and-asm",      r"^\^done,asm_insns=\[src_and_asm_line="),
    ("result-memory",               r"^\^done,memory=\["),
    ("result-varobj-create",        r'^\^done,name="v1",numchild='),
    ("result-varobj-children",      r"^\^done,numchild=\"4\",children=\[child="),
    ("result-symbol-lines",         r"^\^done,lines=\[\{pc="),
    ("result-completion",           r"^\^done,completion="),
    ("result-empty-files-list",     r"^\^done,files=\[\]$"),
    ("exec-stopped-signal",         r'^\*stopped,reason="signal-received"'),
    ("exec-stopped-breakpoint",     r'^\*stopped,reason="breakpoint-hit"'),
    ("exec-stopped-exited",         r'^\*stopped,reason="exited-normally"'),
    ("notify-breakpoint-modified",  r"^=breakpoint-modified,"),
    ("notify-library-loaded",       r"^=library-loaded,"),
    ("notify-thread-group-started", r"^=thread-group-started,"),
    ("notify-thread-created",       r"^=thread-created,"),
    ("console-no-symbols",          r'^~"\(No debugging symbols found'),
    ("log-warning",                 r'^&"warning: '),
]

# Reproduce documented findings that are awkward to capture on demand. Each is
# a shape gdb really produces; see docs/findings.md.
SYNTHETIC = [
    # finding 3: the debuggee's stdout lands in the MI stream verbatim.
    ("garbage-inferior-line", "total=3 argc=1"),
    # gdb writes the prompt without a newline, gluing it to the next record.
    ("prompt-glued-to-record", '(gdb) ^done,value="off"'),
    # octal escapes are bytes: \303\251 is one é, plus a literal tab.
    ("log-octal-and-tab", '&"warning: 78\\t\\303\\251\\n"'),
]


def main():
    for tool in ("gcc", "gdb"):
        if subprocess.run(["which", tool], capture_output=True).returncode != 0:
            sys.exit(f"{tool} is required")

    banner = subprocess.run(["gdb", "--version"], capture_output=True, text=True)
    version = banner.stdout.splitlines()[0] if banner.stdout else "unknown"

    # A fixed build directory, not mkdtemp: gdb echoes the binary's path back
    # in several records, and a random path would make every regeneration diff
    # noise instead of signal.
    tmp = os.path.join(tempfile.gettempdir(), "gdb-wui-mi-capture")
    shutil.rmtree(tmp, ignore_errors=True)
    home = os.path.join(tmp, "home")
    os.makedirs(home)
    hello = compile_fixture(tmp, "hello", "-g", "-O0")
    structs = compile_fixture(tmp, "structs", "-g", "-O0")
    nodebug = compile_fixture(tmp, "nodebug", "-O0")
    subprocess.run(["strip", nodebug], check=True)

    lines = []
    lines += capture(hello, [
        "-gdb-show mi-async", "-gdb-show non-stop", "-list-features",
    ], home)
    lines += capture(hello, [
        "-gdb-set confirm off", "-gdb-set startup-with-shell off",
        "-break-insert main", "-break-insert -f nosuchfunction_xyz",
        "-break-list", "-exec-run --start", "#sleep 0.4", "-break-list",
        '-complete "info thr"', "-data-list-register-names",
        "-exec-continue", "#sleep 0.5",
    ], home)
    # A deliberately nested stop, purely for the multi-frame stack record.
    # Breaking in main would give a one-frame stack, and the corpus would
    # then no longer prove that repeated "frame" keys survive parsing —
    # which is the single most important thing the parser does.
    lines += capture(hello, [
        "-gdb-set confirm off", "-gdb-set startup-with-shell off",
        "-break-insert add", "-exec-run", "#sleep 0.5",
        "-stack-list-frames",
    ], home)
    lines += capture(structs, [
        "-gdb-set confirm off", "-gdb-set startup-with-shell off",
        "-break-insert structs.c:29", "-exec-run", "#sleep 0.6",
        "-stack-list-frames", "-stack-list-variables --simple-values",
        "-data-disassemble -s $pc -e $pc+32 -- 0",
        "-data-disassemble -s $pc -e $pc+32 -- 5",
        "-data-read-memory-bytes $sp 16",
        "-var-create v1 * cfg", "-var-list-children v1",
        "-symbol-list-lines structs.c",
    ], home)
    lines += capture(nodebug, [
        "-break-insert main", "-file-list-exec-source-files",
    ], home)
    lines += capture(hello, ["-exec-continue"], home)
    lines += capture("/usr/bin/sleep", [
        "-gdb-set mi-async on", "-gdb-set confirm off",
        "-exec-arguments 30", "-exec-run", "#sleep 0.6",
        "-exec-continue", "-stack-list-frames",
        "-data-list-register-values x", "-thread-info",
        "-exec-interrupt", "#sleep 0.6", "-exec-kill",
    ], home)

    records, missing = [], []
    for name, pattern in WANTED:
        rx = re.compile(pattern)
        for line in lines:
            if rx.search(line):
                records.append((name, line))
                break
        else:
            missing.append(name)
    records += SYNTHETIC

    with open(OUT, "w") as f:
        f.write(f"# Curated MI records captured from {version}.\n")
        f.write("# Format: '@name' line, then exactly one raw record line, byte-identical\n")
        f.write("# to gdb output. Regenerate with scripts/capture-mi.py.\n")
        for name, rec in records:
            f.write(f"@{name}\n{rec}\n")

    print(f"wrote {len(records)} records to {os.path.relpath(OUT, ROOT)}")
    if missing:
        print("MISSING (this gdb did not produce them):", ", ".join(missing),
              file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
