#!/bin/bash
# verify-release.sh <checkout> [branch]
#
# Prints "<tag> <commit>" for the newest release in <checkout> and exits 0,
# but only when that release's tag is signed by a key in release-signers,
# the file beside this script. Anything else exits non-zero with the reason
# on stderr and prints nothing, so a caller that cannot verify has nothing
# to install.
#
# Why: upgrade, repair, reinstall and the usage panel's installer all fetched
# GitHub's main and ran it, in two cases as root, on nothing but GitHub
# saying so. A stolen GitHub login, or a push to main by mistake, reached
# every Mac at its next update. Now only what the release key signed is
# installed. Code pushed to main that is not a signed release is not offered.
#
# The caller must run the copy of this script it ALREADY has, before it
# updates anything, and then use the commit printed here, not the tag name:
# the signers file in freshly fetched code proves nothing about that code,
# and a tag can be moved between this check and its use. A commit cannot.
#
# The newest release is the highest v* tag on the branch. An unsigned tag
# with a higher number therefore stops updates (and says so) and never
# installs; deleting the newest tags offers nothing older than what runs.
#
# The same key signs the usage panel's releases (claude-code-cost-sidebar).
# Exit: 0 verified, 1 not verified, 2 usage or no git checkout, 3 no release.
set -u

repo="${1:-}"
branch="${2:-origin/main}"
signers="$(cd "$(dirname "$0")" && pwd)/release-signers"

if [ -z "$repo" ] || ! git -C "$repo" rev-parse --git-dir >/dev/null 2>&1; then
  echo "verify-release: usage: $0 <git checkout> [branch]" >&2
  exit 2
fi
if [ ! -s "$signers" ]; then
  echo "verify-release: $signers is missing, so no release can be verified" >&2
  exit 1
fi

tag="$(git -C "$repo" tag --list 'v[0-9]*' --sort=-v:refname --merged "$branch" 2>/dev/null | head -n 1)"
if [ -z "$tag" ]; then
  echo "verify-release: no release tag on $branch in $repo" >&2
  exit 3
fi

# Only these two settings decide the answer, whatever the account's own git
# configuration says about signing.
if ! out="$(git -C "$repo" -c gpg.format=ssh -c gpg.ssh.program=ssh-keygen \
      -c gpg.ssh.allowedSignersFile="$signers" verify-tag "$tag" 2>&1)"; then
  echo "verify-release: release $tag in $repo is not signed by the Claude Burst release key, so it will not be installed." >&2
  echo "verify-release: git said: $(printf '%s' "$out" | head -n 1)" >&2
  exit 1
fi

commit="$(git -C "$repo" rev-parse --verify --quiet "$tag^{commit}")" || {
  echo "verify-release: release $tag in $repo does not name a commit" >&2
  exit 1
}
echo "$tag $commit"
