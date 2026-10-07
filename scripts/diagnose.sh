#!/bin/bash
# Everything needed to see why Claude Burst is not working on this Mac, in one
# report: what is installed, what is running, what Claude Code is pointed at,
# the network path to Anthropic, and the recent logs. Needs no password and
# changes nothing.
#
#   scripts/diagnose.sh            writes ~/burst-diagnose-<time>.txt and copies it
#   scripts/diagnose.sh --check    the checks only: no report, nothing copied
#
# It opens with the checks: one line each, PASS, FAIL or SKIP with the reason
# and what to run, then "N checks, M failed". The exit code is 0 when none
# failed and 1 when any did, so a script (or a person in a hurry) gets the
# answer without reading the report. This replaced connectivity-test.sh,
# which made real paid calls with the stored keys to say less.
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

# Python, not sed: BSD sed has no escapes inside a bracket expression, and a
# redactor that misses a case is worse than none. Order matters: Bearer and
# Basic values first, so "Authorization: Bearer x" loses x, not the word.
# "token(?!s)": input_tokens counts are the point of the report.
redact() {
  /usr/bin/python3 -c '
import re, sys
V = r"[^\s\"\x27,;]+"
rules = [
  (r"(sk-[A-Za-z0-9_-]{4})[A-Za-z0-9_-]+", r"\1...REDACTED"),
  (r"(?i)\b((?:bearer|basic)\s+)" + V, r"\1REDACTED"),
  (r"(?i)([\w-]*(?:api[_-]?key|token(?!s)|secret|password|passwd|cookie|authorization)[\w-]*[\"\x27]?\s*[:=]\s*[\"\x27]?)(?!REDACTED|(?i:bearer|basic)\s)" + V, r"\1REDACTED"),
]
for line in sys.stdin:
  for pat, rep in rules:
    line = re.sub(pat, rep, line)
  sys.stdout.write(line)
'
}
section() { printf '\n===== %s =====\n' "$1"; }
run() { # label, command...
  printf '\n$ %s\n' "$1"; shift
  "$@" 2>&1 | head -200
}
probe() { # url
  curl -sS -m 5 -o /dev/null -w "HTTP %{http_code}, connect %{time_connect}s, total %{time_total}s, ip %{remote_ip}\n" "$1" 2>&1
}

# ---- The checks ----
# Each is one question with a yes or no answer, asked without a password and
# without sending a credential anywhere. A check that does not apply to this
# Mac (the Codex gateway is off, Burst was turned off on purpose) is a SKIP,
# which is not a failure.
CHECKS=""; TOTAL=0; FAILED=0; SKIPPED=0
pass() { TOTAL=$((TOTAL + 1)); CHECKS="${CHECKS}PASS  $1"$'\n'; }
fail() { TOTAL=$((TOTAL + 1)); FAILED=$((FAILED + 1)); CHECKS="${CHECKS}FAIL  $1"$'\n'"      $2"$'\n'; }
skip() { SKIPPED=$((SKIPPED + 1)); CHECKS="${CHECKS}SKIP  $1"$'\n'; }
http_code() { curl -s -m "${2:-5}" -o /dev/null -w '%{http_code}' "$1" 2>/dev/null || true; }
cfg() { # key path, default: a value from config.json
  /usr/bin/python3 -c '
import json, sys
try:
    v = json.load(open(sys.argv[1]))
    for k in sys.argv[2].split("."):
        v = v[k]
    print(v if v not in (None, "") else sys.argv[3])
except Exception:
    print(sys.argv[3])' "$CFG/config.json" "$1" "$2" 2>/dev/null || echo "$2"
}
BIN="$HOME/.local/bin/claude-burst"
LISTEN="$(cfg listen 127.0.0.1:7777)"
ADMIN="$(cfg admin_listen 127.0.0.1:7788)"
CONSOLE="$(cfg console_listen 127.0.0.1:7789)"
MODE="$(cfg intercept.mode base-url)"
OFF=""; [ -f "$CFG/rolled-back" ] && OFF=1

