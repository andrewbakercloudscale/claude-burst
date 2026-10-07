#!/bin/zsh
# Persistent self-heal watchdog for the claude-burst gateway. Runs every
# 30 seconds from its own LaunchAgent (ninja.andrewbaker.claude-burst-selfheal),
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
ROLLED_BACK_MARKER="${CLAUDE_BURST_ROLLED_BACK_MARKER:-$HOME/.config/claude-burst/rolled-back}"
# Proof of life for the dashboard, same contract as the pf guard's. A watchdog
# nobody can see is a watchdog nobody knows has stopped -- and this one is the
# reason a dead gateway comes back at all, so "is it actually running?" needs
# an answer that does not depend on asking launchd.
HEARTBEAT_FILE="${CLAUDE_BURST_SELFHEAL_HEARTBEAT:-$HOME/.config/claude-burst/self-heal.heartbeat}"
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

rotate_log_file "$LOG" "$LOG_MAX_BYTES"

# Before every early return below, including the rolled-back stand-down: a
# heartbeat that only appeared on the cycles that did something would go stale
# exactly when this is working correctly and read as dead.
date +%s > "$HEARTBEAT_FILE" 2>/dev/null || true

log() { echo "$(date '+%Y-%m-%d %H:%M:%S') $*" >> "$LOG"; }

# Passes the message as a genuine argv element to osascript (via `on run
# argv`) rather than interpolating it into the -e script text -- sidesteps
# AppleScript string-escaping entirely for messages this script doesn't
# control the content of (test-connection's `detail` field).
notify() {
  osascript -e 'on run argv' -e 'display notification (item 1 of argv) with title "claude-burst"' -e 'end run' "$1" >/dev/null 2>&1 || true
}

# What this did goes in the audit trail too (the Audit tab): the log above is
# a file nobody opens, and "the watchdog killed the gateway at 03:12" is
# exactly what someone asking why a session dropped needs to find.
audit() { "$DIR/audit-add.sh" "$@" >/dev/null 2>&1 || true; }

# A restart that someone asked for: deploy.sh writes planned-restart before
# it restarts the gateway, and the gateway leaves planned-restart.done when
# it is back up (it removes the first, often before this script's next run).
planned="$HOME/.config/claude-burst/planned-restart"
planned_recent() {
  local f now
  now=$(date +%s)
  for f in "$planned" "$planned.done"; do
    [[ -f "$f" ]] && (( now - $(stat -f %m "$f" 2>/dev/null || echo 0) < 180 )) && return 0
  done
  return 1
}

# --- Crash loop: restarted again and again is not "healed". ---
# Every step below treats one failure: reload a gateway that is gone, kill
# one that hangs. launchd does the same for one that exits. None of them
# counted, so a gateway that died on start came back every ten seconds for
# as long as the Mac was on, each restart reported as a success, and with
# the redirect in place Claude Code was down the whole time with nothing
# said. CRASH_LIMIT restarts inside CRASH_WINDOW seconds, planned ones not
# counted, and it says so: a notification, the log and the audit, naming
# the two commands that end it. It still does not roll back by itself:
# that needs root for /etc/hosts and pf, and this runs with nobody there.
RESTARTS_FILE="$HOME/.config/claude-burst/self-heal-restarts"
PID_FILE="$HOME/.config/claude-burst/self-heal-pid"
CRASHLOOP_FILE="$HOME/.config/claude-burst/self-heal-crashloop"
CRASH_LIMIT="${CLAUDE_BURST_CRASH_LIMIT:-5}"
CRASH_WINDOW="${CLAUDE_BURST_CRASH_WINDOW:-600}"

# recent_restarts prints the restarts still inside the window, one per line.
recent_restarts() {
  local now t
  now=$(date +%s)
  for t in $(cat "$RESTARTS_FILE" 2>/dev/null); do
    [[ "$t" == <-> ]] && (( now - t < CRASH_WINDOW )) && echo "$t"
  done
}

# note_restart counts one restart ($1 says what it was) and escalates at the
# limit, once an hour at most while the loop goes on.
note_restart() {
  local kept n now
  now=$(date +%s)
  kept="$(recent_restarts)"
  { [[ -n "$kept" ]] && echo "$kept"; echo "$now"; } > "$RESTARTS_FILE"
  n=$(wc -l < "$RESTARTS_FILE" | tr -d ' ')
  (( n < CRASH_LIMIT )) && return 0
  if [[ -f "$CRASHLOOP_FILE" ]] && (( now - $(stat -f %m "$CRASHLOOP_FILE" 2>/dev/null || echo 0) < RENOTIFY_SECONDS )); then
    return 0
  fi
  : > "$CRASHLOOP_FILE"
  local msg="Gateway restarted $n times in $(( CRASH_WINDOW / 60 )) minutes and is not staying up (last: $1). Run burst-repair; burst-off takes Burst out of the path meanwhile."
  log "CRASH LOOP: $msg"
  notify "$msg"
  audit watchdog error "Watchdog: the gateway keeps restarting" "$msg"
}

# A window with no restart in it after a loop: say it is over, once.
if [[ -f "$CRASHLOOP_FILE" && -z "$(recent_restarts)" ]]; then
  rm -f "$CRASHLOOP_FILE" "$RESTARTS_FILE"
  log "gateway has stayed up for $(( CRASH_WINDOW / 60 )) minutes: the crash loop is over"
  audit watchdog ok "Watchdog: the gateway is staying up again" "no restart in the last $(( CRASH_WINDOW / 60 )) minutes"
