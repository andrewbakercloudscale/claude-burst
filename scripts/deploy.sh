#!/bin/zsh
# Safe binary deploy: builds the current source, swaps it into place without
# ever risking loss of Claude access, and only restarts the gateway once the
# new build has proven itself.
#
# Order of operations, and why:
#   1. Back up the current binary and config (rollback material).
#   2. Build the new binary to a TEMP file and smoke-test it there --
#      nothing live is touched yet, so a bad build changes nothing.
#   3. `claude-burst disable` -- points Claude Code straight at Anthropic.
#      This is the fail-safe resting state: if anything below goes wrong,
#      leaving this in place means you still have working Claude access
#      with no local gateway in the loop, rather than being stuck pointed
#      at a broken one.
#   4. Atomically install the new binary via `mv` inside the SAME directory
#      (not `cp` in place). This matters: overwriting the bytes of a binary
#      while its own already-running process has it mapped executable trips
#      macOS's own code-signing protection against modifying a running
#      executable's backing file (SIGKILL "Code Signature Invalid" /
#      Taskgated Invalid Signature) -- confirmed by reproducing it. `mv`
#      instead repoints the directory entry to a brand-new, never-mapped
#      inode; the still-running old process keeps its original inode
#      untouched and keeps serving traffic right up until the deliberate
#      restart in the next step. This is also why deploy needs its own
#      script rather than reusing watchdog.sh/rollback.sh's cp-based
#      snapshot pattern for the binary specifically.
#   5. Restart the LaunchAgent so the new code actually loads, then poll
#      /healthz. This is the one step with real (sub-second-to-a-few-second)
#      downtime -- any Claude Code session already running against the
#      gateway may see one connection blip here, same as any process
#      restart. Sessions that start fresh during the whole window are
#      unaffected because of step 3.
#   6. On success: `claude-burst enable` restores the proxy. On failure at
#      any point from step 3 onward: restore the previous binary, restart
#      it, and re-enable only if THAT comes back healthy -- otherwise stay
#      disabled (direct Anthropic) and say so loudly, rather than silently
#      leaving Claude Code pointed at a dead gateway.
set -uo pipefail

# POSIX form, not zsh's ${0:A:h:h}: under bash that expands to an
# unbound-variable error and the deploy dies before doing anything.
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
LABEL="ninja.andrewbaker.claude-burst"
INSTALL_DIR="$HOME/.local/bin"
TARGET="$INSTALL_DIR/claude-burst"
BACKUP_DIR="${CLAUDE_BURST_BACKUP_DIR:-$HOME/.config/claude-burst/backups}"
TS="$(date +%Y%m%d-%H%M%S)"
HEALTH_URL="http://127.0.0.1:7777/healthz"
HEALTH_TIMEOUT=15

log() { echo "[deploy] $*"; }
fail() { echo "[deploy] FAILED: $*" >&2; exit 1; }

wait_healthy() {
  local waited=0
  while (( waited < HEALTH_TIMEOUT )); do
    if curl -sf -m 3 "$HEALTH_URL" >/dev/null 2>&1; then
      return 0
    fi
    sleep 1
    (( waited += 1 ))
  done
  return 1
}

mkdir -p "$BACKUP_DIR"

# --- 1. Back up current binary + config, before touching anything ---
if [[ -x "$TARGET" ]]; then
  cp "$TARGET" "$BACKUP_DIR/claude-burst-bin.$TS.bak"
  cp "$TARGET" "$BACKUP_DIR/claude-burst-bin.latest.bak"
  log "backed up current binary -> $BACKUP_DIR/claude-burst-bin.$TS.bak"
else
  log "no existing binary at $TARGET -- nothing to back up (first install?)"
fi
bash "$ROOT/scripts/backup-config.sh"

# --- 2. Build to a temp file and smoke-test BEFORE going near anything live ---
TMPBIN="$INSTALL_DIR/.claude-burst.new.$$"
log "building..."
if ! (cd "$ROOT" && go build -o "$TMPBIN" ./cmd/claude-burst); then
  rm -f "$TMPBIN"
  fail "build failed -- nothing was touched, gateway untouched, still enabled"
fi
chmod 755 "$TMPBIN"
if ! "$TMPBIN" --help >/dev/null 2>&1; then
  rm -f "$TMPBIN"
  fail "new binary failed its own --help smoke test -- discarded, gateway untouched, still enabled"
fi
log "build OK, smoke test passed"

# no-op guard: skip the disable/restart dance entirely if nothing changed
if [[ -x "$TARGET" ]] && cmp -s "$TMPBIN" "$TARGET"; then
  rm -f "$TMPBIN"
  log "new build is byte-identical to the installed binary -- nothing to deploy"
  exit 0
fi

# --- 3. Fail-safe: point Claude Code straight at Anthropic before the risky part ---
log "disabling proxy (Claude Code -> direct Anthropic) for the swap window..."
"$TARGET" disable || true

# --- 4. Atomic install ---
mv "$TMPBIN" "$TARGET"
chmod 755 "$TARGET"
log "installed new binary at $TARGET"

# --- 5. Restart and verify ---
log "restarting gateway..."
launchctl kickstart -k "gui/$UID/$LABEL" >/dev/null 2>&1

if wait_healthy; then
  log "new gateway is healthy"
  "$TARGET" enable
  log "re-enabled proxy -- restart any Claude Code session for the change to take effect"
  log "deploy complete"
  exit 0
fi

# --- 6. Rollback: new binary didn't come up healthy ---
echo "[deploy] FAILED: new gateway did not become healthy within ${HEALTH_TIMEOUT}s -- rolling back" >&2
if [[ -f "$BACKUP_DIR/claude-burst-bin.latest.bak" ]]; then
  cp "$BACKUP_DIR/claude-burst-bin.latest.bak" "$TARGET"
  chmod 755 "$TARGET"
  launchctl kickstart -k "gui/$UID/$LABEL" >/dev/null 2>&1
  if wait_healthy; then
    "$TARGET" enable
    echo "[deploy] rolled back to previous binary, it is healthy, proxy re-enabled" >&2
  else
    echo "[deploy] rollback binary ALSO failed to come up healthy -- leaving proxy DISABLED (direct Anthropic) so Claude access is not lost. Investigate $HOME/.config/claude-burst/claude-burst.log and Console.app crash reports." >&2
    exit 1
  fi
else
  echo "[deploy] no previous binary backup found to roll back to -- leaving proxy DISABLED (direct Anthropic). Investigate manually." >&2
  exit 1
fi
