#!/bin/bash
# Fixes the common ways Claude Burst stops working on a Mac, in one run, and
# says what it found at each step:
#
#   1. the checkout is behind GitHub          -> synced to main
#   2. old processes hold Burst's ports       -> stopped (the gateway is kept)
#   3. Burst was rolled back or half removed  -> reinstalled in the mode it was in
#   4. the gateway watchdog is not armed      -> armed (install.sh does it)
#   5. Claude Code's CA bundle lost the CA    -> claude-burst enable
#   6. sessions started before the CA changed -> listed, stopped on a "y"
#   7. "keep awake" left on though it is off  -> offered for removal
#   8. a request through Burst is checked, then the dashboard opens
#
# Safe to run any time; it asks before stopping sessions and sudo asks for
# the password where /etc/hosts and pf change. Works piped from GitHub:
#
#   curl -fsSL https://raw.githubusercontent.com/andrewbakercloudscale/claude-burst/main/scripts/repair.sh | bash
#
# If it cannot fix things, `burst-off` takes Burst out of the path.

set -uo pipefail
CFG="$HOME/.config/claude-burst"
LABEL="ninja.andrewbaker.claude-burst"
GH="https://github.com/andrewbakercloudscale/claude-burst.git"
STEP="starting"
# audit records how this ended in the audit trail (the Audit tab). REPO is
# set further down; before that there is no script to call and nothing ran.
audit() { [ -n "${REPO:-}" ] && [ -f "$REPO/scripts/audit-add.sh" ] && sh "$REPO/scripts/audit-add.sh" script "$1" "Script: repair.sh (burst-repair)" "$2" >/dev/null 2>&1; return 0; }
trap 'rc=$?; if [ $rc -ne 0 ]; then audit error "failed (exit $rc) while $STEP"; echo; echo "burst-repair FAILED (exit $rc) while $STEP. Run scripts/diagnose.sh and paste its report when asking for help; burst-off takes Burst out of the path meanwhile."; else audit ok "done"; fi' EXIT

say() { printf '\n== %s\n' "$1"; STEP="$1"; }
# ask reads the answer from the terminal, not stdin: stdin is the script
# itself when it is piped from curl.
ask() { local a=""; [ -r /dev/tty ] && read -r -p "$1 [y/N] " a </dev/tty; [ "$a" = y ] || [ "$a" = Y ]; }

say "finding the Claude Burst checkout"
REPO="${CLAUDE_BURST_REPO:-}"
if [ -z "$REPO" ]; then
  here="$(cd "$(dirname "$0")/.." 2>/dev/null && pwd)"
  # The path install.sh recorded comes first: the checkout can be anywhere.
  recorded="$(cat ~/.local/share/claude-burst/repo 2>/dev/null || true)"
  for d in "$here" "$recorded" ~/claude-burst ~/claude-burst-repo ~/Desktop/github/claude-burst; do
    [ -n "$d" ] || continue
    if [ -f "$d/install.sh" ] && [ -d "$d/.git" ]; then REPO=$d; break; fi
  done
fi
if [ -z "$REPO" ]; then
  echo "none found, cloning to ~/claude-burst"
  git clone -q "$GH" ~/claude-burst || exit 1
  REPO=~/claude-burst
fi
echo "$REPO"
cd "$REPO" || exit 1

say "syncing with GitHub"
if [ -n "$(git status --porcelain --untracked-files=no)" ]; then
  echo "local edits present, leaving them alone (not synced)"
else
  # Best effort: this script is for a Mac where things are broken, and the
  # network (or Burst in front of it) may be what is broken. Until
  # 7 Oct 2026 a failed fetch ended the repair before it had fixed anything.
  if git fetch -q --tags origin && git checkout -q main && git merge -q --ff-only origin/main; then
    echo "up to date with GitHub"
  else
    echo "WARNING: could not sync with GitHub (no network, or main has moved apart). Repairing from this checkout as it is."
  fi
fi
git log --oneline -1

