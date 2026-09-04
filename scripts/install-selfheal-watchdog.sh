#!/bin/zsh
# Installs (or removes) the self-heal watchdog LaunchAgent: a small,
# separate background job that runs scripts/self-heal-watchdog.sh every
# ~2 minutes to reload the gateway's LaunchAgent if it gets killed (e.g. by
# a critical-battery event -- see self-heal-watchdog.sh's own comment for
# the 2026-09-04 incident this exists to catch), and to notify if the
# /etc/hosts redirect goes missing.
#
# Deliberately a SEPARATE LaunchAgent from the gateway's own
# (ninja.andrewbaker.claude-burst): the whole point is that it keeps
# checking even when that one is gone.
#
# Usage:
#   ./scripts/install-selfheal-watchdog.sh              install/reinstall
#   ./scripts/install-selfheal-watchdog.sh uninstall     remove it
set -euo pipefail

LABEL="ninja.andrewbaker.claude-burst-selfheal"
PLIST="$HOME/Library/LaunchAgents/$LABEL.plist"
# POSIX form, not zsh's ${0:A:h}: same reasoning as rollback.sh/deploy.sh --
# this is recovery-adjacent tooling and should work under `bash` too.
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
# Installed to a copy under ~/.local/bin, NOT run in place from the repo
# checkout under ~/Desktop: Desktop/Documents/Downloads are TCC-protected on
# macOS, and a bare LaunchAgent process has no grant to read there (unlike
# an interactive Terminal/Ghostty session, which does) -- confirmed live
# 2026-09-04: the LaunchAgent failed with "can't open input file" against
# the exact same path that ran fine by hand seconds earlier. This is exactly
# why the gateway's own LaunchAgent already points at ~/.local/bin/claude-burst
# rather than the repo's cmd/claude-burst directly.
#
# Re-run this script after editing self-heal-watchdog.sh or
# health-diagnostics.sh in the repo -- same as deploy.sh/install.sh, the
# running LaunchAgent only sees a change once the copy is refreshed.
INSTALL_DIR="$HOME/.local/bin/claude-burst-selfheal"
SCRIPT="$INSTALL_DIR/self-heal-watchdog.sh"

if [[ "$(uname -s)" != "Darwin" ]]; then
  echo "claude-burst is Mac-only in this MVP." >&2
  exit 1
fi

uninstall() {
  launchctl bootout "gui/$UID/$LABEL" >/dev/null 2>&1 || true
  rm -f "$PLIST"
  rm -rf "$INSTALL_DIR"
  echo "Removed the self-heal watchdog LaunchAgent ($LABEL) and $INSTALL_DIR."
}

install() {
  [[ -x "$ROOT/scripts/self-heal-watchdog.sh" ]] || { echo "missing or non-executable: $ROOT/scripts/self-heal-watchdog.sh" >&2; exit 1; }
  mkdir -p "$HOME/Library/LaunchAgents" "$HOME/.config/claude-burst" "$INSTALL_DIR"
  cp "$ROOT/scripts/self-heal-watchdog.sh" "$ROOT/scripts/health-diagnostics.sh" "$INSTALL_DIR/"
  chmod 755 "$SCRIPT"

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
  <key>StartInterval</key><integer>120</integer>
  <key>RunAtLoad</key><true/>
  <key>ProcessType</key><string>Background</string>
  <key>StandardOutPath</key><string>/dev/null</string>
  <key>StandardErrorPath</key><string>$HOME/.config/claude-burst/self-heal-launchd.err.log</string>
</dict>
</plist>
PLIST

  launchctl bootout "gui/$UID/$LABEL" >/dev/null 2>&1 || true
  launchctl bootstrap "gui/$UID" "$PLIST"

  cat <<OUT

Installed self-heal watchdog LaunchAgent: $LABEL
Runs every 2 minutes, checking:
  - is the gateway's own LaunchAgent loaded? reload it if not (no root needed)
  - is real traffic reaching the gateway? notify (macOS notification) if not,
    at most once per hour

Log: ~/.config/claude-burst/self-heal.log

To remove: $0 uninstall
OUT
}

case "${1:-install}" in
  install) install ;;
  uninstall) uninstall ;;
  *) echo "Usage: $0 [install|uninstall]" >&2; exit 2 ;;
esac
