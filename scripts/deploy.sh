#!/bin/zsh
# Safe binary deploy: builds the current source, swaps it into place without
# ever risking loss of Claude access, and only restarts the gateway once the
# new build has proven itself.
#
# Order of operations, and why:
#   1. Back up the current binary and config (rollback material).
#   1.5. Run `go test ./...` against the working tree, BEFORE building --
#      a build that compiles can still contain a broken failover path (the
#      admin/router seam has no compile-time link between them, so a wiring
#      bug there is invisible to `go build`; internal/integration's
#      failover_e2e_test.go exists specifically to catch it). Failing tests
#      abort the deploy here with nothing touched yet, same as a failed build.
#   2. Build the new binary to a TEMP file and smoke-test it there --
#      nothing live is touched yet, so a bad build changes nothing.
#   3. In base-url mode only: `claude-burst disable` -- points Claude Code
#      straight at Anthropic. This is the fail-safe resting state: if
#      anything below goes wrong, leaving this in place means you still have
#      working Claude access with no local gateway in the loop, rather than
#      being stuck pointed at a broken one. It also protects any Claude Code
#      session that STARTS during the swap window (an already-running
#      session can't hot-reload env vars anyway, so this only helps fresh
#      launches).
#      In transparent mode this step is skipped entirely: `disable` there
#      only strips the local CA from the trust bundle -- it CANNOT detach
#      Claude Code from the gateway, because that requires removing the
#      machine-wide pf redirect and /etc/hosts entry, which need root and
#      are deliberately never touched by an unattended script (see
#      transparent-root.sh / rollback.sh). Calling it anyway would strip
#      trust while the redirect stays live, breaking every request on this
#      Mac to api.anthropic.com with a TLS trust error for the entire swap
#      window -- confirmed happening in production on 2026-08-31.
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
#      unaffected because of step 3 (base-url mode only).
#   6. On success: `claude-burst enable` restores the proxy, but ONLY if step
#      3 actually ran `disable` (base-url mode) -- calling it unconditionally
#      would touch CA trust state transparent mode never disturbed. On
#      failure at any point from step 3 onward: restore the previous binary,
#      restart it, and re-enable only if THAT comes back healthy. If the
#      rollback binary also fails: in base-url mode Claude Code is still
#      pointed straight at Anthropic (safe) and we say so; in transparent
#      mode the machine-wide redirect is still live regardless of what this
#      script does, so we say THAT loudly and point at the one command that
#      actually fixes it (`sudo transparent-root.sh remove`), rather than
#      claiming a "disabled (direct Anthropic)" state that transparent mode
#      cannot reach without root.
set -uo pipefail

# POSIX form, not zsh's ${0:A:h:h}: under bash that expands to an
# unbound-variable error and the deploy dies before doing anything.
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
LABEL="ninja.andrewbaker.claude-burst"
INSTALL_DIR="$HOME/.local/bin"
TARGET="$INSTALL_DIR/claude-burst"
BACKUP_DIR="${CLAUDE_BURST_BACKUP_DIR:-$HOME/.config/claude-burst/backups}"
TS="$(date +%Y%m%d-%H%M%S)"
HEALTH_TIMEOUT=15
# Declared up here, not just at its real assignment below step 2, because
# wait_healthy reads it and `set -u` turns an unbound read into an abort --
# which under this script means aborting mid-swap, the one place it must not.
TRANSPARENT=0

log() { echo "[deploy] $*"; }
fail() { echo "[deploy] FAILED: $*" >&2; exit 1; }

# shellcheck source=./health-diagnostics.sh
source "$ROOT/scripts/health-diagnostics.sh"

