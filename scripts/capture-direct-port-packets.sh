#!/bin/zsh
# Read-only packet capture for issue #1: while transparent mode's pf rdr rule
# is loaded, direct connections to the rdr's TARGET port fail ~19 times in 20,
# while the real path through :443 to the same socket is perfect.
#
# WHY THIS AND NOT ANOTHER CANDIDATE FIX. Four hypotheses have been proposed
# on this issue and all four were wrong (see INVESTIGATION-TLS-STORM.md,
# updates (a), (b) and (c)): a port blocklist, a plain filter `pass`, state
# mismatch, and a `no state` rule. Every one of them argued from how pf ought
# to behave. The two things that ever moved the investigation were
# measurements -- diagnose-direct-port.sh naming state-insert as the drop
# reason, and experiment-nostate-rule.sh eliminating the filter path.
#
# THE QUESTION NONE OF THE FOUR ADDRESSED. pf's counters say state insertion
# fails. They do not say what the two endpoints see. This capture answers,
# for the direct path and the working real path side by side:
#
#   1. Does the SYN appear on lo0 at all, or is it dropped before it gets there?
#   2. Does anything come back -- SYN-ACK, RST, or silence?
#   3. If a RST, does it carry the gateway's port as source (the socket
#      refusing) or something else (pf or the stack synthesising it)?
#   4. Do the SYN retransmissions match the +14-per-probe state-insert rise,
#      which would tie the counter to these exact packets rather than to
#      unrelated traffic in the same window?
#
# Silence after the SYN means the packet died inside pf. A RST from :GPORT
# means the socket itself said no, which would move the whole investigation
# off pf and onto the listener. Those two outcomes point in opposite
# directions and no amount of reasoning about pf semantics distinguishes
# them.
#
# IT CHANGES NOTHING. tcpdump is a passive reader and there is not a single
# pfctl write, load or flush here. Safe to run mid-incident. The one thing it
# does touch is a capture file it creates under the invoking user's config
# directory.
#
# Usage: sudo scripts/capture-direct-port-packets.sh [probe_count]
set -uo pipefail

PROBES="${1:-10}"

# Root is NOT required if this account can already open a BPF device -- macOS
# grants that through the access_bpf group, which anything that has installed
# Wireshark's ChmodBPF (and this machine) will have. Demanding sudo anyway
# would refuse to run for a user who can capture perfectly well, and sending
# someone to fetch a password they do not need is how a diagnostic goes unrun.
# The capability is TESTED rather than inferred from group membership, since
# the group is the usual route to it but not the only one.
HAVE_PF_READ=1
if [[ $EUID -ne 0 ]]; then
  HAVE_PF_READ=0
  # A filter that matches nothing, so this only ever tests whether the BPF
  # device opens. tcpdump exits immediately when it cannot; it stays alive
  # waiting for packets when it can.
  tcpdump -Uni lo0 -w /dev/null 'tcp port 65535' >/dev/null 2>&1 &
  probe_pid=$!
  sleep 1
  if kill -0 "$probe_pid" 2>/dev/null; then
    kill "$probe_pid" 2>/dev/null
    wait "$probe_pid" 2>/dev/null
  else
    echo "cannot capture on lo0 as this user, and not running as root." >&2
    echo "re-run with: sudo $0 $*" >&2
    exit 1
  fi
fi

real_home="$(eval echo "~${SUDO_USER:-$USER}")"
cfg="$real_home/.config/claude-burst/config.json"
stamp="$(date +%Y%m%d-%H%M%S)"
OUT="$real_home/.config/claude-burst/direct-port-packets-$stamp.log"
PCAP_DIRECT="$real_home/.config/claude-burst/direct-$stamp.pcap"
PCAP_REAL="$real_home/.config/claude-burst/real-$stamp.pcap"
mkdir -p "$(dirname "$OUT")"

gport="$(python3 -c "import json;print(json.load(open('$cfg')).get('listen','').rsplit(':',1)[-1])" 2>/dev/null || true)"
gport="${gport:-7777}"
host="$(python3 -c "import json;print(json.load(open('$cfg')).get('intercept',{}).get('host','api.anthropic.com'))" 2>/dev/null || echo api.anthropic.com)"

TCPDUMP_PID=""
cleanup() {
  [[ -n "$TCPDUMP_PID" ]] && kill "$TCPDUMP_PID" 2>/dev/null
  wait "$TCPDUMP_PID" 2>/dev/null
  TCPDUMP_PID=""
}
trap 'cleanup; echo; echo "interrupted -- capture stopped, nothing was changed"' INT TERM
trap cleanup EXIT

# Capture, probe, stop. Returns the number of successful probes on stdout so
# the caller can pair "how many worked" with "what the wire showed".
capture_burst() {  # $1 = pcap path, $2 = filter, $3 = url, $4 = count
  local pcap="$1" filter="$2" url="$3" count="$4" ok=0 i
  # -U writes each packet through as it is captured, so a SIGTERM cannot
  # discard a buffer's worth of the evidence we are here for.
  tcpdump -Uni lo0 -s 128 -w "$pcap" "$filter" >/dev/null 2>&1 &
  TCPDUMP_PID=$!
  # tcpdump needs a moment to attach to the interface; probes fired before
  # it is listening produce an empty capture and a confusing "no packets"
  # conclusion that has nothing to do with pf.
  sleep 2
  if ! kill -0 "$TCPDUMP_PID" 2>/dev/null; then
    # An empty capture reads exactly like "pf dropped everything", which is
    # the single most misleading thing this script could print. Say so
    # instead of letting a dead tcpdump masquerade as a finding.
    echo "  ERROR: tcpdump exited immediately -- capture is empty for that reason," >&2
    echo "  not because no packets were sent. Filter was: $filter" >&2
  fi
  for i in $(seq 1 "$count"); do
    curl -sk -m 3 -o /dev/null "$url" 2>/dev/null && ok=$((ok+1))
  done
  sleep 1
  cleanup
  print -r -- "$ok"
}

