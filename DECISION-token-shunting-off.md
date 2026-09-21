# Decision: token shunting is switched off (2026-09-21)

**Status:** off. The code is still in the tree and can be re-enabled with
`claude-burst shunt enable`. It is not recommended, and it may be deleted.

## What it was for

The guard hook blocked whole-file `Read`s (and `cat`/`head`/`tail`) of files over 350 lines
and told Claude to run `claude-burst shunt read --question "..."` instead. A cheap worker
model (GLM-5.3 via Together) would read the file and return a short answer, so the bulk of
the file never entered Claude's context. A second half generated boilerplate the same way.

## Why it is off

**It never shunted anything in real use.** From the first event (2026-09-20 14:21) to
2026-09-21 14:00, `shunt.jsonl` held:

| | count |
|---|---|
| direct reads blocked, in real plugin projects (seo-optimizer 5, backup-restore 4, cyber-devtools 3) | 12 |
| delegated reads that followed, in those projects | **0** |
| delegated reads at all | 5, every one a `shunt-live-check` test run |
| code-generation writes | 0 |

The transcripts of two of the blocked sessions (both standards-review runs in
`wordpress-seo-ai-optimizer`) show why. Claude is blocked, says "windowed reads are
allowed", and then reads the file in 340-line chunks. Not once did it run `shunt read`.
340 is just under the 350 threshold.

That is the design, not a bug in it:

1. **The block message hands over the bypass.** It ends with "Windowed reads are never
   blocked", because Claude must be able to read exact lines it is about to edit or quote.
   Closing that hole breaks legitimate work.
2. **The saving needs Claude to want an answer rather than the code.** For nearly every
   large-file read in practice it wants the code, so it takes the window route. The guard
   then saves nothing and costs a blocked call and a few seconds per file.
3. **The heaviest readers are the ones it should never touch.** The standards-review gate
   (`run-standards-review.sh`, `claude --print` in parallel sections) has to read the code in
   full: a worker's summary would weaken a security gate. Those sessions were the main source
   of blocks, and gained nothing from them.

## Why it failed here when the source article reports ~90%

Checked against the article this was built from (andrewbaker.ninja, 2026-09-17, describing
Spotify's technique). **The implementation matches it**: a `PreToolUse` hook at 350 lines that
denies the read and returns a redirect, windowed reads deliberately allowed ("a targeted window
into a large file still works"), and a deny message that itself says "or re-read with an
explicit offset and limit if you need specific lines". So the window loophole is the source
design, not something we introduced. The gap is in what the number measures and what we tested.

1. **The 90% is the case the technique was built to win.** The article says so: a mean across
   four scenarios on a Java monorepo, the bulk-read case "rather than a whole working day of
   mixed activity". It does not state the scenarios' prompts, and does not state how often the
   model followed the redirect rather than working around it. An independent rebuild it cites
   measured ~60% fewer frontier tokens, ~33% lower total cost, and ~65% more elapsed time.
2. **Our workload is on the article's own "do not delegate" list.** It names editing existing
   code, debugging, security review and architectural reasoning. Our blocks came from
   standards-review runs (security review) and from sessions that were about to edit the files.
3. **Our proof forced the behaviour it was meant to test.** The live tests
   (`TestLiveShunt*`) prompt Claude with "do not pass offset or limit" and "do not use grep, sed,
   head, tail... follow the instructions in the error message exactly". Of course it complied:
   4 blocks, 4 delegated reads. That proved the plumbing works. It could not show whether Claude
   delegates when it has a choice, and with a choice the real rate was 0 of 12.
4. **The saving was small even when it worked.** Five shunts, ~32.7K tokens kept out (an
   estimate), $0.08 of worker cost. Whether that is worth a hook on every read on a Max plan,
   rather than per-token API billing, was never measured.

**What this does not show.** Twelve blocks, mostly from headless review sessions, is a sample
biased toward exactly the workload the article excludes. "Not demonstrated on our workload" is
supported; "the technique does not work" is not. The article's own cheaper alternative is a
subagent pinned to a cheaper model for codebase research, which needs no hook.

## What it cost to keep

About 3,500 lines of Go, 2,000 lines of tests, a dashboard panel, a Claude Code hook in
`~/.claude/settings.json`, and a skill. All of it in exchange for a feature that had not
delivered a single real shunt in its first day of use.

## What was done

- `claude-burst shunt disable`: removes the guard hook and the skill and sets `read` and
  `write` to false. Verified afterwards: `~/.claude/settings.json` differs from the
  post-deploy backup only by the removed hook, and no `shunt guard` entry remains.
- Restart Claude Code for open sessions to drop the hook. Until then they keep calling the
  installed binary's guard, which now allows everything because the feature is off in config.
- Nothing was deleted. `shunt.jsonl` is kept as the evidence above.

## If someone wants to revisit it

Do not re-enable it and hope. It needs a measured reason to believe Claude will actually
delegate:

- Count real shunts, not blocks. `claude-burst shunt log --problems` and the dashboard both
  fold a block and its answer into one `SHUNTED` row; a `NO-SHUNT` row is a block nothing
  answered.
- A design that does not depend on Claude choosing to ask a question, or does not rely on
  blocking, is a different feature, not a tuning of this one.
- Exempt non-interactive review runs before anything else.

## Removing it

`./install.sh uninstall` already removes the hook and skill. Deleting the code is one commit
touching `internal/shunt/`, `cmd/claude-burst/shunt.go`, `internal/admin/shunt.go`, the
`shunt_*` tests, the dashboard panel in `internal/admin/admin.html` and the README section.
Git history keeps it.
