#!/bin/zsh
# Brings an installed usage panel (claude-code-cost-sidebar, a separate
# repo) up to date by re-running its own installer. Called by install.sh and
# deploy.sh, so updating Burst updates the panel too; until 2026-10-02 only a
# missing panel was ever installed and an existing one never changed.
#
# - A checkout beside this repo is pulled (fast-forward only) when it is on
#   main with nothing uncommitted: that is a plain clone. Anything else is
#   someone's work in progress, and its installer runs on it as it is.
# - Otherwise the clone install.sh keeps under ~/.local/share is pulled
#   (fast-forward only) first.
#
# CLAUDE_BURST_REPO names the real checkout when this script runs from a
# temporary copy of it (the dashboard's "Install GitHub version"), where
# "beside this repo" would be the temp folder.
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

# The repo was claudecode-cost-usage-panel until 2026-10-05; either name is found.
beside="$(dirname "${CLAUDE_BURST_REPO:-$ROOT}")"
dir="$beside/claude-code-cost-sidebar"
[[ -f "$dir/claude-panel-setup.sh" ]] || dir="$beside/claudecode-cost-usage-panel"
if [[ -f "$dir/claude-panel-setup.sh" ]]; then
  if [[ ! -d "$dir/.git" ]]; then
    echo "usage panel: $dir is not a git checkout, installing it as it is"
  elif [[ "$(git -C "$dir" symbolic-ref --quiet --short HEAD)" != "main" ]]; then
    echo "usage panel: $dir is not on main, installing it as it is"
  elif [[ -n "$(git -C "$dir" status --porcelain)" ]]; then
    echo "usage panel: $dir has uncommitted changes, installing it as it is"
  else
    git -C "$dir" pull --ff-only --quiet || echo "WARNING: could not update $dir; reinstalling the copy already there" >&2
  fi
else
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
