#!/bin/bash
# sign-release.sh <version tag, such as v0.20.30> [commit]
#
# Creates the signed tag a release needs, and checks it the way every
# installed copy will (scripts/verify-release.sh) before it is pushed. A
# release whose tag is not signed is not installed by anyone's Upgrade
# button, repair or reinstall, so this is the only way to tag one.
#
# The key is ~/.ssh/claude-burst-release_ed25519, or CLAUDE_BURST_RELEASE_KEY.
# Its public half is scripts/release-signers. Push the tag yourself:
#   git push origin <tag>
set -euo pipefail

tag="${1:-}"
commit="${2:-HEAD}"
key="${CLAUDE_BURST_RELEASE_KEY:-$HOME/.ssh/claude-burst-release_ed25519}"
here="$(cd "$(dirname "$0")" && pwd)"
repo="$(git rev-parse --show-toplevel)"

case "$tag" in
  v[0-9]*) ;;
  *) echo "usage: $0 <version tag, such as v0.20.30> [commit]" >&2; exit 2 ;;
esac
[ -f "$key" ] || { echo "sign-release: no release key at $key" >&2; exit 1; }
grep -qF "$(awk '{print $2}' "$key.pub")" "$here/release-signers" || {
  echo "sign-release: $key is not a key in $here/release-signers; a tag it signed would be refused everywhere" >&2
  exit 1
}

git -C "$repo" -c gpg.format=ssh -c user.signingkey="$key.pub" tag -s -m "Version ${tag#v}" "$tag" "$commit"

# Checked against the branch the tag is on, pushed or not.
got="$("$here/verify-release.sh" "$repo" "$tag")" || {
  git -C "$repo" tag -d "$tag" >/dev/null
  echo "sign-release: the new tag did not verify, so it was removed" >&2
  exit 1
}
[ "${got%% *}" = "$tag" ] || {
  git -C "$repo" tag -d "$tag" >/dev/null
  echo "sign-release: $tag is not the newest release ($got is), so it was removed" >&2
  exit 1
}
echo "signed and verified: $got"
echo "next: git push origin $tag"
