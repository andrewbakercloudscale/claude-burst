#!/bin/zsh
# Instant manual rollback: restores ~/.claude/settings.json and
# ~/.config/claude-burst/config.json from the most recent backup-config.sh
# snapshot, and stops the gateway LaunchAgent. Safe to run any time, more
# than once, or when the gateway was never enabled.
set -uo pipefail

BACKUP_DIR="${CLAUDE_BURST_BACKUP_DIR:-$HOME/.config/claude-burst/backups}"
LABEL="ninja.andrewbaker.claude-burst"
SETTINGS="$HOME/.claude/settings.json"
CONFIG="$HOME/.config/claude-burst/config.json"

# Resolve this script's directory. Written the POSIX way rather than with zsh's
# ${0:A:h}: these scripts are recovery tooling, and someone reaching for them in
# an emergency will type `bash scripts/rollback.sh` as readily as `zsh`. Under
# bash the zsh form expands to an unbound-variable error on line 2 and the
# script does nothing at all -- a rollback that silently no-ops is worse than
# one that refuses to run.
DIR="$(cd "$(dirname "$0")" && pwd)"

# STEP 1, BEFORE ANYTHING ELSE: undo the machine-wide transparent-mode changes.
#
# While /etc/hosts redirects api.anthropic.com at a port with nothing behind
# it, EVERY process on this Mac that talks to Anthropic fails -- not just this
# session. That is the widest-blast-radius state the tool can create, so it is
# the first thing undone, before any step that could itself fail.
#
# Needs root. Unattended callers (watchdog.sh) require a sudoers NOPASSWD entry
# for this exact path; without one this prints what to run and continues rather
# than aborting the rest of the rollback.
ROOT_HELPER="$DIR/transparent-root.sh"
if [[ -x "$ROOT_HELPER" ]]; then
  if [[ $EUID -eq 0 ]]; then
    "$ROOT_HELPER" remove
  elif sudo -n true 2>/dev/null; then
    sudo -n "$ROOT_HELPER" remove
  else
    # Only nag if something is actually installed; the common case is a
    # base-url-mode user who never had any of this.
    if grep -q '^# BEGIN claude-burst hosts$' /etc/hosts 2>/dev/null; then
      echo "WARNING: /etc/hosts still contains a claude-burst redirect and this" >&2
      echo "         rollback cannot remove it without root. Run NOW:" >&2
      echo "           sudo $ROOT_HELPER remove" >&2
    else
      echo "no transparent-mode changes present (nothing root-owned to undo)"
    fi
  fi
fi

# STEP 1b: undo the System keychain CA trust added by
# trust-ca-systemwide.sh (see INVESTIGATION-TLS-STORM.md for why that
# exists -- without it, the redirect above breaks TLS for every OTHER app
# on this Mac that happens to reach api.anthropic.com, not just Claude
# Code CLI). Same sudo-or-print pattern as the block above, and only nags
# if the cert is actually present.
UNTRUST_HELPER="$DIR/untrust-ca-systemwide.sh"
if [[ -x "$UNTRUST_HELPER" ]]; then
  if [[ $EUID -eq 0 ]]; then
    "$UNTRUST_HELPER"
  elif sudo -n true 2>/dev/null; then
    sudo -n "$UNTRUST_HELPER"
  elif security find-certificate -c "claude-burst local CA" /Library/Keychains/System.keychain >/dev/null 2>&1; then
    echo "WARNING: the System keychain still trusts claude-burst's local CA and this" >&2
    echo "         rollback cannot remove it without root. Run NOW:" >&2
    echo "           sudo $UNTRUST_HELPER" >&2
  else
    echo "no system-wide CA trust present (nothing root-owned to undo)"
  fi
fi

restored=0
if [[ -f "$BACKUP_DIR/settings.json.latest.bak" ]]; then
  cp "$BACKUP_DIR/settings.json.latest.bak" "$SETTINGS"
  echo "restored $SETTINGS"
  restored=1
fi
if [[ -f "$BACKUP_DIR/config.json.latest.bak" ]]; then
  cp "$BACKUP_DIR/config.json.latest.bak" "$CONFIG"
  echo "restored $CONFIG"
  restored=1
