#!/bin/zsh
# Instant manual rollback: restores ~/.claude/settings.json and
# ~/.config/claude-burst/config.json from the most recent backup-config.sh
# snapshot, and stops the gateway LaunchAgent. Safe to run any time, more
# than once, or when the gateway was never enabled.
set -uo pipefail

BACKUP_DIR="${CLAUDE_BURST_BACKUP_DIR:-$HOME/.config/claude-burst/backups}"
LABEL="ninja.andrewbaker.claude-burst"
SETTINGS="$HOME/.claude/settings.json"
CONFIG="$HOME/.config/claude-burst/config.json"

restored=0
if [[ -f "$BACKUP_DIR/settings.json.latest.bak" ]]; then
  cp "$BACKUP_DIR/settings.json.latest.bak" "$SETTINGS"
  echo "restored $SETTINGS"
  restored=1
fi
if [[ -f "$BACKUP_DIR/config.json.latest.bak" ]]; then
  cp "$BACKUP_DIR/config.json.latest.bak" "$CONFIG"
  echo "restored $CONFIG"
  restored=1
fi

if [[ "$restored" -eq 0 ]]; then
  echo "no backups found in $BACKUP_DIR -- run scripts/backup-config.sh before making changes next time" >&2
fi

launchctl bootout "gui/$UID/$LABEL" >/dev/null 2>&1 || true
pkill -f '/claude-burst serve' >/dev/null 2>&1 || true
echo "stopped claude-burst gateway (if it was running)"
echo "rollback complete -- restart Claude Code"
