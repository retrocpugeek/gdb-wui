---
title: Config file
layout: default
parent: Reference
nav_order: 3
---

# Config file

To avoid retyping the same arguments, put them in a JSON file called
`gdb-wui.json`. gdb-wui looks for one in two places, in this order:

1. The working directory.
2. `~/.config/gdb-wui/` — or `$XDG_CONFIG_HOME/gdb-wui/` if that is set.

**The first file found is used, and the other is not read.** The settings in
effect are always those of a single file.

Arguments given on the command line override the file.

## Format

The keys are the [flag names](flags.md), without the leading dash:

```json
{
  "gdb": "gdb-multiarch",
  "ghidra": "/home/you/ghidra_12.1.2_PUBLIC",
  "project": ".",
  "open": false,
  "idle-exit": "30m"
}
```

There is no separate list of what a config file may contain: anything on the
[flags page](flags.md) works, except `-version`, `-print-url`, `-config` and
`-no-config`, which are actions rather than settings and are refused.

Values are strings, numbers or booleans. `idle-exit` is written as a string,
because JSON has no duration type; gdb-wui parses it with the same code that
parses `-idle-exit`.

Relative paths mean what they mean on the command line: **relative to the
working directory**. `"project": "."` is the directory you ran gdb-wui in, not
the directory the config file is in.

An unknown key is an error rather than being ignored, so a typo is reported
instead of quietly doing nothing:

```
gdb-wui: config: /home/you/.config/gdb-wui/gdb-wui.json: unknown key "gbd" (did you mean "gdb"?)
```

## Which file was used

gdb-wui prints the file it read at startup, before anything else:

```
gdb-wui: config: /home/you/.config/gdb-wui/gdb-wui.json
```

If no line appears, no config file was found or read.

## Choosing a file, or none

| Flag | What it does |
|---|---|
| `-config PATH` | Read this file instead of searching. If it does not exist, gdb-wui fails rather than falling back. |
| `-no-config` | Do not read any config file. |

Use `-no-config` when reproducing a problem, so that the behaviour depends only
on the command line.

## Permissions

gdb-wui refuses a config file that someone else could have written:

- owned by another user;
- writable by others;
- writable by a group that is not your own.

A config file can set `gdb`, which names an executable that gdb-wui then runs
with your privileges, and the working directory is searched. Without this check
a `gdb-wui.json` committed to a repository would choose the program a bare
`gdb-wui` runs — the same shape of problem as direnv's `.envrc`.

A file writable by your own group is accepted, because most distributions give
each user a group of their own, and refusing it would reject a file created with
an ordinary umask.

If a file is refused, the message says what to run:

```
gdb-wui: config: ./gdb-wui.json is writable by others (mode 0666); refusing to
read it because a config file can choose which gdb to run. Run: chmod go-w
./gdb-wui.json
```

## Worked example

Set a foreign-architecture gdb and a Ghidra installation once, for every
project:

```sh
mkdir -p ~/.config/gdb-wui
cat > ~/.config/gdb-wui/gdb-wui.json <<'EOF'
{
  "gdb": "gdb-multiarch",
  "ghidra": "/home/you/ghidra_12.1.2_PUBLIC"
}
EOF
```

Then, in a project that always debugs the same binary, add a local file that
takes precedence:

```sh
cd ~/src/firmware
cat > gdb-wui.json <<'EOF'
{
  "gdb": "gdb-multiarch",
  "exe": "build/firmware.elf",
  "open": false
}
EOF
```

Because the first file found wins, the local file must repeat `gdb`; it does not
inherit from the one in your home directory.

Now `gdb-wui -project .` uses all of it, and `gdb-wui -project . -gdb gdb`
overrides just the debugger for one run.
