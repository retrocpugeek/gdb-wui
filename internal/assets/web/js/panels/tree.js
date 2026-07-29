// The project file tree.
//
// Levels are fetched lazily, one directory at a time, and rendered from a
// flattened array of visible rows. Flattening now rather than nesting DOM keeps
// the switch to a virtualised list in a later milestone a change of renderer
// rather than a rewrite: a monorepo with every directory expanded is tens of
// thousands of rows.

import { fetchTree } from "../core/api.js";

export function createTree({ element, onOpenFile, onError }) {
  const expanded = new Set();
  const children = new Map(); // dir path -> entries
  const loading = new Set();
  let selected = null;
  let fileHandler = null;

  async function load(path) {
    if (children.has(path) || loading.has(path)) return;
    loading.add(path);
    try {
      const listing = await fetchTree(path);
      children.set(path, listing);
    } catch (err) {
      onError?.(`${path || "/"}: ${err.message}`);
      // Cache the failure as an empty listing so a broken directory does not
      // re-request on every render pass.
      children.set(path, { path, entries: [], truncated: false, failed: true });
    } finally {
      loading.delete(path);
      render();
    }
  }

  function flatten() {
    const rows = [];
    walk("", 0);
    return rows;

    function walk(dir, depth) {
      const listing = children.get(dir);
      if (!listing) return;
      for (const entry of listing.entries) {
        rows.push({ entry, depth });
        if (entry.dir && expanded.has(entry.path)) walk(entry.path, depth + 1);
      }
      if (listing.truncated) {
        rows.push({ note: `… listing truncated at ${listing.entries.length} entries`, depth });
      }
      if (listing.failed) {
        rows.push({ note: "could not be listed", depth });
      }
    }
  }

  function render() {
    const rows = flatten();
    const frag = document.createDocumentFragment();

    if (rows.length === 0) {
      const note = document.createElement("div");
      note.className = "tree-note";
      note.textContent = loading.size ? "loading…" : "empty";
      frag.append(note);
    }

    for (const row of rows) {
      if (row.note) {
        const note = document.createElement("div");
        note.className = "tree-note";
        note.style.paddingLeft = `${10 + row.depth * 14}px`;
        note.textContent = row.note;
        frag.append(note);
        continue;
      }
      const { entry, depth } = row;
      const el = document.createElement("div");
      el.className = "tree-row " + (entry.dir ? "is-dir" : "is-file");
      if (entry.symlink) el.classList.add("is-symlink");
      if (entry.kind) el.classList.add(`kind-${entry.kind}`);
      el.setAttribute("role", "treeitem");
      el.dataset.path = entry.path;
      el.dataset.kind = entry.kind ?? "";
      el.style.paddingLeft = `${4 + depth * 14}px`;
      if (entry.path === selected) el.setAttribute("aria-selected", "true");
      if (entry.dir) el.setAttribute("aria-expanded", String(expanded.has(entry.path)));

      const twisty = document.createElement("span");
      twisty.className = "tree-twisty";
      twisty.textContent = entry.dir ? (expanded.has(entry.path) ? "▾" : "▸") : "";
      el.append(twisty);

      const name = document.createElement("span");
      name.className = "tree-name";
      name.textContent = entry.name;
      el.append(name);

      // A badge, not just a colour: what happens when you click a file is not
      // a nuance, it is two entirely different actions, and the user should be
      // able to tell them apart before clicking rather than after.
      if (entry.kind === "elf") {
        el.title = `${entry.path} — click to load into gdb`;
        el.append(badge("ELF", "tree-badge-elf"));
      } else if (entry.kind === "exec") {
        el.title = `${entry.path} — executable, but not an ELF program`;
        el.append(badge("exec", "tree-badge-exec"));
      } else if (entry.symlink) {
        el.title = `${entry.path} (symbolic link)`;
      }

      frag.append(el);
    }

    element.replaceChildren(frag);
  }

  element.addEventListener("click", (ev) => {
    const row = ev.target.closest(".tree-row");
    if (!row) return;
    const path = row.dataset.path;
    const isDir = row.classList.contains("is-dir");
    if (isDir) {
      if (expanded.has(path)) {
        expanded.delete(path);
        render();
      } else {
        expanded.add(path);
        render();
        load(path);
      }
      return;
    }
    selected = path;
    render();
    if (fileHandler?.(path, row.dataset.kind || "")) return;
    onOpenFile?.(path);
  });

  return {
    async start() {
      await load("");
    },
    select(path) {
      selected = path;
      render();
    },
    // setFileHandler installs a first-refusal handler for file clicks. It
    // receives the path and the server's kind, and returns true if it consumed
    // the click; otherwise the file opens in the source view.
    setFileHandler(fn) {
      fileHandler = fn;
    },
  };
}

// badge renders the small right-aligned tag that marks a program.
function badge(text, className) {
  const span = document.createElement("span");
  span.className = `tree-badge ${className}`;
  span.textContent = text;
  return span;
}
