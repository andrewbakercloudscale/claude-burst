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
#   rule missing -> log it, run `transparent-root.sh reload-anchor`, log the
#                   outcome, notify the console user
#   still broken -> after $MAX_FAILURES consecutive failed cycles, REMOVE the
#                   redirect entirely and say so, loudly
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
HOSTS_MARKER="# BEGIN claude-burst hosts"
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

# The live rule, not the anchor FILE and not the pf.conf reference. Both of
# those survived the 2026-09-07 outage intact; only the loaded ruleset lost
# the rule, which is why every check that looked at configuration reported OK.
rdr_rule_loaded() { "$PFCTL" -a "$ANCHOR_NAME" -s nat 2>/dev/null | grep -q 'rdr'; }

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

# --- 2. Is the rule actually loaded? -----------------------------------------
if rdr_rule_loaded; then
  prev=$(failures)
  if (( prev > 0 )); then
    log "recovered: rdr rule is loaded again after $prev failed cycle(s)"
  fi
  clear_failures
  (( CHECK_ONLY )) && log "check: OK -- rdr rule loaded, hosts redirect present"
  return 0
fi

# --- 3. The dangerous state. ------------------------------------------------
log "BROKEN: /etc/hosts still redirects to this gateway but the pf rdr rule is NOT loaded -- every process on this Mac is being refused for the intercepted host"

if (( CHECK_ONLY )); then
  log "check: --check given, so nothing was repaired. Repair with: sudo $ROOT_HELPER reload-anchor"
  return 1
fi

if [[ ! -x "$ROOT_HELPER" ]]; then
  log "FATAL: cannot repair -- no executable helper at $ROOT_HELPER"
  notify "claude-burst: pf rule lost and the repair helper is missing. Run: sudo transparent-root.sh remove"
  return 1
fi

# --- 4. Repair. --------------------------------------------------------------
# reload-anchor is the existing, tested primitive: it rewrites the anchor from
# $STATE_FILE, dry-runs the WHOLE ruleset before loading anything, flushes only
# this anchor's states, verifies the real traffic path, and puts the previous
# anchor back if the reload made things worse. Reimplementing any of that here
# is how the two copies drift and the wrong one runs.
log "repairing: $ROOT_HELPER reload-anchor"
reload_out="$("$ROOT_HELPER" reload-anchor 2>&1)"
reload_rc=$?
printf '%s\n' "$reload_out" | sed 's/^/    /' >> "$LOG" 2>/dev/null || true

if (( reload_rc == 0 )) && rdr_rule_loaded; then
  log "HEALED: rdr rule reloaded successfully"
  clear_failures
  notify "Transparent proxy self-healed: the pf redirect had been lost and was reloaded."
  return 0
fi

# --- 5. Repair failed. Count, and eventually bail out. -----------------------
n=$(( $(failures) + 1 ))
set_failures "$n"
log "repair FAILED (exit $reload_rc), consecutive failures: $n/$MAX_FAILURES"

if (( n < MAX_FAILURES )); then
  notify "claude-burst: pf redirect lost; repair attempt $n of $MAX_FAILURES failed. Retrying."
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
  notify "claude-burst could not repair the pf redirect, so it removed it. Claude works normally again; burst is no longer in the path."
  return 0
fi

log "FATAL: bail-out itself failed (exit $remove_rc). This Mac may still be unable to reach the intercepted host. Run by hand: sudo $ROOT_HELPER remove"
notify "claude-burst: could not repair OR remove the pf redirect. Run: sudo transparent-root.sh remove"
return 1
}

# --- entrypoint --------------------------------------------------------------

# self_test drives every branch of cycle() against a fake /etc/hosts, a fake
# pfctl and a fake transparent-root.sh, with no root and no real pf. It exists
# because the alternative is discovering that the healer's decision tree is
# wrong at the moment it is asked to heal -- and every branch below runs at
# most once every few weeks, on a machine that is already broken.
self_test() {
  local tmp rc out fails=0
  tmp="$(mktemp -d)"
  trap "rm -rf '$tmp'" EXIT

  HOSTS_FILE="$tmp/hosts"
  STATE_DIR="$tmp/state"
  FAIL_COUNT_FILE="$STATE_DIR/pf-heal.failures"
  HEARTBEAT_FILE="$STATE_DIR/pf-heal.heartbeat"
  LOG="$tmp/pf.log"
  PFCTL="$tmp/pfctl"
  ROOT_HELPER="$tmp/helper.sh"
  MAX_FAILURES=2
  CHECK_ONLY=0
  # Silence the GUI: a self-test must not post four notifications.
  notify() { :; }

  mkdir -p "$STATE_DIR"

  # $tmp/rdr controls what the fake pfctl reports; $tmp/helper-rc controls
  # whether the fake repair succeeds, and it flips $tmp/rdr when it does --
  # so "repair worked" and "the rule is now loaded" stay one fact, the way
  # they are in reality.
  cat > "$PFCTL" <<'STUB'
#!/bin/zsh
[[ -f "$(dirname "$0")/rdr" ]] && echo "rdr pass on lo0 inet proto tcp from any to 127.0.0.1 port 443 -> 127.0.0.1 port 7777"
exit 0
STUB
  cat > "$ROOT_HELPER" <<'STUB'
#!/bin/zsh
d="$(dirname "$0")"
case "$1" in
  reload-anchor)
    if [[ -f "$d/helper-rc" && "$(cat "$d/helper-rc")" == "0" ]]; then
      touch "$d/rdr"; echo "reloaded"; exit 0
    fi
    echo "pf rejected the ruleset" >&2; exit 1 ;;
  remove)
    if [[ -f "$d/remove-rc" && "$(cat "$d/remove-rc")" != "0" ]]; then
      echo "remove failed" >&2; exit 1
    fi
    rm -f "$d/hosts" "$d/rdr"; echo "removed"; exit 0 ;;
