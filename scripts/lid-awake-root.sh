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
#   always  SleepDisabled 1 regardless of power source.
#
# An optional idle window narrows either mode to "while in use": awake with the
# lid shut only for IDLE minutes after Claude Code was last used or the lid was
# last open, then normal lid-close sleep. The gateway touches ACTIVITY on each
# real Claude turn; this daemon reads only that file's mtime, never its
# contents, and only from ~/.config/claude-burst/last-activity. Opening the lid
# wakes the Mac and starts a new window. With a window set, the daemon runs in
# both modes and checks every minute as well as on power-source changes.
#
# The screen: with SleepDisabled the built-in screen stays lit behind the
# shut lid (seen 2026-09-30), using power and warming the lid for nobody.
# While the lid is shut and SleepDisabled is 1, the daemon turns the display
# off (`pmset displaysleepnow`: display sleep only, the Mac and Claude Code
# keep running) within about a second, and again if anything wakes it. Not
# while an external display is connected: that is someone working in
# clamshell mode, and display sleep would blank their monitor. The daemon
# therefore now runs in both modes.
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
#   sudo lid-awake-root.sh apply [ac|always] [IDLE_MINUTES ACTIVITY_FILE]
#   sudo lid-awake-root.sh remove
#        lid-awake-root.sh status                # no root
#        lid-awake-root.sh desired [ac|always]   # no root; what reconcile would set now
#        lid-awake-root.sh screen                # no root; "off" or "leave": what the screen check would do
#   (daemon only) reconcile | watch
set -uo pipefail

SELF="$0"
ROOT_DIR="$(cd "$(dirname "$0")" && pwd)"
STATE_DIR="${CLAUDE_BURST_ROOT_STATE_DIR:-/etc/claude-burst}"
STATE_FILE="$STATE_DIR/lid-awake.state"   # SleepDisabled before our first apply
MODE_FILE="$STATE_DIR/lid-awake.mode"     # ac | always
IDLE_FILE="$STATE_DIR/lid-awake.idle"     # minutes; absent or 0 = no window
ACT_FILE="$STATE_DIR/lid-awake.activity"  # path of the gateway's activity file
LIDOPEN_FILE="$STATE_DIR/lid-awake.lidopen" # touched while the lid is open
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
# Read whole, then matched: under pipefail, `pmset ... | head -1` or
# `| grep -q` can kill the writer with SIGPIPE and fail the pipeline, which
# reads as "no match" whatever the output said (see lid_closed).
power_source() {
  local b; b="$(pmset -g batt 2>/dev/null)"
  if [[ "${b%%$'\n'*}" == *"'Battery Power'"* ]]; then echo battery; else echo ac; fi
}

valid_mode() { [[ "$1" == ac || "$1" == always ]]; }

valid_idle() { [[ "$1" =~ ^[0-9]+$ ]] && (( $1 <= 1440 )); }
# Only the gateway's own file under a user's home; read for its mtime only.
valid_activity() { [[ "$1" =~ ^/Users/[A-Za-z0-9._-]+/\.config/claude-burst/last-activity$ ]]; }

lid_closed() {
  # CLAUDE_BURST_TEST_LID (shut|open) is for the tests only; launchd never sets it.
  case "${CLAUDE_BURST_TEST_LID:-}" in shut) return 0 ;; open) return 1 ;; esac
  # Captured, never piped into grep -q: with pipefail, grep stopping at the
  # match killed ioreg mid-write and the pipeline failed, so a shut lid read
  # as open. On 3 Oct 2026 ten of ten checks said open with the lid shut, and
  # the screen stayed lit until macOS's own display sleep turned it off.
  local io; io="$(ioreg -r -k AppleClamshellState -d 4 2>/dev/null)"
  [[ "$io" == *'"AppleClamshellState" = Yes'* ]]
}

mtime() { stat -f %m "$1" 2>/dev/null || echo 0; }

