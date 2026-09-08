#!/bin/zsh
# Installs (or removes) the pf self-heal LaunchDaemon: a root job that runs
# scripts/pf-heal.sh every ~2 minutes to notice when transparent mode's pf rdr
# rule has been dropped from the loaded ruleset, reload it, and -- if it cannot
# -- remove the redirect so this Mac can reach Anthropic directly again.
#
# A DAEMON, not an agent, and root-owned, not run in place:
#
#   - The existing self-heal watchdog is a user LaunchAgent. It already
#     detected this exact failure three times on 2026-09-07 and could do
#     nothing about it, because reloading a pf anchor needs root. That is the
#     entire gap this closes.
#   - The scripts are COPIED to /usr/local/libexec/claude-burst (root:wheel,
#     0755). A root LaunchDaemon pointed at a script inside a user-writable
#     directory hands a root shell to anyone who can write that file, and the
#     repo checkout lives under ~/Desktop. The copy is also what makes this
#     work at all: ~/Desktop is TCC-protected and a background job has no
#     grant to read there (confirmed live 2026-09-04, when the user watchdog
#     failed with "can't open input file" against a path that ran fine by
#     hand seconds earlier).
#
# Because it is a copy, RE-RUN THIS after editing pf-heal.sh or
# transparent-root.sh -- same rule as deploy.sh and install-selfheal-watchdog.sh.
# pf-heal.sh's first line of defence against a stale copy is that it delegates
# every repair to the installed transparent-root.sh, which this refreshes too.
#
# Usage:
#   sudo ./scripts/install-pf-heal.sh              install/reinstall
#   sudo ./scripts/install-pf-heal.sh uninstall    remove it
#   ./scripts/install-pf-heal.sh status            is it loaded? (no root)
set -euo pipefail

LABEL="ninja.andrewbaker.claude-burst-pfheal"
PLIST="${CLAUDE_BURST_PFHEAL_PLIST:-/Library/LaunchDaemons/$LABEL.plist}"
LIBEXEC="${CLAUDE_BURST_PFHEAL_LIBEXEC:-/usr/local/libexec/claude-burst}"
SCRIPT="$LIBEXEC/pf-heal.sh"
LOG="${CLAUDE_BURST_PF_HEAL_LOG:-/var/log/claude-burst-pf.log}"
HEARTBEAT="${CLAUDE_BURST_PFHEAL_HEARTBEAT:-/etc/claude-burst/pf-heal.heartbeat}"
# Absolute path, never the bare word. `install` is also the name of a function
# in this file, and zsh resolves a function before a command: `install -d ...`
# inside install() called ITSELF, forever, and the whole script died with
# "maximum nested function level reached" the first time anyone ran it as root
# -- which was the first time it ran at all, because installing needs root and
# nothing here had ever exercised that path. The functions are do_*-prefixed
# now so the collision cannot come back, and this stays absolute anyway.
INSTALL_BIN=/usr/bin/install
LAUNCHCTL="${CLAUDE_BURST_LAUNCHCTL:-launchctl}"
# Ownership args, split out so --self-test can drop them: chown to root:wheel
# is the one part of the install a non-root run genuinely cannot do.
OWNER_ARGS=(-o root -g wheel)
SELFTEST=0
# POSIX form, not zsh's ${0:A:h}: this is recovery-adjacent tooling and should
# work under `bash` too -- same reasoning as rollback.sh and deploy.sh.
ROOT="$(cd "$(dirname "$0")/.." && pwd)"

if [[ "$(uname -s)" != "Darwin" ]]; then
  echo "claude-burst is Mac-only in this MVP." >&2
  exit 1
fi

need_root() {
  (( SELFTEST )) && return 0
  [[ $EUID -eq 0 ]] || { echo "must run as root: sudo $0 ${1:-install}" >&2; exit 1; }
}

