#!/usr/bin/env python3
"""Check that every relative link and #anchor in the repository's Markdown resolves.

Usage: python3 scripts/ci/check-doc-links.py [repo-root]

External links (http, https, mailto) are not fetched. A relative link must name a
file or directory that exists, and an #anchor must match a heading in the target
file, using GitHub's slug rules. Exits 1 and lists every broken link if any are
found, and always says how many links it checked, so a checker that has stopped
finding links is visible rather than reassuring.
"""
import os
import re
import subprocess
import sys

ROOT = os.path.abspath(sys.argv[1] if len(sys.argv) > 1 else os.path.join(os.path.dirname(__file__), "..", ".."))

LINK = re.compile(r"!?\[(?:[^\[\]]|\[[^\]]*\])*\]\(([^)\s]+)(?:\s+\"[^\"]*\")?\)")
HREF = re.compile(r"""<(?:a|img)\s[^>]*?(?:href|src)=["']([^"']+)["']""")
FENCE = re.compile(r"^\s*(```|~~~)")


def markdown_files():
    out = subprocess.run(["git", "-C", ROOT, "ls-files", "--cached", "--others", "--exclude-standard", "*.md"],
                         capture_output=True, text=True, check=True).stdout.split()
    return sorted(f for f in out if os.path.exists(os.path.join(ROOT, f)))


def slug(heading):
    s = heading.strip().lower()
    s = re.sub(r"<[^>]+>", "", s)
    s = re.sub(r"\[([^\]]*)\]\([^)]*\)", r"\1", s)
    s = re.sub(r"[^\w\- ]", "", s)
    return s.replace(" ", "-")


_anchor_cache = {}


def anchors(path):
    if path in _anchor_cache:
        return _anchor_cache[path]
    seen, result, fenced = {}, set(), False
    with open(path, encoding="utf-8") as f:
        for line in f:
            if FENCE.match(line):
                fenced = not fenced
                continue
            if fenced:
                continue
            m = re.match(r"^(#{1,6})\s+(.*?)\s*#*\s*$", line)
            if not m:
                continue
            base = slug(m.group(2))
            n = seen.get(base, 0)
            seen[base] = n + 1
            result.add(base if n == 0 else f"{base}-{n}")
    _anchor_cache[path] = result
    return result


def links(path):
    fenced = False
    with open(path, encoding="utf-8") as f:
        for no, line in enumerate(f, 1):
            if FENCE.match(line):
                fenced = not fenced
                continue
            if fenced:
                continue
            # Inline code is not a link.
            text = re.sub(r"`[^`]*`", "", line)
            for m in list(LINK.finditer(text)) + list(HREF.finditer(text)):
                yield no, m.group(1)


def main():
    files = markdown_files()
    checked, broken = 0, []
    for rel in files:
        src = os.path.join(ROOT, rel)
        for no, target in links(src):
            if re.match(r"^[a-z][a-z0-9+.-]*:", target, re.I):
                continue
            checked += 1
            path, _, frag = target.partition("#")
            dest = src if path == "" else os.path.normpath(os.path.join(os.path.dirname(src), path))
            if not os.path.exists(dest):
                broken.append(f"{rel}:{no}: missing file {target}")
                continue
            if frag and dest.endswith(".md") and frag not in anchors(dest):
                broken.append(f"{rel}:{no}: missing anchor {target}")
    for b in broken:
        print(b)
    print(f"checked {checked} relative link(s) in {len(files)} Markdown file(s): {len(broken)} broken")
    if checked == 0:
        print("no links found at all: the checker is not looking at anything", file=sys.stderr)
        return 1
    return 1 if broken else 0


if __name__ == "__main__":
    sys.exit(main())
