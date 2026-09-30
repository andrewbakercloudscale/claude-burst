#!/bin/zsh
# Claude Burst installer.
#
# Usage:
#   ./install.sh              install (or reinstall/update) claude-burst
#   ./install.sh uninstall    remove the routing, the token-shunting hook and
#                             skill, the LaunchAgent and the binary
#
# Uninstall intentionally keeps ~/.config/claude-burst (config, state,
# metrics) and the macOS Keychain secret, since those are not things you
# want wiped by an accidental rerun. See the printed message at the end
# of uninstall for how to purge them too.
#
# Note that shunting is switched OFF in config.json as part of uninstall (that is
# how its hook and skill are removed), so a later reinstall needs
# `claude-burst shunt enable` to turn it back on.
set -euo pipefail

LABEL="ninja.andrewbaker.claude-burst"
PLIST="$HOME/Library/LaunchAgents/$LABEL.plist"
INSTALL_DIR="$HOME/.local/bin"
TARGET="$INSTALL_DIR/claude-burst"

if [[ "$(uname -s)" != "Darwin" ]]; then
  echo "claude-burst is Mac-only in this MVP." >&2
  exit 1
fi

# keep_awake_lid_closed (default false) keeps Claude Code in Ghostty, and
# Remote Control, running with the lid shut; keep_awake_lid_closed_power
# picks ac (default, plugged in only) or always. Re-applied from config.json
# on every install, so a reinstall or a new machine with the same config ends
# up in the same state. False touches nothing and asks for no password.
apply_keep_awake() {
  local cfg="$HOME/.config/claude-burst/config.json" on mode
  on="$(python3 -c "import json;print(str(json.load(open('$cfg')).get('keep_awake_lid_closed',False)).lower())" 2>/dev/null || echo false)"
  [[ "$on" == true ]] || return 0
  mode="$(python3 -c "import json;print(json.load(open('$cfg')).get('keep_awake_lid_closed_power') or 'ac')" 2>/dev/null || echo ac)"
  echo "keep_awake_lid_closed is true (mode $mode): applying; sudo will ask for your password."
  defaults write com.mitchellh.ghostty NSAppSleepDisabled -bool YES
  if ! sudo "$ROOT/scripts/lid-awake-root.sh" apply "$mode"; then
    echo "WARNING: keep-awake not applied; the lid will still sleep the Mac. Run:" >&2
    echo "  sudo $ROOT/scripts/lid-awake-root.sh apply $mode" >&2
  fi
}

uninstall() {
  if [[ -x "$TARGET" ]]; then
    # Token shunting puts a hook in ~/.claude/settings.json that runs this binary
    # before every Read and Bash call, and a skill telling Claude to run it. Both
    # have to come out while the binary still exists to remove them: left behind,
    # the hook points at nothing and the skill instructs Claude to run a command
    # that is gone. A no-op when shunting was never enabled.
    "$TARGET" shunt disable >/dev/null 2>&1 || true
    "$TARGET" disable || true
  fi
  launchctl bootout "gui/$UID/$LABEL" >/dev/null 2>&1 || true
  rm -f "$PLIST" "$TARGET"
  # keep_awake_lid_closed leaves a root LaunchDaemon and pmset SleepDisabled
  # behind; an uninstall that kept a Mac that never sleeps would be a trap.
  # Only asks for sudo when something was actually applied.
  if [[ -f /etc/claude-burst/lid-awake.state || -f /Library/LaunchDaemons/ninja.andrewbaker.claude-burst-lidawake.plist ]]; then
    echo "Removing the lid-closed keep-awake setting (needs sudo)..."
    sudo "${0:A:h}/scripts/lid-awake-root.sh" remove || echo "WARNING: run: sudo ${0:A:h}/scripts/lid-awake-root.sh remove" >&2
  fi
  defaults delete com.mitchellh.ghostty NSAppSleepDisabled >/dev/null 2>&1 || true
  echo "Removed Claude Burst routing, the token-shunting hook and skill, and the LaunchAgent."
  echo "Kept ~/.config/claude-burst (config, state, metrics) and the macOS Keychain secret intentionally."
  echo "To purge those too: rm -rf ~/.config/claude-burst"
  echo "  and delete whichever secondary key you stored:"
  echo "    security delete-generic-password -s claude-burst-together     # Together AI"
  echo "    security delete-generic-password -s claude-burst-openrouter   # OpenRouter"
  echo "    security delete-generic-password -s claude-burst-bedrock      # Amazon Bedrock"
}

