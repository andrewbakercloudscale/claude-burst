#!/bin/zsh
# Root-side watchdog for the ONE piece of transparent mode that nothing else
# can guard: the pf rdr rule.
#
# WHY THIS EXISTS: on 2026-09-07 the pf anchor's rdr rule (127.0.0.1:443 ->
# the gateway port) disappeared while the /etc/hosts redirect stayed. That
# combination is the widest-blast-radius state this tool can produce -- every
# process on the Mac gets "connection refused" for api.anthropic.com, not just
# Claude Code. It went unnoticed for hours because:
#
#   - the dashboard's ACTIVE badge read the hosts entry and the CA bundle,
#     neither of which had changed, and never the pf rule;
#   - self-heal-watchdog.sh DID detect it, three times, and could only post a
#     notification -- it runs as a user LaunchAgent and reloading a pf anchor
#     needs root.
#
# We do not control pf. This Mac also runs Zscaler and CrowdStrike, both of
# which own pf anchors and reload the ruleset on network change and on wake;
# a `load anchor` line in /etc/pf.conf does not guarantee the anchor's rules
# stay loaded. So the rule has to be treated as something that WILL vanish
# again, and the recovery has to be unattended.
#
# What this does, every couple of minutes, as root:
#
#   healthy      -> nothing, silently (a log line per cycle would bury the
#                   events this exists to record)
#   not installed-> nothing (no hosts block = nobody is being redirected here;
#                   a rollback must not be undone by a watchdog)
#   path broken  -> log it, repair whichever piece is actually missing, log
#                   the outcome, notify the console user
#   still broken -> after $MAX_FAILURES consecutive failed cycles, REMOVE the
#                   redirect entirely and say so, loudly
#
# WHAT "BROKEN" MEANS, AND WHY IT CHANGED. The first version of this triggered
# on one thing: `pfctl -a claude-burst -s nat` no longer listing an rdr rule.
# On 2026-09-08 the Mac was black-holed for four minutes with this daemon armed,
# beating, and reporting "nothing to report" -- because the pf rule was loaded
# and correct. The GATEWAY had died. hosts -> 127.0.0.1:443 -> rdr -> :7777 ->
# nothing listening -> connection refused, machine-wide, for exactly the same
# user-visible outcome the pf rule going missing produces.
#
# So the trigger is now the outcome, not a component: does the intercepted host
# actually answer, and answer from OUR gateway? That is the same probe the
# dashboard's Test connection button and health-diagnostics.sh already use.
# Only once the answer is no does this ask WHICH piece is missing, and repair
# that one. Guarding a named cause meant being blind to every other cause of
# the identical outage -- the same mistake as a dashboard reporting ACTIVE
# because the config on disk looked right.
#
# That last step is the point of the whole script. A healer that cannot heal
# must fall back to the safe state rather than loop forever: with the redirect
# gone, Claude Code and every other app reach Anthropic directly and the worst
# outcome is that burst is not in the path. Staying black-holed is strictly
# worse than being uninstalled, and "the recovery path must work in states
# nobody predicted" is already why transparent-root.sh's `remove` was written
# before its `install`.
#
# Usage:
#   sudo pf-heal.sh            one cycle (this is what the LaunchDaemon runs)
#   sudo pf-heal.sh --check    report only; never repairs, never removes
#        pf-heal.sh --self-test  no root; drives every branch against fakes
#
# Installed and scheduled by scripts/install-pf-heal.sh. Do NOT point a
# LaunchDaemon at a copy inside a user-writable directory: a root job running
# a script anyone can rewrite is a root shell for anyone who can rewrite it.
# install-pf-heal.sh copies this to a root-owned /usr/local/libexec for
# exactly that reason.
set -uo pipefail

SELF="$0"
DIR="$(cd "$(dirname "$0")" && pwd)"

