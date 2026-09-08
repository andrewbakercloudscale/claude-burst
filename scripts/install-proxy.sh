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
ROLLED_BACK_MARKER="${CLAUDE_BURST_ROLLED_BACK_MARKER:-$HOME/.config/claude-burst/rolled-back}"

source "$DIR/health-diagnostics.sh"

# Acquire root ONCE, UP FRONT, before anything is changed.
#
# This used to be decided at step 4, with `sudo -n` alone: no cached
# credentials meant printing the remaining commands and exiting 2. That is
# right for an unattended caller, and wrong for the far more common case of a
# person running this in a terminal -- it never prompts, so it silently
# performs steps 1-3, stops at the root step, and leaves a half-install whose
# only visible symptom is that nothing is redirected. Observed twice on
# 2026-09-08, the second time after I told the user to run this exact command.
#
# rollback.sh already had the answer and it was never applied here: prompt when
# there is a tty to prompt on, print-and-stop when there is not. Doing it here
# rather than at step 4 also means a run that cannot finish changes NOTHING,
# instead of discovering the problem after settings.json and the CA are done.
HAVE_ROOT=0
if [[ $EUID -eq 0 ]]; then
  HAVE_ROOT=1
elif sudo -n true 2>/dev/null; then
  HAVE_ROOT=1
elif [[ -t 0 ]]; then
  echo "claude-burst transparent install needs sudo for /etc/hosts, pf, and System-keychain CA trust." >&2
  sudo -v && HAVE_ROOT=1
fi
if [[ "$HAVE_ROOT" -ne 1 ]]; then
  echo "WARNING: no sudo credentials and no terminal to prompt on -- nothing has been changed." >&2
  echo "Run this again from a terminal, or pre-authorise with: sudo -v" >&2
  exit 2
fi

# Clear the "a human rolled this back" marker rollback.sh leaves behind, so
# the self-heal watchdog starts minding the gateway again. Done first: every
# path below assumes the gateway is meant to be running.
rm -f "$ROLLED_BACK_MARKER" "$ROLLED_BACK_MARKER.noted"

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
# Fallback matches internal/config.Default. It was 7777, which is now the one
# port the gateway must NOT use (see INVESTIGATION-TLS-STORM.md) -- a fallback
# that silently builds a redirect to a dropped port is the worst shape this
# could fail in: pf loads cleanly and nothing reaches the gateway.
gateway_port="$(python3 -c "import json;print(json.load(open('$HOME/.config/claude-burst/config.json'))['listen'].split(':')[-1])" 2>/dev/null || echo 17777)"
intercept_host="$(python3 -c "import json;print(json.load(open('$HOME/.config/claude-burst/config.json')).get('intercept',{}).get('host','api.anthropic.com'))" 2>/dev/null || echo api.anthropic.com)"
echo "== 4. machine-wide redirect: /etc/hosts + pf (needs root) =="
echo "changing: adds '127.0.0.1 api.anthropic.com' to /etc/hosts, loads a pf anchor"
echo "redirecting 127.0.0.1:443 -> 127.0.0.1:$gateway_port"
# HAVE_ROOT was established at the top, before anything was changed, so this
# cannot be the step that discovers we have no password.
# Pass the host and port EXPLICITLY. This used to invoke `install` bare and
# let transparent-root.sh fall back to its own defaults -- which was invisible
# for as long as those defaults happened to equal the configured values, and
# broke the moment they did not: on 2026-09-08 this printed "redirecting
# 127.0.0.1:443 -> 127.0.0.1:17777" and then installed nothing, because the
# helper probed its default 7777, found nothing there, and refused. A message
# describing one thing while the command does another is the exact failure
# shape this repo keeps rediscovering. The intercept host was silently
# defaulted the same way.
if [[ $EUID -eq 0 ]]; then
  "$ROOT_HELPER" install --host "$intercept_host" --gateway-port "$gateway_port"
else
  sudo -n "$ROOT_HELPER" install --host "$intercept_host" --gateway-port "$gateway_port"
fi || {
  echo "ERROR: the machine-wide redirect did not install. Nothing is redirected," >&2
  echo "so Claude Code still reaches Anthropic directly -- the safe state." >&2
  echo "Undo the rest with: $DIR/rollback.sh" >&2
  exit 2
}

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
else
  sudo -n "$TRUST_HELPER"
fi || {
  echo "WARNING: system-wide CA trust failed. The redirect from step 4 IS live, so" >&2
  echo "other apps reaching api.anthropic.com will fail TLS until this is fixed:" >&2
  echo "    sudo $TRUST_HELPER" >&2
  echo "Or undo everything with: $DIR/rollback.sh" >&2
}

echo
echo "== 6. arming the watchdog =="
nohup "$DIR/watchdog.sh" >/dev/null 2>&1 &
disown
echo "watchdog armed -- auto-rolls back via rollback.sh if the gateway isn't healthy 60s from now"

echo
echo "== 7. arming the pf self-heal daemon =="
# Different job from step 6's watchdog, which only watches the first 60
# seconds and then exits. This one is permanent, and it guards the piece
# nothing else can: the pf rdr rule, which other pf-owning software on this
# Mac has already dropped once (2026-09-07) while /etc/hosts stayed -- the
# state that refuses every connection to the intercepted host, machine-wide.
# Root is already cached here from step 4, so this is the natural place.
if sudo -n "$DIR/install-pf-heal.sh" >/dev/null 2>&1; then
  echo "pf self-heal daemon armed -- log: /var/log/claude-burst-pf.log"
else
  echo "WARNING: could not arm the pf self-heal daemon. The redirect is live but" >&2
  echo "         its rdr rule is unguarded. Run:" >&2
  echo "    sudo $DIR/install-pf-heal.sh" >&2
fi

echo
echo "install complete -- restart Claude Code"
echo "verify any time with: claude-burst status"
echo "check the redirect specifically with: sudo scripts/transparent-root.sh status"
