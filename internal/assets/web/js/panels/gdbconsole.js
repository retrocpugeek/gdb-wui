// The gdb console.
//
// Line mode over MI, not a pty attached to gdb. Giving gdb a terminal would
// conflict with speaking MI over pipes and would bring back the command echo
// the startup handshake exists to turn off. So the line editor lives here —
// about a hundred lines — and each completed line is sent as console.exec.
//
// Tab completion asks gdb via -complete, so this file contains no list of gdb
// commands and cannot fall behind the debugger it is driving.

import { createTerminal } from "../core/terminal.js";

const MAX_HISTORY = 200;

export function createGdbConsole({ element, onSubmit, onComplete }) {
  let line = "";
  let cursor = 0;
  const history = [];
  let historyIndex = 0;
  let busy = false;
  let prompt = "(gdb) ";

  const term = createTerminal({
    element,
    onData: handleData,
    scrollback: 5000,
  });

  function redraw() {
    // \r to the start, write the prompt and line, clear to end of line, then
    // put the cursor where it belongs.
    term.write("\r\x1b[K" + prompt + line);
    const back = line.length - cursor;
    if (back > 0) term.write(`\x1b[${back}D`);
  }

  function submit() {
    const text = line.trim();
    term.write("\r\n");
    line = "";
    cursor = 0;
    if (!text) {
      redraw();
      return;
    }
    if (history[history.length - 1] !== text) history.push(text);
    if (history.length > MAX_HISTORY) history.shift();
    historyIndex = history.length;

    busy = true;
    Promise.resolve(onSubmit?.(text))
      .catch((err) => term.writeln(`\x1b[31m${err?.message ?? err}\x1b[0m`))
      .finally(() => {
        busy = false;
        redraw();
      });
  }

  async function complete() {
    if (!onComplete || !line.trim()) return;
    let res;
    try {
      res = await onComplete(line.slice(0, cursor));
    } catch {
      return;
    }
    if (!res) return;

    if (res.completion && res.completion !== line) {
      line = res.completion;
      cursor = line.length;
      redraw();
      return;
    }
    if (res.matches?.length > 1) {
      term.write("\r\n");
      // A plain list rather than columns: the panel is narrow and gdb's
      // matches are long.
      for (const m of res.matches.slice(0, 40)) term.writeln("  " + m);
      if (res.matches.length > 40 || res.truncated) {
        term.writeln(`  … ${res.matches.length - 40} more`);
      }
      redraw();
    }
  }

  function handleData(data) {
    if (busy) return;
    for (let i = 0; i < data.length; i++) {
      const ch = data[i];
      const code = data.charCodeAt(i);

      if (ch === "\r") {
        submit();
        return;
      }
      if (ch === "\t") {
        complete();
        return;
      }
      if (code === 0x7f || code === 8) {
        if (cursor > 0) {
          line = line.slice(0, cursor - 1) + line.slice(cursor);
          cursor--;
          redraw();
        }
        continue;
      }
      if (code === 3) {
        // Ctrl-C abandons the line being typed, as it does at a real prompt.
        term.write("^C\r\n");
        line = "";
        cursor = 0;
        redraw();
        continue;
      }
      if (code === 21) {
        line = line.slice(cursor);
        cursor = 0;
        redraw();
        continue;
      }
      if (ch === "\x1b") {
        // An escape sequence: arrows for history and cursor movement.
        const seq = data.slice(i);
        if (seq.startsWith("\x1b[A")) {
          if (historyIndex > 0) {
            historyIndex--;
            line = history[historyIndex] ?? "";
            cursor = line.length;
            redraw();
          }
          i += 2;
          continue;
        }
        if (seq.startsWith("\x1b[B")) {
          if (historyIndex < history.length) {
            historyIndex++;
            line = history[historyIndex] ?? "";
            cursor = line.length;
            redraw();
          }
          i += 2;
          continue;
        }
        if (seq.startsWith("\x1b[C")) {
          if (cursor < line.length) {
            cursor++;
            redraw();
          }
          i += 2;
          continue;
        }
        if (seq.startsWith("\x1b[D")) {
          if (cursor > 0) {
            cursor--;
            redraw();
          }
          i += 2;
          continue;
        }
        i += seq.length - 1;
        continue;
      }
      if (code < 32) continue;

      line = line.slice(0, cursor) + ch + line.slice(cursor);
      cursor++;
      redraw();
    }
  }

  return {
    // output writes gdb's own text. The line being edited is redrawn after, so
    // asynchronous output never eats what the user is halfway through typing.
    output(text) {
      term.write("\r\x1b[K" + text.replace(/\n/g, "\r\n"));
      redraw();
    },
    ready(text) {
      if (text) term.writeln(text);
      redraw();
    },
    focus: () => term.focus(),
    resize: () => term.resize(),
    retheme: () => term.retheme(),
    clear() {
      term.clear();
      redraw();
    },
  };
}
