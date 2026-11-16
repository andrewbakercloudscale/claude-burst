#!/bin/zsh
# Sourced, not run. reload_launchagent LABEL PLIST boots a per-user
# LaunchAgent out and back in, and only returns 0 once launchd has loaded it.
#
# bootout returns before the job is gone: the gateway drains on SIGTERM (up
# to 50s with a reply streaming, ~100ms idle, about 11s measured on
# 2026-09-30), and until it exits launchd still holds the label. A bootstrap
# in that window fails with a bare "Input/output error", kickstart then
# finds nothing loaded, and the gateway stays down. That is how both Install
# clicks on 2026-09-30 (15:11, 15:18) stopped a healthy gateway and
# installed nothing. So wait for the label to disappear, and retry bootstrap
# rather than trusting one attempt.
#
# Something else could load the label in that window: the self-heal
# watchdog saw the gateway gone and bootstrapped the same plist one second
# after bootout. Every bootstrap here then failed with the same "Input/output
# error" while a fresh gateway was serving, and the install stopped, said
# "nothing past this point was changed" after it had stopped the gateway,
# and left the redirect off (2026-11-16 09:55). So:
#   - the planned-restart marker is written first, and the watchdog leaves a
#     planned restart alone; it is removed again if this fails, so a failed
#     reload does not keep the watchdog away;
#   - a job running under a pid other than the one booted out counts as
#     loaded, whoever loaded it;
#   - a final failure says what launchd actually holds, not just the error.
#
# internal/integration/launchagent_reload_test.go runs this against a
# stubbed launchctl; keep the two in step.

_launchagent_pid() {
  launchctl list "$1" 2>/dev/null | awk -F'[ ;]+' '/"PID" =/ {print $3; exit}'
}

reload_launchagent() {
  local label="$1" plist="$2" i bootstrap_err old_pid now_pid
  local marker="${CLAUDE_BURST_PLANNED_RESTART:-$HOME/.config/claude-burst/planned-restart}"
  mkdir -p "${marker:h}" 2>/dev/null
  touch "$marker" 2>/dev/null
  old_pid="$(_launchagent_pid "$label")"
  launchctl bootout "gui/$UID/$label" >/dev/null 2>&1 || true
  for i in $(seq 1 65); do
    launchctl print "gui/$UID/$label" >/dev/null 2>&1 || break
    (( i == 1 )) && echo "waiting for the old gateway (pid ${old_pid:-none}) to finish draining and unload..."
    sleep 1
  done
  for i in 1 2 3 4 5; do
    if bootstrap_err="$(launchctl bootstrap "gui/$UID" "$plist" 2>&1)"; then
      launchctl kickstart -k "gui/$UID/$label"
      return 0
    fi
    now_pid="$(_launchagent_pid "$label")"
    if [[ -n "$now_pid" && "$now_pid" != "$old_pid" ]]; then
      echo "$label is already loaded again and running (pid $now_pid, replacing pid ${old_pid:-none}): something else, usually the self-heal watchdog, loaded the same plist first"
      return 0
    fi
    # launchd's own hint ("re-run as root") is wrong for a per-user agent.
    bootstrap_err="$(print -r -- "$bootstrap_err" | grep -v 'richer errors')"
    echo "bootstrap attempt $i of 5 failed: $bootstrap_err" >&2
    sleep 2
  done
  rm -f "$marker"
  _reload_failure_report "$label" "$plist" "$old_pid" >&2
  return 1
}

# What launchd holds after the last refusal. "Input/output error" alone
# names none of the causes it stands for.
_reload_failure_report() {
  local label="$1" plist="$2" old_pid="$3" now_pid state
  now_pid="$(_launchagent_pid "$label")"
  echo "ERROR: launchd would not load $label from $plist"
  if launchctl print "gui/$UID/$label" >/dev/null 2>&1; then
    state="$(launchctl print "gui/$UID/$label" 2>/dev/null | awk '/^[[:space:]]*state = / {print $3; exit}')"
    if [[ -n "$now_pid" && "$now_pid" == "$old_pid" ]]; then
      echo "  launchd still holds the old job (pid $now_pid, state ${state:-unknown}): it did not exit within 65s of bootout"
    else
      echo "  the label is still registered (state ${state:-unknown}, pid ${now_pid:-none}), so every bootstrap is refused"
    fi
  else
    echo "  the label is not registered at all, and launchd still refused it"
  fi
  if launchctl print-disabled "gui/$UID" 2>/dev/null | grep -q "\"$label\" => disabled"; then
    echo "  the service is DISABLED in launchd: launchctl enable gui/$UID/$label"
  fi
  plutil -lint "$plist" >/dev/null 2>&1 || echo "  the plist does not parse: plutil -lint $plist"
  echo "  the gateway was stopped by this attempt; the self-heal watchdog reloads it within a minute, or run: burst-repair"
}
