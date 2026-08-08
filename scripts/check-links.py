#!/usr/bin/env python3
"""Follow every internal link in the built documentation site.

    make docs-check

Checking the Markdown sources is not enough, and that is the whole reason this
exists. A page written as `[Install](install.md)` points at a file that really
is there, so a source-level check passes — and the built site still 404s,
because the built page is install.html. Only the output can answer the
question, so this reads the output.

Exits non-zero and names every dead link, with the page it is on.
"""
import os
import re
import sys
from urllib.parse import unquote, urlparse

LINK = re.compile(r'(?:href|src)="([^"]+)"')


def main(site):
    if not os.path.isdir(site):
        sys.exit(f"{site} is not a directory; build the site first")

    broken = {}
    pages = links = 0

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
                # External, protocol-relative, and pure fragments are not ours.
                if parsed.scheme or parsed.netloc or not parsed.path:
                    continue
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

    print(f"{pages} pages, {links} internal links")
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