fi
CA_BUNDLE="${NODE_EXTRA_CA_CERTS:-$HOME/.claude/certs/node-extra-ca-certs.pem}"
if [[ -f "$BACKUP_DIR/$(basename "$CA_BUNDLE").latest.bak" ]]; then
  cp "$BACKUP_DIR/$(basename "$CA_BUNDLE").latest.bak" "$CA_BUNDLE"
  echo "restored $CA_BUNDLE"
  restored=1
fi

if [[ "$restored" -eq 0 ]]; then
  echo "no backups found in $BACKUP_DIR -- run scripts/backup-config.sh before making changes next time" >&2
fi

launchctl bootout "gui/$UID/$LABEL" >/dev/null 2>&1 || true
pkill -f '/claude-burst serve' >/dev/null 2>&1 || true
echo "stopped claude-burst gateway (if it was running)"

# STEP 2: belt-and-suspenders cleanup for routing overrides that would
# survive the settings.json restore above if this machine was never backed
# up (backup-config.sh never ran). Safe no-op when these keys are absent --
# added after a stale, out-of-repo copy of this script skipped straight to
# "rollback complete" without ever checking whether traffic could actually
# reach Anthropic again.
if [[ -f "$SETTINGS" ]]; then
  python3 - "$SETTINGS" <<'PY'
import json, sys
from pathlib import Path

p = Path(sys.argv[1])
try:
    data = json.loads(p.read_text())
except Exception as e:
    print(f"could not parse {p}: {e}", file=sys.stderr)
    raise SystemExit(0)

env = data.get("env")
remove = {
    "ANTHROPIC_BASE_URL", "HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY",
    "http_proxy", "https_proxy", "all_proxy",
}
removed = [k for k in list(env or {}) if k in remove]
if removed:
    for k in removed:
        env.pop(k, None)
    if not env:
        data.pop("env", None)
    p.write_text(json.dumps(data, indent=2) + "\n")
    print("removed leftover routing overrides from settings.json: " + ", ".join(removed))
PY
fi

for VAR in ANTHROPIC_BASE_URL HTTP_PROXY HTTPS_PROXY ALL_PROXY http_proxy https_proxy all_proxy; do
  launchctl unsetenv "$VAR" >/dev/null 2>&1 || true
done

# A macOS-level HTTP(S) proxy pointed at this Mac is the other way traffic
# can stay stuck even after a clean hosts/pf rollback. Only ever touches a
# proxy explicitly set to 127.0.0.1/localhost.
networksetup -listallnetworkservices 2>/dev/null | tail -n +2 | while IFS= read -r SERVICE; do
  SERVICE="${SERVICE#\*}"
  [[ -z "$SERVICE" ]] && continue
  for TYPE in web secureweb; do
    INFO="$(networksetup -get${TYPE}proxy "$SERVICE" 2>/dev/null || true)"
    SERVER="$(echo "$INFO" | awk '/Server:/ {print $2}')"
    ENABLED="$(echo "$INFO" | awk '/Enabled:/ {print $2}')"
    if [[ "$ENABLED" == "Yes" && ( "$SERVER" == "127.0.0.1" || "$SERVER" == "localhost" ) ]]; then
      echo "disabling localhost $TYPE proxy on: $SERVICE"
      networksetup -set${TYPE}proxystate "$SERVICE" off
    fi
  done
done

# STEP 3: verify, don't just assert. The stale script this replaces printed
# "Rollback complete" unconditionally -- it never actually checked whether
# Anthropic was reachable again, which is exactly how it "worked" and didn't.
echo
echo "verifying direct connectivity to Anthropic..."
RESULT="$(curl --connect-timeout 8 -sS -o /dev/null -w '%{http_code}|%{remote_ip}' https://api.anthropic.com/ 2>/dev/null)" || RESULT="000|"
HTTP="${RESULT%%|*}"
REMOTE="${RESULT#*|}"

if [[ "$HTTP" != "000" && -n "$REMOTE" && "$REMOTE" != 127.* ]]; then
  echo "verified: api.anthropic.com reachable directly (HTTP $HTTP via $REMOTE)"
  echo "rollback complete -- restart Claude Code"
else
  echo "WARNING: could not verify direct connectivity (HTTP ${HTTP:-000}, remote ${REMOTE:-none})" >&2
  echo "  settings/gateway were rolled back, but something is still in the way. Check:" >&2
  echo "    grep -n anthropic /etc/hosts" >&2
  echo "    sudo scripts/transparent-root.sh status" >&2
  echo "    env | grep -Ei 'proxy|anthropic'" >&2
  exit 2
fi
