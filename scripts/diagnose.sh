#!/bin/bash
# Everything needed to see why Claude Burst is not working on this Mac, in one
# report: what is installed, what is running, what Claude Code is pointed at,
# the network path to Anthropic, and the recent logs. Needs no password and
# changes nothing.
#
#   scripts/diagnose.sh            writes ~/burst-diagnose-<time>.txt and copies it
#
# Secrets are redacted (API keys, tokens, Authorization headers, keychain
# contents are never read). Paste the report wherever you are getting help.
#
# Bash on purpose, written to run on a Mac where nothing else works: no Go,
# no repo build, no gateway needed.

set -u
OUT="$HOME/burst-diagnose-$(date +%Y%m%d-%H%M%S).txt"
CFG="$HOME/.config/claude-burst"
LABEL="ninja.andrewbaker.claude-burst"
REPO="$(cd "$(dirname "$0")/.." 2>/dev/null && pwd)"
# Piped from curl, $0 is "bash": look where the README's scripts put it.
if [ ! -d "$REPO/.git" ]; then
  for d in ~/claude-burst ~/claude-burst-repo ~/Desktop/github/claude-burst; do
    [ -d "$d/.git" ] && REPO=$d && break
  done
fi

redact() {
  sed -E \
    -e 's/(sk-[A-Za-z0-9_-]{4})[A-Za-z0-9_-]+/\1...REDACTED/g' \
    -e 's/((api_?key|token|secret|password|authorization|bearer)"?[[:space:]]*[:=][[:space:]]*"?)[^", ]+/\1REDACTED/Ig' \
    -e 's/(Bearer )[A-Za-z0-9._-]+/\1REDACTED/g'
}
section() { printf '\n===== %s =====\n' "$1"; }
run() { # label, command...
  printf '\n$ %s\n' "$1"; shift
  "$@" 2>&1 | head -200
}
probe() { # url
  curl -sS -m 5 -o /dev/null -w "HTTP %{http_code}, connect %{time_connect}s, total %{time_total}s, ip %{remote_ip}\n" "$1" 2>&1
}

