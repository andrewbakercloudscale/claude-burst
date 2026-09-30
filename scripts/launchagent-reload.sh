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
# internal/integration/launchagent_reload_test.go runs this against a
# stubbed launchctl; keep the two in step.

reload_launchagent() {
  local label="$1" plist="$2" i bootstrap_err
  launchctl bootout "gui/$UID/$label" >/dev/null 2>&1 || true
  for i in $(seq 1 65); do
    launchctl print "gui/$UID/$label" >/dev/null 2>&1 || break
    (( i == 1 )) && echo "waiting for the old gateway to finish draining and unload..."
    sleep 1
  done
  for i in 1 2 3 4 5; do
    if bootstrap_err="$(launchctl bootstrap "gui/$UID" "$plist" 2>&1)"; then
      launchctl kickstart -k "gui/$UID/$label"
      return 0
    fi
    echo "bootstrap attempt $i failed: $bootstrap_err" >&2
    sleep 2
  done
  echo "ERROR: launchd would not load $label" >&2
  return 1
}