fi

# --- 0. Did a human deliberately roll back? Then stay out of the way. ---
# Reloading the gateway after rollback.sh stopped it is not self-healing, it
# is undoing someone's decision -- observed 2026-09-07, 90 seconds after a
# rollback. install-proxy.sh removes this marker, so a reinstall re-arms this
# watchdog without anyone having to remember it exists.
if [[ -f "$ROLLED_BACK_MARKER" ]]; then
  # Logged only on the cycle that first sees it: this runs every 30 seconds
  # and a machine left rolled back for a week must not write 5,000 lines.
  if [[ ! -f "$ROLLED_BACK_MARKER.noted" ]]; then
    log "rolled back by hand ($(cat "$ROLLED_BACK_MARKER" 2>/dev/null)) -- standing down until reinstall"
    : > "$ROLLED_BACK_MARKER.noted"
  fi
  exit 0
fi
rm -f "$ROLLED_BACK_MARKER.noted"

# --- 0b. Did launchd restart it since the last check? ---
# A new pid that nobody asked for is the gateway having exited: launchd's
# KeepAlive brought it back, and this is the only place that can count it.
pid_now="$(launchagent_pid)"
pid_last="$(cat "$PID_FILE" 2>/dev/null || true)"
if [[ -n "$pid_now" && -n "$pid_last" && "$pid_now" != "$pid_last" ]] && ! planned_recent; then
  log "gateway restarted by itself (pid $pid_last, now $pid_now)"
  note_restart "it exited and launchd started it again"
fi
if [[ -n "$pid_now" ]]; then echo "$pid_now" > "$PID_FILE"; else rm -f "$PID_FILE"; fi

# --- 1. Is the gateway's own LaunchAgent even loaded? Reload if not. ---
# No root needed for this half: enable/bootstrap on a LaunchAgent is entirely
# within this user's own session, unlike the /etc/hosts + pf half below.
# launchagent_running, not launchagent_loaded: launchd goes on answering for a
# job whose process has died, and on 2026-09-08 that is exactly what happened
# -- this step passed, the reload never ran, and the redirect stayed pointed at
# a gateway that was not there.
if ! launchagent_running; then
  log "gateway is not running (LaunchAgent unloaded or its process gone) -- attempting reload"
  # kickstart -k restarts a registered-but-dead job; ensure_launchagent_loaded
  # handles the harder case where the job is gone from launchd entirely.
  launchctl kickstart -k "gui/$UID/$LABEL" >/dev/null 2>&1
  if launchagent_running || ensure_launchagent_loaded; then
    log "reloaded successfully"
    notify "Gateway had stopped (LaunchAgent was unloaded) -- reloaded automatically."
    audit watchdog warn "Watchdog: reloaded a gateway that had stopped" "its LaunchAgent was unloaded or its process gone"
    # Counted here, so not again as a new pid on the next check.
    rm -f "$PID_FILE"
    note_restart "it had stopped and was reloaded"
  else
    log "FAILED to reload -- see health-diagnostics.sh's ensure_launchagent_loaded output above"
    notify "Gateway is down and could not be reloaded automatically. Check Terminal."
    audit watchdog error "Watchdog: the gateway is down and could not be reloaded" "run burst-repair, or burst-off to take Burst out of the path"
    exit 0
  fi
  # Give the freshly-reloaded process a moment to bind before testing it
  # below, so this doesn't misreport a reload-in-progress as still broken.
  sleep 3
fi

# --- 1b. Running but not answering? Restart it. ---
# Step 1 only sees a process that is gone. A gateway that is there but hung
# passed it, and step 2's check then timed out and said nothing, so traffic
# kept going to a process that answered nobody. Two cycles in a row (30s
# apart) with neither the gateway nor its dashboard answering, and launchd
# gets a fresh one: KeepAlive restarts the job the moment its process dies.
# Never during a planned restart: an upgrade's old process may be draining.
HANG_FILE="$HOME/.config/claude-burst/self-heal-hang"
HANG_LIMIT="${CLAUDE_BURST_HANG_LIMIT:-2}"
KILL="${CLAUDE_BURST_KILL:-kill}"
if planned_recent; then
  rm -f "$HANG_FILE"
elif launchagent_running; then
  if gateway_healthy || curl -s -m 5 -o /dev/null "$ADMIN_URL/api/mod-status" 2>/dev/null; then
    rm -f "$HANG_FILE"
  else
    hung=$(( $(cat "$HANG_FILE" 2>/dev/null || echo 0) + 1 ))
    echo "$hung" > "$HANG_FILE"
    pid="$(launchagent_pid)"
    log "gateway (pid $pid) is running but not answering: check $hung of $HANG_LIMIT"
    if (( hung >= HANG_LIMIT )) && [[ -n "$pid" ]]; then
      log "gateway (pid $pid) hung for $hung checks -- killing it; launchd starts a fresh one"
      "$KILL" -9 "$pid" 2>/dev/null
      rm -f "$HANG_FILE" "$PID_FILE"
      audit watchdog warn "Watchdog: killed a gateway that had stopped answering" "pid $pid answered nothing for $hung checks; launchd starts a fresh one"
      note_restart "it hung and was killed"
      exit 0
    fi
  fi
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