{
section "When and where"
date '+%Y-%m-%d %H:%M:%S %Z'
sw_vers 2>/dev/null | tr '\n' ' '; echo
uname -m
echo "user: $USER, shell: $SHELL"

section "Claude Burst build"
if [ -d "$REPO/.git" ]; then
  run "repo commit" git -C "$REPO" log --oneline -3
  run "repo state" git -C "$REPO" status --short --branch
fi
ls -l "$HOME/.local/bin/claude-burst" 2>&1
b="$(command -v claude-burst 2>/dev/null)"
[ -n "$b" ] && [ "$b" != "$HOME/.local/bin/claude-burst" ] && echo "on PATH instead: $b"
run "claude-burst version" "$HOME/.local/bin/claude-burst" version
run "claude-burst status" "$HOME/.local/bin/claude-burst" status

section "Claude Code"
run "claude version" claude --version
run "which claude" bash -c 'type -a claude'
if [ -f "$HOME/.claude/settings.json" ]; then
  echo
  echo "settings.json env (what Claude Code is pointed at):"
  /usr/bin/python3 - "$HOME/.claude/settings.json" <<'PY' 2>&1 | redact
import json, sys
s = json.load(open(sys.argv[1]))
env = s.get("env", {})
for k in sorted(env):
    if any(w in k.upper() for w in ("BASE_URL", "PROXY", "CA_CERT", "API", "AUTH", "MODEL", "BURST")):
        print(f"  {k} = {env[k]}")
hooks = s.get("hooks", {})
print("  hooks:", {k: len(v) for k, v in hooks.items()})
print("  enabledPlugins:", [k for k, v in s.get("enabledPlugins", {}).items() if v])
PY
fi
echo
echo "environment of this shell:"
env | grep -E '^(ANTHROPIC_|CLAUDE_|NODE_EXTRA_CA|HTTPS?_PROXY|https?_proxy|NO_PROXY|no_proxy)' | redact | sed 's/^/  /'

section "LaunchAgents and LaunchDaemons"
for l in "$LABEL" "$LABEL-selfheal"; do
  printf '\n$ launchctl print gui/%s/%s\n' "$UID" "$l"
  launchctl print "gui/$UID/$l" 2>&1 | grep -E 'state =|pid =|last exit|program|path =|runs =|spawn type' | head -12
done
ls -l "$HOME/Library/LaunchAgents/" 2>/dev/null | grep -i burst
ls -l /Library/LaunchDaemons/ 2>/dev/null | grep -i burst
for l in "$LABEL-lidawake" "$LABEL-pfheal"; do
  printf '\n$ launchctl print system/%s\n' "$l"
  launchctl print "system/$l" 2>&1 | grep -E 'state =|pid =|last exit' | head -5
done
run "burst processes" bash -c 'ps -axo pid,lstart,command | grep -i "[c]laude-burst" | grep -v -e shell-snapshots -e diagnose.sh | cut -c1-200'

section "Listening ports"
run "lsof 7777 7788 17777 443" bash -c 'lsof -nP -iTCP:7777 -iTCP:7788 -iTCP:17777 -iTCP:443 -sTCP:LISTEN'

section "Gateway health"
# The ports config.json names; the defaults when it names none.
LISTEN=$(/usr/bin/python3 -c 'import json,sys; print(json.load(open(sys.argv[1])).get("listen","127.0.0.1:7777"))' "$CFG/config.json" 2>/dev/null || echo 127.0.0.1:7777)
ADMIN=$(/usr/bin/python3 -c 'import json,sys; print(json.load(open(sys.argv[1])).get("admin_listen","127.0.0.1:7788"))' "$CFG/config.json" 2>/dev/null || echo 127.0.0.1:7788)
echo "gateway listens on $LISTEN, dashboard on $ADMIN (from config.json)"
MODE=$(/usr/bin/python3 -c 'import json,sys; print(json.load(open(sys.argv[1])).get("intercept",{}).get("mode","base-url"))' "$CFG/config.json" 2>/dev/null || echo base-url)
echo "intercept mode: $MODE"
if [ "$MODE" = transparent ]; then
  # Here the gateway is reached only as api.anthropic.com, through /etc/hosts
  # and pf; a direct connection to its port can hang on macOS (issue #1).
  # "Network path to Anthropic" below is the real test.
  echo "direct probe skipped in transparent mode: see https://api.anthropic.com/healthz below"
else
  printf '%-40s ' "http://$LISTEN/healthz"; probe "http://$LISTEN/healthz"
fi
printf '%-40s ' "http://$ADMIN/ (dashboard)"; probe "http://$ADMIN/"
printf '%-40s ' "dashboard /api/state"
curl -sS -m 5 "http://$ADMIN/api/state" 2>&1 | head -c 600 | redact; echo

section "Burst config (secrets redacted)"
if [ -f "$CFG/config.json" ]; then
  /usr/bin/python3 - "$CFG/config.json" <<'PY' 2>&1 | redact
import json, sys
c = json.load(open(sys.argv[1]))
for k in ("listen", "admin_listen", "primary", "secondary", "intercept", "metered_failover",
          "primary_compaction", "keep_awake_lid_closed", "keep_awake_lid_closed_power", "automask"):
    if k in c:
        print(f"  {k}: {json.dumps(c[k])[:400]}")
PY
else
  echo "no $CFG/config.json"
fi
ls -la "$CFG" 2>&1 | grep -v -E 'claude-burst\.log\.[0-9]|\.pcap$|tls-storm|direct-port' | head -40

section "Interception (transparent mode)"
run "/etc/hosts entries" grep -n -i 'anthropic\|claude-burst' /etc/hosts
run "pf anchor (no sudo: may say permission denied)" bash -c 'pfctl -a com.apple/claude-burst -s nat 2>&1; pfctl -a claude-burst -s nat 2>&1'
run "System keychain CA" bash -c 'security find-certificate -a -c "Claude Burst" /Library/Keychains/System.keychain 2>&1 | grep -E "labl|alis" | head -4'

section "Network path to Anthropic"
run "default route" bash -c 'route -n get default 2>&1 | grep -E "gateway|interface"'
run "DNS servers" bash -c 'scutil --dns | grep "nameserver\[" | sort -u | head -6'
run "resolve api.anthropic.com" bash -c 'dscacheutil -q host -a name api.anthropic.com | head -6'
printf '%-36s ' "https://api.anthropic.com/healthz"; probe https://api.anthropic.com/healthz
printf '%-36s ' "control: captive.apple.com"; probe https://captive.apple.com/hotspot-detect.html
run "system proxy" bash -c 'scutil --proxy | grep -E "Enable|Proxy :|Port" | head -10'

section "Power"
run "pmset SleepDisabled" bash -c 'pmset -g | grep -i -E "sleepdisabled|^ sleep"'

section "Logs (newest last)"
for f in claude-burst.log launchd.err.log launchd.out.log watchdog.log self-heal.log; do
  if [ -f "$CFG/$f" ]; then
    printf '\n--- %s (last 60 lines) ---\n' "$f"
    tail -60 "$CFG/$f" | redact
  fi
done
for f in /var/log/claude-burst-pf.log /var/log/claude-burst-lidawake.log; do
  [ -r "$f" ] && { printf '\n--- %s (last 15) ---\n' "$f"; tail -15 "$f"; }
done
[ -f "$CFG/notices.json" ] && { printf '\n--- notices.json (last 10 events) ---\n'; /usr/bin/python3 -c '
import json,sys
d=json.load(open(sys.argv[1])); ev=d.get("events",d) if isinstance(d,dict) else d
for e in ev[-10:]: print(e.get("at","")[:19], e.get("severity"), e.get("title"), "|", e.get("detail","")[:160])' "$CFG/notices.json" 2>&1; }

section "End"
} > "$OUT" 2>&1

pbcopy < "$OUT" 2>/dev/null && copied=" and copied to the clipboard" || copied=""
echo "Report written to $OUT$copied ($(wc -l < "$OUT" | tr -d ' ') lines)."
echo "It needs no password and changed nothing. Secrets are redacted; skim it before sharing."