do_status() {
  echo "== pf self-heal daemon =="
  # `launchctl print system/<label>` is denied to a non-root caller (exit 113,
  # for every system daemon, not just ours), so asking launchd and reporting
  # the answer would print "not loaded" to anyone who forgot the sudo -- a
  # gate that reports failure when it merely could not look. The heartbeat is
  # what an unprivileged reader can actually verify, so it decides, and
  # launchd is consulted only when we are root and its answer means something.
  local beat age=""
  if [[ -f "$HEARTBEAT" ]]; then
    beat=$(cat "$HEARTBEAT" 2>/dev/null || echo 0)
    age=$(( $(date +%s) - beat ))
  fi
  if [[ -n "$age" ]] && (( age < 600 )); then
    echo "  daemon        : RUNNING (last check ${age}s ago)"
  elif [[ -f "$PLIST" ]]; then
    echo "  daemon        : INSTALLED BUT NOT RUNNING${age:+ (last check ${age}s ago)}"
    echo "                  reload with: sudo $0"
  else
    echo "  daemon        : not installed"
    echo "                  install with: sudo $0"
  fi
  if [[ $EUID -eq 0 ]]; then
    "$LAUNCHCTL" print "system/$LABEL" >/dev/null 2>&1 \
      && echo "  launchd       : loaded" \
      || echo "  launchd       : NOT loaded"
  fi
  [[ -x "$SCRIPT" ]] && echo "  script        : $SCRIPT" || echo "  script        : absent"
  if [[ -f "$SCRIPT" ]] && ! diff -q "$ROOT/scripts/pf-heal.sh" "$SCRIPT" >/dev/null 2>&1; then
    echo "  STALE         : the installed copy differs from $ROOT/scripts/pf-heal.sh"
    echo "                  re-run: sudo $0"
  fi
  if [[ -f "$LOG" ]]; then
    echo "  log           : $LOG ($(wc -l < "$LOG" | tr -d ' ') lines)"
    echo "  last events   :"
    grep -E 'BROKEN|HEALED|GIVING UP|BAILED OUT|FATAL|recovered' "$LOG" 2>/dev/null | tail -5 | sed 's/^/    /' \
      || echo "    (none yet -- the rule has not been lost since this was installed)"
  else
    echo "  log           : $LOG (not created yet)"
  fi
}

do_uninstall() {
  need_root uninstall
  "$LAUNCHCTL" bootout "system/$LABEL" >/dev/null 2>&1 || true
  rm -f "$PLIST"
  rm -rf "$LIBEXEC"
  # Without this, status() reads a heartbeat from a daemon that no longer
  # exists and reports RUNNING for the next ten minutes.
  rm -f "$HEARTBEAT" "$(dirname "$HEARTBEAT")/pf-heal.failures"
  echo "Removed the pf self-heal LaunchDaemon ($LABEL) and $LIBEXEC."
  echo "Kept $LOG so the history of what it caught survives the uninstall."
}

do_install() {
  need_root install
  for f in pf-heal.sh transparent-root.sh; do
    [[ -f "$ROOT/scripts/$f" ]] || { echo "missing: $ROOT/scripts/$f" >&2; exit 1; }
  done

  "$INSTALL_BIN" -d "${OWNER_ARGS[@]}" -m 755 "$LIBEXEC"
  "$INSTALL_BIN" "${OWNER_ARGS[@]}" -m 755 "$ROOT/scripts/pf-heal.sh" "$ROOT/scripts/transparent-root.sh" "$LIBEXEC/"

  mkdir -p "$(dirname "$PLIST")"
  # The daemon runs the INSTALLED transparent-root.sh, not the repo's, so a
  # root job never executes a file a non-root user can rewrite.
  cat > "$PLIST" <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>$LABEL</string>
  <key>ProgramArguments</key>
  <array>
    <string>/bin/zsh</string>
    <string>$SCRIPT</string>
  </array>
  <key>EnvironmentVariables</key>
  <dict>
    <key>CLAUDE_BURST_ROOT_HELPER</key><string>$LIBEXEC/transparent-root.sh</string>
  </dict>
  <key>StartInterval</key><integer>120</integer>
  <key>RunAtLoad</key><true/>
  <key>ProcessType</key><string>Background</string>
  <key>StandardOutPath</key><string>/dev/null</string>
  <key>StandardErrorPath</key><string>/var/log/claude-burst-pf-launchd.err.log</string>
</dict>
</plist>
PLIST
  (( SELFTEST )) || chown root:wheel "$PLIST"
  chmod 644 "$PLIST"

  mkdir -p "$(dirname "$LOG")"
  touch "$LOG" && chmod 644 "$LOG"

  "$LAUNCHCTL" bootout "system/$LABEL" >/dev/null 2>&1 || true
  "$LAUNCHCTL" bootstrap system "$PLIST"

  cat <<OUT

Installed pf self-heal LaunchDaemon: $LABEL
Runs as root every 2 minutes. Each cycle:
  - no /etc/hosts redirect installed -> does nothing (a rollback stays rolled back)
  - pf rdr rule loaded               -> does nothing, silently
  - rule missing                     -> logs it, runs 'transparent-root.sh reload-anchor',
                                        notifies you, and logs the outcome
  - four failed repairs in a row     -> removes the redirect entirely, so this Mac
                                        reaches Anthropic directly instead of staying
                                        black-holed

Log:    $LOG   (world-readable: tail it without sudo)
Status: $0 status
Remove: sudo $0 uninstall
OUT
}

