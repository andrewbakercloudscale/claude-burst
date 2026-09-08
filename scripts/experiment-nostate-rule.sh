#!/bin/zsh
# EXPERIMENT (not a fix): does a `no state` filter rule restore direct
# connections to the gateway port while transparent mode's rdr is loaded?
#
# THE EVIDENCE THIS TESTS. issue #1: while the pf rdr rule is loaded, direct
# connections to its TARGET port fail ~19 times in 20, whichever port that is,
# while the real path through :443 to the same socket is perfect.
# scripts/diagnose-direct-port.sh measured why: across 20 failing probes every
# filter and block drop counter stayed at zero while `state-insert` -- pf's
# state-insertion-FAILURE counter -- rose by 290, about one per SYN
# retransmission. pf is not filtering these packets. It cannot insert state
# for them.
#
# THE CANDIDATE. A filter rule that explicitly creates no state, so the
# insertion that fails is never attempted:
#
#   pass quick on lo0 inet proto tcp from any to 127.0.0.1 port <gport> no state
#
# This is NOT the plain `pass` already ruled out. That one was proposed to
# override a block, and there is no block -- /etc/pf.conf and the com.apple
# anchor contain none. The operative word here is `no state`.
#
# It also needs something /etc/pf.conf does not currently have: a FILTER
# anchor line. Our anchor is referenced by `rdr-anchor "claude-burst"` alone,
# so filter rules inside it are loaded and never evaluated -- a fix that looks
# applied and does nothing. The experiment adds `anchor "claude-burst"` beside
# the com.apple filter anchor, and takes it out again.
#
# WHY AN EXPERIMENT AND NOT A COMMIT. This is the fourth hypothesis on issue
# #1 and the first three were all wrong, two of them mine and confidently
# stated. One of them cost a machine-wide reconfiguration. So this measures
# before and after, and reverts unconditionally either way -- including on
# error, interrupt, or a failure of its own verification.
#
# SAFETY. /etc/pf.conf and the anchor file are backed up before any edit and
# restored from those backups by an EXIT trap, whatever happens. The new
# ruleset is validated with `pfctl -n -f` before it is loaded. Immediately
# after loading, the REAL path is re-verified -- that is the machine-wide one,
# and if it has broken, the script reverts at once rather than finishing its
# measurements. Nothing here is left behind on success or on failure.
#
# Usage: sudo scripts/experiment-nostate-rule.sh [probe_count]
set -uo pipefail

PROBES="${1:-15}"
ANCHOR_FILE="/etc/pf.anchors/claude-burst"
PF_CONF="/etc/pf.conf"
FILTER_BEGIN="# BEGIN claude-burst pf-filter-experiment"
FILTER_END="# END claude-burst pf-filter-experiment"

[[ $EUID -eq 0 ]] || { echo "must run as root: sudo $0 $*" >&2; exit 1; }

real_home="$(eval echo "~${SUDO_USER:-$USER}")"
cfg="$real_home/.config/claude-burst/config.json"
gport="$(python3 -c "import json;print(json.load(open('$cfg')).get('listen','').rsplit(':',1)[-1])" 2>/dev/null || true)"
gport="${gport:-7777}"
host="$(python3 -c "import json;print(json.load(open('$cfg')).get('intercept',{}).get('host','api.anthropic.com'))" 2>/dev/null || echo api.anthropic.com)"

BACKUP="/etc/claude-burst/experiment-$(date +%Y%m%d-%H%M%S)"
mkdir -p "$BACKUP" || { echo "cannot create $BACKUP" >&2; exit 1; }

REVERTED=0
revert() {
  (( REVERTED )) && return 0
  REVERTED=1
  echo
  echo "== reverting =="
  [[ -f "$BACKUP/pf.conf" ]]     && cp "$BACKUP/pf.conf" "$PF_CONF"
  [[ -f "$BACKUP/anchor" ]]      && cp "$BACKUP/anchor" "$ANCHOR_FILE"
  pfctl -f "$PF_CONF" >/dev/null 2>&1
  local r; r="$(probe_real 5)"
  echo "  restored /etc/pf.conf and $ANCHOR_FILE from $BACKUP"
  if [[ "$r" == "5" ]]; then
    echo "  verified: the real path still answers from the gateway ($r/5)"
  else
    echo "  WARNING: real path answered only $r/5 after revert." >&2
    echo "  Recover with: $(dirname "$0")/transparent-root.sh remove" >&2
  fi
}
trap revert EXIT INT TERM

