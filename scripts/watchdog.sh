#!/bin/zsh
# Auto-revert safety net. Launch this detached (nohup ... & disown)
# immediately after `claude-burst enable`. It waits, then checks the gateway
# is actually alive and serving; if not, it runs rollback.sh automatically.
# Always logs the outcome, and fires a macOS notification either way so this
# is visible even if the terminal that launched it is gone.
#
# Usage: watchdog.sh [delay_seconds]   (default 60)
set -uo pipefail

# Resolve this script's directory. Written the POSIX way rather than with zsh's
# ${0:A:h}: these scripts are recovery tooling, and someone reaching for them in
# an emergency will type `bash scripts/rollback.sh` as readily as `zsh`. Under
# bash the zsh form expands to an unbound-variable error on line 2 and the
# script does nothing at all -- a rollback that silently no-ops is worse than
# one that refuses to run.
DIR="$(cd "$(dirname "$0")" && pwd)"
DELAY="${1:-60}"
LOG="$HOME/.config/claude-burst/watchdog.log"
LABEL="ninja.andrewbaker.claude-burst"
mkdir -p "$HOME/.config/claude-burst"

echo "$(date '+%Y-%m-%d %H:%M:%S') watchdog armed, checking in ${DELAY}s" >> "$LOG"
sleep "$DELAY"

healthy=1

if ! launchctl list "$LABEL" >/dev/null 2>&1; then
  echo "$(date '+%Y-%m-%d %H:%M:%S') FAIL: LaunchAgent $LABEL not loaded" >> "$LOG"
  healthy=0
fi

if ! curl -sf -m 3 http://127.0.0.1:7777/healthz >/dev/null 2>&1; then
  echo "$(date '+%Y-%m-%d %H:%M:%S') FAIL: /healthz not responding" >> "$LOG"
  healthy=0
fi

if [[ "$healthy" -eq 1 ]]; then
  echo "$(date '+%Y-%m-%d %H:%M:%S') OK: gateway healthy after ${DELAY}s, leaving enabled" >> "$LOG"
  osascript -e 'display notification "Gateway healthy after check. Staying enabled." with title "claude-burst"' >/dev/null 2>&1 || true
else
  "$DIR/rollback.sh" >> "$LOG" 2>&1
  echo "$(date '+%Y-%m-%d %H:%M:%S') ROLLED BACK: config restored, gateway stopped" >> "$LOG"
  osascript -e 'display notification "Gateway unhealthy -- auto rolled back. Restart Claude Code." with title "claude-burst"' >/dev/null 2>&1 || true
fi