HOSTS_FILE="${CLAUDE_BURST_HOSTS_FILE:-/etc/hosts}"
STATE_DIR="${CLAUDE_BURST_ROOT_STATE_DIR:-/etc/claude-burst}"
STATE_FILE="$STATE_DIR/transparent.state"
ANCHOR_NAME="claude-burst"
GATEWAY_LABEL="ninja.andrewbaker.claude-burst"
HOSTS_MARKER="# BEGIN claude-burst hosts"
INTERCEPT_HOST="${CLAUDE_BURST_INTERCEPT_HOST:-api.anthropic.com}"
# Absolute-ish, and indirected for --self-test. See INSTALL_BIN in
# install-pf-heal.sh for why a bare command name in a script that also defines
# functions is a trap worth not repeating.
CURL="${CLAUDE_BURST_CURL:-/usr/bin/curl}"
LSOF="${CLAUDE_BURST_LSOF:-/usr/sbin/lsof}"
ROOT_HELPER="${CLAUDE_BURST_ROOT_HELPER:-$DIR/transparent-root.sh}"
# Indirected so --self-test can substitute a stub. Every branch below turns on
# what pfctl reports, and a decision tree that can only be exercised on a
# machine whose pf is already broken is a decision tree nobody ever exercises.
PFCTL="${CLAUDE_BURST_PFCTL:-pfctl}"

# World-readable on purpose: the question "did this break overnight, and did
# it fix itself?" should be answerable with `tail`, not with sudo.
LOG="${CLAUDE_BURST_PF_HEAL_LOG:-/var/log/claude-burst-pf.log}"
FAIL_COUNT_FILE="$STATE_DIR/pf-heal.failures"
# Proof of life for readers who are not root. `launchctl print system/<label>`
# is denied to an unprivileged process (exit 113 -- verified), so the gateway
# and the dashboard cannot ask launchd whether this daemon is running, and the
# LOG cannot answer either: a healthy cycle deliberately writes nothing, so an
# untouched log is indistinguishable from a daemon that was never loaded.
# Without this file the dashboard could only report that a plist exists, which
# is precisely the read-the-configuration mistake that let the original outage
# run for hours. World-readable, one timestamp, rewritten every cycle.
HEARTBEAT_FILE="$STATE_DIR/pf-heal.heartbeat"
LOG_MAX_BYTES=1048576

# Consecutive failed repair cycles before giving up and removing the redirect.
# At the LaunchDaemon's 120s interval that is ~8 minutes of a broken Mac,
# which is long enough to ride out a gateway restart or a wake-from-sleep
# race, and short enough that nobody sits through it twice.
MAX_FAILURES="${CLAUDE_BURST_PF_HEAL_MAX_FAILURES:-4}"

CHECK_ONLY=0
case "${1:-}" in
  --check)     CHECK_ONLY=1 ;;
  --self-test) ;;   # handled at the bottom, after every function is defined
  "")          ;;
  *)           echo "usage: sudo $SELF [--check] | $SELF --self-test" >&2; exit 2 ;;
esac

log() {
  local line="$(date '+%Y-%m-%d %H:%M:%S') $*"
  echo "$line"
  # Best-effort: a full disk must not turn a repair into an exit.
  echo "$line" >> "$LOG" 2>/dev/null || true
}

rotate_log() {
  [[ -f "$LOG" ]] || return 0
  local size
  size=$(stat -f %z "$LOG" 2>/dev/null || echo 0)
  if (( size > LOG_MAX_BYTES )); then
    mv -f "$LOG" "$LOG.1" 2>/dev/null || true
    : > "$LOG" 2>/dev/null || true
    chmod 644 "$LOG" 2>/dev/null || true
  fi
}

# Posts to the console user's GUI session. A root LaunchDaemon has no session
# of its own, so osascript must be re-entered as that user -- without this the
# notification silently goes nowhere, which is the failure shape this whole
# file exists to stop repeating.
notify() {
  local msg="$1" uid
  uid=$(stat -f %u /dev/console 2>/dev/null) || return 0
  [[ -n "$uid" && "$uid" != "0" ]] || return 0
  launchctl asuser "$uid" /usr/bin/osascript \
    -e 'on run argv' \
    -e 'display notification (item 1 of argv) with title "claude-burst"' \
    -e 'end run' "$msg" >/dev/null 2>&1 || true
}