run_checks() {
  local v code pid

  if v="$("$BIN" version 2>/dev/null)" && [ -n "$v" ]; then pass "claude-burst $v is installed and runs"
  else fail "claude-burst is not installed or does not run ($BIN)" "run: burst-reinstall, or ./install.sh from the checkout"; fi

  if [ ! -f "$CFG/config.json" ]; then fail "there is no config.json" "run: claude-burst configure"
  elif /usr/bin/python3 -c 'import json,sys; json.load(open(sys.argv[1]))' "$CFG/config.json" 2>/dev/null; then pass "config.json loads"
  else fail "config.json does not load" "run: claude-burst restore-config"; fi

  if [ -n "$OFF" ]; then
    skip "Burst was turned off by hand (burst-off): the gateway checks do not apply. claude-burst enable turns it back on"
  else
    pid="$(launchctl print "gui/$UID/$LABEL" 2>/dev/null | awk '/^[[:space:]]*pid = [0-9]+/ {print $3; exit}')"
    if [ -n "$pid" ]; then pass "the gateway is running (pid $pid)"
    else fail "the gateway is not running" "run: burst-repair"; fi

    if [ "$MODE" = transparent ]; then
      if grep -qE '^[[:space:]]*127\.0\.0\.1[[:space:]]+api\.anthropic\.com' /etc/hosts 2>/dev/null; then pass "/etc/hosts sends api.anthropic.com to this Mac (transparent mode)"
      else fail "transparent mode, but /etc/hosts does not send api.anthropic.com to this Mac" "run: burst-repair"; fi
      code="$(http_code https://api.anthropic.com/healthz)"
      if [ "$code" = 200 ]; then pass "the gateway answers as api.anthropic.com"
      else fail "the gateway does not answer as api.anthropic.com (HTTP ${code:-000})" "run: burst-repair; burst-off takes Burst out of the path meanwhile"; fi
    else
      code="$(http_code "http://$LISTEN/healthz")"
      if [ "$code" = 200 ]; then pass "the gateway answers on $LISTEN"
      else fail "the gateway does not answer on $LISTEN (HTTP ${code:-000})" "run: burst-repair; burst-off takes Burst out of the path meanwhile"; fi
      if grep -qE "\"ANTHROPIC_BASE_URL\" *: *\"https?://$LISTEN" "$HOME/.claude/settings.json" 2>/dev/null; then pass "Claude Code is pointed at the gateway (settings.json)"
      else fail "Claude Code is not pointed at the gateway: settings.json has no ANTHROPIC_BASE_URL for $LISTEN" "run: claude-burst enable"; fi
    fi

    code="$(http_code "http://$ADMIN/")"
    if [ "$code" = 200 ]; then pass "the dashboard answers on $ADMIN"
    else fail "the dashboard does not answer on $ADMIN (HTTP ${code:-000})" "run: burst-repair"; fi

    if launchctl print "gui/$UID/$LABEL-selfheal" >/dev/null 2>&1; then pass "the self-heal watchdog is loaded"
    else fail "the self-heal watchdog is not loaded: a gateway that stops will stay stopped" "run: scripts/install-selfheal-watchdog.sh"; fi

    if [ -f "$CFG/self-heal-crashloop" ]; then fail "the gateway keeps restarting (the watchdog reported a crash loop)" "see the end of $CFG/launchd.err.log; run: burst-repair"
    else pass "no crash loop reported"; fi
  fi

  if [ "$CONSOLE" = off ]; then skip "the support console is off (console_listen)"
  else
    code="$(http_code "http://$CONSOLE/")"
    if [ "$code" = 200 ]; then pass "the support console answers on $CONSOLE"
    else fail "the support console does not answer on $CONSOLE (HTTP ${code:-000})" "run: scripts/install-console.sh"; fi
  fi

  # The network, then Anthropic through it. In transparent mode this Mac's
  # api.anthropic.com is the gateway, so Anthropic itself is asked about by
  # the gateway: /api/test-connection is its own answer.
  code="$(http_code https://captive.apple.com/hotspot-detect.html)"
  if [ "$code" = 200 ]; then pass "this Mac has a network"
  else fail "this Mac has no working network (captive.apple.com: HTTP ${code:-000})" "nothing in Burst fixes this: check Wi-Fi, VPN and proxy"; fi
  if [ "$MODE" = transparent ] && [ -z "$OFF" ]; then
    if curl -s -m 10 "http://$ADMIN/api/test-connection" 2>/dev/null | grep -q '"ok": *true'; then pass "the gateway reaches Anthropic"
    else fail "the gateway's own connection test fails" "open http://$ADMIN/ and read the Checks; run: burst-repair"; fi
  else
    code="$(http_code https://api.anthropic.com/v1/messages 8)"
    case "$code" in
      ""|000) fail "Anthropic cannot be reached from this Mac" "check the network, DNS and any proxy: env | grep -i proxy" ;;
      *) pass "Anthropic is reachable (HTTP $code with no credentials)" ;;
    esac
  fi

  # The secondary's address, with no key sent: any HTTP answer is a path.
  v="$(cfg secondary.base_url "")"
  if [ -z "$v" ]; then skip "no secondary with an address is configured"
  else
    code="$(http_code "$v" 8)"
    case "$code" in
      ""|000) fail "the secondary cannot be reached ($v)" "check the address in the dashboard's Secondary section" ;;
      *) pass "the secondary is reachable ($v)" ;;
    esac
  fi

  v="$(cfg codex.listen 127.0.0.1:7779)"
  if [ "$v" = off ] || [ -n "$OFF" ]; then skip "the Codex gateway is off"
  elif ! grep -q '^# BEGIN claude-burst' "${CODEX_HOME:-$HOME/.codex}/config.toml" 2>/dev/null; then skip "Codex is not routed through Burst"
  else
    code="$(http_code "http://$v/backend-api/codex/models" 10)"
    case "$code" in
      ""|000) fail "Codex is routed through Burst and its port $v does not answer" "run: burst-repair; claude-burst codex disable sends Codex straight to ChatGPT" ;;
      *) pass "the Codex gateway forwards (HTTP $code with no login)" ;;
    esac
  fi
}

