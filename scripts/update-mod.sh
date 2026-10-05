#!/bin/zsh
# Installs, updates or removes Burst's Claude Code mod (mods/burst):
# Burst and the usage panel inside the session. Called by install.sh and
# deploy.sh, so updating Burst updates the mod too.
#
# Claude Code keeps an installed plugin as a copy, cached by version, so an
# edit to the mod never reaches a session by itself. This compares the copy
# with the source and reinstalls when they differ, whatever the version says.
# New sessions pick it up; running ones keep the copy they loaded.
#
# Claude Code installs from a marketplace, a folder it remembers by path.
# That folder is a copy under ~/.local/share/claude-burst, refreshed here on
# every run, never the checkout: until 2026-10-05 it was whichever checkout
# first installed the mod, so an update run from anywhere else (another
# clone, the dashboard's temporary copy of GitHub's main) reinstalled that
# old checkout's mod, and a Mac on Burst 0.19.0 still had mod 0.3.0.
#
# CLAUDE_BURST_MOD=no skips it. Never fails its caller: the mod is optional,
# so any problem is a warning and exit 0.
#
# Usage: update-mod.sh [install|uninstall]
set -uo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
SRC="$ROOT/mods/burst"
MARKET="$HOME/.local/share/claude-burst/marketplace"
PLUGIN="burst@burst"
# The mod's earlier names: burst-band until Burst 0.19.1, then claude-burst
# until 0.20.2, a name Claude Code reserves for Anthropic's own plugins.
# Removed wherever they are found, or a session would load two and draw the
# band twice.
OLD_PLUGINS=("burst-band@burst" "claude-burst@burst")
MIN_VERSION="2.1.287" # the first Claude Code that loads mods
CLAUDE="${CLAUDE_BIN:-$(command -v claude 2>/dev/null)}"

say() { echo "claude-burst mod: $*"; }
warn() { echo "WARNING: claude-burst mod: $*" >&2; }

if [[ -z "$CLAUDE" || ! -x "$CLAUDE" ]]; then
  say "Claude Code not found, skipped"
  exit 0
fi

# What Claude Code loads: the manifest and the hooks module. Tests and local
# state (.omc) are not part of it.
mod_hash() { # $1 = plugin dir
  [[ -d "$1/hooks" ]] || { echo none; return; }
  (cd "$1" && cat .claude-plugin/plugin.json hooks/*(.N) 2>/dev/null) | shasum -a 256 | cut -c1-16
}

installed_path() { # $1 = plugin id, this mod's when not given
  "$CLAUDE" plugin list --json 2>/dev/null | python3 -c '
import json, sys
try:
    rows = json.load(sys.stdin)
except Exception:
    sys.exit(0)
for r in rows if isinstance(rows, list) else []:
    if r.get("id") == "'"${1:-$PLUGIN}"'":
        print(r.get("installPath", ""))
'
}

case "${1:-install}" in
  uninstall)
    if [[ -n "$(installed_path)" ]]; then
      "$CLAUDE" plugin uninstall "$PLUGIN" >/dev/null 2>&1 || warn "could not uninstall; run: claude plugin uninstall $PLUGIN"
      say "removed"
    fi
    for old in "${OLD_PLUGINS[@]}"; do
      [[ -n "$(installed_path "$old")" ]] && "$CLAUDE" plugin uninstall "$old" >/dev/null 2>&1
    done
    "$CLAUDE" plugin marketplace remove burst >/dev/null 2>&1
    exit 0
    ;;
  install) ;;
  *) echo "Usage: $0 [install|uninstall]" >&2; exit 2 ;;
esac

if [[ "${CLAUDE_BURST_MOD:-yes}" == "no" ]]; then
  say "CLAUDE_BURST_MOD=no, skipped"
  exit 0
fi
version="$("$CLAUDE" --version 2>/dev/null | awk '{print $1}')"
if [[ "$(printf '%s\n%s\n' "$MIN_VERSION" "$version" | sort -V | head -1)" != "$MIN_VERSION" ]]; then
  say "Claude Code $version is older than $MIN_VERSION, which mods need; skipped"
  exit 0
fi

renamed=""
for old in "${OLD_PLUGINS[@]}"; do
  if [[ -n "$(installed_path "$old")" ]]; then
    "$CLAUDE" plugin uninstall "$old" >/dev/null 2>&1 || warn "could not remove the mod under its old name; run: claude plugin uninstall $old"
    renamed="${old%@*}"
  fi
done
current="$(installed_path)"
if [[ -n "$current" && "$(mod_hash "$current")" == "$(mod_hash "$SRC")" ]]; then
  say "up to date"
  exit 0
fi

# The marketplace: a fresh copy of what Claude Code loads, at a path that
# stays put. One registered anywhere else is moved here.
rm -rf "$MARKET" &&
  mkdir -p "$MARKET/.claude-plugin" "$MARKET/mods/burst" &&
  cp "$ROOT/.claude-plugin/marketplace.json" "$MARKET/.claude-plugin/" &&
  cp -R "$SRC/.claude-plugin" "$SRC/hooks" "$MARKET/mods/burst/" ||
  { warn "could not copy the mod to $MARKET"; exit 0; }
at="$("$CLAUDE" plugin marketplace list --json 2>/dev/null | python3 -c '
import json, sys
try:
    rows = json.load(sys.stdin)
except Exception:
    sys.exit(0)
for r in rows if isinstance(rows, list) else []:
    if r.get("name") == "burst":
        print(r.get("path") or r.get("installLocation") or "?")
')"
if [[ "$at" == "$MARKET" ]]; then
  "$CLAUDE" plugin marketplace update burst >/dev/null 2>&1
else
  [[ -n "$at" ]] && "$CLAUDE" plugin marketplace remove burst >/dev/null 2>&1
  "$CLAUDE" plugin marketplace add "$MARKET" >/dev/null 2>&1 || { warn "could not add the marketplace in $MARKET"; exit 0; }
fi
if [[ -n "$current" ]]; then
  "$CLAUDE" plugin uninstall "$PLUGIN" >/dev/null 2>&1
fi
if ! "$CLAUDE" plugin install "$PLUGIN" >/dev/null 2>&1; then
  warn "install failed; run: claude plugin install $PLUGIN"
  exit 0
fi
now="$(installed_path)"
if [[ -z "$now" || "$(mod_hash "$now")" != "$(mod_hash "$SRC")" ]]; then
  warn "installed, but the installed copy does not match $SRC: version $(python3 -c 'import json,sys;print(json.load(open(sys.argv[1])).get("version","?"))' "$now/.claude-plugin/plugin.json" 2>/dev/null || echo unknown) is in place. Run: claude plugin marketplace remove burst, then this again"
  exit 0
fi
msg=installed
[[ -n "$current" ]] && msg=updated
[[ -n "$renamed" ]] && msg="updated (it was called $renamed before)"
say "$msg; new Claude Code sessions load it"
exit 0
