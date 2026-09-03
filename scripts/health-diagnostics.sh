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