# Liveness polling wraps the shared gateway_healthy() from
# health-diagnostics.sh -- see its comment for why probing 127.0.0.1:7777
# alone rolled back a healthy build on 2026-09-03. Kept as a loop here because
# a just-restarted gateway legitimately needs a moment to bind.
wait_healthy() {
  local waited=0
  while (( waited < HEALTH_TIMEOUT )); do
    if gateway_healthy; then
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

# --- 1.5. Run the test suite -- must pass before a single byte gets built ---
# Includes internal/integration's end-to-end failover tests: real HTTP
# listeners for the gateway and admin panel, fake primary/secondary
# upstreams, driven exactly the way Claude Code and the dashboard's buttons
# do. Those exist because a real "Force -> secondary" bug (admin validating
# against config.json while the running gateway's actual secondary Provider
# is built once at startup, so the two can drift apart) compiled fine, built
# fine, and passed both packages' own unit tests -- it only shows up when
# admin and router are driven together over the wire.
log "running test suite..."
if ! (cd "$ROOT" && go test ./... -race); then
  fail "test suite failed -- nothing was built, gateway untouched, still enabled"
fi
log "tests passed"

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

# Determine mode via the (still-old, pre-swap) binary's own status output
# rather than re-parsing config.json here in bash: it's the single place
# that logic already lives, and a second implementation is exactly the kind
# of drift this repo has been bitten by before.
if [[ -x "$TARGET" ]]; then
  # Captured into a variable rather than piped straight into grep: under
  # `pipefail` (set above), a pipeline's exit status is its LAST non-zero
  # command, so if `status` itself ever exits non-zero for any reason --
  # even while printing the right thing -- the `if` sees that failure and
  # not grep's match, and silently falls through to TRANSPARENT=0. Twice
  # in a row on 2026-08-31 this branch ran the base-url disable/enable
  # dance on a transparent-mode machine (self-corrected by the time this
  # script exits, via the normal enable() at the end -- but the whole
  # point of skipping that dance here is to never touch CA trust during
  # the swap in the first place). This form can't have that failure mode:
  # STATUS_OUT is set unconditionally by `||:`, and only string-matched
  # after, with no pipeline exit status left to be poisoned by.
  STATUS_OUT="$("$TARGET" status 2>/dev/null || :)"
  if [[ "$STATUS_OUT" == *$'\n'"intercept: transparent"* || "$STATUS_OUT" == "intercept: transparent"* ]]; then
    TRANSPARENT=1
  fi
fi

# --- 3. Fail-safe: point Claude Code straight at Anthropic before the risky part ---
# Base-url mode only -- see the top-of-file note on why this is actively
# harmful (strips CA trust, achieves nothing) in transparent mode.
DISABLED_FOR_SWAP=0
if [[ "$TRANSPARENT" -eq 0 ]]; then
  log "disabling proxy (Claude Code -> direct Anthropic) for the swap window..."
  "$TARGET" disable || true
  DISABLED_FOR_SWAP=1
else
  log "transparent mode: skipping disable/enable around the swap (see top-of-file note); relying on the atomic mv + health-checked restart instead"
fi

# --- 4. Atomic install ---
mv "$TMPBIN" "$TARGET"
chmod 755 "$TARGET"
log "installed new binary at $TARGET"

# --- 5. Restart and verify ---
log "restarting gateway..."
launchctl kickstart -k "gui/$UID/$LABEL" >/dev/null 2>&1

# Best-effort, transparent mode only, and deliberately AFTER the restart:
# the states suspected of breaking direct connections to the gateway port
# are the ones left behind by the process that just died. Skipped without
# comment-free silence if we cannot get root -- see flush_anchor_states.
if [[ "$TRANSPARENT" -eq 1 ]]; then
  if flush_anchor_states "claude-burst"; then
    log "flushed the pf anchor's stale states"
  else
    log "skipped the pf state flush (needs root) -- harmless; the health check does not use the direct port. To test it: sudo $ROOT/scripts/transparent-root.sh flush-port-states"
  fi
fi

if wait_healthy; then
  log "new gateway is healthy"
  if [[ "$DISABLED_FOR_SWAP" -eq 1 ]]; then
    "$TARGET" enable
    log "re-enabled proxy -- restart any Claude Code session for the change to take effect"
  fi
  log "deploy complete"
  exit 0
fi

# --- 6. Rollback: new binary didn't come up healthy ---
echo "[deploy] FAILED: new gateway did not become healthy within ${HEALTH_TIMEOUT}s -- rolling back" >&2
dump_health_diagnostics "new binary, after kickstart"
if [[ -f "$BACKUP_DIR/claude-burst-bin.latest.bak" ]]; then
  cp "$BACKUP_DIR/claude-burst-bin.latest.bak" "$TARGET"
  chmod 755 "$TARGET"
  launchctl kickstart -k "gui/$UID/$LABEL" >/dev/null 2>&1
  if wait_healthy; then
    if [[ "$DISABLED_FOR_SWAP" -eq 1 ]]; then
      "$TARGET" enable
    fi
    echo "[deploy] rolled back to previous binary, it is healthy, proxy re-enabled" >&2
    exit 0
  fi
  dump_health_diagnostics "rollback binary, after kickstart"
fi

# Both the new binary and the rollback (or there was no backup to roll back
# to) failed the health check. What's actually safe to claim here depends
# entirely on intercept mode -- see the top-of-file note -- and, in
# transparent mode, on whether disable() actually ran (DISABLED_FOR_SWAP):
# it never does there by design (see step 3), so CA trust was never
# disturbed by THIS script. An unconditional "everything is broken, run
# this now" here would itself be inaccurate -- the health check's own
# direct-127.0.0.1:7777 probe has an intermittent, still-unexplained
# false-negative gap (see health-diagnostics.sh), so a failed health check
# in transparent mode is not on its own proof of a real outage the way it
# would be if disable() HAD run. Verify before treating this as an
# emergency, not after.
if [[ "$TRANSPARENT" -eq 1 ]]; then
  if [[ "$DISABLED_FOR_SWAP" -eq 1 ]]; then
    echo "[deploy] gateway is down and the machine-wide pf redirect + /etc/hosts entry are STILL ACTIVE -- this is not a 'disabled, direct to Anthropic' state, every request to api.anthropic.com on this Mac is currently broken. Run now:" >&2
    echo "           sudo $ROOT/scripts/transparent-root.sh remove" >&2
    echo "         or: bash $ROOT/scripts/rollback.sh   (also restores config/CA backups)" >&2
  else
    INTERCEPT_HOST="$(python3 -c "import json;print(json.load(open('$HOME/.config/claude-burst/config.json')).get('intercept',{}).get('host','api.anthropic.com'))" 2>/dev/null || echo "api.anthropic.com")"
    echo "[deploy] health check failed, but CA trust was never touched (transparent mode skips disable/enable around the swap) -- this may be the health check's own known intermittent false-negative, not a real outage. Verify directly before assuming anything is broken:" >&2
    echo "           curl -sk https://127.0.0.1:7777/healthz                          # direct -- the check that just failed" >&2
    echo "           curl -s --cacert \$NODE_EXTRA_CA_CERTS https://$INTERCEPT_HOST/healthz   # the REAL traffic path Claude Code actually uses" >&2
    echo "         If the second one succeeds, the gateway is fine. If both genuinely fail, then: sudo $ROOT/scripts/transparent-root.sh remove" >&2
  fi
else
  echo "[deploy] leaving proxy disabled (Claude Code -> direct Anthropic) so Claude access is not lost." >&2
fi
echo "[deploy] Investigate $HOME/.config/claude-burst/claude-burst.log, Console.app crash reports, and" >&2
echo "         $HOME/.config/claude-burst/health-check-failures.log (captured automatically, above)." >&2
exit 1
