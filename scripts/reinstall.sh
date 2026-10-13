#!/bin/zsh
# burst-reinstall: fetch the newest Claude Burst and install it again, for a
# Mac where Burst is broken or stale. It talks to GitHub and runs install.sh,
# and needs nothing from the gateway, the dashboard or the installed binary.
# The mod's /claude-burst-reinstall runs it in a Terminal window.
#
# install-burst-off.sh copies it to ~/.local/share/claude-burst and records
# the checkout beside it, so it works from anywhere. A checkout on main with
# nothing uncommitted is fast-forwarded to the newest release on GitHub;
# anything else is someone's work in progress and is installed as it is. A
# checkout that has gone is cloned again. install.sh keeps the mode
# config.json names.
#
# Only a release whose tag the release key signed is fetched in: see
# verify-release.sh, which install-burst-off.sh copies beside this script.
# When the newest release does not verify, or cannot be checked, the copy
# already here is installed and the reason is printed.
set -uo pipefail

SHARE="$HOME/.local/share/claude-burst"
URL="https://github.com/andrewbakercloudscale/claude-burst.git"

fail() { print -u2 "burst-reinstall: $1"; print -u2 "Claude Code not working meanwhile? burst-off takes Burst out of the path."; exit 1; }

# newest_release prints "<tag> <commit>" for the newest signed release in
# checkout $1. The verifier is the copy beside this script, never the one in
# the checkout being updated.
newest_release() {
  [[ -x "$SHARE/verify-release.sh" ]] || { print -u2 "verify-release: $SHARE/verify-release.sh is missing, so no release can be verified"; return 1; }
  "$SHARE/verify-release.sh" "$1"
}

repo="${CLAUDE_BURST_REPO:-$(cat "$SHARE/repo" 2>/dev/null)}"
if [[ -z "$repo" || ! -f "$repo/install.sh" || ! -d "$repo/.git" ]]; then
  repo="$SHARE/src"
  if [[ ! -d "$repo/.git" ]]; then
    echo "== no checkout of Claude Burst found: cloning it to $repo"
    # Nothing is checked out until the release has verified: a clone that
    # did not verify is deleted, so it is never taken for a copy that did.
    mkdir -p "$SHARE" && git clone --quiet --no-checkout "$URL" "$repo" || fail "could not clone $URL"
    if rel="$(newest_release "$repo")"; then
      git -C "$repo" checkout --quiet -B main "${rel#* }" || fail "could not check out release ${rel%% *}"
    else
      rm -rf "$repo"
      fail "the newest release on GitHub could not be verified (see above), so nothing was installed"
    fi
  fi
fi

echo "== Claude Burst: reinstall from $repo"
if [[ "$(git -C "$repo" symbolic-ref --quiet --short HEAD)" != "main" ]]; then
  echo "not on main: installing the checkout as it is"
elif [[ -n "$(git -C "$repo" status --porcelain)" ]]; then
  echo "uncommitted changes: installing the checkout as it is"
elif ! git -C "$repo" fetch --quiet --tags origin main; then
  print -u2 "WARNING: could not fetch from GitHub; installing the copy already here"
elif ! rel="$(newest_release "$repo")"; then
  print -u2 "WARNING: the newest release on GitHub could not be verified (see above); installing the copy already here"
elif git -C "$repo" merge --ff-only --quiet "${rel#* }"; then
  echo "on release ${rel%% *}: $(git -C "$repo" log -1 --format='%h %s')"
else
  print -u2 "WARNING: this checkout cannot fast-forward to release ${rel%% *}; installing the copy already here"
fi
echo

zsh "$repo/install.sh" || fail "install.sh failed (exit $?); see above"
echo
echo "Done. A Claude Code session that still fails: restart it with claude --resume (keeps its history)."
