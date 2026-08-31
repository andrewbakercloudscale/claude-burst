#!/bin/zsh
# Installs and removes the two machine-wide pieces of claude-burst's optional
# transparent intercept mode. Everything here needs root; nothing else in
# claude-burst does, and the gateway itself deliberately keeps running as an
# unprivileged user LaunchAgent because it handles a Claude OAuth credential.
#
#   1. /etc/hosts   127.0.0.1 <host>, so Claude Code connects to the gateway
#                   while still believing it reached Anthropic (which is what
#                   keeps Remote Control enabled -- Claude Code disables it
#                   whenever ANTHROPIC_BASE_URL names another host).
#   2. pf rdr       127.0.0.1:443 -> the gateway's port, so the gateway does
#                   not need to bind a privileged port and does not need root.
#
# THIS SCRIPT IS THE UNDO, AND IT EXISTS BEFORE THE DO. `remove` is idempotent
# and safe to run when nothing was ever installed, when only half an install
# succeeded, or twice in a row -- rollback runs unattended from watchdog.sh,
# and a rollback that fails because the state is not what it expected is not a
# rollback. On 2026-08-30 an enable ran before its gateway was listening and
# broke a live session; the lesson was that the recovery path must work in
# states nobody predicted.
#
# Usage:
#   sudo transparent-root.sh install [--host H] [--port P] [--gateway-port G]
#   sudo transparent-root.sh remove
#   sudo transparent-root.sh status
#        transparent-root.sh --self-test     # no root; exercises the text edits
set -uo pipefail

HOST_DEFAULT="api.anthropic.com"
PORT_DEFAULT=443
GATEWAY_PORT_DEFAULT=7777

HOSTS_FILE="${CLAUDE_BURST_HOSTS_FILE:-/etc/hosts}"
PF_CONF="${CLAUDE_BURST_PF_CONF:-/etc/pf.conf}"
PF_ANCHOR_FILE="${CLAUDE_BURST_PF_ANCHOR:-/etc/pf.anchors/claude-burst}"
STATE_DIR="${CLAUDE_BURST_ROOT_STATE_DIR:-/etc/claude-burst}"
STATE_FILE="$STATE_DIR/transparent.state"

ANCHOR_NAME="claude-burst"
BEGIN="# BEGIN claude-burst"
END="# END claude-burst"

# Tags must not be prefixes of one another, or a marker search for one block
# would match the start of the other and delete the wrong lines.
TAG_HOSTS="hosts"
TAG_ADMIN="admin-host"
TAG_RDR="pf-rdr"
TAG_LOAD="pf-load"

die() { echo "error: $*" >&2; exit 1; }
need_root() { [[ $EUID -eq 0 ]] || die "must run as root: sudo $0 $*"; }

# --- text-editing helper -----------------------------------------------------
#
# The Python lives in a temp FILE, not a heredoc. A heredoc becomes the
# interpreter's stdin, so `python3 - <<PY` reads the script from stdin and the
# piped file contents never arrive -- the filters silently emit nothing, and
# piping that back over /etc/hosts truncates it. The first version of this
# script had exactly that bug; --self-test caught it before it ever ran as root.

PY_HELPER=""
cleanup_helper() { [[ -n "$PY_HELPER" ]] && rm -f "$PY_HELPER"; }
trap cleanup_helper EXIT INT TERM

