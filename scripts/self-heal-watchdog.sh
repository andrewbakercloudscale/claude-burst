#!/bin/zsh
# Persistent self-heal watchdog for the claude-burst gateway. Runs every
# ~2 minutes from its own LaunchAgent (ninja.andrewbaker.claude-burst-selfheal),
# separate from the gateway's own LaunchAgent -- so it keeps checking even
# when THAT one gets killed, which is exactly the failure mode this exists
# to catch.
#
# WHY THIS EXISTS: on 2026-09-04, a critical-battery event (2%, see
# `pmset -g log`) caused macOS to kill and fully UNLOAD the gateway's
# LaunchAgent -- not just crash the process, but remove it from launchd
# entirely (`launchctl print` afterward: "Could not find service"). Nothing
# noticed until Claude Code was opened and found broken. The fix reached for
# in the moment -- the external, out-of-repo rollback.sh -- correctly
# restores direct Anthropic access, but it ALSO tears down the /etc/hosts
# redirect, flushes pf, and disables the LaunchAgent, so even after Claude
# access was safely restored, the gateway itself stayed dead and disabled
# until a human noticed and manually reloaded it (as happened this session).
# This closes the loop for the half that doesn't need root: reload the
# LaunchAgent the instant it's found unloaded/disabled, no human required.
#
# What this deliberately does NOT do: touch /etc/hosts or pf. Reinstalling
# the machine-wide redirect needs root, and this runs unattended -- same
# restraint rollback.sh already takes, for the same reason (a password
# prompt with nobody there to answer it is not a recovery path). Instead, if
# the gateway is healthy but real traffic isn't reaching it, this pushes a
# macOS notification with the exact one-line fix, so it's discovered within
# minutes rather than mid-session days later.
set -uo pipefail

# POSIX form, not zsh's ${0:A:h}: this is recovery tooling, same reasoning
# as rollback.sh/watchdog.sh -- see their comments.
DIR="$(cd "$(dirname "$0")" && pwd)"
LOG="$HOME/.config/claude-burst/self-heal.log"
STATE_FILE="$HOME/.config/claude-burst/self-heal-state.json"
LABEL="ninja.andrewbaker.claude-burst"
ADMIN_URL="http://127.0.0.1:7788"
# Re-notify about a missing redirect at most this often, so a laptop left in
# that state doesn't get nagged every single cycle forever.
RENOTIFY_SECONDS=3600
# This log is genuinely low-volume (one line per cycle at most, most cycles
# write nothing) -- rotate.Writer's machinery would be overkill; a hard cap
# with truncate-on-exceed is enough insurance against it ever running away.
LOG_MAX_BYTES=2097152

mkdir -p "$HOME/.config/claude-burst"

# shellcheck source=./health-diagnostics.sh
source "$DIR/health-diagnostics.sh"

if [[ -f "$LOG" ]]; then
  log_size=$(stat -f %z "$LOG" 2>/dev/null || stat -c %s "$LOG" 2>/dev/null || echo 0)
  (( log_size > LOG_MAX_BYTES )) && : > "$LOG"
fi

log() { echo "$(date '+%Y-%m-%d %H:%M:%S') $*" >> "$LOG"; }

# Passes the message as a genuine argv element to osascript (via `on run
# argv`) rather than interpolating it into the -e script text -- sidesteps
# AppleScript string-escaping entirely for messages this script doesn't
# control the content of (test-connection's `detail` field).
notify() {
  osascript -e 'on run argv' -e 'display notification (item 1 of argv) with title "claude-burst"' -e 'end run' "$1" >/dev/null 2>&1 || true
}

# --- 1. Is the gateway's own LaunchAgent even loaded? Reload if not. ---
# No root needed for this half: enable/bootstrap on a LaunchAgent is entirely
# within this user's own session, unlike the /etc/hosts + pf half below.
if ! launchagent_loaded; then
  log "gateway LaunchAgent not loaded -- attempting reload"
  if ensure_launchagent_loaded; then
    log "reloaded successfully"
    notify "Gateway had stopped (LaunchAgent was unloaded) -- reloaded automatically."
  else
    log "FAILED to reload -- see health-diagnostics.sh's ensure_launchagent_loaded output above"
    notify "Gateway is down and could not be reloaded automatically. Check Terminal."
    exit 0
  fi
  # Give the freshly-reloaded process a moment to bind before testing it
  # below, so this doesn't misreport a reload-in-progress as still broken.
  sleep 3
fi

# --- 2. Is real traffic actually reaching it? (transparent mode only) ---
# Reuses the admin UI's own /api/test-connection -- the same live check a
# human would click, run headless. A short timeout and empty-response guard
# make this a silent no-op if the admin server isn't answering yet (covered
# by step 1's reload, checked again next cycle) rather than a spurious error.
result="$(curl -s -m 5 "$ADMIN_URL/api/test-connection" 2>/dev/null || true)"
[[ -z "$result" ]] && exit 0

python3 - "$result" "$STATE_FILE" "$RENOTIFY_SECONDS" <<'PY' >> "$LOG" 2>&1
import json, subprocess, sys, time

body, state_path, renotify_seconds = sys.argv[1], sys.argv[2], int(sys.argv[3])

try:
    resp = json.loads(body)
except Exception:
    sys.exit(0)  # unparseable response -- nothing actionable to say

if resp.get("ok"):
    # Healthy again -- clear any "already notified" state so a future
    # break notifies again instead of staying silent forever.
    try:
        import os
        os.remove(state_path)
    except FileNotFoundError:
        pass
    sys.exit(0)

detail = resp.get("detail", "")
if not detail:
    sys.exit(0)  # base-url mode's short-circuit reports ok=true; nothing else currently reports ok=false with no detail

now = int(time.time())
last_notified = 0
try:
    with open(state_path) as f:
        last_notified = json.load(f).get("last_notified", 0)
except (FileNotFoundError, json.JSONDecodeError):
    pass

if now - last_notified >= renotify_seconds:
    print(f"{time.strftime('%Y-%m-%d %H:%M:%S')} redirect not active: {detail}")
    subprocess.run(
        ["osascript", "-e", "on run argv", "-e",
         'display notification (item 1 of argv) with title "claude-burst"',
         "-e", "end run", detail],
        capture_output=True,
    )
    with open(state_path, "w") as f:
        json.dump({"last_notified": now}, f)
PY