install() {
  ROOT="${0:A:h}"
  ARCH="$(uname -m)"
  case "$ARCH" in
    arm64) BIN="$ROOT/dist/claude-burst-darwin-arm64" ;;
    x86_64) BIN="$ROOT/dist/claude-burst-darwin-amd64" ;;
    *) echo "Unsupported Mac architecture: $ARCH" >&2; exit 1 ;;
  esac

  if [[ ! -x "$BIN" ]]; then
    if ! command -v go >/dev/null 2>&1; then
      echo "No prebuilt binary found and Go is not installed." >&2
      echo "Install Go, then rerun ./install.sh" >&2
      exit 1
    fi
    echo "Building claude-burst locally..."
    (cd "$ROOT" && go build -o /tmp/claude-burst ./cmd/claude-burst)
    BIN=/tmp/claude-burst
  fi

  mkdir -p "$INSTALL_DIR"
  cp "$BIN" "$TARGET"
  chmod 755 "$TARGET"

  ZPROFILE="$HOME/.zprofile"
  PATH_LINE='export PATH="$HOME/.local/bin:$PATH" # claude-burst'
  if ! grep -Fq '# claude-burst' "$ZPROFILE" 2>/dev/null; then
    printf '\n%s\n' "$PATH_LINE" >> "$ZPROFILE"
  fi

  REGION="${AWS_REGION:-${AWS_DEFAULT_REGION:-us-east-1}}"
  "$TARGET" configure --region "$REGION"

  if [[ -n "${AWS_BEARER_TOKEN_BEDROCK:-}" ]]; then
    "$TARGET" keychain-set
  else
    echo "NOTE: no secondary key stored, so Claude Burst runs on your single plan (Claude Enterprise, Pro or Max)."
    echo "Everything but overflow works, and Anthropic's own limits reach Claude Code unchanged."
    echo "To overflow to Bedrock later: export AWS_BEARER_TOKEN_BEDROCK='...' && claude-burst keychain-set"
  fi

  "$TARGET" enable

  apply_keep_awake

  mkdir -p "$HOME/Library/LaunchAgents"
  mkdir -p "$HOME/.config/claude-burst"
  cat > "$PLIST" <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>$LABEL</string>
  <key>ProgramArguments</key>
  <array>
    <string>$TARGET</string>
    <string>serve</string>
  </array>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <!-- Seconds launchd waits after SIGTERM before SIGKILL (default 20, and
       launchd caps it at 60 for an agent). The gateway drains in-flight
       replies for up to 50s on SIGTERM (cmd/claude-burst/drain.go); this
       must stay above that. -->
  <key>ExitTimeOut</key><integer>60</integer>
  <key>ProcessType</key><string>Background</string>
  <key>StandardOutPath</key><string>$HOME/.config/claude-burst/launchd.out.log</string>
  <key>StandardErrorPath</key><string>$HOME/.config/claude-burst/launchd.err.log</string>
</dict>
</plist>
PLIST

  launchctl bootout "gui/$UID/$LABEL" >/dev/null 2>&1 || true
  launchctl bootstrap "gui/$UID" "$PLIST"
  launchctl kickstart -k "gui/$UID/$LABEL"

  # Read the port back rather than printing a literal: the default moved off
  # 7777 (see internal/config's Default), and a summary naming a port nothing
  # is listening on is exactly the kind of confidently-wrong instruction this
  # project keeps getting bitten by.
  local gw
  gw="$(python3 -c "import json;print(json.load(open('$HOME/.config/claude-burst/config.json')).get('listen','127.0.0.1:7777'))" 2>/dev/null || echo '127.0.0.1:7777')"

  cat <<OUT

Installed claude-burst $($TARGET version)
Gateway: http://$gw
Claude Code settings: enabled
LaunchAgent: $LABEL
AWS region: $REGION

Now restart Claude Code and run:
  claude-burst status

To test the local gateway itself:
  curl -s http://$gw/healthz

Lid-closed keep-awake (off by default; see README):
  claude-burst configure --keep-awake-lid-closed true [--keep-awake-power ac|always]

To remove everything later:
  ./install.sh uninstall
OUT

  offer_panel
}

# The usage panel (a separate repo) shows each turn's context and cost next
# to Claude Code, and reads Burst's metrics to mark auto compaction. It is
# optional in both directions, so a failure here never fails this install.
PANEL_REPO="https://github.com/andrewbakercloudscale/claudecode-cost-usage-panel.git"
offer_panel() {
  if [[ -x "$HOME/.local/bin/ccusage-panel.sh" ]]; then
    echo "\nUsage panel: already installed (it shows Burst's auto compaction in its turn table)."
    return 0
  fi
  if [[ "${CLAUDE_BURST_PANEL:-ask}" == "no" ]]; then
    return 0
  fi
  if [[ "${CLAUDE_BURST_PANEL:-ask}" != "yes" ]]; then
    if [[ ! -t 0 ]]; then
      echo "\nOptional usage panel (live context and cost beside Claude Code): rerun with CLAUDE_BURST_PANEL=yes, or see $PANEL_REPO"
      return 0
    fi
    local answer
    echo
    echo "The usage panel shows each turn's context and cost in a split beside Claude Code,"
    echo "including when Burst compacts a long session and what that saved."
    read -r "answer?Install the usage panel too? [Y/n] "
    [[ -z "$answer" || "$answer" == [Yy]* ]] || { echo "Skipped. Install it later from $PANEL_REPO"; return 0; }
  fi

  # A checkout beside this one wins; otherwise keep a clone of our own.
  local dir="${ROOT:h}/claudecode-cost-usage-panel"
  if [[ ! -f "$dir/claude-panel-setup.sh" ]]; then
    dir="$HOME/.local/share/claude-burst/claudecode-cost-usage-panel"
    if [[ -d "$dir/.git" ]]; then
      git -C "$dir" pull --ff-only --quiet || echo "WARNING: could not update $dir; installing the copy already there"
    else
      mkdir -p "${dir:h}"
      git clone --quiet "$PANEL_REPO" "$dir" || { echo "WARNING: could not fetch the usage panel; install it later from $PANEL_REPO"; return 0; }
    fi
  fi
  echo "Installing the usage panel from $dir"
  bash "$dir/claude-panel-setup.sh" || echo "WARNING: the usage panel installer failed (Burst itself is installed); rerun: bash $dir/claude-panel-setup.sh"
}

case "${1:-install}" in
  install) install ;;
  uninstall) uninstall ;;
  *) echo "Usage: $0 [install|uninstall]" >&2; exit 2 ;;
esac