run_checks
SUMMARY="$TOTAL checks, $FAILED failed"
[ "$SKIPPED" -gt 0 ] && SUMMARY="$SUMMARY, $SKIPPED skipped"

if [ "${1:-}" = "--check" ]; then
  printf '%s%s\n' "$CHECKS" "$SUMMARY"
  [ "$FAILED" -eq 0 ]; exit $?
fi

{
section "Checks: $SUMMARY"
printf '%s' "$CHECKS"

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
run "lsof 7777 7788 17777 7779 443" bash -c 'lsof -nP -iTCP:7777 -iTCP:7788 -iTCP:17777 -iTCP:7779 -iTCP:443 -sTCP:LISTEN'

section "Gateway health"
# The ports config.json names; the defaults when it names none.
echo "gateway listens on $LISTEN, dashboard on $ADMIN (from config.json)"
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

section "Codex"
CODEX_TOML="${CODEX_HOME:-$HOME/.codex}/config.toml"
run "claude-burst codex status" "$HOME/.local/bin/claude-burst" codex status
run "Burst's block in $CODEX_TOML" bash -c "sed -n '/^# BEGIN claude-burst/,/^# END claude-burst/p' '$CODEX_TOML'"
run "top-level model_provider lines" bash -c "grep -n '^[[:space:]]*model_provider[[:space:]]*=' '$CODEX_TOML'"
run "Codex gateway port 7779" bash -c 'lsof -nP -iTCP:7779 -sTCP:LISTEN'
# No credential is sent, so ChatGPT answers 401: any HTTP status proves the
# gateway forwards; 000 means nothing answered the port.
run "Codex gateway forwards (expect 401 without a login)" bash -c 'curl -s -o /dev/null -w "HTTP %{http_code}, total %{time_total}s\n" -m 10 http://127.0.0.1:7779/backend-api/codex/models'
run "Codex processes" bash -c 'ps -axo pid,lstart,command | grep -E "MacOS/[c]odex |bin/[c]odex( |$)" | cut -c1-160 | head -8'
if [ -f "$CFG/codex-metrics.jsonl" ]; then
  printf '\n--- codex-metrics.jsonl (last 8 turns, metadata only) ---\n'
  tail -8 "$CFG/codex-metrics.jsonl" | redact
fi

section "Power"
run "pmset SleepDisabled" bash -c 'pmset -g | grep -i -E "sleepdisabled|^ sleep"'

section "Logs (newest last)"
if [ -f "$CFG/claude-burst.log" ]; then
  printf '\n--- claude-burst.log, Codex lines (last 20) ---\n'
  grep 'codex:' "$CFG/claude-burst.log" | tail -20 | redact
fi
for f in claude-burst.log launchd.err.log launchd.out.log watchdog.log self-heal.log; do
  if [ -f "$CFG/$f" ]; then
    printf '\n--- %s (last 60 lines) ---\n' "$f"
    tail -60 "$CFG/$f" | redact
  fi
done
for f in /var/log/claude-burst-pf.log /var/log/claude-burst-lidawake.log; do
  [ -r "$f" ] && { printf '\n--- %s (last 15) ---\n' "$f"; tail -15 "$f" | redact; }
done
[ -f "$CFG/notices.json" ] && { printf '\n--- notices.json (last 10 events) ---\n'; /usr/bin/python3 -c '
import json,sys
d=json.load(open(sys.argv[1])); ev=d.get("events",d) if isinstance(d,dict) else d
for e in ev[-10:]: print(e.get("at","")[:19], e.get("severity"), e.get("title"), "|", e.get("detail","")[:160])' "$CFG/notices.json" 2>&1; }

section "End"
} > "$OUT" 2>&1

pbcopy < "$OUT" 2>/dev/null && copied=" and copied to the clipboard" || copied=""
printf '%s%s\n\n' "$CHECKS" "$SUMMARY"
echo "Report written to $OUT$copied ($(wc -l < "$OUT" | tr -d ' ') lines)."
echo "It needs no password and changed nothing. Secrets are redacted; skim it before sharing."
[ "$FAILED" -eq 0 ]
