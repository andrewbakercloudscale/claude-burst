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
    echo "-- claude-burst.log, last 15 lines (bind timing is logged explicitly since 2026-08-31) --"
    tail -15 "$HOME/.config/claude-burst/claude-burst.log" 2>&1
    echo "-- launchd.err.log, last 10 lines --"
    tail -10 "$HOME/.config/claude-burst/launchd.err.log" 2>&1
    echo
  } >> "$out" 2>&1
  echo "diagnostics for this failure appended to $out"
}
