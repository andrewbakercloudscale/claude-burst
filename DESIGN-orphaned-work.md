# Design: uncommitted work left behind by a session that has ended

Status: proposed, 2026-10-01. Nothing here is built yet.

## The incident

On 2026-10-01 one Claude Code session ran `scripts/deploy.sh` in claude-burst.
`deploy.sh` builds from the working tree, and the tree held another session's
uncommitted edits to `internal/router/compact_run.go` and `compact_test.go`
(keeping compaction notices across restarts). They shipped. The tests
happened to pass, so the gateway was fine, but:

- production ran code that existed in no commit;
- the session that wrote it had ended, so nobody could be told;
- the session that shipped it could only say "whoever picks it up should
  commit those two files", and hope.

The same thing happened three times in the WordPress plugins (2026-08-14 and
2026-08-17, see their CLAUDE.md "Deployment archive"). It is the commonest
way multi-session work goes wrong here.

Session coordination was **off** at the time (`coordination` unset in
config.json), so as built it could not have helped. Even when on, it would
not have helped, for two reasons:

1. **A master that ends with no other contributor lets the file go.**
   `handOn` finds no live contributor, deletes the entry and logs "free".
   The uncommitted changes are still on disk but no longer belong to anyone,
   and nothing will ever mention them again.
2. **Only git staging commands are checked.** `preBash` looks at `git add`
   and `git commit`. A deploy or install script that builds from the working
   tree sweeps up other people's work just as `git add -A` does, and nothing
   looks at it.

## Goals

- Uncommitted work never becomes ownerless silently. It is either owned by
  a live session, or visibly **orphaned** with a name and a date on it.
- Shipping a working tree that holds someone else's uncommitted work needs
  a deliberate yes, and leaves a record either way.
- The first layer works with coordination off.
- Still fail-open: a broken coordinator must never block work, only warn.

Non-goals: merging, locking files, or committing on anyone's behalf. A
commit is always made by a session that has read the diff.

## The two rules (the main design)

Orphans and guards are fallbacks. The fix is to stop work being left
uncommitted in the first place, so that every handover is clean.

### Rule 1: commit before going idle

A session must commit the files it masters before it stops.

- At **Stop** (the end of every turn), if this session masters any file
  that git says is uncommitted, the Stop hook answers
  `{"decision": "block", "reason": …}`. Claude Code then does not stop: the
  session carries on with the reason as its instruction:

  > COORDINATION: you master these files and they have uncommitted changes:
  > <list>. Commit them now, by name, with a message saying what they are
  > (`git add <files> && git commit`), then stop. Commit locally only, never
  > push. If a change is unfinished, commit it anyway with "WIP:" in the
  > message, so the next session can see and continue it.

- It blocks **once per turn**. Claude Code sets `stop_hook_active` on the
  second Stop of the same turn; then the session is let go, so a session
  that cannot commit (a failing pre-commit hook, a conflict) never loops.
  What is still uncommitted then falls to layer 2 and is orphaned, with the
  reason the session gave.
- Contributors are covered by their master: a contributor's change is
  committed by the master, as today, so only masters are asked.
- **Unfinished work is committed as WIP, not left on disk.** A WIP commit is
  visible, attributable, revertable and survives anything; an uncommitted
  edit is none of those. This is the trade-off being chosen.
- It applies only while coordination is on and only in a git repository.
  Files git does not track are settled at Stop, as now.

### Rule 2: an idle master can be replaced by the session that needs the file

Today an idle or ended master hands the file to its most recently active
contributor, or lets it go. Instead, **the session that is asking for the
file takes it over**:

- When a session Edits or Writes a file whose master has been idle longer
  than **Master idle** (15 minutes by default) or has ended, that session
  becomes the master on the spot. Its edit goes through, and it is told:
  "You are now the master of <file>; <name> was, and has been idle for
  <time>. Commit it when your work in it is done."
- Because of rule 1 the old master committed before it went idle, so the
  new master inherits a clean file. If it did not (blocked commit, crash),
  the new master is also told "It holds <name>'s uncommitted changes: read
  `git diff -- <file>`, keep them, and commit them with yours."
- The old master, if it comes back, is told at its next prompt that it lost
  the file and to whom, so it does not commit over the new master's work.
- **Take over by hand** for a master that is busy, not idle: `claude-burst
  coord take <path>` and a Take over button on the dashboard. It asks the
  current master first (a message) and only takes the file if the master
  does not answer within Master idle, so a working session is never robbed
  mid-change.
- With no session asking for the file, nothing happens at idle: the file
  stays with its idle master, which has already committed it (rule 1). The
  passing-on in `handOn` today becomes unnecessary and is removed.

Together: work is committed when a session goes quiet, and the file is free
to whoever needs it next. No session waits on another, and there is nothing
to inherit.

## Layer 1: the ship guard (coordination on or off)

A check that runs before any script that builds from the working tree:
claude-burst's `scripts/deploy.sh` and `install.sh`, and each WordPress
plugin's `deploy-wordpress.sh`.

1. List what is uncommitted: `git status --porcelain`, with untracked files.
2. Nothing uncommitted: go ahead, as now.
3. Otherwise print the files, and for each one who changed it, where that is
   known (coordination state, layer 2), else "unknown session".
4. Refuse unless one of these is given:
   - `--ship-uncommitted`: go ahead knowingly. The list is written to the
     deploy log and the deploy archive's manifest, so "what shipped that was
     not in git" can be answered later.
   - `--only-committed`: build from a temporary checkout of HEAD
     (`git worktree add`, build there, remove it). Nobody's work is touched
     and nothing uncommitted ships.

`--only-committed` should become the default once it is proven: shipping
exactly a commit is what every reviewer assumes a deploy does. For now the
refusal plus the two flags make the choice explicit, which is the part that
was missing.

