// The console and raw-MI log pane.
//
// Two streams in one pane, distinguished by class: gdb's console output, and —
// when the server is started with -mi-log — the raw MI traffic. The raw view is
// the developer's window into what the server is actually saying to gdb, and it
// is the fastest way to diagnose a protocol misunderstanding.
//
// Lines are capped. An unbounded log is a memory leak with a UI.

const MAX_LINES = 2000;

// formatMillis keeps a duration readable at both ends: analysis is minutes and
// a decompile is milliseconds.
function formatMillis(ms) {
  if (ms < 1000) return `${ms}ms`;
  if (ms < 60000) return `${(ms / 1000).toFixed(1)}s`;
  const m = Math.floor(ms / 60000);
  return `${m}m ${Math.round((ms % 60000) / 1000)}s`;
}

export function createLog({ element }) {
  let lines = [];
  let pinned = true;

  function render() {
    const frag = document.createDocumentFragment();
    for (const line of lines) {
      const div = document.createElement("div");
      div.className = `log-line log-${line.kind}`;
      div.textContent = line.text;
      frag.append(div);
    }
    element.replaceChildren(frag);
    if (pinned) element.scrollTop = element.scrollHeight;
  }

  // Stay pinned to the bottom unless the user has scrolled up to read
  // something, in which case yanking them back is infuriating.
  element.addEventListener("scroll", () => {
    const distance = element.scrollHeight - element.scrollTop - element.clientHeight;
    pinned = distance < 24;
  }, { passive: true });

  function push(kind, text) {
    for (const part of String(text).split("\n")) {
      if (part === "" ) continue;
      lines.push({ kind, text: part });
    }
    if (lines.length > MAX_LINES) lines = lines.slice(-MAX_LINES);
    render();
  }

  return {
    console(text, stream = "console") {
      push(stream === "inferior" ? "inferior" : stream === "log" ? "gdblog" : "console", text);
    },
    mi(direction, text) {
      push(direction === "out" ? "mi-out" : "mi-in", text);
    },
    // decomp is the decompiler's own activity: lifecycle, one line per
    // operation, and Ghidra's milestones. Not behind a flag like the MI
    // stream, because the volume is human-paced and the alternative is a pane
    // that says "starting" for a minute with nothing to show for it.
    decomp(text, level = "info", millis = 0) {
      const kind = level === "error" ? "decomp-error"
        : level === "warn" ? "decomp-warn" : "decomp";
      push(kind, millis > 0 ? `ghidra: ${text} (${formatMillis(millis)})` : `ghidra: ${text}`);
    },
    system(text) {
      push("system", text);
    },
    clear() {
      lines = [];
      render();
    },
  };
}