# in_use: within the idle window of the last Claude turn or the last time the
# lid was seen open. True when no window is set.
in_use() {
  local idle; idle="$(cat "$IDLE_FILE" 2>/dev/null)"
  valid_idle "$idle" && (( idle > 0 )) || return 0
  lid_closed || touch "$LIDOPEN_FILE" 2>/dev/null
  local act; act="$(cat "$ACT_FILE" 2>/dev/null)"
  local last; last="$(mtime "$LIDOPEN_FILE")"
  if valid_activity "$act"; then
    local a; a="$(mtime "$act")"; (( a > last )) && last=$a
  fi
  (( $(date +%s) - last < idle * 60 ))
}

desired_for() { # mode -> 0|1
  if [[ "$1" == always || "$(power_source)" == ac ]] && in_use; then echo 1; else echo 0; fi
}

# Always: the screen check below runs in every mode.
needs_daemon() { return 0; }

# external_displays: how many displays are online and not built in.
# CLAUDE_BURST_TEST_DISPLAYS (system_profiler-shaped text) is for the tests.
external_displays() {
  { if [[ -n "${CLAUDE_BURST_TEST_DISPLAYS:-}" ]]; then print -r -- "$CLAUDE_BURST_TEST_DISPLAYS"
    else system_profiler SPDisplaysDataType 2>/dev/null; fi } | awk '
    /^        [^ ].*:$/ { if (name != "" && online && !internal) n++; name = $0; online = 0; internal = 0; next }
    /Online: Yes/ { online = 1 }
    /Connection Type: Internal/ { internal = 1 }
    END { if (name != "" && online && !internal) n++; print n + 0 }'
}

# screen_decision: "off" when the lid is shut, SleepDisabled is 1 (so the
# Mac is being kept awake) and no external display is connected; else
# "leave". CLAUDE_BURST_TEST_SLEEPDISABLED stands in for pmset in the tests.
screen_decision() {
  local sd="${CLAUDE_BURST_TEST_SLEEPDISABLED:-$(current)}"
  if lid_closed && [[ "$sd" == 1 ]] && (( $(external_displays) == 0 )); then echo off; else echo leave; fi
}

DARK_FILE="$STATE_DIR/lid-awake.dark"   # exists while the screen is off for this closing
LID_SEEN=""   # the lid as the last check saw it, so each change is logged once
screen_check() {
  # Every closing is logged with what was decided, so a screen left lit
  # behind the lid shows why. On 3 Oct 2026 a closing left no line at all
  # and nothing could say whether it was missed or judged "leave".
  local lid=open; lid_closed && lid=shut
  if [[ "$lid" != "$LID_SEEN" ]]; then
    [[ -n "$LID_SEEN" && "$lid" == shut ]] && log "lid shut seen: SleepDisabled $(current), external displays $(external_displays), screen $(screen_decision)"
    LID_SEEN=$lid
  fi
  [[ "$lid" == open ]] && { rm -f "$DARK_FILE"; return 0; }
  if [[ "$(screen_decision)" == off ]]; then
    pmset displaysleepnow 2>/dev/null
    [[ -f "$DARK_FILE" ]] || { touch "$DARK_FILE"; log "lid shut: screen off, the Mac stays awake"; }
  else
    rm -f "$DARK_FILE"
  fi
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
  local mode="${1:-ac}" idle="${2:-0}" act="${3:-}"
  valid_mode "$mode" || die "power mode must be ac or always, got '$mode'"
  valid_idle "$idle" || die "idle minutes must be 0 to 1440, got '$idle'"
  (( idle == 0 )) || valid_activity "$act" || die "activity file must be ~/.config/claude-burst/last-activity, got '$act'"
  local before; before="$(current)"
  [[ "$before" == 0 || "$before" == 1 ]] || die "could not read SleepDisabled from pmset -g (got '$before')"
  mkdir -p "$STATE_DIR" || die "cannot create $STATE_DIR"
  # Record only the FIRST apply's prior value. A later apply would otherwise
  # record our own 1 and `remove` would then restore 1.
  [[ -f "$STATE_FILE" ]] || echo "$before" > "$STATE_FILE" || die "cannot write $STATE_FILE"
  echo "$mode" > "$MODE_FILE" || die "cannot write $MODE_FILE"
  echo "$idle" > "$IDLE_FILE" || die "cannot write $IDLE_FILE"
  if (( idle > 0 )); then echo "$act" > "$ACT_FILE"; else rm -f "$ACT_FILE"; fi
  touch "$LIDOPEN_FILE"   # applying counts as use: the window starts now

  if needs_daemon "$mode"; then
    daemon_install
  else
    daemon_uninstall
  fi
  set_sleep_disabled "$(desired_for "$mode")" || die "pmset -a disablesleep failed"
  local want after; want="$(desired_for "$mode")"; after="$(current)"
  [[ "$after" == "$want" ]] || die "SleepDisabled reads '$after', wanted '$want'"
  log "applied mode=$mode idle=$idle"
  if (( idle > 0 )); then
    echo "lid-closed awake: ON, ${mode/ac/on mains power only}, for $idle minutes after Claude Code was last used or the lid was last open (SleepDisabled $after; was $before)"
    echo "  daemon $LABEL checks every minute; log: $LOG"
  elif [[ "$mode" == always ]]; then
    echo "lid-closed awake: ON, always (SleepDisabled 1; was $before)"
  else
    echo "lid-closed awake: ON, on mains power only (now on $(power_source): SleepDisabled $after; was $before)"
    echo "  daemon $LABEL follows the power source; log: $LOG"
  fi
  screen_line
}