esac
exit 2
STUB
  chmod 755 "$PFCTL" "$ROOT_HELPER"

  check() {
    local name="$1" want_rc="$2" want_text="$3"
    if [[ "$rc" != "$want_rc" ]]; then
      echo "  FAIL $name: exit $rc, want $want_rc"; fails=$((fails + 1)); return
    fi
    if [[ -n "$want_text" ]] && ! printf '%s' "$out" | grep -q "$want_text"; then
      echo "  FAIL $name: output did not contain '$want_text'"
      printf '%s\n' "$out" | sed 's/^/      /'
      fails=$((fails + 1)); return
    fi
    echo "  ok   $name"
  }

  echo "== pf-heal self-test =="

  # 0. The heartbeat must be written on EVERY cycle, including the ones that
  #    decide to do nothing -- otherwise it goes stale exactly when the daemon
  #    is healthiest, and the dashboard reports a working guard as dead.
  rm -f "$HOSTS_FILE" "$tmp/rdr" "$HEARTBEAT_FILE"
  cycle >/dev/null 2>&1
  if [[ -s "$HEARTBEAT_FILE" ]]; then
    echo "  ok   heartbeat written on a no-op cycle"
  else
    echo "  FAIL: no heartbeat after a cycle that did nothing"; fails=$((fails + 1))
  fi

  # 1. Nothing installed: never touch anything. This is the branch that keeps
  #    a rollback rolled back.
  rm -f "$HOSTS_FILE" "$tmp/rdr"
  out="$(cycle 2>&1)"; rc=$?
  check "no hosts block -> no action" 0 ""
  [[ -z "$out" ]] || { echo "  FAIL: expected silence, got: $out"; fails=$((fails + 1)); }

  # 2. Installed and healthy: silent.
  echo "$HOSTS_MARKER" > "$HOSTS_FILE"
  touch "$tmp/rdr"
  out="$(cycle 2>&1)"; rc=$?
  check "rule loaded -> no action" 0 ""
  [[ -z "$out" ]] || { echo "  FAIL: expected silence, got: $out"; fails=$((fails + 1)); }

  # 3. THE outage: hosts block present, rule gone, repair works.
  rm -f "$tmp/rdr"; echo 0 > "$tmp/helper-rc"
  out="$(cycle 2>&1)"; rc=$?
  check "rule missing, repair works -> HEALED" 0 "HEALED"
  [[ -f "$tmp/rdr" ]] || { echo "  FAIL: repair did not restore the rule"; fails=$((fails + 1)); }

  # 4. Repair keeps failing: count up, then bail out by removing the redirect.
  rm -f "$tmp/rdr"; echo 1 > "$tmp/helper-rc"; rm -f "$FAIL_COUNT_FILE"
  out="$(cycle 2>&1)"; rc=$?
  check "repair fails once -> retry" 1 "1/2"
  out="$(cycle 2>&1)"; rc=$?
  check "repair fails twice -> BAILED OUT" 0 "BAILED OUT"
  grep -qF "$HOSTS_MARKER" "$HOSTS_FILE" 2>/dev/null \
    && { echo "  FAIL: bail-out left the hosts redirect in place"; fails=$((fails + 1)); } \
    || echo "  ok   bail-out removed the hosts redirect"

  # 5. Bail-out itself fails: say so rather than reporting success.
  echo "$HOSTS_MARKER" > "$HOSTS_FILE"; echo 1 > "$tmp/remove-rc"
  echo "$MAX_FAILURES" > "$FAIL_COUNT_FILE"
  out="$(cycle 2>&1)"; rc=$?
  check "bail-out fails -> FATAL, non-zero" 1 "FATAL"

  # 6. Recovery after failures is announced, so the log shows the whole arc.
  rm -f "$tmp/remove-rc"; echo "$HOSTS_MARKER" > "$HOSTS_FILE"
  touch "$tmp/rdr"; echo 3 > "$FAIL_COUNT_FILE"
  out="$(cycle 2>&1)"; rc=$?
  check "rule back after failures -> recovered" 0 "recovered"

  # 7. --check never repairs, whatever it finds.
  rm -f "$tmp/rdr"; CHECK_ONLY=1; echo 0 > "$tmp/helper-rc"
  out="$(cycle 2>&1)"; rc=$?
  check "--check reports without repairing" 1 "BROKEN"
  [[ -f "$tmp/rdr" ]] && { echo "  FAIL: --check repaired something"; fails=$((fails + 1)); } \
                      || echo "  ok   --check changed nothing"

  echo
  if (( fails == 0 )); then
    echo "self-test: all checks passed"
    return 0
  fi
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
