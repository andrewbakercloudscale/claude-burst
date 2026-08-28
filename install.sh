#!/bin/zsh
# Claude Burst installer.
#
# Usage:
#   ./install.sh              install (or reinstall/update) claude-burst
#   ./install.sh uninstall    remove the LaunchAgent and binary
#
# Uninstall intentionally keeps ~/.config/claude-burst (config, state,
# metrics) and the macOS Keychain secret, since those are not things you
# want wiped by an accidental rerun. See the printed message at the end
# of uninstall for how to purge them too.
set -euo pipefail

LABEL="ninja.andrewbaker.claude-burst"
PLIST="$HOME/Library/LaunchAgents/$LABEL.plist"
INSTALL_DIR="$HOME/.local/bin"
TARGET="$INSTALL_DIR/claude-burst"

if [[ "$(uname -s)" != "Darwin" ]]; then
  echo "claude-burst is Mac-only in this MVP." >&2
  exit 1
fi

uninstall() {
  if [[ -x "$TARGET" ]]; then "$TARGET" disable || true; fi
  launchctl bootout "gui/$UID/$LABEL" >/dev/null 2>&1 || true
  rm -f "$PLIST" "$TARGET"
  echo "Removed Claude Burst routing and LaunchAgent."
  echo "Kept ~/.config/claude-burst (config, state, metrics) and the macOS Keychain secret intentionally."
  echo "To purge those too: rm -rf ~/.config/claude-burst && security delete-generic-password -s claude-burst-bedrock"
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
    echo "NOTE: AWS_BEARER_TOKEN_BEDROCK is not set, so the Bedrock key was not stored yet."
    echo "Before you need overflow: export AWS_BEARER_TOKEN_BEDROCK='...' && claude-burst keychain-set"
  fi

  "$TARGET" enable

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
  <key>ProcessType</key><string>Background</string>
  <key>StandardOutPath</key><string>$HOME/.config/claude-burst/launchd.out.log</string>
  <key>StandardErrorPath</key><string>$HOME/.config/claude-burst/launchd.err.log</string>
</dict>
</plist>
PLIST

  launchctl bootout "gui/$UID/$LABEL" >/dev/null 2>&1 || true
  launchctl bootstrap "gui/$UID" "$PLIST"
  launchctl kickstart -k "gui/$UID/$LABEL"

  cat <<OUT

Installed claude-burst $($TARGET version)
Gateway: http://127.0.0.1:7777
Claude Code settings: enabled
LaunchAgent: $LABEL
AWS region: $REGION

Now restart Claude Code and run:
  claude-burst status

To test the local gateway itself:
  curl -s http://127.0.0.1:7777/healthz

To remove everything later:
  ./install.sh uninstall
OUT
}

case "${1:-install}" in
  install) install ;;
  uninstall) uninstall ;;
  *) echo "Usage: $0 [install|uninstall]" >&2; exit 2 ;;
esac
