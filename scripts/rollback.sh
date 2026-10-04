#!/bin/zsh
# Instant manual rollback (installed on the PATH as burst-off): removes Claude Burst's own entries from
# ~/.claude/settings.json and the CA bundle (everything else in them is kept),
# restores ~/.config/claude-burst/config.json from the most recent snapshot,
# and stops the gateway LaunchAgent. Safe to run any time, more
# than once, or when the gateway was never enabled.
set -uo pipefail

BACKUP_DIR="${CLAUDE_BURST_BACKUP_DIR:-$HOME/.config/claude-burst/backups}"
LABEL="ninja.andrewbaker.claude-burst"
SETTINGS="$HOME/.claude/settings.json"
CONFIG="$HOME/.config/claude-burst/config.json"
ROLLED_BACK_MARKER="${CLAUDE_BURST_ROLLED_BACK_MARKER:-$HOME/.config/claude-burst/rolled-back}"

# Resolve this script's directory. Written the POSIX way rather than with zsh's
# ${0:A:h}: these scripts are recovery tooling, and someone reaching for them in
# an emergency will type `bash scripts/rollback.sh` as readily as `zsh`. Under
# bash the zsh form expands to an unbound-variable error on line 2 and the
# script does nothing at all -- a rollback that silently no-ops is worse than
# one that refuses to run.
DIR="$(cd "$(dirname "$0")" && pwd)"

# Acquire root once, upfront, for the two helper steps below. A NOPASSWD
# sudoers entry (or an already-cached credential) satisfies this silently --
# that's what lets watchdog.sh call this script unattended. Otherwise, only
# prompt for the password when there's an actual terminal to prompt on: an
# unattended caller has no tty and must never block waiting for input, but a
# human running this by hand needs the same interactive `sudo -v` prompt the
# old out-of-repo rollback.sh used. Skipping straight to "print what to run
# and continue" here is what made this script silently no-op the /etc/hosts
# and CA-trust cleanup on a machine with no NOPASSWD rule configured.
HAVE_ROOT=0
if [[ $EUID -eq 0 ]]; then
  HAVE_ROOT=1
elif sudo -n true 2>/dev/null; then
  HAVE_ROOT=1
elif [[ -t 0 ]]; then
  echo "claude-burst rollback needs sudo to remove /etc/hosts, pf, and CA trust state." >&2
  sudo -v && HAVE_ROOT=1
fi

