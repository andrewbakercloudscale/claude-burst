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

# Not as root. This script asks for root itself, for the two steps that need
# it, and does everything else as you. Run whole under sudo it executed, as
# root, files a non-root user can rewrite: the claude-burst binary (the
# pass-through below), audit-add.sh and codex-unroute.sh beside it, and
# whatever python3 came first on a PATH that the stock macOS sudoers passes
# through unchanged. It also left root-owned backups in your home that the
# next rollback, run as you, could not replace. Refused before the audit
# trap is set, so not even that runs as root.
if [[ $EUID -eq 0 ]]; then
  echo "run this without sudo: it asks for your password itself, for the two steps that need root." >&2
  echo "  $0" >&2
  exit 1
fi

# Read-only here, and overridable only so a test never depends on whether the
# developer's own Mac has the redirect installed.
HOSTS_FILE="${CLAUDE_BURST_HOSTS_FILE:-/etc/hosts}"
LIBEXEC="${CLAUDE_BURST_PFHEAL_LIBEXEC:-/usr/local/libexec/claude-burst}"

# How this ended goes in the audit trail (the Audit tab), whoever ran it: a
# person, the dashboard's button, the mod, or watchdog.sh with nobody there.
trap 'rc=$?; if [ $rc -eq 0 ]; then "$DIR/audit-add.sh" script warn "Script: rollback.sh (burst-off)" "done: Burst is out of the path" >/dev/null 2>&1; else "$DIR/audit-add.sh" script error "Script: rollback.sh (burst-off)" "ended with exit $rc: check that Claude Code reaches Anthropic" >/dev/null 2>&1; fi' EXIT

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
if sudo -n true 2>/dev/null; then
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
#
# Which copy of the helper root runs. The one beside this script lives in the
# checkout or in ~/.local/share/claude-burst, both writable by you and so by
# anything running as you, and `sudo -n` below asks nobody: whatever that
# file holds at that moment runs as root. install-pf-heal.sh puts a
# root-owned copy in /usr/local/libexec/claude-burst for its daemon, so that
# one is used whenever it is there and nobody but root can change it. When it
# is not (the pf guard is optional), the copy beside this script still runs,
# and the line below says so: refusing would leave /etc/hosts redirecting
# with no way to undo it from here, which is the one thing a rollback must
# never do.
root_owned() {
  # A regular file owned by root, in a directory owned by root, neither
  # writable by group or other. The directory counts: a root-owned file in a
  # directory you can write is yours to replace.
  local f="$1" d m
  [[ -f "$f" && ! -L "$f" ]] || return 1
  d="$(dirname "$f")"
  [[ "$(stat -f %u "$f" 2>/dev/null)" == 0 && "$(stat -f %u "$d" 2>/dev/null)" == 0 ]] || return 1
  for m in "$(stat -f %Lp "$f" 2>/dev/null)" "$(stat -f %Lp "$d" 2>/dev/null)"; do
    case "$m" in ''|*[!0-7]*) return 1 ;; esac
    (( (8#$m & 8#022) == 0 )) || return 1
  done
}
ROOT_HELPER="$DIR/transparent-root.sh"
HELPER_NOTE=""
if root_owned "$LIBEXEC/transparent-root.sh"; then
  ROOT_HELPER="$LIBEXEC/transparent-root.sh"
elif [[ -x "$ROOT_HELPER" ]]; then
  HELPER_NOTE="note: root is running $ROOT_HELPER, which is not a root-owned copy (there is none in $LIBEXEC; sudo scripts/install-pf-heal.sh installs one)"
else
  # burst-off runs a copy of this script; fall back to the installed helper
  # if the copy beside it is missing.
  ROOT_HELPER="$LIBEXEC/transparent-root.sh"
fi
if [[ -x "$ROOT_HELPER" ]]; then
  if [[ "$HAVE_ROOT" -eq 1 ]]; then
    [[ -n "$HELPER_NOTE" ]] && echo "$HELPER_NOTE" >&2
    sudo -n "$ROOT_HELPER" remove
  else
    # Only nag if something is actually installed; the common case is a
    # base-url-mode user who never had any of this.
    if grep -q '^# BEGIN claude-burst hosts$' "$HOSTS_FILE" 2>/dev/null; then
      echo "WARNING: /etc/hosts still contains a claude-burst redirect and this" >&2
      echo "         rollback cannot remove it without root. Run NOW:" >&2
      echo "           sudo $ROOT_HELPER remove" >&2
    else
      echo "no transparent-mode changes present (nothing root-owned to undo)"
    fi
  fi
fi

# Still redirected (no root, or the remove failed): api.anthropic.com points
# at this Mac, and the gateway is the only thing answering there. Stopping
# it would cut every app on the Mac off from Anthropic, so it is left
# running, and the step that stops it below is skipped.
REDIRECT_LEFT=0
grep -q '^# BEGIN claude-burst hosts$' "$HOSTS_FILE" 2>/dev/null && REDIRECT_LEFT=1

# STEP 1b: undo the System keychain CA trust added by
# trust-ca-systemwide.sh (see docs/history/investigation-tls-storm.md for why that
# exists -- without it, the redirect above breaks TLS for every OTHER app
# on this Mac that happens to reach api.anthropic.com, not just Claude
# Code CLI). Same sudo-or-print pattern as the block above, and only nags
# if the cert is actually present.
UNTRUST_HELPER="$DIR/untrust-ca-systemwide.sh"
if [[ -x "$UNTRUST_HELPER" ]]; then
  if [[ "$HAVE_ROOT" -eq 1 ]]; then
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
# Kept while the redirect is: Claude Code reaches the gateway in its place
# and needs the CA to trust it.
if (( REDIRECT_LEFT )); then
  echo "kept claude-burst's CA in $CA_BUNDLE: the redirect is still in place"
elif [[ -f "$CA_BUNDLE" ]] && grep -q '^# BEGIN claude-burst CA' "$CA_BUNDLE"; then
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
if (( REDIRECT_LEFT )); then
  echo "WARNING: left the gateway RUNNING: /etc/hosts still sends api.anthropic.com to it," >&2
  echo "         and stopping it would cut every app on this Mac off from Anthropic." >&2
  echo "         Run: sudo $ROOT_HELPER remove, then run this rollback again." >&2
else
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
#
# Only Burst's own processes. These are ordinary port numbers, and until
# 2026-10-12 whatever listened on one was killed, a developer's own server on
# 7777 included. A process counts as Burst's when the program it runs is
# named claude-burst, wherever that file is: the installed binary, a build in
# a checkout, the copy a test run left behind. `ps -o comm=` prints the
# program's path on macOS (observed: /Users/.../.local/bin/claude-burst for
# both `serve` and `console`). Anything else is left running and named, so
# a port still held is explained rather than silently skipped.
KEEP="$(cat "$HOME/.config/claude-burst/passthrough.pid" 2>/dev/null)"
for port in ${(u)PORTS}; do
  for pid in $(lsof -nP -t -iTCP:"$port" -sTCP:LISTEN 2>/dev/null); do
    [[ "$pid" == "$KEEP" ]] && continue
    comm="$(ps -o comm= -p "$pid" 2>/dev/null)"
    if [[ "${comm:t}" != claude-burst ]]; then
      echo "port $port: left pid $pid (${comm:-unknown program}) running: it is not Claude Burst. If Burst needs this port, stop it yourself."
      continue
    fi
    echo "port $port: stopping pid $pid ($comm)"
    kill "$pid" 2>/dev/null
    for _ in 1 2 3 4 5; do kill -0 "$pid" 2>/dev/null || break; sleep 1; done
    kill -0 "$pid" 2>/dev/null && kill -9 "$pid" 2>/dev/null
  done
done
echo "  marked $ROLLED_BACK_MARKER so the self-heal watchdog leaves it stopped"
fi

# STEP 2: belt-and-suspenders cleanup for routing overrides that would
# survive the settings.json restore above if this machine was never backed
# up (backup-config.sh never ran). Safe no-op when these keys are absent --
# added after a stale, out-of-repo copy of this script skipped straight to
# "rollback complete" without ever checking whether traffic could actually
# reach Anthropic again.
#
# What counts as Burst's, here and in the two blocks after it: a value that
# names this Mac AND one of Burst's own ports (PORTS above: the defaults, plus
# what config.json said before it was restored). Until 2026-10-12 the port
# was not looked at, so any local value went: a developer's own proxy on
# 127.0.0.1:8080 (mitmproxy, Charles), or a local model gateway they had
# pointed ANTHROPIC_BASE_URL at themselves. Burst only ever writes
# ANTHROPIC_BASE_URL, and always with its port; the proxy variables and the
# macOS proxy are checked because a value pointing at a Burst port is dead
# once the gateway stops, whoever set it.
is_burst_value() {
  local v="${1:l}" hp
  v="${v#http://}"; v="${v#https://}"
  hp="${v%%/*}"
  case "$hp" in
    127.0.0.1:*|localhost:*|\[::1\]:*) ;;
    *) return 1 ;;
  esac
  (( ${PORTS[(Ie)${hp##*:}]} ))
}
if [[ -f "$SETTINGS" ]]; then
  python3 - "$SETTINGS" "$CONFIG" ${(u)PORTS} <<'PY'
import json, sys
from pathlib import Path
from urllib.parse import urlsplit

PORTS = set(sys.argv[3:])

p = Path(sys.argv[1])
try:
    data = json.loads(p.read_text())
except Exception as e:
    print(f"could not parse {p}: {e}", file=sys.stderr)
    raise SystemExit(0)

env = data.get("env")
# Only values that point at this Mac, on one of Burst's ports, are Burst's.
# A Portkey or corporate gateway URL, a real corporate proxy, or the user's
# own proxy on another local port is the user's and stays.
def ours(v):
    v = str(v).strip().lower()
    if "://" not in v:
        v = "http://" + v
    try:
        u = urlsplit(v)
        host, port = u.hostname, u.port
    except ValueError:
        return False
    return host in ("127.0.0.1", "localhost", "::1") and str(port) in PORTS
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

# Same rule for launchd's environment: this Mac, on one of Burst's ports.
for VAR in ANTHROPIC_BASE_URL HTTP_PROXY HTTPS_PROXY ALL_PROXY http_proxy https_proxy all_proxy; do
  VAL="$(launchctl getenv "$VAR" 2>/dev/null || true)"
  [[ -n "$VAL" ]] || continue
  if is_burst_value "$VAL"; then
    launchctl unsetenv "$VAR" >/dev/null 2>&1 || true
    echo "removed launchd $VAR=$VAL"
  else
    case "${VAL:l}" in
      *127.0.0.1*|*localhost*|*\[::1\]*) echo "left launchd $VAR=$VAL as it is (not one of Burst's ports)" ;;
    esac
  fi
done

# A macOS-level HTTP(S) proxy pointed at this Mac is the other way traffic
# can stay stuck even after a clean hosts/pf rollback. Only ever touches a
# proxy set to 127.0.0.1/localhost on one of Burst's ports.
#
# The fields are matched from the start of the line. `networksetup
# -getwebproxy` also prints "Authenticated Proxy Enabled: 0", so the old
# /Enabled:/ picked up two lines, never equalled "Yes", and this block had
# never once switched a proxy off. Anchoring it is what makes it act at all,
# which is why the port is checked in the same change.
networksetup -listallnetworkservices 2>/dev/null | tail -n +2 | while IFS= read -r SERVICE; do
  SERVICE="${SERVICE#\*}"
  [[ -z "$SERVICE" ]] && continue
  for TYPE in web secureweb; do
    INFO="$(networksetup -get${TYPE}proxy "$SERVICE" 2>/dev/null || true)"
    SERVER="$(echo "$INFO" | awk '/^Server:/ {print $2}')"
    PPORT="$(echo "$INFO" | awk '/^Port:/ {print $2}')"
    ENABLED="$(echo "$INFO" | awk '/^Enabled:/ {print $2}')"
    [[ "$ENABLED" == "Yes" && ( "$SERVER" == "127.0.0.1" || "$SERVER" == "localhost" ) ]] || continue
    if is_burst_value "$SERVER:$PPORT"; then
      echo "disabling localhost $TYPE proxy on: $SERVICE (port $PPORT is Burst's)"
      networksetup -set${TYPE}proxystate "$SERVICE" off
    else
      echo "left the $TYPE proxy on $SERVICE as it is: $SERVER:$PPORT is not one of Burst's ports"
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

# The network is half of it. Claude Code in base-url mode is sent to the
# gateway by settings.json, and a rollback that left that line behind points
# every new session at a port nothing listens on, with this check green.
# Burst's ports only, the same rule as the removal above: a base URL on
# another local port is the user's own gateway, was left alone on purpose,
# and must not fail the rollback here.
PORT_ALT=${(j:|:)${(u)PORTS}}
LEFT="$(grep -nE "\"ANTHROPIC_BASE_URL\" *: *\"https?://(127\\.0\\.0\\.1|localhost):($PORT_ALT)([/\"]|\$)" "$HOME/.claude/settings.json" 2>/dev/null || true)"
if [[ -n "$LEFT" ]] && ! pgrep -f 'claude-burst passthrough' >/dev/null 2>&1; then
  echo "WARNING: ~/.claude/settings.json still sends Claude Code to this Mac, and nothing stands in for the gateway:" >&2
  echo "  $LEFT" >&2
  echo "  Remove that ANTHROPIC_BASE_URL line (or run: claude-burst disable), then start a new session." >&2
  exit 2
fi

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