setup_helper() {
  PY_HELPER="$(mktemp -t claude-burst-edit)" || die "mktemp failed"
  cat > "$PY_HELPER" <<'PY'
import sys

def strip(text, begin, end, tag):
    """Remove one marker-delimited block. Markers are matched including their
    trailing newline so that tag 'pf' could never match 'pf2'."""
    b, e = "%s %s\n" % (begin, tag), "%s %s" % (end, tag)
    start = text.find(b)
    if start == -1:
        return text, False
    stop = text.find(e, start)
    if stop == -1:
        # Begin marker with no end: refuse to guess where the block stops
        # rather than risk deleting a line somebody else depends on.
        return text, False
    stop += len(e)
    if stop < len(text) and text[stop] == "\n":
        stop += 1
    return text[:start] + text[stop:], True

def main():
    cmd, begin, end = sys.argv[1], sys.argv[2], sys.argv[3]
    args = sys.argv[4:]
    text = sys.stdin.read()

    if cmd == "block-add":
        payload, tag = args[0], args[1]
        text, _ = strip(text, begin, end, tag)
        if text and not text.endswith("\n"):
            text += "\n"
        sys.stdout.write("%s%s %s\n%s\n%s %s\n" % (text, begin, tag, payload, end, tag))

    elif cmd == "block-remove":
        text, _ = strip(text, begin, end, args[0])
        sys.stdout.write(text)

    elif cmd == "pf-add":
        anchor, anchor_file, tag_rdr, tag_load = args[0], args[1], args[2], args[3]
        text, _ = strip(text, begin, end, tag_rdr)
        text, _ = strip(text, begin, end, tag_load)
        out, inserted = [], False
        for line in text.splitlines(keepends=True):
            out.append(line)
            # pf requires translation rules (nat/rdr) before filter rules, so
            # ours goes beside the existing rdr-anchor. A ruleset in the wrong
            # order is rejected outright, which means no redirect at all.
            if not inserted and line.strip().startswith("rdr-anchor"):
                out.append('%s %s\nrdr-anchor "%s"\n%s %s\n' % (begin, tag_rdr, anchor, end, tag_rdr))
                inserted = True
        if not inserted:
            sys.stderr.write("no rdr-anchor line in pf.conf; refusing to guess placement\n")
            return 1
        text = "".join(out)
        if not text.endswith("\n"):
            text += "\n"
        text += '%s %s\nload anchor "%s" from "%s"\n%s %s\n' % (
            begin, tag_load, anchor, anchor_file, end, tag_load)
        sys.stdout.write(text)

    elif cmd == "pf-remove":
        tag_rdr, tag_load = args[0], args[1]
        for tag in (tag_rdr, tag_load):
            while True:
                text, changed = strip(text, begin, end, tag)
                if not changed:
                    break
        sys.stdout.write(text)

    else:
        sys.stderr.write("unknown command %s\n" % cmd)
        return 1
    return 0

sys.exit(main())
PY
}

block_add()    { python3 "$PY_HELPER" block-add    "$BEGIN" "$END" "$1" "$2"; }
block_remove() { python3 "$PY_HELPER" block-remove "$BEGIN" "$END" "$1"; }
pf_conf_add()  { python3 "$PY_HELPER" pf-add       "$BEGIN" "$END" "$ANCHOR_NAME" "$PF_ANCHOR_FILE" "$TAG_RDR" "$TAG_LOAD"; }
pf_conf_remove() { python3 "$PY_HELPER" pf-remove  "$BEGIN" "$END" "$TAG_RDR" "$TAG_LOAD"; }

block_present() { [[ -f "$1" ]] && grep -q "^$BEGIN $2\$" "$1" 2>/dev/null; }

edit_file() {
  # edit_file <file> <filter-fn> [args...] -- rewrite in place, preserving inode
  local f="$1"; shift
  [[ -f "$f" ]] || die "$f does not exist"
  local tmp="$f.claude-burst.$$"
  if ! "$@" < "$f" > "$tmp"; then
    rm -f "$tmp"; die "failed editing $f (left unchanged)"
  fi
  # Never replace a system file with nothing, whatever went wrong upstream.
  if [[ ! -s "$tmp" ]]; then
    rm -f "$tmp"; die "refusing to write an empty $f (left unchanged)"
  fi
  cat "$tmp" > "$f"   # preserves the original inode, owner and mode
  rm -f "$tmp"
}

# --- state -------------------------------------------------------------------

state_get() { [[ -f "$STATE_FILE" ]] && grep -E "^$1=" "$STATE_FILE" 2>/dev/null | tail -1 | cut -d= -f2- }
state_set() {
  mkdir -p "$STATE_DIR" && chmod 755 "$STATE_DIR"
  local tmp="$STATE_FILE.$$"
  { [[ -f "$STATE_FILE" ]] && grep -vE "^$1=" "$STATE_FILE"; echo "$1=$2"; } > "$tmp" 2>/dev/null
  mv "$tmp" "$STATE_FILE" && chmod 644 "$STATE_FILE"
}

