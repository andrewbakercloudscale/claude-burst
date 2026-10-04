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
trap 'rc=$?; [ $rc -ne 0 ] && echo && echo "burst-repair FAILED (exit $rc) while $STEP. Run scripts/diagnose.sh and paste its report when asking for help; burst-off takes Burst out of the path meanwhile."' EXIT

say() { printf '\n== %s\n' "$1"; STEP="$1"; }
# ask reads the answer from the terminal, not stdin: stdin is the script
# itself when it is piped from curl.
ask() { local a=""; [ -r /dev/tty ] && read -r -p "$1 [y/N] " a </dev/tty; [ "$a" = y ] || [ "$a" = Y ]; }

say "finding the Claude Burst checkout"
REPO="${CLAUDE_BURST_REPO:-}"
if [ -z "$REPO" ]; then
  here="$(cd "$(dirname "$0")/.." 2>/dev/null && pwd)"
  for d in "$here" ~/claude-burst ~/claude-burst-repo ~/Desktop/github/claude-burst; do
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
  git fetch -q --tags origin && git checkout -q main && git merge -q --ff-only origin/main || exit 1
fi
git log --oneline -1

say "freeing Burst's ports"
GW=$(launchctl print "gui/$UID/$LABEL" 2>/dev/null | awk '/^\tpid = /{print $3}' || true)
for port in 7777 17777 7788; do
  for pid in $(lsof -nP -iTCP:"$port" -sTCP:LISTEN -t 2>/dev/null || true); do
    [ "$pid" = "$GW" ] && continue
    echo "port $port held by pid $pid ($(ps -o comm= -p "$pid" 2>/dev/null)), stopping it"
    kill "$pid" 2>/dev/null || sudo kill "$pid"
  done
done

say "reinstalling"
MODE=$(sed -n 's/.*"mode": *"\([a-z-]*\)".*/\1/p' "$CFG/config.json" 2>/dev/null | head -1)
[ "$MODE" = "base-url" ] || MODE=transparent
echo "mode: $MODE"
rm -f "$CFG"/rolled-back "$CFG"/rolled-back-*
CLAUDE_BURST_MODE=$MODE CLAUDE_BURST_FORCE=1 ./install.sh || exit 1

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

say "checking keep-awake"
if pmset -g 2>/dev/null | grep -q 'SleepDisabled *1' && grep -q '"keep_awake_lid_closed": *false' "$CFG/config.json" 2>/dev/null; then
  echo "SleepDisabled is on although keep-awake is off: this Mac will never sleep"
  ask "Turn it off (asks for your password)?" && sudo ./scripts/lid-awake-root.sh remove
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
echo "Repaired. Any Claude Code session that still times out: quit it and run claude --continue."