# STEP 0: keep the Claude Code sessions already open working. In base-url
# mode each one sends to the gateway's port for its whole life, so stopping
# the gateway below would cut every one of them. A pass-through takes the port
# the moment the gateway lets go and forwards straight to Anthropic (or the
# adopted corporate gateway); it stops itself after an hour unused. Started
# now, before config.json is restored, so it reads the live listen address.
# The binary decides: transparent mode needs none.
BIN="${CLAUDE_BURST_BIN:-$HOME/.local/bin/claude-burst}"
# Burst's ports, read now: config.json may be restored to a snapshot below.
# The defaults too (gateway 7777, transparent 17777, dashboard 7788), so a
# leftover from an older setup is caught as well.
PORTS=(7777 17777 7788 7779)
if [[ -f "$CONFIG" ]]; then
  PORTS+=($(python3 -c 'import json,sys
c=json.load(open(sys.argv[1]))
for k in ("listen","admin_listen"):
    v=c.get(k) or ""
    if ":" in v: print(v.rsplit(":",1)[1])' "$CONFIG" 2>/dev/null))
fi
if [[ -x "$BIN" ]]; then
  "$BIN" passthrough --detach || echo "could not start the pass-through: sessions already open need restarting (claude --resume keeps their history)" >&2
fi

# STEP 1, BEFORE ANYTHING ELSE: undo the machine-wide transparent-mode changes.
#
# While /etc/hosts redirects api.anthropic.com at a port with nothing behind
# it, EVERY process on this Mac that talks to Anthropic fails -- not just this
# session. That is the widest-blast-radius state the tool can create, so it is
# the first thing undone, before any step that could itself fail.
ROOT_HELPER="$DIR/transparent-root.sh"
# burst-off runs a copy of this script; prefer the installed root helper if
# the copy beside it is missing.
[[ -x "$ROOT_HELPER" ]] || ROOT_HELPER="/usr/local/libexec/claude-burst/transparent-root.sh"
if [[ -x "$ROOT_HELPER" ]]; then
  if [[ $EUID -eq 0 ]]; then
    "$ROOT_HELPER" remove
  elif [[ "$HAVE_ROOT" -eq 1 ]]; then
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
# trust-ca-systemwide.sh (see docs/history/investigation-tls-storm.md for why that
# exists -- without it, the redirect above breaks TLS for every OTHER app
# on this Mac that happens to reach api.anthropic.com, not just Claude
# Code CLI). Same sudo-or-print pattern as the block above, and only nags
# if the cert is actually present.
UNTRUST_HELPER="$DIR/untrust-ca-systemwide.sh"
if [[ -x "$UNTRUST_HELPER" ]]; then
  if [[ $EUID -eq 0 ]]; then
    "$UNTRUST_HELPER"
  elif [[ "$HAVE_ROOT" -eq 1 ]]; then
    sudo -n "$UNTRUST_HELPER"
  elif security find-certificate -c "claude-burst local CA" /Library/Keychains/System.keychain >/dev/null 2>&1; then
    echo "WARNING: the System keychain still trusts claude-burst's local CA and this" >&2
    echo "         rollback cannot remove it without root. Run NOW:" >&2
    echo "           sudo $UNTRUST_HELPER" >&2
  else
    echo "no system-wide CA trust present (nothing root-owned to undo)"
  fi
fi

# settings.json and the CA bundle are NOT restored from a backup copy. Both
# hold far more than Claude Burst's own entries: settings.json carries the
# user's hooks, status line, permissions, display settings and possibly a
# corporate gateway (Portkey and similar: ANTHROPIC_BASE_URL plus headers);
# the CA bundle may hold an employer's CAs. Copying an old snapshot over
# them loses everything changed since, and on 2026-10-02 the snapshot was a
# test's throwaway file: the restore cut a 7KB settings.json to 71 bytes and
# every running session went blank. Instead, each file is copied aside and
# only Burst's own entries are removed from it (STEP 2 below for settings,
# here for the bundle). config.json is Burst's own file, so it is restored.
restored=0
TS="$(date +%Y%m%d-%H%M%S)"
if [[ -f "$SETTINGS" ]]; then
  mkdir -p "$BACKUP_DIR" && cp "$SETTINGS" "$BACKUP_DIR/settings.json.before-rollback-$TS.bak" &&
    echo "kept a copy of $SETTINGS as $BACKUP_DIR/settings.json.before-rollback-$TS.bak"
fi
if [[ -f "$BACKUP_DIR/config.json.latest.bak" ]]; then
  cp "$BACKUP_DIR/config.json.latest.bak" "$CONFIG"
  echo "restored $CONFIG"
  restored=1
fi
CA_BUNDLE="${NODE_EXTRA_CA_CERTS:-$HOME/.claude/certs/node-extra-ca-certs.pem}"
if [[ -f "$CA_BUNDLE" ]] && grep -q '^# BEGIN claude-burst CA' "$CA_BUNDLE"; then
  cp "$CA_BUNDLE" "$BACKUP_DIR/$(basename "$CA_BUNDLE").before-rollback-$TS.bak"
  # Same rule as tlsca.StripBlock: remove only a complete marked block.
  python3 - "$CA_BUNDLE" <<'PY'
import sys
from pathlib import Path
p = Path(sys.argv[1])
s = p.read_text()
b = s.find("# BEGIN claude-burst CA")
e = s.find("# END claude-burst CA", b)
if b >= 0 and e >= 0:
    e += len("# END claude-burst CA")
    if s[e:e+1] == "\n":
        e += 1
    tmp = p.with_name(p.name + ".tmp")
    tmp.write_text(s[:b] + s[e:])
    tmp.chmod(0o600)
    tmp.replace(p)
    print(f"removed claude-burst's CA from {p}, everything else in it kept")
PY
  restored=1
fi

if [[ "$restored" -eq 0 ]]; then
  echo "no backups found in $BACKUP_DIR -- run scripts/backup-config.sh before making changes next time" >&2
fi

# STEP 1d: Codex. Burst's block at the top of ~/.codex/config.toml sends
# Codex to the gateway's Codex port; without the gateway that port is dead.
# Only the lines between Burst's own markers are removed. Codex sessions
# already open keep sending to the port until restarted.
"$(dirname "$0")/codex-unroute.sh" || true

# Written BEFORE the gateway is stopped, not after. self-heal-watchdog.sh
# runs every ~2 minutes and reloads the gateway LaunchAgent the instant it
# finds it unloaded -- which, on 2026-09-07, meant it booted the gateway
# straight back up 90 seconds after this script had deliberately stopped it.
# Nothing was harmed that time (hosts and pf were already gone, so nothing
# routed to it), but a rollback that an unattended watchdog silently undoes
# is not a rollback. The marker says "a human chose this state"; the watchdog
# honours it, and install-proxy.sh clears it when the gateway is wanted again.
mkdir -p "$(dirname "$ROLLED_BACK_MARKER")" 2>/dev/null
date '+%Y-%m-%d %H:%M:%S rolled back by scripts/rollback.sh' > "$ROLLED_BACK_MARKER"

launchctl bootout "gui/$UID/$LABEL" >/dev/null 2>&1 || true
pkill -f '/claude-burst serve' >/dev/null 2>&1 || true
echo "stopped claude-burst gateway (if it was running)"

# STEP 1c: nothing else may hold Burst's ports. A stuck gateway, a leftover
# from a test run or an older install answers on them and fails every
# session sent there (2026-10-04: test leftovers held 17777 for 20 minutes).
# The pass-through from STEP 0 is the one thing kept: it is what keeps open
# sessions working, and it is waiting for exactly this port.
KEEP="$(cat "$HOME/.config/claude-burst/passthrough.pid" 2>/dev/null)"
for port in ${(u)PORTS}; do
  for pid in $(lsof -nP -t -iTCP:"$port" -sTCP:LISTEN 2>/dev/null); do
    [[ "$pid" == "$KEEP" ]] && continue
    echo "port $port: stopping pid $pid ($(ps -o comm= -p "$pid" 2>/dev/null))"
    kill "$pid" 2>/dev/null
    for _ in 1 2 3 4 5; do kill -0 "$pid" 2>/dev/null || break; sleep 1; done
    kill -0 "$pid" 2>/dev/null && kill -9 "$pid" 2>/dev/null
  done
done
echo "  marked $ROLLED_BACK_MARKER so the self-heal watchdog leaves it stopped"

# STEP 2: belt-and-suspenders cleanup for routing overrides that would
# survive the settings.json restore above if this machine was never backed
# up (backup-config.sh never ran). Safe no-op when these keys are absent --
# added after a stale, out-of-repo copy of this script skipped straight to
# "rollback complete" without ever checking whether traffic could actually
# reach Anthropic again.
if [[ -f "$SETTINGS" ]]; then
  python3 - "$SETTINGS" "$CONFIG" <<'PY'
import json, sys
from pathlib import Path

p = Path(sys.argv[1])
try:
    data = json.loads(p.read_text())
except Exception as e:
    print(f"could not parse {p}: {e}", file=sys.stderr)
    raise SystemExit(0)

env = data.get("env")
# Only values that point at this Mac are Burst's. A Portkey or corporate
# gateway URL, or a real corporate proxy, is the user's and stays.
def ours(v):
    v = str(v).lower()
    for pre in ("http://", "https://", ""):
        for host in ("127.0.0.1", "localhost", "[::1]"):
            if v.startswith(pre + host):
                return True
    return False
keys = {
    "ANTHROPIC_BASE_URL", "HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY",
    "http_proxy", "https_proxy", "all_proxy",
}
removed = [k for k in list(env or {}) if k in keys and ours(env[k])]
kept = [f"{k}={env[k]}" for k in list(env or {}) if k in keys and not ours(env[k])]
for k in kept:
    print(f"left as it is (not ours): {k}")
# enable may have adopted an enterprise gateway (Portkey and similar) as
# the primary; it goes back so Claude Code is left as it was found.
adopted = ""
try:
    adopted = json.loads(Path(sys.argv[2]).read_text()).get("adopted_base_url", "")
except Exception:
    pass
if removed:
    for k in removed:
        env.pop(k, None)
    if "ANTHROPIC_BASE_URL" in removed and adopted:
        env["ANTHROPIC_BASE_URL"] = adopted
        print(f"put ANTHROPIC_BASE_URL back to {adopted}")
    if not env:
        data.pop("env", None)
    tmp = p.with_name(p.name + ".tmp")
    tmp.write_text(json.dumps(data, indent=2) + "\n")
    tmp.replace(p)
    print("removed Claude Burst's routing from settings.json: " + ", ".join(removed))
PY
fi

# Same rule for launchd's environment: only values pointing at this Mac.
for VAR in ANTHROPIC_BASE_URL HTTP_PROXY HTTPS_PROXY ALL_PROXY http_proxy https_proxy all_proxy; do
  VAL="$(launchctl getenv "$VAR" 2>/dev/null || true)"
  case "$VAL" in
    http://127.0.0.1*|https://127.0.0.1*|http://localhost*|https://localhost*|127.0.0.1*|localhost*)
      launchctl unsetenv "$VAR" >/dev/null 2>&1 || true
      echo "removed launchd $VAR=$VAL" ;;
  esac
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
  echo "rollback complete: sessions already open keep working, new ones go straight to Anthropic"
else
  echo "WARNING: could not verify direct connectivity (HTTP ${HTTP:-000}, remote ${REMOTE:-none})" >&2
  echo "  settings/gateway were rolled back, but something is still in the way. Check:" >&2
  echo "    grep -n anthropic /etc/hosts" >&2
  echo "    sudo scripts/transparent-root.sh status" >&2
  echo "    env | grep -Ei 'proxy|anthropic'" >&2
  exit 2
fi