hosts_redirect_present() { grep -qF "$HOSTS_MARKER" "$HOSTS_FILE" 2>/dev/null; }

# The real path, end to end, exactly as Claude Code travels it: resolve the
# intercepted host (which /etc/hosts sends here), complete TLS against our
# local CA, and confirm the body is OURS. The gateway stamps "overflow" into
# its own /healthz precisely so this can tell "reached the gateway" from
# "reached the real Anthropic", which returns 404 to an unauthenticated
# /healthz and would otherwise look like a success.
intercept_path_healthy() {
  local body
  body="$($CURL -s -m 8 "https://$INTERCEPT_HOST/healthz" 2>/dev/null)" || return 1
  printf '%s' "$body" | grep -q '"overflow"'
}

# Is anything listening on the gateway port at all? Distinguishes "pf is not
# redirecting" from "pf is redirecting into a hole", which need different
# repairs. Deliberately does NOT probe over the redirect: this is the one
# question about the gateway itself.
gateway_listening() { $LSOF -nP -iTCP:"$(gateway_port)" -sTCP:LISTEN >/dev/null 2>&1; }

gateway_port() {
  local g; g="$(grep -E "^gateway_port=" "$STATE_FILE" 2>/dev/null | tail -1 | cut -d= -f2-)"
  printf '%s' "${g:-17777}"
}

# Restarting the gateway is a USER LaunchAgent operation and this runs as root,
# so it has to be re-entered as the console user -- `launchctl kickstart` from
# root against gui/<uid> is refused. Best-effort by design: if there is no
# console user (nobody logged in) there is nothing to restart into, and the
# bail-out below is the right answer anyway.
restart_gateway() {
  local uid
  uid=$(stat -f %u /dev/console 2>/dev/null) || return 1
  [[ -n "$uid" && "$uid" != "0" ]] || return 1
  launchctl asuser "$uid" launchctl enable "gui/$uid/$GATEWAY_LABEL" >/dev/null 2>&1
  launchctl asuser "$uid" launchctl kickstart -k "gui/$uid/$GATEWAY_LABEL" >/dev/null 2>&1 && return 0
  # kickstart fails outright when the job is not merely stopped but unloaded
  # from launchd's database -- the 2026-09-04 critical-battery shape. Bootstrap
  # it back before giving up.
  local plist
  plist="$(dscl . -read "/Users/$(id -un "$uid")" NFSHomeDirectory 2>/dev/null | awk '{print $2}')/Library/LaunchAgents/$GATEWAY_LABEL.plist"
  [[ -f "$plist" ]] || return 1
  launchctl asuser "$uid" launchctl bootstrap "gui/$uid" "$plist" >/dev/null 2>&1
}

# The live rule, not the anchor FILE and not the pf.conf reference. Both of
# those survived the 2026-09-07 outage intact; only the loaded ruleset lost
# the rule, which is why every check that looked at configuration reported OK.
# TWO separate facts, and only both together mean traffic is redirected.
#
# `pfctl -a claude-burst -s nat` lists the rules INSIDE our anchor. It says
# nothing about whether the main ruleset still routes anything into that
# anchor -- and the main ruleset is the half other pf-owning software rewrites
# when it reloads /etc/pf.conf from its own copy. An anchor full of correct
# rules that nothing references is inert, and reports itself as perfectly
# loaded. Checking only the first of these is why the guard sat silent through
# a live outage on 2026-09-08.
#
# The grep is anchored to the start of the rule, too: our anchor also contains
# a `no rdr` line (see write_anchor in transparent-root.sh), and a bare
# `grep rdr` matches that one -- so the redirect could be gone entirely while
# its exemption alone kept the check green.
rdr_rule_loaded() {
  "$PFCTL" -a "$ANCHOR_NAME" -s nat 2>/dev/null | grep -qE '^[[:space:]]*rdr[[:space:]]'
}