say "freeing Burst's ports"
GW=$(launchctl print "gui/$UID/$LABEL" 2>/dev/null | awk '/^\tpid = /{print $3}' || true)
for port in 7777 17777 7788 7779; do
  for pid in $(lsof -nP -iTCP:"$port" -sTCP:LISTEN -t 2>/dev/null || true); do
    [ "$pid" = "$GW" ] && continue
    echo "port $port held by pid $pid ($(ps -o comm= -p "$pid" 2>/dev/null)), stopping it"
    kill "$pid" 2>/dev/null || sudo kill "$pid"
  done
done

# Piped from curl, stdin is this script, so sudo inside install.sh finds no
# terminal and gives up: ask for the password here, on the terminal, first.
# install.sh runs with the terminal as its stdin for the same reason.
say "asking for your password (for /etc/hosts and the pf redirect)"
if [ -r /dev/tty ]; then sudo -v </dev/tty || exit 1; else sudo -v || exit 1; fi
( while sleep 50; do kill -0 $$ 2>/dev/null && sudo -n true 2>/dev/null || exit; done ) &
KEEPALIVE=$!
trap 'kill $KEEPALIVE 2>/dev/null' INT TERM

say "reinstalling"
# The mode this Mac chose, read as the gateway reads it: no "mode" key means
# base-url (the field is omitempty), never transparent, which would edit
# /etc/hosts and pf on a Mac that chose not to. No config at all is a fresh
# install, which gets the default.
if [ -f "$CFG/config.json" ]; then
  # A config that cannot be read stops the gateway starting and would be
  # taken for base-url mode below, on a Mac that may be transparent: put
  # the newest backup that loads back first, or stop and say so.
  # The gateway's own reader decides, where it is installed: it refuses
  # more than broken JSON. restore-config does nothing to a config that loads.
  if [ -x ~/.local/bin/claude-burst ]; then
    if ! ~/.local/bin/claude-burst restore-config; then
      echo "config.json cannot be read and no backup could be restored. Fix or remove $CFG/config.json (backups are in $CFG/backups), or run burst-off."
      exit 1
    fi
  elif ! /usr/bin/python3 -c 'import json,sys; json.load(open(sys.argv[1]))' "$CFG/config.json" 2>/dev/null; then
    echo "config.json cannot be read. Fix or remove $CFG/config.json (backups are in $CFG/backups), or run burst-off."
    exit 1
  fi
  MODE=$(/usr/bin/python3 -c 'import json,sys; print((json.load(open(sys.argv[1])).get("intercept") or {}).get("mode") or "base-url")' "$CFG/config.json" 2>/dev/null)
  [ "$MODE" = transparent ] || MODE=base-url
else
  MODE=transparent
fi
echo "mode: $MODE"
rm -f "$CFG"/rolled-back "$CFG"/rolled-back-*
if [ -r /dev/tty ]; then
  CLAUDE_BURST_MODE=$MODE CLAUDE_BURST_FORCE=1 ./install.sh </dev/tty || exit 1
else
  CLAUDE_BURST_MODE=$MODE CLAUDE_BURST_FORCE=1 ./install.sh || exit 1
fi

say "checking the gateway watchdog"
if launchctl print "gui/$UID/$LABEL-selfheal" >/dev/null 2>&1; then echo "armed"
else ./scripts/install-selfheal-watchdog.sh || exit 1; fi

