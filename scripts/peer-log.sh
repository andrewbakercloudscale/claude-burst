#!/bin/zsh
# Arms or disarms the gateway's TLS peer attribution -- the diagnostic that
# names the process behind the open TLS handshake-error storm
# (INVESTIGATION-TLS-STORM.md).
#
# WHAT IT TURNS ON. cmd/claude-burst/peerlog.go wraps the listener and, for
# every accepted connection, runs `lsof` scoped to that exact ephemeral port
# BEFORE the TLS handshake can fail and tear the socket down. That ordering is
# the whole point: the two previous attempts at this both lost the same race.
# dtrace, the investigation's own documented next step, is blocked outright by
# SIP on this machine; a tight lsof polling loop was too slow because the
# reject-and-close cycle beats any independent timer. Identifying at Accept()
# wins by construction instead of by being lucky.
#
# WHY IT IS NOT ALWAYS ON. identify() is synchronous, so accepting a
# connection blocks for one lsof -- measured at 40-70ms on this machine -- and
# connections are accepted one at a time while it runs. That is a real tax on
# every request the gateway serves, which is why the code calls itself
# diagnostic-only.
#
# WHEN TO RUN IT. The storm is intermittent with hours between bursts, so it
# has to stay armed to catch one. Arm it when you are not working -- the last
# burst ran 23:20 to 07:05 -- and disarm it in the morning. Overnight the
# latency costs nothing because nothing is waiting on it.
#
# The plist is backed up before it is edited and `off` restores the armed
# state to exactly what it was, so this leaves nothing behind either way.
#
# Usage: scripts/peer-log.sh on | off | status
set -uo pipefail

LABEL="ninja.andrewbaker.claude-burst"
PLIST="$HOME/Library/LaunchAgents/$LABEL.plist"
# TWO logs, and conflating them is why the first version of this script
# reported "none yet" forever. The gateway's own logger (main.go:157) writes
# to a rotating claude-burst.log -- that is where peer-log lines land. Go's
# http.Server writes TLS handshake errors to ITS error log, which is stderr,
# so those land in launchd.err.log. The join this script exists to perform
# needs one line from each file.
PEERLOG="$HOME/.config/claude-burst/claude-burst.log"
LOG="$HOME/.config/claude-burst/launchd.err.log"
BACKUP_DIR="$HOME/.config/claude-burst/backups"
KEY="CLAUDE_BURST_LOG_TLS_PEERS"

[[ -f "$PLIST" ]] || { echo "no plist at $PLIST" >&2; exit 1; }

armed() {
  python3 - "$PLIST" "$KEY" <<'PY'
import plistlib, sys
d = plistlib.load(open(sys.argv[1], "rb"))
print("yes" if (d.get("EnvironmentVariables") or {}).get(sys.argv[2]) else "no")
PY
}

restart() {
  # `launchctl kickstart -k` restarts the PROCESS but reuses launchd's cached
  # job definition, so an edited EnvironmentVariables block does not reach it.
  # Verified the hard way on 2026-09-08: the plist said armed, the plist WAS
  # armed, and `ps eww` on the running process showed the variable absent.
  # bootout + bootstrap makes launchd re-read the file.
  launchctl bootout "gui/$UID/$LABEL" >/dev/null 2>&1
  launchctl bootstrap "gui/$UID" "$PLIST" >/dev/null 2>&1
  launchctl kickstart -k "gui/$UID/$LABEL" >/dev/null 2>&1
  # A gateway that has not finished binding answers nothing, and reporting
  # that as "armed" would be a diagnostic that quietly is not running.
  local i
  for i in $(seq 1 20); do
    if curl -s -m 2 -o /dev/null "http://127.0.0.1:7788/api/state" 2>/dev/null; then
      return 0
    fi
    sleep 0.5
  done
  echo "WARNING: gateway did not come back healthy within 10s -- check $LOG" >&2
  return 1
}

case "${1:-status}" in
  on|off)
    want="$1"
    mkdir -p "$BACKUP_DIR"
    cp "$PLIST" "$BACKUP_DIR/$(basename "$PLIST").$(date +%Y%m%d-%H%M%S).bak" || {
      echo "plist backup failed -- refusing to edit" >&2; exit 1; }
    python3 - "$PLIST" "$KEY" "$want" <<'PY'
import plistlib, sys
path, key, want = sys.argv[1], sys.argv[2], sys.argv[3]
d = plistlib.load(open(path, "rb"))
env = dict(d.get("EnvironmentVariables") or {})
if want == "on":
    env[key] = "1"
else:
    env.pop(key, None)
# An empty EnvironmentVariables dict is removed rather than left behind, so
# `off` returns the file to the shape it had before this script touched it.
if env:
    d["EnvironmentVariables"] = env
else:
    d.pop("EnvironmentVariables", None)
plistlib.dump(d, open(path, "wb"))
PY
    restart
    echo "peer attribution: $(armed)  (restarted $LABEL)"
    if [[ "$want" == "on" ]]; then
      # Proof it is actually running, not just configured. The banner is
      # printed by main.go at startup only when the variable is set.
      sleep 1
      if grep -q "peer-log: diagnostic peer attribution ENABLED" <(tail -40 "$PEERLOG" 2>/dev/null); then
        echo "confirmed in the log: the listener is wrapped and identifying peers"
      else
        echo "WARNING: no 'peer attribution ENABLED' banner in the last 40 log lines." >&2
        echo "It may be configured but not running. Check: tail $PEERLOG" >&2
      fi
      echo
      echo "when the next burst fires, read it with:"
      echo "  scripts/peer-log.sh status"
      echo "and DISARM afterwards -- it costs 40-70ms on every connection:"
      echo "  scripts/peer-log.sh off"
    fi
    ;;
  status)
    echo "peer attribution armed: $(armed)"
    echo
    echo "== most recent TLS handshake errors =="
    grep -h "TLS handshake error" "$LOG" 2>/dev/null | tail -5 | sed 's/^/  /'
    echo
    echo "== peer attributions captured =="
    # The join that answers the question: peer-log names the process behind an
    # ephemeral port, and the handshake error names the port that failed.
    if ! grep -q "peer-log: connection" "$PEERLOG" 2>/dev/null; then
      echo "  none yet -- either not armed, or no connection since arming"
    else
      for port in $(grep -h "TLS handshake error" "$LOG" 2>/dev/null | tail -20 |
                    grep -oE "127\.0\.0\.1:[0-9]+" | cut -d: -f2 | sort -u); do
        hit="$(grep -h "peer-log: connection from 127.0.0.1:$port " "$PEERLOG" 2>/dev/null | tail -1)"
        if [[ -n "$hit" ]]; then
          echo "  port $port -> ${hit#*lsof: }"
        else
          echo "  port $port -> (no attribution; connection predates arming)"
        fi
      done
    fi
    ;;
  *)
    echo "usage: $0 on | off | status" >&2
    exit 1
    ;;
esac
