#!/bin/zsh
# Shared by deploy.sh and watchdog.sh (source, don't run directly): captures
# a snapshot of everything relevant to a health-check failure, at the moment
# it happens, into a persistent append-only log.
#
# Written because of an intermittent, still-unexplained gap first seen
# 2026-08-31: direct 127.0.0.1:7777 connections sometimes time out at the
# raw TCP level for up to ~15s right after a restart, then work normally
# again, while the real traffic path (through the intercepted hostname on
# 443, pf-redirected) is often unaffected at the very same moment. Ruled out
# so far: the macOS Application Firewall (confirmed disabled), other
# pf-touching software (none running), and the claude-burst pf anchor's own
# rule (it only redirects port 443, never touches 7777 as a destination).
# Root-causing it further needs `sudo pfctl -a claude-burst -sr`/`-ss` state
# at the exact moment of failure, which nothing here can run
# non-interactively -- so instead of re-investigating live under time
# pressure next time, the evidence is captured automatically, every time,
# whether anyone is watching or not.
#
# Usage: dump_health_diagnostics "<label describing what just failed>"

# gateway_healthy: the ONE liveness check, shared by deploy.sh and watchdog.sh.
#
# Both used to probe only 127.0.0.1:7777 directly, and both were wrong in the
# same way. Direct connections to the gateway port intermittently have their
# SYNs dropped while the gateway is perfectly healthy -- measured 2026-09-03
# with the listen queue empty (netstat -L: 0/0/4096) and the pf-redirected
# path answering the SAME socket in 28ms at the same instant. See
# https://github.com/andrewbakercloudscale/claude-burst/issues/1; the cause is
# still unconfirmed, but the consequence was not theoretical: deploy.sh rolled
# back a healthy build, and watchdog.sh will auto-run rollback.sh -- unattended
# -- on the same false negative.
#
# So: direct first (fast, and the only option in base-url mode or before the
# redirect is installed), then the real traffic path as a fallback.
#
# The fallback is mode-agnostic and cannot produce a false POSITIVE, which is
# why it greps the body instead of trusting a status code. In transparent mode
# the hostname resolves to 127.0.0.1 and only our gateway can answer with the
# "overflow" field. In base-url mode there is no /etc/hosts entry, so the
# request reaches the real Anthropic -- whose /healthz does not return our JSON
# and therefore does not match. A gateway that is genuinely down fails both.
gateway_healthz_body() {
  local host="api.anthropic.com"
  if command -v python3 >/dev/null 2>&1 && [ -f "$HOME/.config/claude-burst/config.json" ]; then
    host="$(python3 -c "import json;print(json.load(open('$HOME/.config/claude-burst/config.json')).get('intercept',{}).get('host','api.anthropic.com'))" 2>/dev/null || echo "api.anthropic.com")"
  fi
  curl -sk -m 3 "https://$host/healthz" 2>/dev/null
}

gateway_healthy() {
  curl -skf -m 3 "https://127.0.0.1:7777/healthz" >/dev/null 2>&1 && return 0
  curl -sf  -m 3 "http://127.0.0.1:7777/healthz"  >/dev/null 2>&1 && return 0
  case "$(gateway_healthz_body)" in
    *'"overflow"'*) return 0 ;;
  esac
  return 1
}

# flush_anchor_states: drop the pf states belonging to one anchor, if we can
# do it without prompting. Returns 0 if the flush ran, 1 if it was skipped.
#
# Why: direct connections to the gateway port are intermittently dropped
# after a restart while the gateway is healthy, and the anchor's state table
# carries entries that should not exist for a rule that only ever redirects
# :443 -> :7777 -- including one with the gateway port on BOTH sides. The
# working theory is that a new direct SYN matches a leftover state and is
# reverse-translated back to :443, where nothing listens.
#
# THE THEORY IS NOT CONFIRMED. Run `sudo transparent-root.sh flush-port-states`
# to test it properly; this call is a best-effort cleanup, not a fix, and
# nothing in the deploy depends on it -- gateway_healthy() stopped relying on
# the direct probe precisely so it would not have to.
#
# Never prompts and never fails a caller: pfctl needs root, deploy.sh
# deliberately runs unprivileged (transparent-root.sh exists for exactly that
# split), and a password prompt between installing a new binary and
# restarting it is the last thing a deploy should stop on. `sudo -n` either
# works because credentials are already cached or fails instantly.
#
# Scoped to the one anchor on purpose. Machine-wide `pfctl -F states` drops
# every tracked connection on the host.
flush_anchor_states() {
  local anchor="${1:-claude-burst}"
  command -v pfctl >/dev/null 2>&1 || return 1
  if [ "$(id -u)" = "0" ]; then
    pfctl -a "$anchor" -F states >/dev/null 2>&1
    return 0
  fi
  if sudo -n true >/dev/null 2>&1; then
    sudo -n pfctl -a "$anchor" -F states >/dev/null 2>&1
    return 0
  fi
  return 1
}

