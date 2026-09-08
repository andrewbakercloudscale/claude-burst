#!/bin/zsh
# Read-only diagnostic for issue #1: while transparent mode's pf rdr rule is
# loaded, direct connections to the rdr's TARGET port fail ~19 times in 20,
# while the real path through :443 to the same socket is perfect.
#
# What this settles, that nothing else has: WHICH pf subsystem drops the SYN.
# pf maintains per-reason counters, so running a burst of failing probes
# between two counter snapshots names the responsible one instead of leaving
# us to guess. Earlier guesses -- a security product blocklisting the port
# number, and a missing filter `pass` rule -- were both wrong, and both would
# have been ruled out in a minute by this output.
#
# It CHANGES NOTHING: every pfctl call here is a read (-s). It does not load,
# flush, enable or disable anything.
#
# Usage: sudo scripts/diagnose-direct-port.sh [probe_count]
set -uo pipefail

PROBES="${1:-20}"
OUT="$HOME/.config/claude-burst/direct-port-diagnosis-$(date +%Y%m%d-%H%M%S).log"
[[ $EUID -eq 0 ]] || { echo "must run as root: sudo $0 $*" >&2; exit 1; }

# The invoking user's home, not root's -- sudo keeps HOME by default on macOS
# but do not rely on it for the file we tell them to read.
real_home="$(eval echo "~${SUDO_USER:-$USER}")"
OUT="$real_home/.config/claude-burst/direct-port-diagnosis-$(date +%Y%m%d-%H%M%S).log"
mkdir -p "$(dirname "$OUT")"

gport="$(python3 -c "import json;print(json.load(open('$real_home/.config/claude-burst/config.json')).get('listen','').rsplit(':',1)[-1])" 2>/dev/null || true)"
gport="${gport:-7777}"
host="$(python3 -c "import json;print(json.load(open('$real_home/.config/claude-burst/config.json')).get('intercept',{}).get('host','api.anthropic.com'))" 2>/dev/null || echo api.anthropic.com)"

probe_direct() {
  local ok=0 i
  for i in $(seq 1 "$PROBES"); do
    curl -sk -m 3 -o /dev/null "https://127.0.0.1:$gport/healthz" 2>/dev/null && ok=$((ok+1))
  done
  print -r -- "$ok"
}
probe_real() {
  local ok=0 i
  for i in $(seq 1 10); do
    curl -sk -m 3 -o /dev/null "https://$host/healthz" 2>/dev/null && ok=$((ok+1))
  done
  print -r -- "$ok"
}

{
  echo "===== direct-port diagnosis $(date '+%Y-%m-%d %H:%M:%S %Z') ====="
  echo "gateway port: $gport   intercepted host: $host   probes: $PROBES"
  echo
  echo "--- 1. rules as ACTUALLY LOADED (not what the files say) ---"
  echo "# main filter ruleset:";      pfctl -s rules 2>&1
  echo "# main translation ruleset:"; pfctl -s nat   2>&1
  echo "# our anchor:";               pfctl -a claude-burst -s nat 2>&1
  echo
  echo "--- 2. states mentioning the gateway port, BEFORE ---"
  pfctl -s state 2>/dev/null | grep -F ":$gport" || echo "(none)"
  echo
  echo "--- 3. counters BEFORE ---"
  pfctl -s info 2>&1
  echo
  echo "--- 4. running $PROBES direct probes (expected to mostly fail) ---"
  direct_ok="$(probe_direct)"
  echo "direct  127.0.0.1:$gport -> $direct_ok/$PROBES succeeded"
  real_ok="$(probe_real)"
  echo "real    https://$host    -> $real_ok/10 succeeded  (control)"
  echo
  echo "--- 5. counters AFTER ---"
  pfctl -s info 2>&1
  echo
  echo "--- 6. states mentioning the gateway port, AFTER ---"
  pfctl -s state 2>/dev/null | grep -F ":$gport" || echo "(none)"
  echo
  echo "READ THIS: diff sections 3 and 5. Whichever Counter moved by roughly"
  echo "$PROBES is the drop reason -- state-mismatch, no-route, and the block"
  echo "counters each mean something quite different. If NOTHING moved, pf is"
  echo "not dropping these packets at all and the cause is above or below it."
} 2>&1 | tee "$OUT"

echo
echo "saved: $OUT"
chown "${SUDO_USER:-$USER}" "$OUT" 2>/dev/null
