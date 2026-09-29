#!/usr/bin/env bash
# SessionEnd hook, installed by claude-burst (internal/handover). Generated:
# edits here are overwritten; change internal/handover/scripts/end.sh instead.
#
# Closing a Ghostty window ends the session with reason "other". The terminal
# and this hook are about to disappear and writing takes a minute, so the work
# is handed to write.sh in its own session (setsid), out of reach of SIGHUP.
set -uo pipefail
DIR="$(cd "$(dirname "$0")" && pwd)"
log="$DIR/handover.log"

[[ -n "${CLAUDE_HANDOVER_WRITER:-}" ]] && exit 0   # never recurse from the writer
[[ -f "$DIR/settings.json" ]] || exit 0

input=$(cat)
# prints: write-enabled min-prompts typed-prompts session cwd reason
read -r write min prompts sid reason cwd < <(python3 - "$DIR/settings.json" "$input" <<'PY'
import json, sys
s = json.load(open(sys.argv[1])); d = json.loads(sys.argv[2] or "{}")
n = 0
try:
    for line in open(d.get("transcript_path") or "", errors="replace"):
        try: e = json.loads(line)
        except ValueError: continue
        if e.get("type") != "user" or e.get("isMeta") or e.get("isSidechain"): continue
        c = (e.get("message") or {}).get("content")
        if isinstance(c, list):  # tool results are lists without text parts
            c = " ".join(p.get("text", "") for p in c if isinstance(p, dict) and p.get("type") == "text")
        if isinstance(c, str) and c.strip() and not c.lstrip().startswith(("<command-", "<local-command", "<system-reminder")):
            n += 1
except OSError:
    pass
print(int(bool(s.get("write"))), s.get("min_prompts", 2), n, d.get("session_id") or "-", d.get("reason") or "-", d.get("cwd") or ".")
PY
)
[[ "$write" == 1 && "$sid" != "-" ]] || exit 0
root=$(git -C "$cwd" rev-parse --show-toplevel 2>/dev/null) || exit 0
[[ -f "$root/HANDOFF.md" ]] || exit 0

ts=$(date '+%Y-%m-%d %H:%M:%S')
if (( prompts < min )); then
  echo "$ts skip   $root: $prompts typed prompt(s), fewer than $min [$sid]" >>"$log"
  exit 0
fi
echo "$ts queue  $root: $prompts typed prompts, session ended ($reason) [$sid]" >>"$log"
nohup /usr/bin/perl -MPOSIX -e 'POSIX::setsid(); exec @ARGV' \
  "$DIR/write.sh" "$sid" "$cwd" "$root" >>"$log" 2>&1 </dev/null &
exit 0
