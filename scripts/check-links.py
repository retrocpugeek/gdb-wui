#!/usr/bin/env python3
"""Follow every internal link in the built documentation site.

    make docs-check

Checking the Markdown sources is not enough, and that is the whole reason this
exists. A page written as `[Install](install.md)` points at a file that really
is there, so a source-level check passes — and the built site still 404s,
because the built page is install.html. Only the output can answer the
question, so this reads the output.

Fragments are checked too. A link to `troubleshooting.md#gdb-does-not-know-that-
architecture` lands on the right page whatever the heading is called, so
renaming a heading breaks the link silently — the reader arrives at the top of a
long page instead of at the section.

Exits non-zero and names every dead link, with the page it is on.
"""
import os
import re
import sys
from urllib.parse import unquote, urlparse

LINK = re.compile(r'(?:href|src)="([^"]+)"')
ANCHOR = re.compile(r'\bid="([^"]+)"')


def anchors(path, cache):
    """The ids a built page defines. Kramdown gives every heading one."""
    if path not in cache:
        try:
            body = open(path, encoding="utf-8", errors="replace").read()
        except OSError:
            cache[path] = None
        else:
            cache[path] = set(ANCHOR.findall(body))
    return cache[path]


def main(site):
    if not os.path.isdir(site):
        sys.exit(f"{site} is not a directory; build the site first")

    broken = {}
    cache = {}
    pages = links = fragments = 0

    for root, _, files in os.walk(site):
        for name in files:
            if not name.endswith(".html"):
                continue
            page = os.path.join(root, name)
            pages += 1
            body = open(page, encoding="utf-8", errors="replace").read()
            for match in LINK.finditer(body):
                target = match.group(1)
                parsed = urlparse(target)
                # External and protocol-relative links are not ours.
                if parsed.scheme or parsed.netloc:
                    continue

                # A bare "#section" points into this page.
                resolved = page
                if parsed.path:
                    links += 1
                    path = unquote(parsed.path)
                    resolved = (
                        os.path.join(site, path.lstrip("/"))
                        if path.startswith("/")
                        else os.path.normpath(os.path.join(os.path.dirname(page), path))
                    )
                    if os.path.isdir(resolved):
                        resolved = os.path.join(resolved, "index.html")
                    if not os.path.exists(resolved):
                        broken.setdefault(os.path.relpath(page, site), set()).add(target)
                        continue

                if not parsed.fragment or not resolved.endswith(".html"):
                    continue
                fragments += 1
                ids = anchors(resolved, cache)
                if ids is not None and unquote(parsed.fragment) not in ids:
                    broken.setdefault(os.path.relpath(page, site), set()).add(target)

    print(f"{pages} pages, {links} internal links, {fragments} fragments")
    if not broken:
        return 0
    for page in sorted(broken):
        for target in sorted(broken[page]):
            print(f"BROKEN  {page} -> {target}", file=sys.stderr)
    print(f"\n{sum(len(v) for v in broken.values())} broken links", file=sys.stderr)
    return 1


if __name__ == "__main__":
    root = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
    sys.exit(main(sys.argv[1] if len(sys.argv) > 1 else os.path.join(root, "docs", "_site")))
