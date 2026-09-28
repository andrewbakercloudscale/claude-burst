#!/bin/zsh
# Keeps the Mac awake with the lid shut, so a Claude Code session in Ghostty
# keeps working and Remote Control stays reachable. Driven by
# `keep_awake_lid_closed` / `keep_awake_lid_closed_power` in config.json
# through `claude-burst configure --keep-awake-lid-closed true|false
# --keep-awake-power ac|always`.
#
# Why pmset disablesleep and nothing gentler: closing the lid without an
# external display forces sleep regardless of `pmset sleep 0` or any
# `caffeinate` assertion -- this Mac already shows "sleep prevented by
# caffeinate" and still sleeps on lid close. SleepDisabled is the only switch
# that overrides clamshell sleep. It is machine-wide and needs root; the user
# half (Ghostty's App Nap) is done by claude-burst itself, unprivileged.
#
# Two power modes:
#   ac      (default) awake with the lid shut only while on mains power.
#           SleepDisabled is ONE global value -- pmset has no -b/-c form of it
#           -- so this installs a root LaunchDaemon that follows the power
#           source (`pmset -g pslog`, event-driven, not a poll) and sets
#           SleepDisabled 1 on AC, 0 on battery. Unplugging therefore restores
#           normal lid-close sleep, which is the point: a closed laptop that
#           never sleeps, in a bag, on battery, gets hot and flat.
#   always  SleepDisabled 1 regardless of power source. No daemon.
#
# The daemon runs a root-owned COPY in /usr/local/libexec/claude-burst, never
# this file: a root job pointed at a user-writable script under ~/Desktop is a
# root shell for anyone who can edit it (same reasoning as install-pf-heal.sh).
# Re-run `apply` after editing this script.
#
# `remove` restores the value recorded at the first `apply`, and does nothing
# if this script never applied -- so it cannot switch off a SleepDisabled
# somebody set for their own reasons. Everything is idempotent.
#
# Usage:
#   sudo lid-awake-root.sh apply [ac|always]     # default ac
#   sudo lid-awake-root.sh remove
#        lid-awake-root.sh status                # no root
#        lid-awake-root.sh desired [ac|always]   # no root; what reconcile would set now
#   (daemon only) reconcile | watch
set -uo pipefail

SELF="$0"
ROOT_DIR="$(cd "$(dirname "$0")" && pwd)"
STATE_DIR="${CLAUDE_BURST_ROOT_STATE_DIR:-/etc/claude-burst}"
STATE_FILE="$STATE_DIR/lid-awake.state"   # SleepDisabled before our first apply
MODE_FILE="$STATE_DIR/lid-awake.mode"     # ac | always
LABEL="ninja.andrewbaker.claude-burst-lidawake"
PLIST="/Library/LaunchDaemons/$LABEL.plist"
LIBEXEC="/usr/local/libexec/claude-burst"
INSTALLED="$LIBEXEC/lid-awake-root.sh"
LOG="/var/log/claude-burst-lidawake.log"
# Absolute: zsh resolves a function before a command, see install-pf-heal.sh.
INSTALL_BIN=/usr/bin/install

die() { echo "error: $*" >&2; exit 1; }
need_root() { [[ $EUID -eq 0 ]] || die "must run as root: sudo $SELF $1"; }
log() { print -r -- "$(date '+%Y-%m-%d %H:%M:%S %z') $*" >> "$LOG" 2>/dev/null; }

# pmset -g prints "SleepDisabled\t\t0|1" under "System-wide power settings".
current() { pmset -g | awk '$1 == "SleepDisabled" { print $2; exit }'; }

# "ac" or "battery". A Mac with no battery reports AC Power, as it should.
power_source() {
  if pmset -g batt | head -1 | grep -q "'Battery Power'"; then echo battery; else echo ac; fi
}

valid_mode() { [[ "$1" == ac || "$1" == always ]]; }

desired_for() { # mode -> 0|1
  if [[ "$1" == always || "$(power_source)" == ac ]]; then echo 1; else echo 0; fi
}

set_sleep_disabled() {
  [[ "$(current)" == "$1" ]] && return 0
  pmset -a disablesleep "$1" || return 1
  log "SleepDisabled -> $1 (power: $(power_source))"
}

daemon_uninstall() {
  launchctl bootout "system/$LABEL" >/dev/null 2>&1 || true
  rm -f "$PLIST" "$INSTALLED"
  rmdir "$LIBEXEC" 2>/dev/null || true   # shared with pf-heal; only if empty
}

