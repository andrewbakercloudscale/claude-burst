#!/usr/bin/env bash
# SessionStart hook, installed by claude-burst (internal/handover). Generated:
# edits here are overwritten; change internal/handover/scripts/start.sh instead.
#
# Puts the latest HANDOFF.md section, and what happened since it was written,
# in front of Claude. Opt-in per repo: does nothing unless the repo has HANDOFF.md.
set -uo pipefail
DIR="$(cd "$(dirname "$0")" && pwd)"

[[ -n "${CLAUDE_HANDOVER_WRITER:-}" ]] && exit 0   # the writer's own session
[[ -f "$DIR/settings.json" ]] || exit 0

input=$(cat)
read -r brief source cwd < <(python3 - "$DIR/settings.json" "$input" <<'PY'
import json, sys
s = json.load(open(sys.argv[1])); d = json.loads(sys.argv[2] or "{}")
print(int(bool(s.get("brief"))), d.get("source") or "-", d.get("cwd") or ".")
PY
)
[[ "$brief" == 1 ]] || exit 0
# resume and compact already carry the conversation; only fresh starts need briefing
[[ "$source" == "startup" || "$source" == "clear" ]] || exit 0

root=$(git -C "$cwd" rev-parse --show-toplevel 2>/dev/null) || exit 0
file="$root/HANDOFF.md"
[[ -f "$file" ]] || exit 0

{
  python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["briefing"].replace("{{file}}", sys.argv[2]))' "$DIR/settings.json" "$file"
  echo
  echo "===== Latest handover section ====="
  # from the top to the line before the second top-level heading
  awk 'NR>1 && /^# /{exit} {print}' "$file" | head -n 200
  echo
  last=$(git -C "$root" log -1 --format=%H -- HANDOFF.md 2>/dev/null)
  if [[ -n "$last" ]]; then
    echo "===== Commits since HANDOFF.md was last committed ($(git -C "$root" log -1 --date=format-local:'%Y-%m-%d %H:%M' --format=%ad "$last") local) ====="
    git -C "$root" log --date=format-local:'%m-%d %H:%M' --format='%h %ad %s' "$last"..HEAD | head -n 40
    echo
  fi
  echo "===== Working tree ====="
  git -C "$root" status --short --branch | head -n 30
  echo
  echo "===== Tracked layout ====="
  git -C "$root" ls-files | awk -F/ 'NF>1{print $1"/"(NF>2?$2"/":$2)} NF==1{print}' | sort -u | head -n 60
} | head -c 9500 | python3 -c 'import json,sys; print(json.dumps({"hookSpecificOutput":{"hookEventName":"SessionStart","additionalContext":sys.stdin.read()}}))'
