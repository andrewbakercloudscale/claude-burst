#!/bin/zsh
# Parse-check the dashboard's inline JavaScript. The page is one HTML file
# with its script inline, so a syntax error ships as a dead dashboard that
# every Go test still passes. node --check parses without running anything.
set -euo pipefail
page="${0:A:h}/../../internal/admin/admin.html"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
python3 - "$page" "$tmp" <<'PY'
import re, sys
s = open(sys.argv[1]).read()
blocks = re.findall(r'<script>(.*?)</script>', s, re.S)
if not blocks:
    sys.exit("no inline <script> block found in " + sys.argv[1])
for i, b in enumerate(blocks):
    open(f"{sys.argv[2]}/block{i}.js", "w").write(b)
print(f"{len(blocks)} inline script block(s), {sum(map(len, blocks))} bytes")
PY
for f in "$tmp"/block*.js; do node --check "$f"; done
echo "admin.html JavaScript parses"