quiet_pf() { grep -vE 'ALTQ|Use of -f option|present in the main ruleset|See /etc/pf.conf' | sed 's/^/  /'; }

flush_dns() {
  dscacheutil -flushcache 2>/dev/null
  killall -HUP mDNSResponder 2>/dev/null
  echo "  flushed DNS cache"
}

# --- actions -----------------------------------------------------------------

do_install() {
  local host="$HOST_DEFAULT" port="$PORT_DEFAULT" gport="$GATEWAY_PORT_DEFAULT"
  while [[ $# -gt 0 ]]; do
    case "$1" in
      --host) host="$2"; shift 2 ;;
      --port) port="$2"; shift 2 ;;
      --gateway-port) gport="$2"; shift 2 ;;
      *) die "unknown argument: $1" ;;
    esac
  done
  need_root install
  setup_helper

  # The gateway must already be listening. Installing the redirect first is
  # how a live session gets broken: /etc/hosts starts sending every process on
  # this Mac to a port with nothing behind it.
  if ! curl -sf -m 3 "http://127.0.0.1:$gport/healthz" >/dev/null 2>&1; then
    die "gateway is not responding on 127.0.0.1:$gport -- start it before installing the redirect"
  fi
  echo "gateway healthy on 127.0.0.1:$gport"

  # Resolve BEFORE the hosts entry exists: afterwards every resolver on the
  # machine answers 127.0.0.1, so this is the only chance to record a real
  # address for diagnostics.
  local upstream
  upstream=$(dscacheutil -q host -a name "$host" 2>/dev/null | awk '/^ip_address:/ {print $2; exit}')
  [[ -n "$upstream" ]] || die "cannot resolve $host right now -- refusing to install a redirect while DNS is unhealthy"
  echo "resolved $host -> $upstream"

  mkdir -p "$STATE_DIR"
  cp -p "$HOSTS_FILE" "$STATE_DIR/hosts.pre-install.bak"
  cp -p "$PF_CONF" "$STATE_DIR/pf.conf.pre-install.bak"
  echo "backed up $HOSTS_FILE and $PF_CONF to $STATE_DIR"

  state_set host "$host"
  state_set upstream "$upstream"
  state_set gateway_port "$gport"

  echo "== pf anchor =="
  mkdir -p "$(dirname "$PF_ANCHOR_FILE")"
  cat > "$PF_ANCHOR_FILE" <<EOF
# Managed by claude-burst (transparent intercept mode). Remove with:
#   sudo transparent-root.sh remove
rdr pass on lo0 inet proto tcp from any to 127.0.0.1 port $port -> 127.0.0.1 port $gport
EOF
  chmod 644 "$PF_ANCHOR_FILE"
  echo "  wrote $PF_ANCHOR_FILE"

  echo "== /etc/pf.conf =="
  edit_file "$PF_CONF" pf_conf_add
  echo "  referenced anchor \"$ANCHOR_NAME\""
  pfctl -f "$PF_CONF" 2>&1 | quiet_pf

  if pfctl -s info 2>/dev/null | head -1 | grep -q Enabled; then
    state_set pf_was_enabled 1
    echo "  pf already enabled; left alone"
  else
    local token
    token=$(pfctl -E 2>&1 | awk '/Token/ {print $3}')
    state_set pf_was_enabled 0
    state_set pf_token "$token"
    echo "  pf enabled (token $token; released on remove)"
  fi

  # Verify the redirect works BEFORE pointing DNS at it. If this fails there is
  # still nothing redirecting, so the machine is untouched in any way that matters.
  if ! curl -sf -m 5 "http://127.0.0.1:$port/healthz" >/dev/null 2>&1; then
    echo "  redirect verification FAILED -- rolling back before touching /etc/hosts" >&2
    do_remove
    die "pf redirect did not take effect; nothing was changed in /etc/hosts"
  fi
  echo "  verified: 127.0.0.1:$port reaches the gateway"

  # /etc/hosts goes LAST. It is the machine-wide switch, so it is only thrown
  # once everything it depends on is proven working.
  echo "== /etc/hosts =="
  edit_file "$HOSTS_FILE" block_add "127.0.0.1 $host" "$TAG_HOSTS"
  echo "  added 127.0.0.1 $host"
  flush_dns

  echo
  echo "installed. undo with: sudo $0 remove"
}

