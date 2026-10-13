#!/bin/bash
# fetch-panel.sh <folder>
#
# Makes <folder> a copy of the usage panel's repo (claude-code-cost-sidebar)
# at its newest release, and only when that release is signed by the release
# key (scripts/verify-release.sh, the copy beside this script). The panel's
# installer is run by Burst's own installer, deploy and dashboard, so an
# unsigned copy of it would be code nobody signed running on every update.
#
# A folder that is already a copy is fetched and moved to the release. When
# the release does not verify, a copy that was already there is left exactly
# as it was (the caller may install it: it verified when it was fetched) and
# a copy made a moment ago is deleted, so it is never taken for one that
# verified. Exit 0 with the release on stdout, or 1 with the reason on stderr.
set -u

dir="${1:-}"
here="$(cd "$(dirname "$0")" && pwd)"
PANEL_REPO="${CLAUDE_BURST_PANEL_REPO:-https://github.com/andrewbakercloudscale/claude-code-cost-sidebar.git}"
[ -n "$dir" ] || { echo "usage: $0 <folder>" >&2; exit 1; }

fresh=0
if [ -d "$dir/.git" ]; then
  git -C "$dir" fetch --quiet --tags origin main || { echo "usage panel: could not fetch from $PANEL_REPO" >&2; exit 1; }
else
  fresh=1
  rm -rf "$dir"
  mkdir -p "$(dirname "$dir")"
  # Nothing is checked out until the release has verified.
  git clone --quiet --no-checkout "$PANEL_REPO" "$dir" || { echo "usage panel: could not fetch from $PANEL_REPO" >&2; exit 1; }
fi

if rel="$("$here/verify-release.sh" "$dir")" && git -C "$dir" checkout --quiet --detach "${rel#* }"; then
  echo "usage panel: release ${rel%% *}, signed by the release key"
  exit 0
fi
[ "$fresh" = 1 ] && rm -rf "$dir"
echo "usage panel: its newest release could not be verified (see above), so it was not fetched" >&2
exit 1
