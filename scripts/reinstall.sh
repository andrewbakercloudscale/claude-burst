#!/bin/zsh
# burst-reinstall: fetch the newest Claude Burst and install it again, for a
# Mac where Burst is broken or stale. It talks to GitHub and runs install.sh,
# and needs nothing from the gateway, the dashboard or the installed binary.
# The mod's /claude-burst-reinstall runs it in a Terminal window.
#
# install-burst-off.sh copies it to ~/.local/share/claude-burst and records
# the checkout beside it, so it works from anywhere. A checkout on main with
# nothing uncommitted is fast-forwarded to GitHub's main; anything else is
# someone's work in progress and is installed as it is. A checkout that has
# gone is cloned again. install.sh keeps the mode config.json names.
set -uo pipefail

SHARE="$HOME/.local/share/claude-burst"
URL="https://github.com/andrewbakercloudscale/claude-burst.git"

fail() { print -u2 "burst-reinstall: $1"; print -u2 "Claude Code not working meanwhile? burst-off takes Burst out of the path."; exit 1; }

repo="${CLAUDE_BURST_REPO:-$(cat "$SHARE/repo" 2>/dev/null)}"
if [[ -z "$repo" || ! -f "$repo/install.sh" || ! -d "$repo/.git" ]]; then
  repo="$SHARE/src"
  if [[ ! -d "$repo/.git" ]]; then
    echo "== no checkout of Claude Burst found: cloning it to $repo"
    mkdir -p "$SHARE" && git clone --quiet "$URL" "$repo" || fail "could not clone $URL"
  fi
fi

echo "== Claude Burst: reinstall from $repo"
if [[ "$(git -C "$repo" symbolic-ref --quiet --short HEAD)" != "main" ]]; then
  echo "not on main: installing the checkout as it is"
elif [[ -n "$(git -C "$repo" status --porcelain)" ]]; then
  echo "uncommitted changes: installing the checkout as it is"
elif git -C "$repo" pull --ff-only --quiet origin main; then
  echo "up to date with GitHub: $(git -C "$repo" log -1 --format='%h %s')"
else
  print -u2 "WARNING: could not fetch from GitHub; installing the copy already here"
fi
echo

zsh "$repo/install.sh" || fail "install.sh failed (exit $?); see above"
echo
echo "Done. A Claude Code session that still fails: restart it with claude --resume (keeps its history)."