if [ "$MODE" = transparent ]; then
  say "checking Claude Code's CA bundle"
  B=$(launchctl getenv NODE_EXTRA_CA_CERTS || true)
  echo "NODE_EXTRA_CA_CERTS=${B:-unset}"
  if [ -n "$B" ] && grep -q "BEGIN CERTIFICATE" "$B" 2>/dev/null; then echo "bundle OK"
  else echo "bundle missing, running claude-burst enable"; ~/.local/bin/claude-burst enable || exit 1; fi

  # A session reads the bundle once, at startup: one started before the CA
  # last changed refuses the gateway's certificate until it is restarted,
  # and its requests time out with nothing in Burst's request log.
  say "looking for sessions started before the CA changed"
  CHANGED=$(stat -f %m "$CFG/ca" 2>/dev/null || echo 0)
  echo "CA changed $(date -r "$CHANGED" '+%Y-%m-%d %H:%M:%S')"
  STALE=()
  while read -r pid; do
    [ -n "$pid" ] || continue
    START=$(LC_ALL=C ps -o lstart= -p "$pid" 2>/dev/null) || continue
    S=$(LC_ALL=C date -j -f "%a %b %d %T %Y" "$START" +%s 2>/dev/null) || continue
    DIR=$(lsof -a -p "$pid" -d cwd -Fn 2>/dev/null | sed -n 's/^n//p')
    if [ "$S" -lt "$CHANGED" ]; then echo "  STALE pid $pid, started $START, in $DIR"; STALE+=("$pid")
    else echo "  ok    pid $pid, started $START, in $DIR"; fi
  done < <(pgrep -x claude || true)
  if [ ${#STALE[@]} -gt 0 ]; then
    if ask "Stop the ${#STALE[@]} stale session(s)? Their conversations are kept."; then
      kill "${STALE[@]}" 2>/dev/null
      echo "stopped. In each of those terminals run: claude --continue"
    else
      echo "left running: they will keep timing out until restarted"
    fi
  else
    echo "none"
  fi
fi

say "checking Codex"
CODEX_TOML="${CODEX_HOME:-$HOME/.codex}/config.toml"
if grep -q '^# BEGIN claude-burst' "$CODEX_TOML" 2>/dev/null; then
  # Routed through Burst: the Codex port must answer, or every Codex turn
  # fails. Any HTTP status means the gateway forwarded (401: no login sent).
  code=000
  for _ in $(seq 1 15); do
    code=$(curl -s -o /dev/null -w '%{http_code}' -m 5 http://127.0.0.1:7779/backend-api/codex/models || true)
    [ "$code" != 000 ] && break
    sleep 1
  done
  if [ "$code" != 000 ]; then
    echo "Codex goes through Burst and its port answers (HTTP $code)"
  else
    echo "Codex is routed through Burst but nothing answers on 127.0.0.1:7779"
    if ask "Send Codex straight to ChatGPT instead?"; then
      ./scripts/codex-unroute.sh
      echo "done: restart Codex"
    fi
  fi
else
  echo "Codex not routed through Burst: nothing to check"
fi

say "checking keep-awake"
if pmset -g 2>/dev/null | grep -q 'SleepDisabled *1' && grep -q '"keep_awake_lid_closed": *false' "$CFG/config.json" 2>/dev/null; then
  echo "SleepDisabled is on although keep-awake is off: this Mac will never sleep"
  ask "Turn it off?" && sudo ./scripts/lid-awake-root.sh remove </dev/tty
else
  echo "OK"
fi

say "checking a request through Burst"
# Transparent: the real hostname, which the redirect sends to the gateway.
# Base-url: the gateway's own address, which is what settings.json names.
URL=https://api.anthropic.com/healthz
[ "$MODE" = base-url ] && URL="http://$(sed -n 's/.*"listen": *"\([^"]*\)".*/\1/p' "$CFG/config.json" | head -1)/healthz"
ok=""
for _ in $(seq 1 30); do
  if curl -sf -o /dev/null --max-time 5 "$URL"; then ok=1; break; fi
  sleep 1
done
if [ -n "$ok" ]; then echo "$URL answers"
else echo "$URL did not answer: run scripts/diagnose.sh and paste the report"; exit 1; fi

for _ in $(seq 1 30); do curl -sf -o /dev/null http://127.0.0.1:7788/ && break; sleep 1; done
open "http://127.0.0.1:7788/" 2>/dev/null
echo
kill "$KEEPALIVE" 2>/dev/null
echo "Repaired. Any Claude Code session that still times out: quit it and run claude --continue."
