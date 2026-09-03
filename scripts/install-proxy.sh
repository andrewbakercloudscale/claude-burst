#!/bin/zsh
# Brings the gateway back up and re-enables transparent intercept mode, in
# the order ROLLBACK.md requires:
#   1. back up first
#   2. the gateway must be healthy BEFORE anything points traffic at it
#   3. claude-burst enable (CA trust + settings.json)
#   4. the machine-wide switch (/etc/hosts + pf) goes last
#   5. arm the watchdog immediately after
#
# The machine-wide step (transparent-root.sh install) needs root and is not
# silently escalated: same reasoning as `claude-burst enable` itself printing
# it as a manual step rather than running it -- a change that affects every
# process on this Mac should happen with the person at the keyboard watching
# it happen. If sudo credentials aren't already cached, this prints the exact
# command and stops rather than hanging on a password prompt with nothing
# attached to answer it.
#
# Usage: scripts/install-proxy.sh
set -uo pipefail

DIR="$(cd "$(dirname "$0")" && pwd)"
LABEL="ninja.andrewbaker.claude-burst"
PLIST="$HOME/Library/LaunchAgents/$LABEL.plist"
BIN="$HOME/.local/bin/claude-burst"
ROOT_HELPER="$DIR/transparent-root.sh"

source "$DIR/health-diagnostics.sh"

echo "== 1. backing up current config =="
"$DIR/backup-config.sh"

echo
echo "== 2. starting the gateway =="
if [[ ! -x "$BIN" ]]; then
  echo "no gateway binary at $BIN -- run ./install.sh from the repo root first" >&2
  exit 1
fi
if [[ ! -f "$PLIST" ]]; then
  echo "no LaunchAgent plist at $PLIST -- run ./install.sh from the repo root first" >&2
  exit 1
fi
# bootout first: harmless if it's not loaded, and avoids "service already
# bootstrapped" if a previous rollback left it half-registered.
launchctl bootout "gui/$UID/$LABEL" >/dev/null 2>&1 || true
launchctl bootstrap "gui/$UID" "$PLIST"
launchctl kickstart -k "gui/$UID/$LABEL"

echo "waiting for /healthz..."
healthy=0
for i in $(seq 1 15); do
  if gateway_healthy; then healthy=1; break; fi
  sleep 1
done
if [[ "$healthy" -ne 1 ]]; then
  dump_health_diagnostics "install-proxy.sh: gateway never came healthy"
  echo "gateway did not become healthy -- see $HOME/.config/claude-burst/health-check-failures.log" >&2
  echo "not touching settings.json or the machine-wide redirect while the gateway is down" >&2
  exit 1
fi
echo "gateway healthy"

echo
echo "== 3. claude-burst enable (CA trust + settings.json) =="
"$BIN" enable

echo
echo "== 4. machine-wide redirect (needs root) =="
if [[ $EUID -eq 0 ]]; then
  "$ROOT_HELPER" install
elif sudo -n true 2>/dev/null; then
  sudo -n "$ROOT_HELPER" install
else
  echo "WARNING: cannot run the machine-wide step without a password. Run NOW:" >&2
  echo "    sudo $ROOT_HELPER install" >&2
  echo "Then arm the watchdog yourself:" >&2
  echo "    nohup $DIR/watchdog.sh & disown" >&2
  echo "(gateway is up and settings.json/CA are already done -- only this last step is left)" >&2
  exit 2
fi

echo
echo "== 5. arming the watchdog =="
nohup "$DIR/watchdog.sh" >/dev/null 2>&1 &
disown
echo "watchdog armed -- auto-rolls back via rollback.sh if the gateway isn't healthy 60s from now"

echo
echo "install complete -- restart Claude Code"
echo "verify any time with: claude-burst status"
echo "check the redirect specifically with: sudo scripts/transparent-root.sh status"
