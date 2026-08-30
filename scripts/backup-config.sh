#!/bin/zsh
# Snapshots the two files claude-burst mutates on this machine --
# ~/.claude/settings.json (rewritten by `enable`/`disable`) and
# ~/.config/claude-burst/config.json (rewritten by `configure`) -- to a local,
# git-ignored backups directory. Never commit the backups themselves: they
# can contain machine-specific hooks, paths and identifiers that don't belong
# in a public repo. Run this before any `enable`/`disable`/`configure` call.
set -euo pipefail

BACKUP_DIR="${CLAUDE_BURST_BACKUP_DIR:-$HOME/.config/claude-burst/backups}"
SETTINGS="$HOME/.claude/settings.json"
CONFIG="$HOME/.config/claude-burst/config.json"
TS="$(date +%Y%m%d-%H%M%S)"

mkdir -p "$BACKUP_DIR"

for f in "$SETTINGS" "$CONFIG"; do
  [[ -f "$f" ]] || continue
  base="$(basename "$f")"
  cp "$f" "$BACKUP_DIR/$base.$TS.bak"
  cp "$f" "$BACKUP_DIR/$base.latest.bak"
  echo "backed up $f -> $BACKUP_DIR/$base.$TS.bak"
done