# self_test runs the REAL do_install and do_uninstall against a temp prefix
# with a stub launchctl -- no root, nothing outside the temp dir touched.
#
# It exists because this script shipped with an infinite recursion on its
# install path (see INSTALL_BIN above) that nobody could have hit: `status`
# runs without root and worked fine, and installing needs root, so the only
# code path that mattered had never once been executed. "A rollback path that
# has never run is the one that fails when it is finally needed" is already
# written in transparent-root.sh; this is the same lesson, learned again.
self_test() {
  SELFTEST=1
  local tmp fails=0
  tmp="$(mktemp -d)"
  trap "rm -rf '$tmp'" EXIT

  PLIST="$tmp/LaunchDaemons/$LABEL.plist"
  LIBEXEC="$tmp/libexec"
  SCRIPT="$LIBEXEC/pf-heal.sh"
  LOG="$tmp/var/claude-burst-pf.log"
  HEARTBEAT="$tmp/etc/pf-heal.heartbeat"
  OWNER_ARGS=()
  LAUNCHCTL="$tmp/launchctl"
  printf '#!/bin/zsh\necho "$@" >> "$(dirname "$0")/launchctl.calls"\nexit 0\n' > "$LAUNCHCTL"
  chmod 755 "$LAUNCHCTL"

  ok()   { echo "  ok   $1"; }
  bad()  { echo "  FAIL $1"; fails=$((fails + 1)); }

  # This file runs under `set -e`, so a bare `out="$(do_install)"` that FAILS
  # aborts the whole self-test on the spot -- it never reaches the check, and
  # the run ends looking like it simply printed less. That is exactly what
  # happened when the shipped recursion bug was replayed against a first
  # version of this test: silence, and a zero exit. Every call goes through
  # this instead, which always survives to be judged.
  run() { out="$(eval "$1" 2>&1)" && rc=0 || rc=$?; }

  echo "== install-pf-heal self-test =="

  # The regression itself: a run that recurses never reaches its own output.
  local out rc
  run do_install
  (( rc == 0 )) && ok "do_install completes" || { bad "do_install exited $rc"; printf '%s\n' "$out" | sed 's/^/      /'; }
  if printf '%s' "$out" | grep -q "maximum nested function level"; then
    bad "do_install recursed into itself (a function is shadowing a command it calls)"
  fi

  [[ -x "$SCRIPT" ]] && ok "pf-heal.sh copied and executable" || bad "pf-heal.sh not installed at $SCRIPT"
  [[ -x "$LIBEXEC/transparent-root.sh" ]] && ok "transparent-root.sh copied" || bad "transparent-root.sh not installed"
  [[ -f "$PLIST" ]] && ok "plist written" || bad "no plist at $PLIST"
  grep -q "bootstrap system $PLIST" "$tmp/launchctl.calls" 2>/dev/null \
    && ok "daemon bootstrapped" || bad "launchctl bootstrap was never called"
  # The daemon must run the INSTALLED helper, never the repo copy: that is the
  # difference between a root job and a root job anyone can rewrite.
  grep -q "$LIBEXEC/transparent-root.sh" "$PLIST" 2>/dev/null \
    && ok "plist points at the root-owned helper" || bad "plist does not name $LIBEXEC/transparent-root.sh"
  grep -q "$ROOT/scripts" "$PLIST" 2>/dev/null \
    && bad "plist points into the user-writable repo checkout" || ok "plist does not reference the repo checkout"

  # A stale heartbeat must not survive an uninstall, or status lies for
  # ten minutes about a daemon that is gone.
  mkdir -p "$(dirname "$HEARTBEAT")"; date +%s > "$HEARTBEAT"
  run do_status
  printf '%s' "$out" | grep -q "RUNNING" && ok "status reads the heartbeat" || { bad "status did not report RUNNING"; printf '%s\n' "$out" | sed 's/^/      /'; }

  run do_uninstall
  (( rc == 0 )) && ok "do_uninstall completes" || { bad "do_uninstall exited $rc"; printf '%s\n' "$out" | sed 's/^/      /'; }
  [[ -e "$PLIST" ]] && bad "uninstall left the plist behind" || ok "uninstall removed the plist"
  [[ -e "$LIBEXEC" ]] && bad "uninstall left $LIBEXEC behind" || ok "uninstall removed the libexec copy"
  [[ -e "$HEARTBEAT" ]] && bad "uninstall left the heartbeat behind" || ok "uninstall cleared the heartbeat"

  run do_status
  printf '%s' "$out" | grep -q "not installed" && ok "status reports not installed afterwards" || bad "status still claims an install"

  echo
  if (( fails == 0 )); then echo "self-test: all checks passed"; return 0; fi
  echo "self-test: $fails FAILED" >&2
  return 1
}

case "${1:-install}" in
  install) do_install ;;
  uninstall) do_uninstall ;;
  status) do_status ;;
  --self-test) self_test ;;
  *) echo "Usage: sudo $0 [install|uninstall|status]   |   $0 --self-test" >&2; exit 2 ;;
esac