anchor_referenced() {
  "$PFCTL" -s nat 2>/dev/null | grep -q "rdr-anchor \"$ANCHOR_NAME\""
}

failures() { cat "$FAIL_COUNT_FILE" 2>/dev/null || echo 0; }
set_failures() {
  mkdir -p "$STATE_DIR" 2>/dev/null
  echo "$1" > "$FAIL_COUNT_FILE" 2>/dev/null || true
}
clear_failures() { rm -f "$FAIL_COUNT_FILE" 2>/dev/null || true; }

# Written FIRST in every cycle, before any decision. A heartbeat that only
# appeared on the paths that did something would go stale exactly when the
# daemon is working perfectly, and read as dead.
beat() {
  mkdir -p "$STATE_DIR" 2>/dev/null && chmod 755 "$STATE_DIR" 2>/dev/null
  date +%s > "$HEARTBEAT_FILE" 2>/dev/null || return 0
  chmod 644 "$HEARTBEAT_FILE" 2>/dev/null || true
}

# --- one cycle ---------------------------------------------------------------
# The whole cycle is one function so --self-test can run it repeatedly against
# fakes. It returns the status the daemon exits with.
cycle() {
rotate_log
beat

# --- 1. Is transparent mode even supposed to be installed? -------------------
# The hosts block is the authority, not the state file: `remove` deletes both,
# but a rollback that got halfway is exactly the situation here, and the hosts
# block is the half that does the damage. No block, nothing to guard.
if ! hosts_redirect_present; then
  clear_failures
  (( CHECK_ONLY )) && log "check: no /etc/hosts redirect installed -- nothing to guard"
  return 0
fi

# --- 2. Does the real path work? ----------------------------------------------
# The outcome, not a component. Everything below only runs when a request to
# the intercepted host does NOT come back from this gateway -- which is the
# single condition under which this Mac is broken, whatever the cause.
if intercept_path_healthy; then
  prev=$(failures)
  if (( prev > 0 )); then
    log "recovered: $INTERCEPT_HOST answers from the gateway again after $prev failed cycle(s)"
  fi
  clear_failures
  (( CHECK_ONLY )) && log "check: OK -- $INTERCEPT_HOST resolves to this gateway and it answered"
  return 0
fi

# --- 3. The dangerous state. --------------------------------------------------
gport="$(gateway_port)"
if rdr_rule_loaded;   then rdr=loaded;     else rdr=MISSING;        fi
if anchor_referenced; then ref=referenced; else ref=NOT-REFERENCED; fi
if gateway_listening; then gw=listening;   else gw=DOWN;           fi
log "BROKEN: /etc/hosts redirects $INTERCEPT_HOST here but it does not answer from this gateway -- every process on this Mac is affected (rdr rule: $rdr, main ruleset: $ref, gateway on :$gport: $gw)"

if (( CHECK_ONLY )); then
  log "check: --check given, so nothing was repaired."
  return 1
fi

if [[ ! -x "$ROOT_HELPER" ]]; then
  log "FATAL: cannot repair -- no executable helper at $ROOT_HELPER"
  notify "claude-burst: the intercept is broken and the repair helper is missing. Run: sudo transparent-root.sh remove"
  return 1
fi

# --- 4. Repair whichever piece is actually missing. ---------------------------
# Both are attempted when both are wrong, cheapest first, and the verdict comes
# from re-probing the real path rather than from either repair's exit code -- a
# repair that "succeeded" while the path stayed broken is not a repair.
if [[ "$gw" == "DOWN" ]]; then
  log "repairing: gateway is not listening on :$gport -- restarting its LaunchAgent"
  if restart_gateway; then
    # Binding is not instant, and reporting failure during the second it takes
    # would burn a bail-out budget on a gateway that was coming back fine.
    for i in 1 2 3 4 5 6 7 8 9 10; do
      gateway_listening && break
      sleep 1
    done
    gateway_listening && log "  gateway is listening again" || log "  gateway still not listening"
  else
    log "  could not restart the gateway LaunchAgent (no console user, or no plist)"
  fi
fi

# Unconditional, not "only when the rule looks missing". reload-anchor rewrites
# the anchor AND reloads /etc/pf.conf, so it repairs both halves -- and the half
# that looks fine to rdr_rule_loaded is exactly the one that was broken on
# 2026-09-08. It dry-runs the whole ruleset first and restores the previous
# anchor if the result is worse, so running it when pf was already correct
# costs a second and changes nothing.
log "repairing: reloading the pf anchor and main ruleset -- $ROOT_HELPER reload-anchor"
reload_out="$("$ROOT_HELPER" reload-anchor 2>&1)"
printf '%s\n' "$reload_out" | sed 's/^/    /' >> "$LOG" 2>/dev/null || true

if intercept_path_healthy; then
  log "HEALED: $INTERCEPT_HOST answers from this gateway again"
  clear_failures
  notify "Transparent proxy self-healed: the intercept was broken and has been repaired."
  return 0
fi

# --- 5. Repair failed. Count, and eventually bail out. -----------------------
n=$(( $(failures) + 1 ))
set_failures "$n"
log "repair FAILED, consecutive failures: $n/$MAX_FAILURES"

if (( n < MAX_FAILURES )); then
  notify "claude-burst: the intercept is broken; repair attempt $n of $MAX_FAILURES did not fix it. Retrying."
  return 1
fi

# Giving up is a decision, and it is the right one: with the redirect removed,
# every app on this Mac talks to Anthropic directly again. Burst is out of the
# path until someone reinstalls it, which is a far better resting state than a
# machine that cannot reach Anthropic at all.
log "GIVING UP after $n failed repairs -- removing the redirect so this Mac can reach Anthropic directly again"
remove_out="$("$ROOT_HELPER" remove 2>&1)"
remove_rc=$?
printf '%s\n' "$remove_out" | sed 's/^/    /' >> "$LOG" 2>/dev/null || true

if (( remove_rc == 0 )); then
  log "BAILED OUT: transparent mode removed. Claude Code now talks to Anthropic directly. Reinstall with: sudo $ROOT_HELPER install"
  clear_failures
  notify "claude-burst could not repair the intercept, so it removed it. Claude works normally again; burst is no longer in the path."
  return 0
fi

log "FATAL: bail-out itself failed (exit $remove_rc). This Mac may still be unable to reach $INTERCEPT_HOST. Run by hand: sudo $ROOT_HELPER remove"
notify "claude-burst: could not repair OR remove the redirect. Run: sudo transparent-root.sh remove"
return 1
}

