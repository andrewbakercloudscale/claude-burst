#!/bin/zsh
# Auto-revert safety net. Launch this detached (nohup ... & disown)
# immediately after `claude-burst enable`. It waits, then checks the gateway
# is actually alive and serving; if not, it runs rollback.sh automatically.
# Always logs the outcome, and fires a macOS notification either way so this
# is visible even if the terminal that launched it is gone.
#
# Usage: watchdog.sh [delay_seconds]   (default 60)
set -uo pipefail

DIR="${0:A:h}"
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
