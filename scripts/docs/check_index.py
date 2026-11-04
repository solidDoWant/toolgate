#!/usr/bin/env python3
"""Fails when a page under docs/ is missing from the tree in docs/README.md.

The tree is the first fenced code block after the "## The tree" heading. A
line ending in "/" that starts in column 0 names a directory; an indented
"name.md" line under it names a page in that directory. A "docs/x.md" line in
column 0 names a page directly.

Usage: check_index.py docs/README.md PAGE...
"""
import re
import sys


def listed_pages(readme):
    text = open(readme, encoding="utf-8").read()
    match = re.search(r"^## The tree\n.*?^```\n(.*?)^```", text, re.S | re.M)
    if not match:
        sys.exit(f"{readme}: no '## The tree' code block")
    pages = set()
    directory = ""
    for line in match.group(1).splitlines():
        first = line.split(maxsplit=1)[0] if line.strip() else ""
        if not line.startswith(" "):
            directory = ""
            if first.endswith("/"):
                directory = first
            elif first.endswith(".md"):
                pages.add(first)
        elif directory and first.endswith(".md"):
            pages.add(directory + first)
    return pages


def main():
    readme, pages = sys.argv[1], sys.argv[2:]
    listed = listed_pages(readme)
    missing = [page for page in pages if page.startswith("docs/") and page not in listed]
    for page in missing:
        print(f"{readme}: the tree does not list {page}", file=sys.stderr)
    return 1 if missing else 0


if __name__ == "__main__":
    sys.exit(main())
