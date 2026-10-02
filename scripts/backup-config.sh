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

# The CA bundle is only touched in transparent intercept mode, but on a machine
# behind a corporate proxy it holds the employer's CAs -- losing it would break
# every TLS connection Claude Code makes, not just this tool's.
CA_BUNDLE="${NODE_EXTRA_CA_CERTS:-$HOME/.claude/certs/node-extra-ca-certs.pem}"

for f in "$SETTINGS" "$CONFIG" "$CA_BUNDLE"; do
  [[ -f "$f" ]] || continue
  base="$(basename "$f")"
  cp "$f" "$BACKUP_DIR/$base.$TS.bak"
  cp "$f" "$BACKUP_DIR/$base.latest.bak"
  echo "backed up $f -> $BACKUP_DIR/$base.$TS.bak"
done

# /etc/hosts is root-owned but world-readable, so it can be snapshotted without
# privilege. Restoring it needs root -- that is transparent-root.sh's job, and
# it keeps its own pre-install copy under /etc/claude-burst. This copy is for
# your own reference when comparing after the fact.
if [[ -f /etc/hosts ]]; then
  cp /etc/hosts "$BACKUP_DIR/hosts.$TS.bak"
  cp /etc/hosts "$BACKUP_DIR/hosts.latest.bak"
  echo "backed up /etc/hosts -> $BACKUP_DIR/hosts.$TS.bak (restore needs root: transparent-root.sh remove)"
fi

# Keep the newest 30 timestamped copies of each file; the history had no cap
# and reached 2.4GB by 2026-10-02. *.latest.bak, the restore point, is kept.
# Plain sh on purpose: deploy.sh runs this file with bash, other callers with
# zsh. Names hold a sortable timestamp, so reverse name order is newest first.
for base in config.json settings.json node-extra-ca-certs.pem hosts ninja.andrewbaker.claude-burst.plist; do
  # grep finding no copies of a file is not a failure (pipefail is on).
  ls -1 "$BACKUP_DIR" | { grep -E "^${base//./\\.}\.[0-9]{8}-[0-9]{6}\.bak$" || true; } | sort -r | tail -n +31 |
    while IFS= read -r old; do rm -f -- "$BACKUP_DIR/$old"; done
done