daemon_install() {
  "$INSTALL_BIN" -d -o root -g wheel -m 755 "$LIBEXEC" || die "cannot create $LIBEXEC"
  "$INSTALL_BIN" -o root -g wheel -m 755 "$ROOT_DIR/lid-awake-root.sh" "$INSTALLED" || die "cannot install $INSTALLED"
  cat > "$PLIST" <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>$LABEL</string>
  <key>ProgramArguments</key>
  <array>
    <string>/bin/zsh</string>
    <string>$INSTALLED</string>
    <string>watch</string>
  </array>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>ProcessType</key><string>Background</string>
  <key>StandardOutPath</key><string>/dev/null</string>
  <key>StandardErrorPath</key><string>/var/log/claude-burst-lidawake.err.log</string>
</dict>
</plist>
PLIST
  chown root:wheel "$PLIST"; chmod 644 "$PLIST"
  touch "$LOG" && chmod 644 "$LOG"
  launchctl bootout "system/$LABEL" >/dev/null 2>&1 || true
  launchctl bootstrap system "$PLIST" || die "launchctl bootstrap $PLIST failed"
}

do_apply() {
  need_root apply
  local mode="${1:-ac}"
  valid_mode "$mode" || die "power mode must be ac or always, got '$mode'"
  local before; before="$(current)"
  [[ "$before" == 0 || "$before" == 1 ]] || die "could not read SleepDisabled from pmset -g (got '$before')"
  mkdir -p "$STATE_DIR" || die "cannot create $STATE_DIR"
  # Record only the FIRST apply's prior value. A later apply would otherwise
  # record our own 1 and `remove` would then restore 1.
  [[ -f "$STATE_FILE" ]] || echo "$before" > "$STATE_FILE" || die "cannot write $STATE_FILE"
  echo "$mode" > "$MODE_FILE" || die "cannot write $MODE_FILE"

  if [[ "$mode" == always ]]; then
    daemon_uninstall
  else
    daemon_install
  fi
  set_sleep_disabled "$(desired_for "$mode")" || die "pmset -a disablesleep failed"
  local want after; want="$(desired_for "$mode")"; after="$(current)"
  [[ "$after" == "$want" ]] || die "SleepDisabled reads '$after', wanted '$want'"
  log "applied mode=$mode"
  if [[ "$mode" == always ]]; then
    echo "lid-closed awake: ON, always (SleepDisabled 1; was $before)"
  else
    echo "lid-closed awake: ON, on mains power only (now on $(power_source): SleepDisabled $after; was $before)"
    echo "  daemon $LABEL follows the power source; log: $LOG"
  fi
}

do_remove() {
  need_root remove
  daemon_uninstall
  rm -f "$MODE_FILE"
  if [[ ! -f "$STATE_FILE" ]]; then
    echo "lid-closed awake: never applied by claude-burst; SleepDisabled left at $(current)"
    return 0
  fi
  local before; before="$(<"$STATE_FILE")"
  [[ "$before" == 0 || "$before" == 1 ]] || before=0
  pmset -a disablesleep "$before" || die "pmset -a disablesleep $before failed"
  rm -f "$STATE_FILE"
  log "removed; SleepDisabled restored to $before"
  echo "lid-closed awake: OFF (SleepDisabled restored to $before)"
}

do_reconcile() {
  need_root reconcile
  local mode; mode="$(cat "$MODE_FILE" 2>/dev/null)"
  valid_mode "$mode" || return 0   # removed underneath us: touch nothing
  set_sleep_disabled "$(desired_for "$mode")"
}

do_watch() {
  need_root watch
  do_reconcile
  # pslog prints a line on every power-source change and keeps running.
  # If it ever exits, so do we, and launchd's KeepAlive restarts the watch.
  pmset -g pslog 2>/dev/null | while IFS= read -r _; do do_reconcile; done
}

do_status() {
  local mode; mode="$(cat "$MODE_FILE" 2>/dev/null)"
  echo "SleepDisabled: $(current) (power: $(power_source))"
  if [[ -f "$STATE_FILE" ]]; then
    echo "applied by claude-burst: yes, mode ${mode:-unknown} (restores to $(<"$STATE_FILE") on remove)"
  else
    echo "applied by claude-burst: no"
  fi
  if [[ -f "$PLIST" ]]; then
    echo "power-source daemon: installed ($PLIST)"
    if [[ -f "$INSTALLED" ]] && ! diff -q "$ROOT_DIR/lid-awake-root.sh" "$INSTALLED" >/dev/null 2>&1 \
       && [[ "$ROOT_DIR/lid-awake-root.sh" != "$INSTALLED" ]]; then
      echo "  STALE: installed copy differs from $ROOT_DIR/lid-awake-root.sh; re-run: sudo $SELF apply ${mode:-ac}"
    fi
  else
    echo "power-source daemon: not installed"
  fi
}

case "${1:-}" in
  apply)     do_apply "${2:-ac}" ;;
  remove)    do_remove ;;
  status)    do_status ;;
  desired)   valid_mode "${2:-ac}" || die "mode must be ac or always"; desired_for "${2:-ac}" ;;
  reconcile) do_reconcile ;;
  watch)     do_watch ;;
  *) echo "usage: sudo $SELF apply [ac|always] | remove   |   $SELF status | desired [ac|always]" >&2; exit 2 ;;
esac