do_remove() {
  need_root remove
  setup_helper
  local removed_any=0

  # /etc/hosts first: while it points at a port with nothing behind it, every
  # process on this Mac that talks to the host fails. Undo the machine-wide
  # thing before anything that could itself fail.
  if block_present "$HOSTS_FILE" "$TAG_HOSTS"; then
    edit_file "$HOSTS_FILE" block_remove "$TAG_HOSTS"
    echo "removed /etc/hosts entry"
    removed_any=1
  else
    echo "no /etc/hosts entry present"
  fi
  flush_dns

  if grep -q "^$BEGIN $TAG_RDR\$" "$PF_CONF" 2>/dev/null || grep -q "^$BEGIN $TAG_LOAD\$" "$PF_CONF" 2>/dev/null; then
    edit_file "$PF_CONF" pf_conf_remove
    echo "removed pf.conf anchor references"
    removed_any=1
  else
    echo "no pf.conf anchor reference present"
  fi

  if [[ -f "$PF_ANCHOR_FILE" ]]; then
    rm -f "$PF_ANCHOR_FILE" && echo "removed $PF_ANCHOR_FILE"
    removed_any=1
  fi

  # Flush our anchor even if pf.conf was already clean, so a half-finished
  # install cannot leave a live redirect behind.
  pfctl -a "$ANCHOR_NAME" -F all >/dev/null 2>&1 && echo "flushed anchor rules"
  [[ -f "$PF_CONF" ]] && pfctl -f "$PF_CONF" 2>&1 | quiet_pf

  # Restore pf's enabled/disabled state only if we changed it.
  if [[ "$(state_get pf_was_enabled)" == "0" ]]; then
    local token="$(state_get pf_token)"
    if [[ -n "$token" ]]; then
      pfctl -X "$token" >/dev/null 2>&1 && echo "released pf token $token"
    else
      pfctl -d >/dev/null 2>&1 && echo "disabled pf (no token recorded)"
    fi
  fi

  [[ -f "$STATE_FILE" ]] && rm -f "$STATE_FILE" && echo "cleared $STATE_FILE"
  [[ "$removed_any" == "0" ]] && echo "nothing was installed; machine already clean"
  echo "pf now: $(pfctl -s info 2>/dev/null | head -1)"
  return 0
}

do_status() {
  setup_helper
  local host="$(state_get host)"; : "${host:=$HOST_DEFAULT}"
  echo "== transparent intercept =="
  if block_present "$HOSTS_FILE" "$TAG_HOSTS"; then
    echo "  hosts entry   : PRESENT ($(grep -A1 "^$BEGIN $TAG_HOSTS\$" "$HOSTS_FILE" | tail -1))"
  else
    echo "  hosts entry   : absent"
  fi
  [[ -f "$PF_ANCHOR_FILE" ]] && echo "  anchor file   : $PF_ANCHOR_FILE" || echo "  anchor file   : absent"
  grep -q "^$BEGIN $TAG_RDR\$" "$PF_CONF" 2>/dev/null && echo "  pf.conf ref   : PRESENT" || echo "  pf.conf ref   : absent"
  echo "  pf status     : $(pfctl -s info 2>/dev/null | head -1 | sed 's/  */ /g')"

  local rules
  rules=$(pfctl -a "$ANCHOR_NAME" -s nat 2>/dev/null | grep rdr)
  if [[ -n "$rules" ]]; then
    echo "  live rdr rule : $rules"
  elif block_present "$HOSTS_FILE" "$TAG_HOSTS"; then
    # The dangerous state: DNS still redirects but nothing listens.
    echo "  live rdr rule : MISSING -- hosts still redirects, so traffic to $host will be REFUSED"
    echo "                  fix with: sudo $0 remove"
  else
    echo "  live rdr rule : none (consistent with the hosts entry being absent)"
  fi
  echo "  state file    : $STATE_FILE$([[ -f "$STATE_FILE" ]] || echo ' (absent)')"
  if block_present "$HOSTS_FILE" "$TAG_ADMIN"; then
    echo "  admin hostname: $(grep -A1 "^$BEGIN $TAG_ADMIN\$" "$HOSTS_FILE" | tail -1)"
  fi
}

