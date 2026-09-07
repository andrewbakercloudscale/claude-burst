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
PLIST="/Library/LaunchDaemons/$LABEL.plist"
LIBEXEC="/usr/local/libexec/claude-burst"
SCRIPT="$LIBEXEC/pf-heal.sh"
LOG="/var/log/claude-burst-pf.log"
# POSIX form, not zsh's ${0:A:h}: this is recovery-adjacent tooling and should
# work under `bash` too -- same reasoning as rollback.sh and deploy.sh.
ROOT="$(cd "$(dirname "$0")/.." && pwd)"

if [[ "$(uname -s)" != "Darwin" ]]; then
  echo "claude-burst is Mac-only in this MVP." >&2
  exit 1
fi

need_root() {
  [[ $EUID -eq 0 ]] || { echo "must run as root: sudo $0 ${1:-install}" >&2; exit 1; }
}

status() {
  echo "== pf self-heal daemon =="
  if launchctl print "system/$LABEL" >/dev/null 2>&1; then
    echo "  daemon        : LOADED ($LABEL)"
  else
    echo "  daemon        : not loaded"
    echo "                  install with: sudo $0"
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

uninstall() {
  need_root uninstall
  launchctl bootout "system/$LABEL" >/dev/null 2>&1 || true
  rm -f "$PLIST"
  rm -rf "$LIBEXEC"
  echo "Removed the pf self-heal LaunchDaemon ($LABEL) and $LIBEXEC."
  echo "Kept $LOG so the history of what it caught survives the uninstall."
}

install() {
  need_root install
  for f in pf-heal.sh transparent-root.sh; do
    [[ -f "$ROOT/scripts/$f" ]] || { echo "missing: $ROOT/scripts/$f" >&2; exit 1; }
  done

  install -d -o root -g wheel -m 755 "$LIBEXEC"
  install -o root -g wheel -m 755 "$ROOT/scripts/pf-heal.sh" "$ROOT/scripts/transparent-root.sh" "$LIBEXEC/"

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
  chown root:wheel "$PLIST"
  chmod 644 "$PLIST"

  touch "$LOG" && chmod 644 "$LOG"

  launchctl bootout "system/$LABEL" >/dev/null 2>&1 || true
  launchctl bootstrap system "$PLIST"

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

case "${1:-install}" in
  install) install ;;
  uninstall) uninstall ;;
  status) status ;;
  *) echo "Usage: sudo $0 [install|uninstall|status]" >&2; exit 2 ;;
esac