One shared script, `scripts/check-uncommitted.sh`, called by every build or
deploy script, like the WordPress build gates. One copy, not five: the
help-doc generators drifted exactly because they were copied.

## Layer 2: orphaned work in session coordination (the fallback)

With rule 1 in force this is rare: a session that could not commit, crashed,
was killed, or ran with coordination off. It must still never be silent.

### Orphan instead of free

When a master ends (Stop with the session gone, `endSession`, or `gc` of a
stale session) or goes idle, and `handOn` finds no live contributor:

- if git says the file is clean or unknown to git: free it, as now;
- if the file still has uncommitted changes: keep the entry and mark it
  **orphaned**:

```json
"internal/router/compact_run.go": {
  "master": "",
  "orphaned": 1790845200.0,
  "last_master": "1fa228…",
  "last_master_name": "claude-burst-74",
  "transcript": "/Users/…/1fa228….jsonl",
  "contributors": {}
}
```

The transcript path is kept so the adopter can read what the work was for.

### Who finds out

- **SessionStart** in the same repository: the briefing gets a section,
  "Uncommitted work left by sessions that have ended", listing each orphan
  with the session name, how long ago, and `git diff --stat` for it.
- **UserPromptSubmit**, once per session per orphan set: the same list,
  shorter, so a session that started before the orphaning still hears.
- **The ship guard** (layer 1) names the orphan's last master instead of
  "unknown session".
- **The dashboard**: an "Orphaned work" list in Session coordination, red,
  with the session name, age, files and the actions below. Its menu dot goes
  amber while any orphan exists.
- **A macOS notification** when work is orphaned and no other session is
  active in that repository, since then nobody else will be briefed.

### Getting an owner again

- **Resume**: if the session that left the work comes back (`claude --resume`
  keeps the session id), `touch` gives it its files back as master. This is
  the common case and needs no decision.
- **Adopt by editing**: the next session to Edit an orphaned file becomes its
  master and is told: "You have adopted <name>'s uncommitted changes in
  <file>. Read `git diff -- <file>`, and the transcript at <path> if you
  need to know what they were for. Commit them with your own work, or tell
  the user they should be discarded. Never discard them yourself."
- **Adopt explicitly**: `claude-burst coord adopt <path|repo>` and an Adopt
  button on the dashboard, which make a chosen live session the master and
  send it the same message.
- **Commit settles it**: any commit that includes the file ends the orphan,
  as it ends sharing today.

Discarding is deliberately not an action. Throwing work away is the user's
call, made in git, not a button.

### Messages to a session that has ended

`Send` to a session that is not alive currently fails. It should instead
attach the message to that session's orphans, so whoever adopts them reads
it ("your edits shipped in the 21:02 deploy; they are live, so commit them").
The sender is told the session has ended and where the message went.

### Shipped orphans

When the ship guard is passed with `--ship-uncommitted`, each orphan in the
list is marked `shipped` with the time. A shipped orphan is the urgent case,
because production now runs code that is in no commit. It is shown first,
and its briefing line says so.

## Edge cases

- **Edits not made with Edit/Write** (sed, a script, a formatter) are
  invisible to the hooks, so they never had a master. At Stop, the session's
  repository is checked with `git status`; dirty files nobody masters are
  attributed to the ending session if its transcript shows a Bash command
  naming them, else listed as "uncommitted, author unknown". Unknown is
  still better than invisible.
- **Coordination off**: layer 2 does not run, layer 1 still does, and its
  message says coordination would have named the author.
- **Several orphans from one session**: grouped per session and repository
  everywhere, so adopting the group is one action.
- **State file growth**: orphans age out only when committed, adopted or
  released by hand. The dashboard shows their age so a stale one is visible.

## Build order

1. **Rule 1**, commit before going idle: the Stop hook block, once per turn.
   Smallest change with the biggest effect.
2. **Rule 2**, take over from an idle or ended master on Edit, plus the
   "you lost the file" notice; remove the contributor hand-on.
3. `check-uncommitted.sh` and the refusal in claude-burst's `deploy.sh` and
   `install.sh` (layer 1; would have stopped this incident even with
   coordination off).
4. The same guard in the five WordPress deploy scripts.
5. Orphan instead of free, with SessionStart briefing and resume reclaim.
6. Dashboard list, Adopt and Take over buttons, notifications.
7. Messages to ended sessions, shipped-orphan marking.
8. `--only-committed` builds from a worktree of HEAD, then make it the
   default.

## Decisions for the user

- **Rule 1 changes how every session ends a turn**: a turn that edited files
  ends with a local commit. That is more commits, many of them WIP. The
  alternative (ask, do not insist) is what failed here, so the design
  insists, once per turn.
- **Master idle** (15 minutes) now decides when another session may take a
  file. Shorter means less waiting, longer means fewer surprises for a
  session that was only reading.

## Tests to write first

- Stop with an uncommitted mastered file blocks once with the commit
  instruction; with `stop_hook_active` it lets the session stop and orphans
  the file; with everything committed it does not block.
- An Edit of a file whose master is idle or ended makes the editor master;
  an Edit of a file whose master is active does not.
- The old master is told at its next prompt that it lost the file.
- A master that ends with uncommitted changes leaves an orphan, not a free
  entry; with a clean file it frees as before.
- A resumed session gets its orphans back; another session's Edit adopts
  them and is told so.
- A commit of an orphaned file settles it.
- The ship guard refuses on a dirty tree, passes on a clean one, and
  `--ship-uncommitted` writes the manifest.
- Every failure path of the guard (no git, not a repository) lets the
  deploy through with a warning: fail open, like the rest of coordination.