# --- self-test ---------------------------------------------------------------
#
# install/remove need root and rewrite system files, so they cannot be run
# casually. The text edits are where a mistake does lasting damage, so every
# branch is exercised here against temp files -- a rollback path that has never
# executed is the one that fails when it is finally needed.

self_test() {
  setup_helper
  local failures=0
  check() {
    if [[ "$2" == "$3" ]]; then
      echo "PASS: $1"
    else
      echo "FAIL: $1"
      echo "  got:  $(print -r -- "$2" | head -8)"
      echo "  want: $(print -r -- "$3" | head -8)"
      failures=$((failures + 1))
    fi
  }

  echo "== /etc/hosts editing =="
  local orig=$'##\n# Host Database\n##\n127.0.0.1\tlocalhost\n255.255.255.255\tbroadcasthost\n::1\tlocalhost\n10.0.0.5\tmy-nas.local\n'
  local added; added=$(print -rn -- "$orig" | block_add "127.0.0.1 api.anthropic.com" "$TAG_HOSTS")

  print -r -- "$added" | grep -q "^127.0.0.1 api.anthropic.com$" \
    && check "add inserts the redirect" yes yes || check "add inserts the redirect" no yes
  print -r -- "$added" | grep -q "my-nas.local" \
    && check "add preserves unrelated entries" yes yes || check "add preserves unrelated entries" no yes

  check "remove restores the original byte-for-byte" \
    "$(print -r -- "$added" | block_remove "$TAG_HOSTS")" "$(print -rn -- "$orig")"
  check "add is idempotent" \
    "$(print -rn -- "$orig" | block_add "127.0.0.1 api.anthropic.com" "$TAG_HOSTS" | block_add "127.0.0.1 api.anthropic.com" "$TAG_HOSTS")" \
    "$added"
  check "remove on a clean file is a no-op" \
    "$(print -rn -- "$orig" | block_remove "$TAG_HOSTS")" "$(print -rn -- "$orig")"

  local truncated="${orig}${BEGIN} ${TAG_HOSTS}"$'\n127.0.0.1 api.anthropic.com\n'
  check "remove leaves a block with no end marker alone" \
    "$(print -rn -- "$truncated" | block_remove "$TAG_HOSTS")" "$(print -rn -- "$truncated")"

  echo
  echo "== /etc/pf.conf editing =="
  local pforig=$'scrub-anchor "com.apple/*"\nnat-anchor "com.apple/*"\nrdr-anchor "com.apple/*"\ndummynet-anchor "com.apple/*"\nanchor "com.apple/*"\nload anchor "com.apple" from "/etc/pf.anchors/com.apple"\n'
  local pfadded; pfadded=$(print -rn -- "$pforig" | pf_conf_add)

  print -r -- "$pfadded" | grep -q 'rdr-anchor "claude-burst"' \
    && check "pf add inserts rdr-anchor" yes yes || check "pf add inserts rdr-anchor" no yes
  print -r -- "$pfadded" | grep -q 'load anchor "claude-burst"' \
    && check "pf add inserts load directive" yes yes || check "pf add inserts load directive" no yes

  local rdr_line anchor_line
  rdr_line=$(print -r -- "$pfadded" | grep -n 'rdr-anchor "claude-burst"' | cut -d: -f1)
  anchor_line=$(print -r -- "$pfadded" | grep -n '^anchor "com.apple/\*"' | cut -d: -f1)
  if [[ -n "$rdr_line" && -n "$anchor_line" ]] && (( rdr_line < anchor_line )); then
    check "pf add keeps rdr before filter anchors" yes yes
  else
    check "pf add keeps rdr before filter anchors" no yes
  fi

  check "pf remove restores the original" \
    "$(print -r -- "$pfadded" | pf_conf_remove)" "$(print -rn -- "$pforig")"
  check "pf add is idempotent" \
    "$(print -rn -- "$pforig" | pf_conf_add | pf_conf_add)" "$pfadded"
  check "pf remove on a clean file is a no-op" \
    "$(print -rn -- "$pforig" | pf_conf_remove)" "$(print -rn -- "$pforig")"

  # The tag-prefix trap: removing the rdr block must not eat the load block.
  local only_load; only_load=$(print -r -- "$pfadded" | python3 "$PY_HELPER" block-remove "$BEGIN" "$END" "$TAG_RDR")
  print -r -- "$only_load" | grep -q 'load anchor "claude-burst"' \
    && check "removing rdr block leaves the load block intact" yes yes \
    || check "removing rdr block leaves the load block intact" no yes

  echo
  echo "== block independence =="
  # Two separate /etc/hosts blocks coexist. Removing the transparent-mode one
  # must not disturb the admin hostname, or `remove` would silently break the
  # admin URL as a side effect.
  local both; both=$(print -rn -- "$orig" \
    | block_add "127.0.0.1 api.anthropic.com" "$TAG_HOSTS" \
    | block_add "127.0.0.1 cloudscale-claudeburst.test" "$TAG_ADMIN")
  local after; after=$(print -r -- "$both" | block_remove "$TAG_HOSTS")
  print -r -- "$after" | grep -q "cloudscale-claudeburst.test" \
    && check "removing transparent block keeps the admin hostname" yes yes \
    || check "removing transparent block keeps the admin hostname" no yes
  print -r -- "$after" | grep -q "api.anthropic.com" \
    && check "transparent block actually went" no yes \
    || check "transparent block actually went" yes yes
  local after2; after2=$(print -r -- "$both" | block_remove "$TAG_ADMIN")
  print -r -- "$after2" | grep -q "api.anthropic.com" \
    && check "removing admin hostname keeps the transparent block" yes yes \
    || check "removing admin hostname keeps the transparent block" no yes

  echo
  if (( failures == 0 )); then echo "self-test: all edit paths OK"; return 0; fi
  echo "self-test: $failures check(s) failed"
  return 1
}

