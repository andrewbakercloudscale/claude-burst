#!/bin/zsh
# Puts burst-off on the PATH: one command that turns Claude Burst off and gets
# Claude Code working again, for whoever is stuck and has no idea where the
# repo went. It is scripts/rollback.sh, copied with the two helpers it calls,
# so a moved or deleted checkout cannot take it away. claude-burst enable
# turns Burst back on.
#
# burst-reinstall goes with it: scripts/reinstall.sh, which fetches the newest
# Burst and installs it again, and the path of this checkout for it to use.
# The mod's /claude-burst-revert and /claude-burst-reinstall run the two.
#
# Called by install.sh and by both endings of deploy.sh, so the copy never
# lags the rollback.sh it came from. Never fails its caller.
ROOT="${0:A:h:h}"
INSTALL_DIR="${CLAUDE_BURST_INSTALL_DIR:-$HOME/.local/bin}"
OFF_DIR="$HOME/.local/share/claude-burst"

mkdir -p "$OFF_DIR" "$INSTALL_DIR" &&
  cp -f "$ROOT/scripts/rollback.sh" "$ROOT/scripts/transparent-root.sh" "$ROOT/scripts/untrust-ca-systemwide.sh" "$ROOT/scripts/codex-unroute.sh" "$ROOT/scripts/reinstall.sh" "$ROOT/scripts/audit-add.sh" "$OFF_DIR/" &&
  printf '%s\n' "${CLAUDE_BURST_REPO:-$ROOT}" > "$OFF_DIR/repo" &&
  printf '%s\n' '#!/bin/zsh' \
    '# Fetch the newest Claude Burst and install it again. Installed by claude-burst.' \
    'exec /bin/zsh "$HOME/.local/share/claude-burst/reinstall.sh" "$@"' > "$INSTALL_DIR/burst-reinstall" &&
  chmod 755 "$INSTALL_DIR/burst-reinstall" &&
  chmod 755 "$OFF_DIR"/*.sh &&
  printf '%s\n' '#!/bin/zsh' \
    '# Turn Claude Burst off and get Claude Code working again. Installed by claude-burst.' \
    '# Undo with: claude-burst enable' \
    'exec /bin/zsh "$HOME/.local/share/claude-burst/rollback.sh" "$@"' > "$INSTALL_DIR/burst-off" &&
  chmod 755 "$INSTALL_DIR/burst-off" ||
  echo "WARNING: could not install burst-off; scripts/rollback.sh does the same from this checkout" >&2

# burst-repair: scripts/repair.sh, which finds the checkout from the path
# recorded above, or clones one. The watchdog's alerts, diagnose.sh and the
# docs told people to run it from the day repair.sh was written, and until
# 12 Oct 2026 nothing put it on the PATH. Apart from the chain above, so
# that a problem here cannot cost anyone burst-off.
cp -f "$ROOT/scripts/repair.sh" "$OFF_DIR/" &&
  chmod 755 "$OFF_DIR/repair.sh" &&
  printf '%s\n' '#!/bin/zsh' \
    '# Fix the common ways Claude Burst stops working. Installed by claude-burst.' \
    'exec /bin/bash "$HOME/.local/share/claude-burst/repair.sh" "$@"' > "$INSTALL_DIR/burst-repair" &&
  chmod 755 "$INSTALL_DIR/burst-repair" ||
  echo "WARNING: could not install burst-repair; scripts/repair.sh does the same from this checkout" >&2
exit 0