# launchagent_disabled: true if launchd's own database has the gateway's
# LaunchAgent marked disabled (from `launchctl disable` -- e.g. the external,
# non-repo rollback.sh some machines still have calls this; this repo's own
# rollback.sh never does).
launchagent_disabled() {
  local svc_label="${LABEL:-ninja.andrewbaker.claude-burst}"
  launchctl print-disabled "gui/$UID" 2>/dev/null | grep -q "\"$svc_label\" => disabled"
}

# launchagent_loaded: true if the LaunchAgent is currently loaded into
# launchd at all (bootstrapped), independent of whether it's disabled.
launchagent_loaded() {
  local svc_label="${LABEL:-ninja.andrewbaker.claude-burst}"
  launchctl print "gui/$UID/$svc_label" >/dev/null 2>&1
}

# ensure_launchagent_loaded recovers from the one failure mode that looks
# exactly like a broken build but isn't: the LaunchAgent disabled or
# unloaded entirely in launchd's own database. `launchctl kickstart -k`
# against a service in that state fails outright with no such service, and
# every symptom downstream -- the new binary AND an identical rollback
# binary both failing the same health check -- looks exactly like "the new
# build is broken", because kickstart never had anything to restart either
# time. Confirmed live 2026-09-03: a deploy rolled back a good build over
# exactly this, having been left disabled by an earlier rollback.sh run.
#
# Returns 0 if the agent was already loaded (nothing to do) or recovery
# succeeded; 1 if recovery was attempted but did not stick, so the caller's
# own kickstart/health-check failure can be reported for what it actually
# is instead of blamed on the build.
ensure_launchagent_loaded() {
  local svc_label="${LABEL:-ninja.andrewbaker.claude-burst}"
  local plist="$HOME/Library/LaunchAgents/$svc_label.plist"
  if launchagent_loaded; then
    return 0
  fi
  echo "[deploy] $svc_label is not loaded in launchd -- this is not a build problem; checking why before assuming one" >&2
  if launchagent_disabled; then
    echo "[deploy] $svc_label is marked disabled in launchd's own database (most likely a prior rollback called \`launchctl disable\`) -- re-enabling" >&2
    launchctl enable "gui/$UID/$svc_label" >&2
  fi
  if [[ ! -f "$plist" ]]; then
    echo "[deploy] no plist found at $plist -- cannot load it automatically" >&2
    return 1
  fi
  launchctl bootstrap "gui/$UID" "$plist" >&2
  launchagent_loaded
}

dump_health_diagnostics() {
  local label="${1:-health check}"
  local out="$HOME/.config/claude-burst/health-check-failures.log"
  local svc_label="${LABEL:-ninja.andrewbaker.claude-burst}"
  mkdir -p "$(dirname "$out")"
  {
    echo "===== $(date -u '+%Y-%m-%dT%H:%M:%SZ') health check failed: $label ====="
    echo "-- direct 127.0.0.1:7777, https then http (verbose, what actually happened) --"
    curl -v -k -m 3 "https://127.0.0.1:7777/healthz" 2>&1 | tail -15
    curl -v -m 3 "http://127.0.0.1:7777/healthz" 2>&1 | tail -15
    echo "-- real traffic path for comparison (same moment): via the intercepted hostname, port 443 --"
    if command -v python3 >/dev/null 2>&1 && [[ -f "$HOME/.config/claude-burst/config.json" ]]; then
      local doh_host
      doh_host="$(python3 -c "import json;print(json.load(open('$HOME/.config/claude-burst/config.json')).get('intercept',{}).get('host','api.anthropic.com'))" 2>/dev/null || echo "api.anthropic.com")"
      curl -sk -m 3 -o /dev/null -w '%{url_effective} -> HTTP %{http_code} in %{time_total}s\n' "https://$doh_host/healthz" 2>&1
    fi
    echo "-- lsof -iTCP:7777 --"
    lsof -nP -iTCP:7777 2>&1
    echo "-- launchctl list $svc_label --"
    launchctl list "$svc_label" 2>&1
    echo "-- launchagent loaded / disabled --"
    if launchagent_loaded; then echo "loaded: yes"; else echo "loaded: NO -- kickstart cannot restart what isn't loaded"; fi
    if launchagent_disabled; then echo "disabled in launchd's database: YES -- this is very likely the real cause, not the binary"; else echo "disabled in launchd's database: no"; fi
    echo "-- claude-burst.log, last 15 lines (bind timing is logged explicitly since 2026-08-31) --"
    tail -15 "$HOME/.config/claude-burst/claude-burst.log" 2>&1
    echo "-- launchd.err.log, last 10 lines --"
    tail -10 "$HOME/.config/claude-burst/launchd.err.log" 2>&1
    echo
  } >> "$out" 2>&1
  echo "diagnostics for this failure appended to $out"
}
