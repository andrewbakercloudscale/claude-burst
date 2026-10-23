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
# A symlinked config (a dotfiles repo) is edited at its target: sed -i would
# otherwise replace the link with a plain file.
[ -L "$TOML" ] && TOML="$(readlink -f "$TOML")"
if ! grep -q '^# BEGIN claude-burst' "$TOML"; then
  # The Codex app rewrites config.toml when a setting changes in it, which
  # drops the markers and turns Burst's inline table into a table. Taken out
  # in that form too, as the binary does (codex.withoutUnmarked), but only
  # when the table is Burst's: named "Claude Burst", on this Mac's loopback.
  awk '/^\[model_providers\.("?)claude-burst("?)\]/{t=1;next} /^\[/{t=0} t&&/^name = "Claude Burst"/{n=1} t&&/^base_url = "http:\/\/127\.0\.0\.1:/{u=1} END{exit !(n&&u)}' "$TOML" || exit 0
  grep -q '^model_provider = "claude-burst"' "$TOML" || exit 0
  cp -p "$TOML" "$TOML.before-burst-removal" || exit 1
  awk 'BEGIN{top=1} /^\[/{top=0; t=/^\[model_providers\.("?)claude-burst("?)\]/; if(t)next} t{next} top&&/^model_provider = "claude-burst"/{next} {print}' "$TOML.before-burst-removal" >"$TOML" || exit 1
  echo "Codex: removed Burst's provider from $TOML (restart Codex sessions that were open)"
  exit 0
fi
# A begin with no end: leave the file alone rather than delete to its end.
if ! grep -q '^# END claude-burst' "$TOML"; then
  echo "Codex: $TOML has Burst's BEGIN marker but no END; not touched, edit it by hand" >&2
  exit 1
fi
cp -p "$TOML" "$TOML.before-burst-removal" || exit 1
sed -i '' '/^# BEGIN claude-burst/,/^# END claude-burst/d' "$TOML" || exit 1
echo "Codex: removed Burst's provider from $TOML (restart Codex sessions that were open)"
