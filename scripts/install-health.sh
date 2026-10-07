# Sourced by install.sh after it restarts the gateway on a new binary. Not
# run by itself.
#
# install.sh used to stop at the restart: a build that passed its --help
# smoke test and then died on start (a config it cannot read, a port it
# cannot take, a crash in start-up) was left installed, and burst-repair and
# burst-reinstall, which both end in install.sh, could leave a Mac worse off
# than they found it. scripts/deploy.sh has always checked and gone back;
# this is the same for an install.
#
# Needs gateway_healthy from health-diagnostics.sh.

# burst_wait_healthy SECONDS: 0 as soon as the gateway answers /healthz.
burst_wait_healthy() {
  local limit="$1" waited=0
  while (( waited < limit )); do
    gateway_healthy && return 0
    sleep "${CLAUDE_BURST_INSTALL_HEALTH_SLEEP:-1}"
    waited=$((waited + 1))
  done
  return 1
}

# burst_health_or_rollback TARGET BACKUP LABEL SHA_FILE OLD_SHA
#
# Waits for the gateway now running TARGET to answer. If it does not and
# BACKUP (the binary TARGET replaced) exists, puts BACKUP back and restarts.
# Returns:
#   0  the new binary is healthy
#   1  not healthy and nothing to go back to (a first install)
#   2  not healthy; the previous binary is back and IS healthy
#   3  not healthy on either binary, so it is not the build; the previous
#      binary is the one left in place
burst_health_or_rollback() {
  local target="$1" backup="$2" label="$3" sha_file="$4" old_sha="$5"
  local limit="${CLAUDE_BURST_INSTALL_HEALTH_TIMEOUT:-30}"
  burst_wait_healthy "$limit" && return 0

  echo "ERROR: the gateway did not answer within ${limit}s of starting on the new binary." >&2
  if [[ ! -f "$backup" ]]; then
    echo "  There is no previous binary to go back to. Run scripts/diagnose.sh; the end of" >&2
    echo "  ~/.config/claude-burst/launchd.err.log says why it stopped." >&2
    return 1
  fi
  echo "  Going back to the binary it replaced ($backup)..." >&2
  # A copy beside it and a rename, never cp over the target: rewriting a
  # running binary's bytes is what macOS's code signing kills.
  cp "$backup" "$target.rollback.tmp" && chmod 755 "$target.rollback.tmp" && mv -f "$target.rollback.tmp" "$target" || {
    echo "  ERROR: could not put the previous binary back. burst-off takes Burst out of the path." >&2
    return 3
  }
  if [[ -n "$old_sha" ]]; then printf '%s\n' "$old_sha" > "$sha_file"; else rm -f "$sha_file"; fi
  launchctl kickstart -k "gui/$UID/$label" >/dev/null 2>&1 || true
  if burst_wait_healthy "$limit"; then
    echo "  The previous binary is back and answering. The new build was NOT installed:" >&2
    echo "  the end of ~/.config/claude-burst/launchd.err.log says why it stopped." >&2
    return 2
  fi
  echo "  The previous binary does not answer either, so the build is not the cause." >&2
  echo "  Run scripts/diagnose.sh for what is; burst-off takes Burst out of the path meanwhile." >&2
  return 3
}
