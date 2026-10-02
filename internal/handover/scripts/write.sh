#!/usr/bin/env bash
# Detached worker started by end.sh, installed by claude-burst (internal/handover).
# Generated: edits here are overwritten; change internal/handover/scripts/write.sh.
#
# Resumes a fork of the finished session (so the writer has the whole
# conversation), lets it update HANDOFF.md, then commits that one file locally.
# Never pushes.
set -uo pipefail
DIR="$(cd "$(dirname "$0")" && pwd)"
sid=$1 cwd=$2 root=$3
file="$root/HANDOFF.md"
out="$DIR/last-run.json"   # the writer's full reply, for when it goes wrong
say() { echo "$(date '+%Y-%m-%d %H:%M:%S') $* [$sid]"; }
notify() {  # CLAUDE_HANDOVER_NO_NOTIFY is for the tests, which run this script for real
  [[ -n "${CLAUDE_HANDOVER_NO_NOTIFY:-}" ]] && return 0
  /usr/bin/osascript -e "display notification \"$1\" with title \"Claude handover\"" >/dev/null 2>&1 || true
}

# one writer per repo at a time: closing Ghostty ends every tab's session at once
lock="$root/.git/handover.lock"
for _ in $(seq 1 600); do mkdir "$lock" 2>/dev/null && break; sleep 1; done
[[ -d "$lock" ]] || { say "FAILED $root: gave up waiting for $lock"; exit 1; }
trap 'rmdir "$lock" 2>/dev/null' EXIT

now=$(date '+%Y-%m-%d %H:%M')
read -r model commit < <(python3 -c 'import json,sys; s=json.load(open(sys.argv[1])); print(s.get("model") or "opus", int(bool(s.get("commit"))))' "$DIR/settings.json")
prompt=$(python3 -c 'import json,sys; t=json.load(open(sys.argv[1]))["instructions"]; print(t.replace("{{file}}", sys.argv[2]).replace("{{now}}", sys.argv[3]).replace("{{session}}", sys.argv[4]))' "$DIR/settings.json" "$file" "$now" "$sid")

dirty_before=$(git -C "$root" status --porcelain -- HANDOFF.md)
before=$(shasum "$file" | cut -d' ' -f1)
say "write  $root with $model"

cd "$cwd" || { say "FAILED $root: $cwd is gone"; exit 1; }
# The prompt goes on stdin: --allowedTools is variadic and swallows a trailing
# prompt argument, which then fails with a confusing "deferred tool marker" error.
CLAUDE_HANDOVER_WRITER=1 /usr/bin/perl -e 'alarm 900; exec @ARGV' claude -p \
  --resume "$sid" --fork-session --model "$model" \
  --permission-mode acceptEdits \
  --allowedTools="Read,Edit,Write,Grep,Glob,Bash(git log:*),Bash(git status:*),Bash(git diff:*),Bash(git show:*)" \
  --output-format json <<<"$prompt" >"$out" 2>&1
rc=$?

# The instructions forbid em and en dashes and a model still wrote one in
# testing. Older sections are already dash-free, so a whole-file pass only
# touches the new one.
[[ $rc -eq 0 ]] && python3 -c 'import sys; p=sys.argv[1]; s=open(p).read(); t=s.replace(" \u2014 ", ", ").replace("\u2014", ", ").replace("\u2013", "-"); t!=s and open(p,"w").write(t)' "$file"
after=$(shasum "$file" | cut -d' ' -f1)

if [[ $rc -ne 0 ]]; then
  say "FAILED $root: claude exited $rc: $(tail -c 300 "$out" | tr '\n' ' ')"
  notify "Handover FAILED for $(basename "$root"), see the claude-burst dashboard"; exit 1
fi
if [[ "$before" == "$after" ]]; then
  say "none   $root: nothing worth handing over"; exit 0
fi
if [[ "$commit" != 1 ]]; then
  say "wrote  $root (not committed: auto-commit is off)"
  notify "Handover updated in $(basename "$root")"; exit 0
fi
# A gitignored HANDOFF.md is local notes by choice (a public repo should not
# publish them), so there is nothing to commit.
if git -C "$root" check-ignore -q HANDOFF.md 2>/dev/null; then
  say "wrote  $root (local only: HANDOFF.md is gitignored)"
  notify "Handover updated in $(basename "$root")"; exit 0
fi
if [[ -n "$dirty_before" ]]; then
  say "wrote  $root (not committed: HANDOFF.md already had uncommitted edits)"
  notify "Handover updated in $(basename "$root"), left uncommitted"; exit 0
fi
if git -C "$root" commit -q -m "Update the handover at session close ($now)" -- HANDOFF.md; then
  say "wrote  $root, committed $(git -C "$root" rev-parse --short HEAD)"
else
  say "wrote  $root (commit failed)"
fi
notify "Handover updated in $(basename "$root")"