# --- entrypoint --------------------------------------------------------------

# self_test drives every branch of cycle() against a fake /etc/hosts, a fake
# pfctl and a fake transparent-root.sh, with no root and no real pf. It exists
# because the alternative is discovering that the healer's decision tree is
# wrong at the moment it is asked to heal -- and every branch below runs at
# most once every few weeks, on a machine that is already broken.
self_test() {
  local tmp fails=0
  tmp="$(mktemp -d)"
  trap "rm -rf '$tmp'" EXIT

  HOSTS_FILE="$tmp/hosts"
  STATE_DIR="$tmp/state"
  FAIL_COUNT_FILE="$STATE_DIR/pf-heal.failures"
  HEARTBEAT_FILE="$STATE_DIR/pf-heal.heartbeat"
  LOG="$tmp/pf.log"
  PFCTL="$tmp/pfctl"
  CURL="$tmp/curl"
  LSOF="$tmp/lsof"
  ROOT_HELPER="$tmp/helper.sh"
  MAX_FAILURES=2
  CHECK_ONLY=0
  notify() { :; }
  # Restarting a LaunchAgent needs a console user and a real launchd; the stub
  # records the attempt and flips the gateway flag, which is what the decision
  # tree actually turns on.
  restart_gateway() { echo restart >> "$tmp/restarts"; [[ -f "$tmp/gw-wont-start" ]] && return 1; touch "$tmp/gw"; return 0; }

  mkdir -p "$STATE_DIR"

  # Three flag files stand in for the three things that can be true or not:
  #   $tmp/rdr  -- the pf rdr rule is loaded
  #   $tmp/gw   -- something is listening on the gateway port
  #   both      -- the real path answers from our gateway
  # Wiring the path probe to BOTH is the whole point: the outage this missed
  # had rdr present and gw absent.
  # Models both pf facts independently: $tmp/rdr is a rule inside the anchor,
  # $tmp/ref is the main ruleset routing traffic into it. The "no rdr" line is
  # always emitted, because a check that matches it would pass with the real
  # redirect gone. The path works only when rdr AND ref AND gw all hold.
  cat > "$PFCTL" <<'STUB'
#!/bin/zsh
d="$(dirname "$0")"
if [[ "$1" == "-a" ]]; then
  echo "no rdr on lo0 inet proto tcp from any to 127.0.0.1 port 7777"
  [[ -f "$d/rdr" ]] && echo "rdr pass on lo0 inet proto tcp from any to 127.0.0.1 port 443 -> 127.0.0.1 port 7777"
else
  [[ -f "$d/ref" ]] && echo 'rdr-anchor "claude-burst" all'
fi
exit 0
STUB
  printf '#!/bin/zsh\nd="$(dirname "$0")"\n[[ -f "$d/rdr" && -f "$d/ref" && -f "$d/gw" ]] && { echo "{\\"overflow\\":false}"; exit 0; }\nexit 7\n' > "$CURL"
  printf '#!/bin/zsh\n[[ -f "$(dirname "$0")/gw" ]] && exit 0\nexit 1\n' > "$LSOF"
  cat > "$ROOT_HELPER" <<'STUB'
#!/bin/zsh
d="$(dirname "$0")"
case "$1" in
  reload-anchor)
    [[ -f "$d/anchor-wont-load" ]] && { echo "pf rejected the ruleset" >&2; exit 1; }
    touch "$d/rdr" "$d/ref"; echo "reloaded"; exit 0 ;;
  remove) rm -f "$d/hosts" "$d/rdr" "$d/ref"; echo "removed"; exit 0 ;;
