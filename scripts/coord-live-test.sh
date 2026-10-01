#!/usr/bin/env bash
# coord-live-test.sh: session coordination with two REAL Claude Code sessions.
#
# `go test ./internal/coord` drives the hooks with made-up hook input. This
# runs the real thing: two `claude -p` sessions, alpha and beta, take turns
# adding one word each to one file until they have written a sentence. It
# checks that nobody was refused or made to wait, that no word was lost,
# that alpha (first to edit) is the master and beta is recorded as
# coordinating with it, that beta may not commit the file, and that alpha's
# commit settles it.
#
# Everything is temporary: a fresh git repo, a freshly built claude-burst,
# its own coordination state, and the hooks passed with --settings, so your
# ~/.claude/settings.json and config.json are not touched and coordination
# does not need to be switched on. It makes about 11 short Haiku requests on
# whatever Claude Code normally uses (your subscription).
#
#   bash scripts/coord-live-test.sh          # keep going on a failure: KEEP=1 keeps the temp dir
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
TMP="$(mktemp -d "${TMPDIR:-/tmp}/coord-live.XXXXXX")"
[ "${KEEP:-}" = 1 ] || trap 'rm -rf "$TMP"' EXIT
MODEL="${MODEL:-haiku}"
PASS=0 FAIL=0
ok()   { PASS=$((PASS+1)); printf '  ok   %s\n' "$1"; }
bad()  { FAIL=$((FAIL+1)); printf '  FAIL %s\n' "$1"; }
check() { if eval "$2"; then ok "$1"; else bad "$1"; fi; }

command -v claude >/dev/null || { echo "claude (Claude Code) is not on PATH"; exit 2; }

echo "building claude-burst"
BIN="$TMP/bin/claude-burst"
(cd "$ROOT" && go build -o "$BIN" ./cmd/claude-burst)

REPO="$TMP/repo"
mkdir -p "$REPO" && cd "$REPO"
git init -q && git config user.email t@example.com && git config user.name coord-live-test
printf '<end>\n' > sentence.txt
git add sentence.txt && git commit -qm "empty sentence"

# The hooks the dashboard installs, pointing at the fresh binary, less one:
# SessionEnd. Each `claude -p` below ends its session when it answers, and
# resuming it starts it again, so SessionEnd would free the file between
# every word. An interactive session ends once, when you close it.
SETTINGS="$TMP/settings.json"
hook() { printf '{"matcher":"%s","hooks":[{"type":"command","command":"\\"%s\\" coord %s","timeout":10}]}' "$1" "$BIN" "$2"; }
cat > "$SETTINGS" <<JSON
{"hooks":{
 "SessionStart":[$(hook "" session-start)],
 "PreToolUse":[$(hook "Edit|Write|MultiEdit|NotebookEdit|Bash" pre-tool)],
 "PostToolUse":[$(hook "Edit|Write|MultiEdit|NotebookEdit|Bash" post-tool)],
 "UserPromptSubmit":[$(hook "" prompt)],
 "Stop":[$(hook "" stop)]
}}
JSON
# Its repository is a temporary directory, which coordination otherwise ignores.
export CLAUDE_BURST_COORD_FORCE=1 CLAUDE_BURST_COORD_TRACK_TMP=1 CLAUDE_BURST_COORD_DIR="$TMP/coord"

ALPHA="$(uuidgen | tr 'A-Z' 'a-z')"
BETA="$(uuidgen | tr 'A-Z' 'a-z')"
started_alpha=0 started_beta=0

# turn <alpha|beta> <prompt>: one prompt in that session, first starting it.
turn() {
  local who="$1" prompt="$2" id flag
  if [ "$who" = alpha ]; then id="$ALPHA"; else id="$BETA"; fi
  if [ "$who" = alpha ] && [ $started_alpha = 0 ]; then flag=(--session-id "$id"); started_alpha=1
  elif [ "$who" = beta ] && [ $started_beta = 0 ]; then flag=(--session-id "$id"); started_beta=1
  else flag=(--resume "$id"); fi
  # The prompt goes first: --allowedTools takes every argument after it.
  claude -p "$prompt" --model "$MODEL" --settings "$SETTINGS" "${flag[@]}" \
    --allowedTools "Read,Edit,Bash(git add:*),Bash(git commit:*),Bash(git status:*)" \
    < /dev/null > "$TMP/$who.last" 2>&1 || true
}

WORDS=(the quick brown fox jumps over the lazy dog)
echo "writing the sentence, alternating alpha and beta"
for i in "${!WORDS[@]}"; do
  who=alpha; [ $((i % 2)) = 1 ] && who=beta
  w="${WORDS[$i]}"
  turn "$who" "In sentence.txt in the current directory, use the Edit tool to replace the exact text <end> with the text '$w <end>' (the word, a space, then <end>). Read the file first. Change nothing else, do not commit, and reply DONE."
  printf '  %-5s %-5s -> %s\n' "$who" "$w" "$(tr -d '\n' < sentence.txt)"
done

echo "checks"
check "the sentence is complete, every word in order" \
  '[ "$(tr -d "\n" < sentence.txt)" = "the quick brown fox jumps over the lazy dog <end>" ]'
check "nobody was refused an edit" '! grep -q "refuse Write" "$TMP/coord/coord.log" 2>/dev/null'
check "beta shared the file 4 times, each logged" '[ "$(grep -c "share .*sentence.txt: ${BETA:0:8} edits, master ${ALPHA:0:8}" "$TMP/coord/coord.log")" = 4 ]'
STATUS="$("$BIN" coord status)"
echo "$STATUS" | sed 's/^/    /'
check "alpha is the master" 'echo "$STATUS" | grep -q "master .*\[${ALPHA:0:8}\]"'
check "beta is shown coordinating with it" 'echo "$STATUS" | grep -q "also changed by .*\[${BETA:0:8}\]"'

echo "commits"
turn beta "Commit sentence.txt now with git add sentence.txt and git commit -m beta. If a hook refuses it, do not try another way; reply REFUSED."
check "beta's commit of the file alpha masters was refused" '[ "$(git log --format=%s -1)" = "empty sentence" ] && grep -q "refuse ${BETA:0:8} staging" "$TMP/coord/coord.log"'
turn alpha "Commit sentence.txt now: git add sentence.txt && git commit -m 'sentence by alpha and beta'. Reply DONE."
check "alpha committed it, both sessions' words included" '[ "$(git log --format=%s -1)" = "sentence by alpha and beta" ] && git show HEAD:sentence.txt | grep -q "lazy dog"'
check "committed, so no longer shared" '"$BIN" coord status | grep -A1 "FILES BEING EDITED" | grep -q "none"'

echo
echo "$PASS passed, $FAIL failed"
[ "${KEEP:-}" = 1 ] && echo "kept: $TMP"
[ $FAIL = 0 ]
