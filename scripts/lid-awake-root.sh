#!/bin/zsh
# Keeps the Mac awake with the lid shut, so a Claude Code session in Ghostty
# keeps working and Remote Control stays reachable. Driven by
# `keep_awake_lid_closed` in config.json (default false) through
# `claude-burst configure --keep-awake-lid-closed true|false`.
#
# Why pmset disablesleep and nothing gentler: closing the lid without an
# external display forces sleep regardless of `pmset sleep 0` or any
# `caffeinate` assertion -- this Mac already shows "sleep prevented by
# caffeinate" and still sleeps on lid close. SleepDisabled is the only switch
# that overrides clamshell sleep. It is machine-wide and needs root; the user
# half (Ghostty's App Nap) is done by claude-burst itself, unprivileged.
#
# The cost is real: a closed laptop that never sleeps, in a bag, on battery,
# gets hot and flat. That is why the flag defaults to false.
#
# `remove` restores the value recorded at `apply`, and does nothing if this
# script never applied -- so it cannot switch off a SleepDisabled somebody set
# for their own reasons. Both are idempotent.
#
# Usage:
#   sudo lid-awake-root.sh apply
#   sudo lid-awake-root.sh remove
#        lid-awake-root.sh status        # no root
set -uo pipefail

SELF="$0"
STATE_DIR="${CLAUDE_BURST_ROOT_STATE_DIR:-/etc/claude-burst}"
STATE_FILE="$STATE_DIR/lid-awake.state"

die() { echo "error: $*" >&2; exit 1; }
need_root() { [[ $EUID -eq 0 ]] || die "must run as root: sudo $SELF $1"; }

# pmset -g prints "SleepDisabled\t\t0|1" under "System-wide power settings".
current() { pmset -g | awk '$1 == "SleepDisabled" { print $2; exit }'; }

do_apply() {
  need_root apply
  local before; before="$(current)"
  [[ "$before" == 0 || "$before" == 1 ]] || die "could not read SleepDisabled from pmset -g (got '$before')"
  mkdir -p "$STATE_DIR" || die "cannot create $STATE_DIR"
  # Record only the FIRST apply's prior value. A second apply would otherwise
  # record our own 1 and `remove` would then restore 1.
  [[ -f "$STATE_FILE" ]] || echo "$before" > "$STATE_FILE" || die "cannot write $STATE_FILE"
  pmset -a disablesleep 1 || die "pmset -a disablesleep 1 failed"
  local after; after="$(current)"
  [[ "$after" == 1 ]] || die "pmset accepted the change but SleepDisabled reads '$after'"
  echo "lid-closed awake: ON (SleepDisabled 1; was $before)"
}

do_remove() {
  need_root remove
  if [[ ! -f "$STATE_FILE" ]]; then
    echo "lid-closed awake: never applied by claude-burst; SleepDisabled left at $(current)"
    return 0
  fi
  local before; before="$(<"$STATE_FILE")"
  [[ "$before" == 0 || "$before" == 1 ]] || before=0
  pmset -a disablesleep "$before" || die "pmset -a disablesleep $before failed"
  rm -f "$STATE_FILE"
  echo "lid-closed awake: OFF (SleepDisabled restored to $before)"
}

do_status() {
  local v; v="$(current)"
  echo "SleepDisabled: ${v:-unknown}"
  if [[ -f "$STATE_FILE" ]]; then
    echo "applied by claude-burst: yes (restores to $(<"$STATE_FILE") on remove)"
  else
    echo "applied by claude-burst: no"
  fi
}

case "${1:-}" in
  apply)  do_apply ;;
  remove) do_remove ;;
  status) do_status ;;
  *) echo "usage: sudo $SELF apply|remove   |   $SELF status" >&2; exit 2 ;;
esac