# screen_line: which of the two is true right now, in so many words.
screen_line() {
  if [[ "$(screen_decision)" == off ]]; then
    echo "Screen Turned Off (lid shut; the Mac and Claude Code keep running)"
  elif (( $(external_displays) > 0 )); then
    echo "Screen Turned On (an external display is connected, so it is left on even with the lid shut)"
  elif lid_closed; then
    echo "Screen Turned On (the Mac is not being kept awake, so the lid sleeps it as usual)"
  else
    echo "Screen Turned On (lid open; it turns off within about a second of the lid shutting)"
  fi
}

do_remove() {
  need_root remove
  daemon_uninstall
  rm -f "$MODE_FILE" "$IDLE_FILE" "$ACT_FILE" "$LIDOPEN_FILE"
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
  # The idle window needs a clock as well as power-source events.
  ( while sleep 60; do do_reconcile; done ) &
  local clock=$!
  # The screen, every second: dark before anyone sees it lit behind the lid.
  # Five seconds left it glowing long enough to look like no fix at all, and
  # with the lid open each check is one ioreg call.
  ( while sleep 1; do screen_check; done ) &
  local screen=$!
  trap "kill $clock $screen 2>/dev/null" EXIT
  # pslog prints a line on every power-source change and keeps running.
  # If it ever exits, so do we, and launchd's KeepAlive restarts the watch.
  pmset -g pslog 2>/dev/null | while IFS= read -r _; do do_reconcile; done
}

do_status() {
  local mode; mode="$(cat "$MODE_FILE" 2>/dev/null)"
  echo "SleepDisabled: $(current) (power: $(power_source))"
  local idle; idle="$(cat "$IDLE_FILE" 2>/dev/null)"
  if valid_idle "$idle" && (( idle > 0 )); then
    if in_use; then echo "idle window: $idle minutes, in use now"; else echo "idle window: $idle minutes, idle: lid-close sleep allowed"; fi
  fi
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
  apply)     do_apply "${2:-ac}" "${3:-0}" "${4:-}" ;;
  remove)    do_remove ;;
  status)    do_status ;;
  desired)   valid_mode "${2:-ac}" || die "mode must be ac or always"; desired_for "${2:-ac}" ;;
  screen)    screen_decision ;;
  screen-line) screen_line ;;
  reconcile) do_reconcile ;;
  watch)     do_watch ;;
  *) echo "usage: sudo $SELF apply [ac|always] | remove   |   $SELF status | desired [ac|always]" >&2; exit 2 ;;
esac
