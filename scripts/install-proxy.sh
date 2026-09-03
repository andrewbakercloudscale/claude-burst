#!/bin/zsh
# Brings the gateway back up and re-enables transparent intercept mode, in
# the order ROLLBACK.md requires:
#   1. back up first
#   2. the gateway must be healthy BEFORE anything points traffic at it
#   3. claude-burst enable (CA trust for Claude Code CLI + settings.json)
#   4. the machine-wide redirect (/etc/hosts + pf) goes last
#   5. machine-wide CA trust (System keychain), so the redirect it just
#      installed doesn't silently break every OTHER app that happens to
#      talk to api.anthropic.com
#   6. arm the watchdog immediately after
#
# Both machine-wide root steps (4 and 5) are spelled out explicitly rather
# than folded into one another or silently chained, and neither is
# silently escalated: same reasoning as `claude-burst enable` itself
# printing its remaining step rather than running it -- a change that
# affects every process on this Mac should happen with the person at the
# keyboard watching it happen, and knowing specifically what changed. If
# sudo credentials aren't already cached, this prints the exact commands
# and stops rather than hanging on a password prompt with nothing attached
# to answer it.
#
# Why step 5 exists at all: root-caused 2026-09-03 (see
# INVESTIGATION-TLS-STORM.md). Step 3's CA trust only covers Claude Code
# CLI (via NODE_EXTRA_CA_CERTS) -- Claude Desktop and everything else on
# this Mac has never heard of this CA, so step 4's redirect makes THEM
# fail TLS handshakes against a certificate they don't trust the moment
# their own traffic touches api.anthropic.com. Claude Desktop's own
# auto-updater checks that host roughly hourly; without step 5 that
# produces the TLS handshake-error storm this investigation chased for
# days, and at least once broke Claude Desktop's Cowork/MCP-filesystem
# startup outright. Step 5 is what step 4 needs to actually be silent
# rather than merely working for the one process that has its own CA
# bundle.
#
# Usage: scripts/install-proxy.sh
set -uo pipefail

DIR="$(cd "$(dirname "$0")" && pwd)"
LABEL="ninja.andrewbaker.claude-burst"
PLIST="$HOME/Library/LaunchAgents/$LABEL.plist"
BIN="$HOME/.local/bin/claude-burst"
ROOT_HELPER="$DIR/transparent-root.sh"
TRUST_HELPER="$DIR/trust-ca-systemwide.sh"

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
# `launchctl disable` is persistent -- it survives bootout/bootstrap and
# outlives the process that set it, so a prior rollback that disabled this
# service (the original stock script this repo's rollback.sh replaced did)
# leaves bootstrap failing with a bare "Input/output error" and no mention
# of "disabled" anywhere in the message. Re-enable is idempotent and a
# no-op when the service was never disabled.
launchctl enable "gui/$UID/$LABEL" >/dev/null 2>&1 || true

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
gateway_port="$(python3 -c "import json;print(json.load(open('$HOME/.config/claude-burst/config.json'))['listen'].split(':')[-1])" 2>/dev/null || echo 7777)"
echo "== 4. machine-wide redirect: /etc/hosts + pf (needs root) =="
echo "changing: adds '127.0.0.1 api.anthropic.com' to /etc/hosts, loads a pf anchor"
echo "redirecting 127.0.0.1:443 -> 127.0.0.1:$gateway_port"
if [[ $EUID -eq 0 ]]; then
  "$ROOT_HELPER" install
elif sudo -n true 2>/dev/null; then
  sudo -n "$ROOT_HELPER" install
else
  echo "WARNING: cannot run the machine-wide step without a password. Run NOW:" >&2
  echo "    sudo $ROOT_HELPER install" >&2
  echo "Then (see step 5 below) run:" >&2
  echo "    sudo $TRUST_HELPER" >&2
  echo "Then arm the watchdog yourself:" >&2
  echo "    nohup $DIR/watchdog.sh & disown" >&2
  echo "(gateway is up and settings.json/CA for Claude Code CLI are already done -- these two root steps are what's left)" >&2
  exit 2
fi

echo
echo "== 5. machine-wide CA trust: System keychain (needs root) =="
echo "changing: imports $HOME/.config/claude-burst/ca/ca-cert.pem into"
echo "  /Library/Keychains/System.keychain as a trusted root (CN: claude-burst local CA)"
echo "why: step 4's redirect now catches traffic from every app on this Mac, not just"
echo "  Claude Code CLI -- without this, anything else that happens to reach"
echo "  api.anthropic.com (Claude Desktop's auto-updater, for one) fails its TLS"
echo "  handshake against a certificate it doesn't trust. See INVESTIGATION-TLS-STORM.md."
if [[ $EUID -eq 0 ]]; then
  "$TRUST_HELPER"
elif sudo -n true 2>/dev/null; then
  sudo -n "$TRUST_HELPER"
else
  echo "WARNING: cannot run this without a password. Run NOW:" >&2
  echo "    sudo $TRUST_HELPER" >&2
  echo "Then arm the watchdog yourself:" >&2
  echo "    nohup $DIR/watchdog.sh & disown" >&2
  echo "(the redirect from step 4 is already live -- this is the last step)" >&2
  exit 2
fi

echo
echo "== 6. arming the watchdog =="
nohup "$DIR/watchdog.sh" >/dev/null 2>&1 &
disown
echo "watchdog armed -- auto-rolls back via rollback.sh if the gateway isn't healthy 60s from now"

echo
echo "install complete -- restart Claude Code"
echo "verify any time with: claude-burst status"
echo "check the redirect specifically with: sudo scripts/transparent-root.sh status"
