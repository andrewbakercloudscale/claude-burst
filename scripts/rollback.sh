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

DIR="${0:A:h}"

# STEP 1, BEFORE ANYTHING ELSE: undo the machine-wide transparent-mode changes.
#
# While /etc/hosts redirects api.anthropic.com at a port with nothing behind
# it, EVERY process on this Mac that talks to Anthropic fails -- not just this
# session. That is the widest-blast-radius state the tool can create, so it is
# the first thing undone, before any step that could itself fail.
#
# Needs root. Unattended callers (watchdog.sh) require a sudoers NOPASSWD entry
# for this exact path; without one this prints what to run and continues rather
# than aborting the rest of the rollback.
ROOT_HELPER="$DIR/transparent-root.sh"
if [[ -x "$ROOT_HELPER" ]]; then
  if [[ $EUID -eq 0 ]]; then
    "$ROOT_HELPER" remove
  elif sudo -n true 2>/dev/null; then
    sudo -n "$ROOT_HELPER" remove
  else
    # Only nag if something is actually installed; the common case is a
    # base-url-mode user who never had any of this.
    if grep -q '^# BEGIN claude-burst hosts$' /etc/hosts 2>/dev/null; then
      echo "WARNING: /etc/hosts still contains a claude-burst redirect and this" >&2
      echo "         rollback cannot remove it without root. Run NOW:" >&2
      echo "           sudo $ROOT_HELPER remove" >&2
    else
      echo "no transparent-mode changes present (nothing root-owned to undo)"
    fi
  fi
fi

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
CA_BUNDLE="${NODE_EXTRA_CA_CERTS:-$HOME/.claude/certs/node-extra-ca-certs.pem}"
if [[ -f "$BACKUP_DIR/$(basename "$CA_BUNDLE").latest.bak" ]]; then
  cp "$BACKUP_DIR/$(basename "$CA_BUNDLE").latest.bak" "$CA_BUNDLE"
  echo "restored $CA_BUNDLE"
  restored=1
fi

if [[ "$restored" -eq 0 ]]; then
  echo "no backups found in $BACKUP_DIR -- run scripts/backup-config.sh before making changes next time" >&2
fi

launchctl bootout "gui/$UID/$LABEL" >/dev/null 2>&1 || true
pkill -f '/claude-burst serve' >/dev/null 2>&1 || true
echo "stopped claude-burst gateway (if it was running)"
echo "rollback complete -- restart Claude Code"