probe_direct() {  # $1 = count
  local ok=0 i
  for i in $(seq 1 "$1"); do
    curl -sk -m 3 -o /dev/null "https://127.0.0.1:$gport/healthz" 2>/dev/null && ok=$((ok+1))
  done
  print -r -- "$ok"
}
probe_real() {    # $1 = count; body must be OURS, not Anthropic's
  local ok=0 i body
  for i in $(seq 1 "$1"); do
    body="$(curl -sk -m 3 "https://$host/healthz" 2>/dev/null)"
    [[ "$body" == *'"overflow"'* ]] && ok=$((ok+1))
  done
  print -r -- "$ok"
}

echo "===== no-state rule experiment $(date '+%Y-%m-%d %H:%M:%S %Z') ====="
echo "gateway port: $gport   host: $host   probes per phase: $PROBES"
echo "backups: $BACKUP"
echo

cp "$PF_CONF" "$BACKUP/pf.conf"   || { echo "backup of pf.conf failed" >&2; exit 1; }
cp "$ANCHOR_FILE" "$BACKUP/anchor" || { echo "backup of anchor failed" >&2; exit 1; }

echo "== 0. preconditions =="
pre_real="$(probe_real 5)"
if [[ "$pre_real" != "5" ]]; then
  echo "the real path is already unhealthy ($pre_real/5) -- fix that before experimenting" >&2
  exit 1
fi
echo "  real path healthy ($pre_real/5); safe to proceed"
echo

echo "== 1. baseline, rule NOT applied =="
base_direct="$(probe_direct "$PROBES")"
echo "  direct 127.0.0.1:$gport -> $base_direct/$PROBES"
echo

echo "== 2. applying the candidate =="
# Translation rules must precede filter rules within an anchor file, or pf
# rejects the whole ruleset.
{
  grep -vE '^\s*pass ' "$BACKUP/anchor"
  echo "pass quick on lo0 inet proto tcp from any to 127.0.0.1 port $gport no state"
} > "$ANCHOR_FILE"

# The filter anchor reference goes beside com.apple's, which is where pf
# evaluates filter anchors -- after all translation anchors.
python3 - "$PF_CONF" "$FILTER_BEGIN" "$FILTER_END" <<'PY'
import sys
path, begin, end = sys.argv[1], sys.argv[2], sys.argv[3]
text = open(path).read()
if begin in text:
    sys.exit(0)
out, done = [], False
for line in text.splitlines(keepends=True):
    out.append(line)
    if not done and line.strip() == 'anchor "com.apple/*"':
        out.append('%s\nanchor "claude-burst"\n%s\n' % (begin, end))
        done = True
if not done:
    sys.stderr.write("no com.apple filter anchor line found; refusing to guess placement\n")
    sys.exit(1)
open(path, "w").write("".join(out))
PY
if (( $? != 0 )); then echo "  pf.conf edit failed -- nothing loaded" >&2; exit 1; fi
echo "  anchor file now ends with: $(tail -1 "$ANCHOR_FILE")"
echo "  pf.conf filter anchor added"

if ! dry="$(pfctl -n -f "$PF_CONF" 2>&1)"; then
  echo "  REJECTED by pfctl -n -f, nothing loaded:" >&2
  echo "$dry" | sed 's/^/    /' >&2
  exit 1
fi
pfctl -f "$PF_CONF" >/dev/null 2>&1 || { echo "  pfctl -f failed" >&2; exit 1; }
echo "  loaded"
echo "  rules now live in our anchor:"
pfctl -a claude-burst -s nat 2>/dev/null | grep -vE 'ALTQ' | sed 's/^/    /'
pfctl -a claude-burst -s rules 2>/dev/null | grep -vE 'ALTQ' | sed 's/^/    /'
echo

echo "== 3. did the machine-wide path survive? =="
mid_real="$(probe_real 5)"
echo "  real path -> $mid_real/5"
if [[ "$mid_real" != "5" ]]; then
  echo "  the candidate BROKE the real path -- reverting now, no further measurement" >&2
  exit 1
fi
echo

echo "== 4. measurement, rule applied =="
after_direct="$(probe_direct "$PROBES")"
echo "  direct 127.0.0.1:$gport -> $after_direct/$PROBES"
echo

echo "===== VERDICT ====="
echo "  direct before : $base_direct/$PROBES"
echo "  direct after  : $after_direct/$PROBES"
if (( after_direct > base_direct + PROBES / 2 )); then
  echo "  the no-state rule FIXES it. Worth committing -- with the pf.conf"
  echo "  filter anchor, which transparent-root.sh does not currently write."
elif (( after_direct > base_direct )); then
  echo "  improved but not fixed. Suggestive, not conclusive -- re-run before"
  echo "  drawing anything from it; the baseline rate is ~1 in 20 by itself."
else
  echo "  no improvement. The no-state rule is NOT the fix; record it as ruled"
  echo "  out in INVESTIGATION-TLS-STORM.md so nobody proposes it a second time."
fi
echo
echo "(reverting unconditionally, whatever the verdict)"