{
  echo "===== direct-port packet capture $(date '+%Y-%m-%d %H:%M:%S %Z') ====="
  echo "gateway port: $gport   intercepted host: $host   probes per phase: $PROBES"
  echo

  echo "== 0. is the rdr actually loaded? =="
  # If it is not, both paths will look fine and the capture proves nothing --
  # the symptom only exists while the redirect is installed. /dev/pf is
  # root-only, so without root this is stated as unknown rather than quietly
  # skipped: "no output" here must not read as "no rdr rule".
  if (( HAVE_PF_READ )); then
    pfctl -a claude-burst -s nat 2>/dev/null | sed 's/^/  /'
  else
    echo "  UNKNOWN -- reading pf rules needs root and this run is unprivileged."
    echo "  Capture below is still valid; confirm the rdr separately with:"
    echo "    sudo pfctl -a claude-burst -s nat"
  fi
  echo

  echo "== 1. FAILING path: direct to 127.0.0.1:$gport =="
  ok_direct="$(capture_burst "$PCAP_DIRECT" "tcp port $gport" "https://127.0.0.1:$gport/healthz" "$PROBES")"
  echo "  probes succeeded: $ok_direct/$PROBES"
  echo "  packets:"
  tcpdump -nr "$PCAP_DIRECT" 2>/dev/null | sed 's/^/    /'
  echo
  echo "  summary:"
  echo "    total packets : $(tcpdump -nr "$PCAP_DIRECT" 2>/dev/null | wc -l | tr -d ' ')"
  echo "    SYNs          : $(tcpdump -nr "$PCAP_DIRECT" 'tcp[tcpflags] & tcp-syn != 0 and tcp[tcpflags] & tcp-ack == 0' 2>/dev/null | wc -l | tr -d ' ')"
  echo "    SYN-ACKs      : $(tcpdump -nr "$PCAP_DIRECT" 'tcp[tcpflags] & tcp-syn != 0 and tcp[tcpflags] & tcp-ack != 0' 2>/dev/null | wc -l | tr -d ' ')"
  echo "    RSTs          : $(tcpdump -nr "$PCAP_DIRECT" 'tcp[tcpflags] & tcp-rst != 0' 2>/dev/null | wc -l | tr -d ' ')"
  echo "  RSTs in full (the source port is the whole question -- :$gport means"
  echo "  the socket refused; anything else means it was synthesised):"
  tcpdump -nr "$PCAP_DIRECT" 'tcp[tcpflags] & tcp-rst != 0' 2>/dev/null | sed 's/^/    /'
  echo

  echo "== 2. CONTROL, working path: https://$host (:443 -> :$gport) =="
  ok_real="$(capture_burst "$PCAP_REAL" "tcp port 443 or tcp port $gport" "https://$host/healthz" "$PROBES")"
  echo "  probes succeeded: $ok_real/$PROBES"
  echo "  packets:"
  tcpdump -nr "$PCAP_REAL" 2>/dev/null | sed 's/^/    /'
  echo
  echo "  summary:"
  echo "    total packets : $(tcpdump -nr "$PCAP_REAL" 2>/dev/null | wc -l | tr -d ' ')"
  echo "    SYNs          : $(tcpdump -nr "$PCAP_REAL" 'tcp[tcpflags] & tcp-syn != 0 and tcp[tcpflags] & tcp-ack == 0' 2>/dev/null | wc -l | tr -d ' ')"
  echo "    SYN-ACKs      : $(tcpdump -nr "$PCAP_REAL" 'tcp[tcpflags] & tcp-syn != 0 and tcp[tcpflags] & tcp-ack != 0' 2>/dev/null | wc -l | tr -d ' ')"
  echo "    RSTs          : $(tcpdump -nr "$PCAP_REAL" 'tcp[tcpflags] & tcp-rst != 0' 2>/dev/null | wc -l | tr -d ' ')"
  echo

  echo "===== HOW TO READ THIS ====="
  echo "Compare phase 1 against phase 2, which is the same socket reached the"
  echo "way that works."
  echo
  echo "  SYN present, nothing back at all"
  echo "    -> pf swallowed it. Consistent with the state-insert counter, and"
  echo "       the investigation stays on pf."
  echo "  SYN present, RST back from 127.0.0.1:$gport"
  echo "    -> the SOCKET refused the connection. pf is not the culprit and"
  echo "       four hypotheses have been aimed at the wrong layer; look at the"
  echo "       listener, and at whether the 'no rdr' rule leaves the packet"
  echo "       addressed to something nothing is bound to."
  echo "  SYN present, RST back from some OTHER address/port"
  echo "    -> synthesised by the stack or pf. The source tells you which."
  echo "  no SYN on lo0 at all"
  echo "    -> it never left the client. Not a pf problem; look above the"
  echo "       socket layer (curl, or the local address chosen)."
  echo
  echo "  SYN retransmissions matter too: diagnose-direct-port.sh measured"
  echo "  state-insert rising ~14 per 3-second probe. If the SYN count here is"
  echo "  about 14 per failing probe, the counter belongs to these packets. If"
  echo "  it is far lower, the counter was measuring something else and the"
  echo "  best evidence in this investigation needs re-reading."
  echo
  echo "raw captures kept for a second look:"
  echo "  $PCAP_DIRECT"
  echo "  $PCAP_REAL"
} 2>&1 | tee "$OUT"

chown "${SUDO_USER:-$USER}" "$OUT" "$PCAP_DIRECT" "$PCAP_REAL" 2>/dev/null
echo
echo "written to $OUT"
