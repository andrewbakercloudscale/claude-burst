# Sourced by deploy.sh and install.sh. Both build the WORKING TREE, so they
# can ship changes that are in no commit: on 2026-10-01 a deploy shipped
# another session's uncommitted compaction edits, and that session had
# ended, so nobody could be told. Git then did not contain what was running.
#
# This does not refuse. It cannot tell whose changes are whose, so refusing
# would block every ordinary "edit, then deploy". Refusing a deploy while
# ANOTHER session has uncommitted work is session coordination's job (its
# pre-tool hook knows who is asking). Here: say plainly what is shipping
# that no commit holds, and keep a copy of it, so "what was running" can
# always be answered and restored. `deploy.sh --only-committed` ships
# exactly HEAD instead.

# record_uncommitted ROOT WHAT: lists uncommitted files in ROOT and, if any,
# saves them as a patch (tracked changes) plus a tarball (untracked files)
# and appends a line to the shipped-uncommitted log. WHAT names the caller
# ("deploy", "install"). Never fails the caller: a record that cannot be
# written is reported, not fatal, since the build is already decided.
record_uncommitted() {
  local root="$1" what="$2" dir="$HOME/.config/claude-burst/shipped-uncommitted"
  local ts files head untracked n
  git -C "$root" rev-parse --git-dir >/dev/null 2>&1 || return 0
  files="$(git -C "$root" status --porcelain 2>/dev/null)" || return 0
  [[ -z "$files" ]] && return 0
  ts="$(date +%Y%m%d-%H%M%S)"
  head="$(git -C "$root" rev-parse --short HEAD 2>/dev/null || echo none)"
  n="$(printf '%s\n' "$files" | wc -l | tr -d ' ')"
  echo "[$what] WARNING: shipping $n uncommitted file(s), in no commit (HEAD is $head):"
  printf '%s\n' "$files" | sed "s/^/[$what]   /"
  if ! mkdir -p "$dir" 2>/dev/null; then
    echo "[$what] could not create $dir: no copy of the uncommitted changes was kept" >&2
    return 0
  fi
  git -C "$root" diff HEAD --binary > "$dir/$ts-$what.patch" 2>/dev/null
  untracked="$(git -C "$root" ls-files --others --exclude-standard 2>/dev/null)"
  if [[ -n "$untracked" ]]; then
    printf '%s\n' "$untracked" | (cd "$root" && tar -czf "$dir/$ts-$what-untracked.tgz" -T - 2>/dev/null)
  fi
  printf '%s %s %s HEAD=%s %s file(s): %s\n' "$ts" "$what" "$root" "$head" "$n" \
    "$(printf '%s\n' "$files" | awk '{print $NF}' | tr '\n' ' ')" >> "$dir/log"
  echo "[$what] copy kept: $dir/$ts-$what.patch${untracked:+ (+ untracked files in $ts-$what-untracked.tgz)}"
  echo "[$what] commit these so git holds what is running, or deploy with --only-committed to ship HEAD alone."
  # Keep the newest 50 records.
  ls -1t "$dir"/*.patch 2>/dev/null | tail -n +51 | while read -r old; do
    rm -f "$old" "${old%.patch}-untracked.tgz"
  done
  return 0
}