esac
exit 2
STUB
  chmod 755 "$PFCTL" "$CURL" "$LSOF" "$ROOT_HELPER"

  run()  { out="$(cycle 2>&1)" && rc=0 || rc=$?; }
  ok()   { echo "  ok   $1"; }
  bad()  { echo "  FAIL $1"; fails=$((fails + 1)); if [[ -n "${2:-}" ]]; then printf '%s\n' "$out" | sed 's/^/      /'; fi }
  want() { printf '%s' "$out" | grep -q "$1"; }

  echo "== pf-heal self-test =="

  # 0. Heartbeat on every cycle, including no-op ones.
  rm -f "$HOSTS_FILE" "$tmp/rdr" "$tmp/gw" "$HEARTBEAT_FILE"
  run
  [[ -s "$HEARTBEAT_FILE" ]] && ok "heartbeat written on a no-op cycle" || bad "no heartbeat after a do-nothing cycle"

  # 1. Nothing installed -> never act. Keeps a rollback rolled back.
  run
  (( rc == 0 )) && [[ -z "$out" ]] && ok "no hosts block -> silent no-op" || bad "expected silence with no hosts block" show

  # 2. Fully healthy -> silent.
  echo "$HOSTS_MARKER" > "$HOSTS_FILE"; touch "$tmp/rdr" "$tmp/ref" "$tmp/gw"
  run
  (( rc == 0 )) && [[ -z "$out" ]] && ok "path healthy -> silent no-op" || bad "expected silence when healthy" show

  # 3. THE 2026-09-08 OUTAGE: pf rule perfectly loaded, gateway dead. The old
  #    version reported "nothing to report" here while the Mac was black-holed.
  rm -f "$tmp/gw"; rm -f "$tmp/restarts"
  run
  want "BROKEN" && ok "gateway down with rdr loaded -> detected" || bad "MISSED the outage this was rewritten for" show
  want "gateway on :7777: DOWN" && ok "diagnosis names the gateway, not pf" || bad "diagnosis blamed the wrong component" show
  [[ -f "$tmp/restarts" ]] && ok "restarted the gateway rather than reloading pf" || bad "never attempted a gateway restart"
  want "HEALED" && ok "restart healed it" || bad "did not heal after the gateway came back" show

  # 3b. THE OTHER HALF, and the one that reported itself healthy: the anchor
  #     still holds a correct rdr rule, but the MAIN ruleset no longer routes
  #     anything into it. `pfctl -a claude-burst -s nat` looks perfect.
  echo "$HOSTS_MARKER" > "$HOSTS_FILE"; touch "$tmp/rdr" "$tmp/gw"; rm -f "$tmp/ref"
  run
  want "BROKEN" && ok "anchor referenced by nothing -> detected" || bad "MISSED an unreferenced anchor" show
  want "main ruleset: NOT-REFERENCED" && ok "diagnosis names the main ruleset" || bad "diagnosis did not name the main ruleset" show
  want "HEALED" && ok "reload-anchor restored the reference" || bad "did not heal an unreferenced anchor" show

  # 3c. A `no rdr` line alone must not read as a working redirect.
  rm -f "$tmp/rdr"; touch "$tmp/ref" "$tmp/gw"
  run
  want "rdr rule: MISSING" && ok "'no rdr' line alone does not count as loaded" || bad "counted the no-rdr exemption as the redirect" show

  # 4. The original failure mode still works: rule gone, gateway fine.
  rm -f "$tmp/rdr"; rm -f "$tmp/restarts"
  run
  want "rdr rule: MISSING" && ok "rdr missing -> detected and named" || bad "did not name the missing rdr rule" show
  want "HEALED" && ok "reload-anchor healed it" || bad "reload-anchor did not heal" show

  # 5. Both broken at once -> both repaired.
  rm -f "$tmp/rdr" "$tmp/ref" "$tmp/gw" "$tmp/restarts"
  run
  want "HEALED" && ok "both pieces broken -> both repaired" || bad "did not repair both" show

  # 6. Unrepairable -> count, then bail out by removing the redirect.
  rm -f "$tmp/rdr" "$tmp/ref" "$tmp/gw" "$FAIL_COUNT_FILE"
  touch "$tmp/anchor-wont-load" "$tmp/gw-wont-start"
  run; want "1/2" && ok "first failure -> retry" || bad "did not count the first failure" show
  run; want "BAILED OUT" && ok "second failure -> bailed out" || bad "did not bail out at the limit" show
  grep -qF "$HOSTS_MARKER" "$HOSTS_FILE" 2>/dev/null \
    && bad "bail-out left the hosts redirect in place" || ok "bail-out removed the hosts redirect"

  # 7. Recovery after failures is announced, so the log shows the whole arc.
  rm -f "$tmp/anchor-wont-load" "$tmp/gw-wont-start"
  echo "$HOSTS_MARKER" > "$HOSTS_FILE"; touch "$tmp/rdr" "$tmp/ref" "$tmp/gw"; echo 3 > "$FAIL_COUNT_FILE"
  run
  want "recovered" && ok "recovery after failures is logged" || bad "recovery was silent" show

  # 8. --check never repairs, whatever it finds.
  rm -f "$tmp/gw"; CHECK_ONLY=1; rm -f "$tmp/restarts"
  run
  want "BROKEN" && ok "--check reports the outage" || bad "--check did not report" show
  [[ -f "$tmp/restarts" ]] && bad "--check restarted the gateway" || ok "--check changed nothing"

  echo
  if (( fails == 0 )); then echo "self-test: all checks passed"; return 0; fi
  echo "self-test: $fails FAILED" >&2
  return 1
}

if [[ "${1:-}" == "--self-test" ]]; then
  self_test
  exit $?
fi

if [[ $EUID -ne 0 ]]; then
  echo "pf-heal must run as root (it reloads a pf anchor): sudo $SELF ${1:-}" >&2
  exit 1
fi

cycle
exit $?