# admin-host is independent of transparent mode: it only maps a friendly name
# to 127.0.0.1 so the admin UI has a nicer URL than a bare IP. It is a separate
# /etc/hosts block with its own tag, so removing one never disturbs the other.
do_admin_host() {
  need_root admin-host
  setup_helper
  local name="${1:-}"
  if [[ -z "$name" ]]; then
    die "usage: sudo $0 admin-host <name>   (e.g. cloudscale-claudeburst.test)"
  fi
  if [[ "$name" != *.* ]]; then
    echo "note: '$name' has no dot, so browsers may treat it as a search term."
    echo "      A dotted name such as '$name.test' is more reliable (.test is"
    echo "      reserved by RFC 6761 and will never collide with a real domain)."
  fi
  edit_file "$HOSTS_FILE" block_add "127.0.0.1 $name" "$TAG_ADMIN"
  flush_dns
  echo "added 127.0.0.1 $name"
  echo
  echo "Now tell the gateway to accept that Host header:"
  echo "  claude-burst configure --admin-hostname $name"
  echo "  launchctl kickstart -k gui/\$UID/ninja.andrewbaker.claude-burst"
  echo
  echo "Undo with: sudo $0 admin-host-remove"
}

do_admin_host_remove() {
  need_root admin-host-remove
  setup_helper
  if block_present "$HOSTS_FILE" "$TAG_ADMIN"; then
    edit_file "$HOSTS_FILE" block_remove "$TAG_ADMIN"
    flush_dns
    echo "removed the admin hostname from $HOSTS_FILE"
  else
    echo "no admin hostname entry present"
  fi
  echo "also clear it from config: claude-burst configure --admin-hostname off"
}

case "${1:-}" in
  install)     shift; do_install "$@" ;;
  admin-host)  shift; do_admin_host "$@" ;;
  admin-host-remove) do_admin_host_remove ;;
  remove)      do_remove ;;
  status)      do_status ;;
  --self-test) self_test ;;
  *)
    cat <<EOF
usage: sudo $0 install [--host H] [--port P] [--gateway-port G]
       sudo $0 remove
       sudo $0 status
       sudo $0 admin-host <name>     # friendly URL for the admin UI
       sudo $0 admin-host-remove
            $0 --self-test

install and remove require root. remove is idempotent and safe to run at any
time, including when nothing was ever installed.
EOF
    exit 2
    ;;
esac
