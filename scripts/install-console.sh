#!/bin/zsh
# Installs (or removes) the support console LaunchAgent: `claude-burst
# console` on 127.0.0.1:7789, a page with the audit, the log and the
# restart and repair buttons.
#
# Its own LaunchAgent, apart from the gateway's, for the same reason as the
# watchdog: it has to be up when the gateway is not. It loads no provider
# and tolerates a broken config, so whatever stops the gateway leaves the
# console standing to show why. rollback.sh leaves it running: turning
# Burst off is exactly when someone needs the page that turns it back on.
#
# Usage:
#   ./scripts/install-console.sh              install/reinstall (restarts it)
#   ./scripts/install-console.sh uninstall    remove it
set -euo pipefail

LABEL="ninja.andrewbaker.claude-burst-console"
PLIST="${CLAUDE_BURST_CONSOLE_PLIST:-$HOME/Library/LaunchAgents/$LABEL.plist}"
BIN="${CLAUDE_BURST_BIN:-$HOME/.local/bin/claude-burst}"
LOGDIR="$HOME/.config/claude-burst"

uninstall() {
  launchctl bootout "gui/$UID/$LABEL" >/dev/null 2>&1 || true
  rm -f "$PLIST"
  echo "Removed the support console LaunchAgent ($LABEL)."
}

install() {
  [[ -x "$BIN" ]] || { echo "missing $BIN: install Burst first" >&2; exit 1; }
  mkdir -p "$(dirname "$PLIST")" "$LOGDIR"
  cat > "$PLIST" <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>$LABEL</string>
  <key>ProgramArguments</key>
  <array>
    <string>$BIN</string>
    <string>console</string>
  </array>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>ThrottleInterval</key><integer>5</integer>
  <key>ProcessType</key><string>Background</string>
  <key>StandardOutPath</key><string>$LOGDIR/console.out.log</string>
  <key>StandardErrorPath</key><string>$LOGDIR/console.err.log</string>
</dict>
</plist>
PLIST
  # bootout then bootstrap: kickstart alone ignores an edited plist.
  launchctl bootout "gui/$UID/$LABEL" >/dev/null 2>&1 || true
  local tries=0
  until launchctl bootstrap "gui/$UID" "$PLIST" 2>/dev/null; do
    (( ++tries >= 10 )) && { echo "ERROR: launchctl bootstrap gui/$UID $PLIST keeps failing" >&2; exit 1; }
    sleep 1
  done
  echo "Support console: http://127.0.0.1:7789/"
}

case "${1:-install}" in
  install) install ;;
  uninstall) uninstall ;;
  *) echo "usage: $0 [install|uninstall]" >&2; exit 2 ;;
esac
