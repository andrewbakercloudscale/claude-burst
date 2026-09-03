#!/bin/zsh
# One-shot diagnostic for the open TLS handshake-error storm documented in
# INVESTIGATION-TLS-STORM.md: identifies which process is actually making
# the connections that the gateway rejects, using the exact dtrace probe
# the investigation already specified as the definitive next step.
#
# This only captures -- it does not install, remove, or change anything.
# It requires transparent mode to be ACTIVELY installed (see
# install-proxy.sh) for the storm to reproduce at all: the failing
# connections only exist because something's TLS client got transparently
# redirected to the gateway and rejected its certificate.
#
# Needs root (dtrace). Run this yourself, interactively -- it prompts for
# your password and then sits capturing for DURATION seconds. If dtrace
# fails immediately with "failed to initialize dtrace: DTrace requires
# additional privileges", System Integrity Protection is blocking it on
# this machine and no capture is possible without disabling SIP, which
# this script will not do for you.
#
# Usage: sudo scripts/capture-tls-storm-client.sh [duration_seconds]
set -uo pipefail

DURATION="${1:-90}"
OUT="$HOME/.config/claude-burst/tls-storm-capture-$(date +%Y%m%d-%H%M%S).log"

if [[ $EUID -ne 0 ]]; then
  echo "needs root -- run: sudo $0 $*" >&2
  exit 1
fi

mkdir -p "$HOME/.config/claude-burst"
echo "capturing connect() calls to 127.0.0.1:7777 for ${DURATION}s..."
echo "output: $OUT"
echo "(today's bursts have been recurring every few minutes -- this window should catch one)"
echo

dtrace -n '
  syscall::connect:entry
  /((struct sockaddr_in *)copyin(arg1, arg2))->sin_port == htons(7777)/
  { printf("%Y  %s [pid %d, ppid %d]\n", walltimestamp, execname, pid, ppid); }
' > "$OUT" 2>&1 &
DPID=$!

# dtrace needs a moment to attach before we can trust it's actually running.
sleep 2
if ! kill -0 "$DPID" 2>/dev/null; then
  echo "dtrace exited immediately -- likely SIP-blocked. Output:" >&2
  cat "$OUT" >&2
  exit 1
fi

sleep "$((DURATION - 2))"
kill "$DPID" 2>/dev/null
wait "$DPID" 2>/dev/null

echo "== capture complete =="
if [[ -s "$OUT" ]] && grep -q 'pid' "$OUT"; then
  cat "$OUT"
  echo
  echo "== by process =="
  grep -oE '^\S+  \S+' "$OUT" | awk '{print $2}' | sort | uniq -c | sort -rn
else
  echo "no connect() calls to 127.0.0.1:7777 seen in this window -- the storm didn't fire" \
       "during the capture. Re-run with a longer duration or try again when it's active."
fi
