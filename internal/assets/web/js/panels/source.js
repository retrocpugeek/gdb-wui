// The source viewer.
//
// M2 renders every line. That is honestly a placeholder: a 20k-line file is
// 20k DOM nodes and it will not survive M3's stepping hot path, where the
// virtualised list with pooled rows and a translateY sizer replaces this
// renderer. The row markup here is already the shape that list will pool — a
// two-column grid with a sticky gutter cell — so the CSS and the click targets
// carry over unchanged.

import { fetchFile } from "../core/api.js";

export function createSource({ element, pathLabel, metaLabel, onGutterClick }) {
  let current = null;

  function showMessage(text, className) {
    const div = document.createElement("div");
    div.className = className;
    div.textContent = text;
    element.replaceChildren(div);
  }

  async function open(path) {
    current = path;
    pathLabel.textContent = path;
    pathLabel.title = path;
    metaLabel.textContent = "loading…";
    showMessage("loading…", "src-empty");

    let file;
    try {
      file = await fetchFile(path);
    } catch (err) {
      // Stale response: the user clicked another file while this was in
      // flight. Dropping it is the whole reason `current` exists.
      if (current !== path) return;
      metaLabel.textContent = "";
      const hint = err.code === "too_large"
        ? "This file is too large to display. The hex viewer arrives in M7."
        : err.code === "not_found"
          ? "That file no longer exists."
          : err.message;
      showMessage(hint, "src-error");
      return;
    }
    if (current !== path) return;

    const lines = file.text.split("\n");
    // A trailing newline yields a final empty element that is not a line.
    if (lines.length > 1 && lines[lines.length - 1] === "") lines.pop();

    const frag = document.createDocumentFragment();
    lines.forEach((text, i) => {
      const row = document.createElement("div");
      row.className = "src-row";
      row.dataset.line = String(i + 1);

      const gutter = document.createElement("span");
      gutter.className = "src-gutter";
      gutter.textContent = String(i + 1);
      row.append(gutter);

      const code = document.createElement("span");
      code.className = "src-code";
      code.textContent = text;
      row.append(code);

      frag.append(row);
    });
    element.replaceChildren(frag);
    metaLabel.textContent = `${lines.length} lines · ${formatBytes(file.text.length)}`;
  }

  element.addEventListener("click", (ev) => {
    const gutter = ev.target.closest(".src-gutter");
    if (!gutter) return;
    const line = Number(gutter.parentElement.dataset.line);
    onGutterClick?.(current, line);
  });

  function clear() {
    current = null;
    pathLabel.textContent = "No file open";
    metaLabel.textContent = "";
    showMessage("Choose a file from the tree.", "src-empty");
  }

  return { open, clear, get path() { return current; } };
}

function formatBytes(n) {
  if (n < 1024) return `${n} B`;
  if (n < 1024 * 1024) return `${(n / 1024).toFixed(1)} KiB`;
  return `${(n / (1024 * 1024)).toFixed(1)} MiB`;
}
