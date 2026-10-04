#!/bin/bash
# Sends Codex straight to ChatGPT again: removes Burst's block (the lines
# between "# BEGIN claude-burst" and "# END claude-burst") from Codex's
# config.toml and nothing else. Shell, not the claude-burst binary, so
# rollback, uninstall and repair can run it when the binary is gone or broken.
# The same edit as `claude-burst codex disable`. Exit 0 also when there was
# nothing to remove.
#
#   scripts/codex-unroute.sh            # ${CODEX_HOME:-~/.codex}/config.toml
set -u
TOML="${CODEX_HOME:-$HOME/.codex}/config.toml"
[ -f "$TOML" ] || exit 0
grep -q '^# BEGIN claude-burst' "$TOML" || exit 0
# A begin with no end: leave the file alone rather than delete to its end.
if ! grep -q '^# END claude-burst' "$TOML"; then
  echo "Codex: $TOML has Burst's BEGIN marker but no END; not touched, edit it by hand" >&2
  exit 1
fi
cp -p "$TOML" "$TOML.before-burst-removal" || exit 1
sed -i '' '/^# BEGIN claude-burst/,/^# END claude-burst/d' "$TOML" || exit 1
echo "Codex: removed Burst's provider from $TOML (restart Codex sessions that were open)"
