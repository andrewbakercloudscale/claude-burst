#!/bin/zsh
# Turns Claude Burst back on after burst-off (scripts/rollback.sh), in one
# run and with one password: the undo of everything rollback.sh took away.
#
#   1. clears the turned-off marker, so the self-heal watchdog guards again
#   2. starts the gateway and waits until it answers
#   3. claude-burst enable: Burst's CA back in Claude Code's CA bundle
#      (transparent), or ANTHROPIC_BASE_URL back in settings.json (base-url)
#   4. transparent mode only:
#        - puts the api.anthropic.com redirect back (/etc/hosts and pf, root)
#        - trusts Burst's CA in the System keychain again (root)
#   5. checks a request through Burst, the way Claude Code sends one
#
# Until 11 Nov 2026 there was no single way back. The console's Restart
# cleared the marker and started the gateway but left the redirect and the
# trust removed, so the console went on saying "Redirect: refused", and the
# way back was three separate root commands, each with its own password.
#
# The support console's "Turn Burst back on" button runs this in a Terminal
# window; installed on the PATH as burst-on. Safe to run more than once.
set -uo pipefail

DIR="$(cd "$(dirname "$0")" && pwd)"
LABEL="ninja.andrewbaker.claude-burst"
CFG_DIR="$HOME/.config/claude-burst"
CONFIG="$CFG_DIR/config.json"
BIN="${CLAUDE_BURST_BIN:-$HOME/.local/bin/claude-burst}"
LIBEXEC="${CLAUDE_BURST_PFHEAL_LIBEXEC:-/usr/local/libexec/claude-burst}"
STEP="starting"

if [[ $EUID -eq 0 ]]; then
  echo "run this without sudo: it asks for your password itself, once, for the steps that need root." >&2
  echo "  $0" >&2
  exit 1
fi

trap 'rc=$?; if [ $rc -eq 0 ]; then "$DIR/audit-add.sh" script ok "Script: turn-on.sh (burst-on)" "done: Burst is back in the path" >/dev/null 2>&1; else "$DIR/audit-add.sh" script error "Script: turn-on.sh (burst-on)" "failed (exit $rc) while $STEP" >/dev/null 2>&1; echo; echo "burst-on FAILED (exit $rc) while $STEP. burst-repair reinstalls everything; burst-off takes Burst out of the path."; fi' EXIT

say() { printf '\n== %s\n' "$1"; STEP="$1"; }

# The mode this Mac chose, as the gateway reads it: no "mode" key is
# base-url, never transparent, which would edit /etc/hosts and pf on a Mac
# that chose not to.
cfg() { /usr/bin/python3 -c 'import json,sys
c=json.load(open(sys.argv[1]))
k=sys.argv[2]
if k=="mode": print((c.get("intercept") or {}).get("mode") or "base-url")
elif k=="host": print((c.get("intercept") or {}).get("host") or "api.anthropic.com")
elif k=="port": print((c.get("listen") or "127.0.0.1:17777").rsplit(":",1)[1])' "$CONFIG" "$1" 2>/dev/null; }

say "reading config.json"
if [[ ! -f "$CONFIG" ]] || ! MODE="$(cfg mode)"; then
  echo "$CONFIG is missing or cannot be read: run burst-repair, which restores it and reinstalls." >&2
  exit 1
fi
[[ "$MODE" == transparent ]] || MODE=base-url
HOST="$(cfg host)"
PORT="$(cfg port)"
echo "mode $MODE, gateway port $PORT"

if [[ "$MODE" == transparent ]]; then
  say "asking for your password, once (for /etc/hosts, pf and the System keychain)"
  sudo -v || exit 1
  # Kept fresh while the gateway starts, which can take a while on a Mac
  # that has just booted.
  ( while sleep 50; do kill -0 $$ 2>/dev/null && sudo -n true 2>/dev/null || exit; done ) &
  KEEPALIVE=$!
fi

say "clearing the turned-off marker"
rm -f "$CFG_DIR/rolled-back" "$CFG_DIR/rolled-back.noted"
echo "the self-heal watchdog guards the gateway again"

say "starting the gateway"
if launchctl print "gui/$UID/$LABEL" >/dev/null 2>&1; then
  launchctl kickstart "gui/$UID/$LABEL" >/dev/null 2>&1 || true
  echo "already loaded"
else
  launchctl bootstrap "gui/$UID" "$HOME/Library/LaunchAgents/$LABEL.plist" || {
    echo "could not load $HOME/Library/LaunchAgents/$LABEL.plist: run burst-repair, which reinstalls it." >&2
    exit 1
  }
  echo "loaded"
fi
# The root helper refuses to redirect to a port nothing answers on, so the
# gateway must be serving first. 90 seconds: after a boot the Mac can be
# busy enough that it takes a while.
up=""
for _ in {1..90}; do
  if [[ "$MODE" == transparent ]]; then
    curl -sk -o /dev/null --max-time 3 "https://127.0.0.1:$PORT/healthz" && { up=1; break; }
  else
    curl -s -o /dev/null --max-time 3 "http://127.0.0.1:$PORT/healthz" && { up=1; break; }
  fi
  sleep 1
done
if [[ -z "$up" ]]; then
  echo "the gateway did not answer on port $PORT within 90 seconds. Nothing machine-wide was changed." >&2
  echo "Look at the support console's log lines (http://127.0.0.1:7789/), or run burst-repair." >&2
  exit 1
fi
echo "answering on port $PORT"

# Transparent: Burst's CA back in Claude Code's CA bundle. Base-url:
# ANTHROPIC_BASE_URL back in settings.json. rollback.sh removed either.
# --force: a session open now that does not trust the CA is told to restart
# at the end, the same as after any CA change.
say "putting Claude Code's side back (claude-burst enable)"
"$BIN" enable --force | sed -n '1,3p'
(( ${pipestatus[1]} == 0 )) || exit 1

if [[ "$MODE" == transparent ]]; then
  say "putting the $HOST redirect back (/etc/hosts and pf)"
  # The root-owned copy when there is one: nobody but root can change it.
  # rollback.sh chooses the same way.
  HELPER="$DIR/transparent-root.sh"
  [[ -x "$LIBEXEC/transparent-root.sh" && "$(stat -f %u "$LIBEXEC/transparent-root.sh")" == 0 ]] && HELPER="$LIBEXEC/transparent-root.sh"
  sudo -n "$HELPER" install --host "$HOST" --gateway-port "$PORT" || exit 1

  say "trusting Burst's CA in the System keychain again"
  # Without it every other app that reaches $HOST (Claude Desktop's updater,
  # curl) fails TLS, and the pf guard counts the redirect as broken.
  sudo -n "$DIR/trust-ca-systemwide.sh" || exit 1
  kill "$KEEPALIVE" 2>/dev/null
fi

say "checking a request through Burst"
if [[ "$MODE" == transparent ]]; then URL="https://$HOST/healthz"; else URL="http://127.0.0.1:$PORT/healthz"; fi
ok=""
for _ in {1..30}; do
  # "overflow" is in the gateway's /healthz and never in Anthropic's.
  curl -s --max-time 5 "$URL" 2>/dev/null | grep -q '"overflow"' && { ok=1; break; }
  sleep 1
done
if [[ -z "$ok" ]]; then
  echo "$URL did not reach Burst. Run burst-repair, or scripts/diagnose.sh for a report." >&2
  exit 1
fi
echo "$URL reaches the gateway"
echo
echo "Burst is back on. A Claude Code session that still fails: quit it and run claude --continue."
