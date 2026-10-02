#!/bin/zsh
# Brings an installed usage panel (claudecode-cost-usage-panel, a separate
# repo) up to date by re-running its own installer. Called by install.sh and
# deploy.sh, so updating Burst updates the panel too; until 2026-10-02 only a
# missing panel was ever installed and an existing one never changed.
#
# - A checkout beside this repo is someone's working copy: run its installer
#   as it is, never pull.
# - Otherwise the clone install.sh keeps under ~/.local/share is pulled
#   (fast-forward only) first.
#
# The panel's installer keeps existing options and only adds new ones, so a
# re-run is safe. Never fails its caller: the panel is optional in both
# directions, so any problem is a warning and exit 0.
set -uo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"

if [[ ! -x "$HOME/.local/bin/ccusage-panel.sh" ]]; then
  echo "usage panel: not installed, nothing to update"
  exit 0
fi

dir="$(dirname "$ROOT")/claudecode-cost-usage-panel"
if [[ ! -f "$dir/claude-panel-setup.sh" ]]; then
  dir="$HOME/.local/share/claude-burst/claudecode-cost-usage-panel"
  if [[ ! -d "$dir/.git" ]]; then
    echo "WARNING: usage panel is installed but no copy of its repo was found to update it from; reinstall with CLAUDE_BURST_PANEL=yes ./install.sh" >&2
    exit 0
  fi
  git -C "$dir" pull --ff-only --quiet || echo "WARNING: could not update $dir; reinstalling the copy already there" >&2
fi

echo "usage panel: updating from $dir"
if ! bash "$dir/claude-panel-setup.sh"; then
  echo "WARNING: the usage panel installer failed (Burst is unaffected); rerun: bash $dir/claude-panel-setup.sh" >&2
fi
exit 0
