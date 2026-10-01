# Handover, 2026-10-01 20:43: coordination metrics and issues, masters commit others' work first, subagents, lock starvation fix
<!-- session: 0125e44d-d7bf-4778-8143-1c51dcab7da4 -->

## State right now

- Written at 20:43. Verify before acting.
- `main` at `774da37`, pushed, working tree clean (git status at 20:43). Every commit below deployed with `scripts/deploy.sh`; gateway running under launchd (pid 43873).
- Session coordination is ON with seven hooks in `~/.claude/settings.json` (SubagentStop added today).
- Every repo under `~/Desktop/github` was in step with GitHub at about 19:00 (fetched, 0 ahead / 0 behind), except files other sessions have left uncommitted (listed under Open 4).
- Dashboard Issues: 12 lock errors from 18:18-18:19 cleared with the new Clear button; 0 open at 20:40.

## What was done

- `7bcf6de` Dashboard "Is it working?": Today / 7 days / 14 days, tiles (Coordinated = shared + refused + taken over + handed on + asked for), per-day table, Issues table (hook errors, stopped with uncommitted work), red menu dot while any is open. Counted from `~/.config/claude-burst/coord/coord.log` by `internal/admin/coordstats.go`; `/api/coordination?days=`. Scratch and temp-dir files are no longer tracked (`scratchPath`; tests set `CLAUDE_BURST_COORD_TRACK_TMP=1`).
- `917ef4b` The user's direction: nobody waits on a master, commits are accelerated.
  - Shared edit: the master gets a PRIORITY message at its next tool call to commit the file now, ahead of its own work ("WIP:" if unfinished).
  - Refused deploy: the coordinator itself messages each master holding uncommitted work in that repo (`mastersIn`); the deployer is told to carry on and retry.
  - Briefing rule 7: PRIORITY messages first, never make another session wait.
  - Subagents: hook input from inside a subagent carries `agent_id` with the parent's session id. Files record the subagent that last edited them (`shared.Agent`), sessions keep running agents (`session.Agents`, removed by the SubagentStop hook, gone after 2h silent), and Stop skips a running agent's files. Cause: cyber-devtools session held at 18:08 to commit its comment-flood agent's files.
  - Each session shows the latest typed request of 5+ words (`sessionTask`), because folder-named sessions share a window title. `ab37b02` skips `[Image ...]` placeholders and reads up to 16 MB back.
- `fa39692` Lock starvation: every hook held the state lock while running one git process per tracked file in series; with many agents firing hooks, others waited out the 3s lockWait (12 errors at 18:18-18:19). Now `prewarm` asks git about all tracked files in parallel before the lock (`tx.git` uses the answers); holds over 1s log "slow: held the state lock". `TestBusyHooksDoNotStarveTheLock` fails on the old code.
- `e657906` Clear hook errors button (logs a marker line; earlier errors leave Issues, stay counted). `774da37` stopped-uncommitted issues ask git whether pending files are still dirty (a merged worktree left one open forever); Status() now logs the files it frees.
- `715f541` README coordination section rewritten: design rule, subagents, metrics, Issues, three Mermaid diagrams.
- Outside this repo: pushed cyber-devtools (32 commits), CloudScale-Wp-Proxy (3), cloudscale-wp-shared (1), cloudscale-submission-validator (1), iphone-image-manager (5); deploy-record ignores (`archive/*.manifest.txt`, `*.uncommitted.patch`, `*.untracked.tgz`) added to all five plugins' `.gitignore`; cleanup and crash-recovery SVN-mirror clones reset to GitHub (they were 114 and 44 behind, their one commit would have deleted newer work; backups deleted on the user's word); `claude-traffic-light` fast-forwarded 824 commits.
- A blog brief on how coordination works (with three Mermaid diagrams) was put on the clipboard for the CloudScale-Wp-Proxy session to write up.

## Open

1. **Offered, user not yet answered:** an `--only-committed` mode for each plugin's `deploy-wordpress.sh` (build from a throwaway worktree of HEAD, like `scripts/deploy.sh --only-committed`), which removes the last case where a deploy waits on another session. Gitignored script, five repos.
2. Unverified live: that Claude Code fires SubagentStop for background agents (unit tests only). Check: after a session's background agent finishes, `~/.config/claude-burst/coord/state.json` has no entry for it under that session's `agents`.
3. `scripts/coord-live-test.sh` not rerun since these changes; the dashboard section not checked in a browser (Chrome extension was not connected); the API and served page were checked, and the page JS parses.
4. Uncommitted work left by others, not touched: `CloudScaleWpPluginHelpDocs/plugins/cyber-devtools/generate.js`; `deploy-wordpress.sh` in `cloudscale-test-accounts` and `cloudscale-sql-runner`; `raspberry-pis` submodule changes and untracked `andrew-baker-cobalt/`; `.omc/`, `.codex/`, `.playwright-mcp/` clutter; `linkedin-chrome-plugin/HANDOVER.md`.
5. From the 17:25 handover, still open unless done since: screen-off with lid shut (`scripts/lid-awake-root.sh`), P0 list in memory `claude_burst_next_priorities.md`, restart-warning decision.

## Things that will bite you

- Running sessions only see briefing rule 7 when they next start; PRIORITY messages reach them now.
- `prewarm` reads `state.json` without the lock (safe: written by rename) and its answers are milliseconds old under the lock. A file not tracked before the hook is asked live.
- Test repos are temp dirs, which coordination now ignores: any new test that runs hooks needs `CLAUDE_BURST_COORD_TRACK_TMP=1` (set in coord's TestMain; set per test in admin).
- Error issues stay open 24h unless cleared; the Clear marker is the literal line `hook errors cleared from the dashboard` in coord.log.
- This session ran `git stash -- internal/coord/coord.go` once in the shared claude-burst tree (popped at once, own file only). Do not: rule 4.
- No em or en dashes; no Claude co-author trailers in commits.

# Handover, 2026-10-01 17:25: session coordination, single plan, version check, /compact-async, screen off with lid shut
<!-- session: e84447e0-d2f1-4a53-a55d-17c6390846ba -->

## State right now

- Written at session close. Verify before acting.
- git could not be run at close (read-only git commands needed approval), so everything below about commits and the working tree is from the conversation, not checked now.
- Last known pushed and deployed: `main` at the commit "Session coordination dashboard: message a session, hand a file on, recent activity" (after `1f10b9d`), release tag `v0.3.0` at `74aa733`. Unverified whether later commits exist.
- **Uncommitted at close (as last seen):** `scripts/lid-awake-root.sh`, the screen-off change (see Open 1). Not tested, not committed, not installed: the root daemon runs a root-owned copy in `/usr/local/libexec/claude-burst`, so it only takes effect after `sudo scripts/lid-awake-root.sh apply ...` (the dashboard's Apply, which asks for a password).
- Several files under `internal/coord`, `internal/admin` (upgrade, finder), `scripts/coord-live-test.sh`, `internal/router/doh_outage_test.go` changed on disk after this session last touched them, by another session. The SessionStart briefing now mentions rules this session did not write (commit before stopping with "WIP:", `coord take`, an idle master's files going to whoever edits next). Treat `internal/coord` as moved on; read it before changing it.
- Session coordination appears to be ON now (a SessionStart coordination briefing was injected at close). This session left it off; unverified who switched it on.
- oh-my-claudecode autopilot keyword trigger disabled in `~/.config/claude-omc/config.jsonc` (`keywordDetector.disabled: ["autopilot"]`); plugin still installed. This session's autopilot state cleared.
- Usage panel repo (claudecode-cost-usage-panel) pushed and deployed at close of that work: compaction rows "*** Async Compaction Started / Pending (next prompt) / Finished ***", fake Finished row fix, Sonnet 5.5 price, alert floor fix.
- Live config: panel restart warning still 400000 (same as Compact at); user not yet asked to decide beyond the suggestion of 900k or 0.

## What was done

- Install race fix: `963d9df`, then the reload step moved to `scripts/launchagent-reload.sh` with stubbed-launchctl tests.
- Tests for the 2026-09-30 fixes (DoH outage, dashboard JS under node): `86e28cd`.
- Finder shortcuts install/remove: `be7c78d`.
- Single plan (Claude Enterprise or one subscription): keyless secondary counts as none, Anthropic's 429 passes through unchanged, no window armed, stale windows ignored: `66bdcb1`.
- Upgrade / Check version dialog, Install GitHub version (temp worktree, rollback for unpushed commits), release 0.3.0 `74aa733` tagged v0.3.0 with a GitHub release.
- Pauseless Compaction: savings per day chart, tiles with percentages ("tokens of context compacted", "async context savings, after costs"), mid-turn PostToolUse notice, "Leading Edge" wording, README example month and screenshot `docs/screenshots/savings-per-day.png`.
- `/compact-async` command (`~/.claude/commands/compact-async.md`, marker `claude-burst:compact-async`), README section.
- Session coordination (master model, user's choice: first editor is master and commits; others' Edits go through; whole-file Write over a master's work, staging a file someone else masters, and `git add -A/./-u` / `commit -a` while others have work are refused): `internal/coord`, `claude-burst coord`, dashboard section with Message, Hand on, Recent activity; `scripts/coord-live-test.sh` passed 8/8 with two real Haiku sessions.
- `deploy.sh` takes a lock (`~/.config/claude-burst/deploy.lock`); the GitHub install window closes itself 10s after success.
- GitHub issue #3 (TLS storm) closed with daily counts.

## Open

1. **Screen stays lit with the lid shut** (user report). Draft in `scripts/lid-awake-root.sh` (uncommitted): `screen_decision` returns off when the lid is shut, SleepDisabled is 1 and no external display is online (parsed from `system_profiler SPDisplaysDataType`); the watch loop runs `pmset displaysleepnow` every 5s; `needs_daemon` now always true. Check: `zsh scripts/lid-awake-root.sh screen` with `CLAUDE_BURST_TEST_LID=shut CLAUDE_BURST_TEST_SLEEPDISABLED=1` prints off, and with a test external display prints leave; add a Go test in `internal/keepawake/script_test.go`; then commit, deploy, and the user presses Apply (password). Unverified on the real Mac that `displaysleepnow` darkens the built-in screen behind a shut lid.
2. P0 list in memory `claude_burst_next_priorities.md`, none started: `Status()` returns live maps (concurrent map crash), dashboard Restart skips the drain, settings.json/config.json/state.json written in place, secondary stream errors look like success.
3. Ask the user: restart warning 900k or 0 (it is 400k, equal to Compact at, so it nags during a pending compaction).
4. User actions still pending: `bash install.sh` (needs sudo), keep-awake Apply.
5. Offered, not started: compactions by repo table; decimal comma in price fields; swapping a ready summary on a mid-turn message.

## Things that will bite you

- A compaction summary only swaps in on a plain prompt; messages sent mid-turn arrive as system text, so a long turn holds it back (logged as "compaction waiting ... system[text]").
- `claude -p` ends its session after each answer, so SessionEnd fires every turn; the live coord test leaves that hook out.
- Two deploys at once race; `deploy.sh` now waits on a lock, but an older checkout's deploy.sh does not.
- A Go file named `*_js_test.go` only builds for GOOS=js; dashboard JS tests live in `page_script_test.go`.
- Keyword hooks from oh-my-claudecode may still inject "[MAGIC KEYWORD: ...]" for other skills; do not start workflows the user did not ask for by name.
- No em or en dashes; no Claude co-author trailers in commits (memory rules), despite the attribution reminder.

# Handover, 2026-09-30 15:21: DoH block outage, resolver fallbacks, Back to primary button
<!-- session: dc634997-1f9a-42b8-a980-f04d6fc1016f -->

## State right now

- Written at session close. Verify before acting.
- Pushed to origin/main: `0c08eba` and `57015ce`. Working tree clean at close (git status).
- Deployed: both builds installed via `scripts/deploy.sh`; gateway running under launchd (pid 23851 at 15:20).
- **Burst is NOT in the traffic path.** The user clicked Revert to normal Claude at 14:49 (rollback.sh): /etc/hosts had no api.anthropic.com entry at 15:21, dashboard showed NOT ACTIVE, 5/6. Claude Code talks to Anthropic directly (a direct curl got 405 from 160.79.104.10 at 15:20).
- The user says they clicked Install Transparent Proxy, but at 15:21 /etc/hosts still had no entry and no install process was visible. Whether the install ran, failed, or is waiting for a password in a Terminal window: unverified.
- A `revert.command` zsh process (pid 20526) was still alive at 15:20, probably an open Terminal window from the revert.

## What was done

- Root cause: from about 14:18 this network reset TLS to cloudflare-dns.com and dns.google (curl exit 35), while https://1.1.1.1/dns-query and Anthropic worked. With one DoH endpoint, Opus first failed over to GLM every 70s; after `ab5005d` (lookup error treated as local network, never fail over) plus a restart that emptied the address cache, every request got 502.
- `0c08eba`:
  - Resolver tries the configured endpoint, then https://1.1.1.1/dns-query, https://8.8.8.8/resolve, then plain UDP DNS to 1.1.1.1:53 and 8.8.8.8:53 (hand-parsed, `internal/router/resolver_udp.go`). Remembers the endpoint that answered. Last good answer kept in `~/.config/claude-burst/resolver-cache.json`.
  - LookupError is safe to resend (30s ladder) and counts towards failover after it; removed from isLocalConnectivityFailure.
  - Fixed a panic: a ladder retry that succeeded fell through to `err.Error()` on a nil err.
  - `PrimaryHealth` in router.go, `primary_health` in /api/state; new critical dashboard check "Anthropic answering".
  - Route badge shows `SECONDARY · <model>` for per-model windows; before it read only the account-wide window, so it said PRIMARY while Opus went to GLM.
- `57015ce`: Back to primary button beside the route badge, always visible, greyed when nothing is on the secondary. Tested in Chrome: forced haiku for 5 min, button enabled, click cleared the window.
- Memory added: `claude_burst_doh_block.md`.

## Open

1. Get Burst back in the path. Check: `grep anthropic /etc/hosts` shows the 127.0.0.1 entry and the dashboard's Traffic reaching gateway is green. If the Install window failed, read its Terminal output and `~/.config/claude-burst/claude-burst.log`.
2. After reinstall, confirm the fallback works on this network: no new `cloudflare-dns.com` 502s in the log, and `resolver-cache.json` exists.
3. Consider making the default `resolver_doh` https://1.1.1.1/dns-query (config.go, and the user's config.json which names cloudflare-dns.com), so each process does not try the blocked endpoint first. Not done.

## Things that will bite you

- A named DoH host can be blocked by SNI; IP-literal endpoints were not blocked here. Compare curl by name and by IP before blaming Anthropic.
- `PrimaryHealth` times serialise as year 1 when unset (go.mod is go 1.23, no omitzero); the dashboard JS treats `0001-` as empty.
- deploy.sh clears the rolled-back marker, so the self-heal watchdog reloads the gateway even while the redirect is removed. The log restarts at 15:11, 15:18, 15:20 were that and deploys, not crashes.
- With every resolver dead, each primary request waits about 30s on the ladder before failing over.

# Handover, 2026-09-30 14:48: dashboard settings, hotspot, keep-awake window, compaction and failover fixes
<!-- session: c93b899c-afc6-4902-b8d2-cba7d51999dc -->

## State right now
- Written at session close. Verify before acting.
- claude-burst: working tree clean, main == origin/main at `ab5005d` (checked with git). Last deploy (`scripts/deploy.sh`) at about 14:40 included `ab5005d`.
- `e0d1fa1` (price claude-sonnet-5-5, handover pills, byline panel) is in history but was not made in this session; content unverified.
- Panel repo (claudecode-cost-usage-panel): last pushed commits in this session were `adecf25` and earlier; current state not rechecked at close (unverified).
- Live on this Mac: keep-awake mode `always` with a 120 minute idle window, applied 13:25 (`/etc/claude-burst/lid-awake.idle` = 120, daemon running). Hotspot set to "Andrews IPhone 18 ProMax", lid-closed only, password in Keychain service `claude-burst-hotspot`. Prompt-notice hook `~/.config/claude-burst/prompt-notice.sh` is in `~/.claude/settings.json`.
- Ghostty `NSAppSleepDisabled` was set back to 1 by hand after tests had deleted it.

## What was done
- Compaction: a second summary waits without dropping the first (`8cc4166`); summary goroutine gets its own map copy, race fix (`495a7cf`); dropped summary reopens the window and logs which message changed (`318ea45`); failed summary retries after 5 min (`9b29851`); state keyed per conversation (session|model|hash of first message) so subagents no longer drop the parent's summary (`7e5c684`).
- Prompt notice under the prompt in Claude Code (`551022d`), test line button (`0687614`), branded "Claude Burst, pauseless compaction: done. Context down N%" (`5272ae4`).
- Dashboard: This Mac & sessions, usage panel install/remove with screenshot (`500413c`); grouped menu, failover & pricing editor, notifications, advanced, hotspot (`686df90`); Apply button for lid setting, menu/page order (`5b271ac`); handover table by repository, Save buttons (`862632f`); README screenshots (`c32b378`, `aeda3f0`); README leads with pauseless compaction (`302690a`).
- Hotspot watcher and test: `a0f2d9a` to `2e8baff` (password required, Show via Touch ID, streamed attempts, timing fields: check 5s, 2 failed checks, 60s gap, 30 min give-up); no rejoin while still on a 172.20.10.x hotspot address, lid-open retries at once, Test button id clash fixed plus `TestPageIDsAreUnique` (`5272ae4`).
- Keep-awake idle window `keep_awake_idle_minutes` via `scripts/lid-awake-root.sh apply MODE IDLE ACTIVITY_FILE`, gateway touches `~/.config/claude-burst/last-activity` (`afa6462`); caffeinate blocker note in plain English (`a2e3d5c`, `70625b2`).
- Tests no longer touch the real Mac: Ghostty defaults stubbed in admin and integration uninstall tests, sudo stubbed (`bcb27d9`, `afa6462`).
- Failover: DoH lookup falls back to the last cached address, `LookupError` never counts; primary transport errors safe to resend are retried 2s/4s/8s/16s before counting; failover and its end announced in the prompt notice (`ab5005d`).
- Memory written: `claude_burst_next_priorities.md`, `claude_burst_tests_touch_real_mac.md`.

## Open
1. Confirm `ab5005d` in real use: next network blip should show `retry route=... attempt=` lines in `~/.config/claude-burst/claude-burst.log` and no `failures within 60s` note in metrics.jsonl for a single DoH reset. Also confirm the "Claude Burst: ... now go to the secondary" line appears under a prompt.
2. The user's last screenshot showed Claude Code itself saying "502 anthropic upstream error ... Retrying in 18s, attempt 8/10" at about 14:45, after `ab5005d` was deployed. Not investigated: check the log around then for why requests still returned 502 (network down fast-fail path?).
3. P0 list in memory `claude_burst_next_priorities.md`: `Status()` returns live maps (crash risk), Restart button skips drain, non-atomic writes of settings.json/config.json/state.json, secondary stream errors look like success.
4. SleepDisabled was re-set to 1 twice at 13:23:56 and 13:24:37 by the daemon; cause not found. `/Library/Preferences/com.apple.PowerManagement.plist` was written at 13:24. Check `/var/log/claude-burst-lidawake.log` for more `-> 1` lines with no apply.
5. User asked for a `/compact-async` slash command (not started). User also asked that requests be queued with a message "Queuing requests while restoring network connection" during network issues (not started). User said Routing nav click highlights Secondary (scroll-spy, not fixed) and could not find "force to primary" in the UI (not addressed).
6. cf832d9d session (wordpress-cyber-devtools) compaction failed at 14:04 on the DoH error; should succeed now, check log for `compaction summary ready` on that session.

## Things that will bite you
- `defaults`, `sudo`, `pmset`, `networksetup` ignore a test's temp HOME. Stub them; after tests check `defaults read com.mitchellh.ghostty NSAppSleepDisabled` reads 1.
- Review agents run under the same session id; before `7e5c684` they dropped the main summary. Keying now includes a first-message hash, so the first deploy after it lost a waiting summary.
- `$(...)` in admin.html is getElementById: a duplicate id silently breaks buttons. `TestPageIDsAreUnique` guards it.
- `cycle.sh` holds a `caffeinate -s` that keeps the Mac awake on battery regardless of the idle window; user wants it kept, it is shown in the UI.
- A stale write gets one immediate retry only; the 30s ladder only for other resend-safe errors. `TestStaleWriteRetryIsBoundedToOne` and `TestPrimaryTransportErrorsAreRetriedBeforeFailover` pin this.
- No em or en dashes anywhere; no Claude co-author trailers in commits.

# Handover, 2026-09-29 20:23: ccusage panel label tweaks (panel repo, not claude-burst)
<!-- session: ac015185-ea7e-44d8-acae-42272a30f68f -->

## State right now

- Written at session close. Verify before acting.
- This session touched **no claude-burst code**. All work was in
  `~/Desktop/github/claudecode-cost-usage-panel` (public repo).
- Panel changes are **live and pushed**: edited in both `~/.local/bin/ccusage-panel.sh` and
  its source heredoc in `claude-panel-setup.sh`; the extracted heredoc was diffed against the
  live file and was byte-identical after each change, so `deploy.sh` is a no-op, not a revert.
  Pushed `f150372..3e5e630` to `origin/main` at the user's request (~20:21).
- The uncommitted claude-burst files at session start (`internal/router/compact.go`,
  `compact_run.go`, `compact_test.go`) belong to other work; not touched here. Current git
  state unverified (read-only git commands were not approved at close).

## What was done

- `7436f43`: today line's per-model labels drop the `-YYYYMMDD` suffix
  (`haiku-4-5-20251001` shows as `haiku-4-5`). Only the label; the unpriced-model check
  still matches the full name.
- `3e5e630`: model line shows `SID: *0f68f`; the `*` marks the id as truncated (last 5
  chars of the UUID). The code comment explaining it was updated.
- Both passed `bash -n`. Not checked by eye in the running panel (unverified).

## Open

1. Glance at the panel: today line should read `haiku-4-5`, model line `SID: *xxxxx`.
2. The today models line was clipped at the right edge before the fix; if it still clips,
   options offered: shorter labels or wrap to two lines. User has not answered.

## Things that will bite you

- Panel scripts in `~/.local/bin` are generated by `claude-panel-setup.sh`; edit both and
  diff the heredoc against the live file (memory `ccusage-panel-source-of-truth`).
- Top Sessions also uses a `*` prefix (marks the current session row); same session, so no
  clash, but do not give `*` a third meaning.
- The panel repo is public: no real spend figures or session ids in comments.

# Handover, 2026-09-29 20:20: session handover hooks, peer-log outage, flashing restart banner
<!-- session: 5f048949-f3ee-463c-87ca-cba36b3b7a01 -->

## State right now

- Written at session close. Verify before acting.
- **Session handover hooks are installed and live** (the user clicked Install on the dashboard ~13:15). `~/.claude/settings.json` has SessionStart `~/.config/claude-burst/handover/start.sh` and SessionEnd `.../end.sh`, beside the user's own panel and cost-alert hooks. Settings are the defaults (brief on, write on, commit on, min 2 prompts, writer model `opus` = Opus 5.5). Check: dashboard "Session handover" section says INSTALLED, or `curl -s 127.0.0.1:7788/api/state` field `handover.installed`.
- The hand-installed first version (`~/.claude/hooks/handover-*.sh`, `~/.claude/handover/`) is **deleted**; nothing references it.
- **Peer-log is OFF** since ~13:04 (was armed since 28 Sep 22:48). See Things that will bite you.
- Deployed binary: built from `29d0c39` at ~13:08 from a clean git worktree of HEAD. Whether the later compaction commit `fcb1ef1` (another session, 20:14) was deployed is **unverified**.
- Pushed: everything up to `92992e0`. `main` is 1 ahead with `fcb1ef1` (not this session's). Uncommitted: `internal/router/compact_run.go`, `compact_test.go` (not this session's; leave them to whoever owns the compaction work).
- Panel repo `~/Desktop/github/claudecode-cost-usage-panel`: pushed through `f150372`, clean. Live `~/.local/bin/ccusage-panel.sh` matched the heredoc in `claude-panel-setup.sh` at ~14:00.

## What was done

- `88a779a` Session handover feature: `internal/handover` (scripts embedded from `internal/handover/scripts/{start,end,write}.sh`, installed to `~/.config/claude-burst/handover/`), dashboard section "Session handover" (`/api/handover`, `/api/handover-install`), README section. Opt-in per repo: only acts where the git root has `HANDOFF.md`.
  - Start: on startup or `/clear`, injects a briefing, the top HANDOFF.md section, commits since HANDOFF.md last changed, `git status` and the tracked layout.
  - End: SessionEnd (closing a Ghostty window arrives as reason `other`, proven by closing a pty) queues `write.sh` under perl `POSIX::setsid` so SIGHUP cannot kill it. It runs `claude -p --resume <sid> --fork-session --model <model>`, updates HANDOFF.md, commits only that file, posts a macOS notification. Lock: `.git/handover.lock`. Log: `~/.config/claude-burst/handover/handover.log`; writer reply: `last-run.json`.
- `29d0c39` The legacy hand-installed pair no longer counts as "installed" (it ignores the dashboard settings); it shows "old hooks active" instead.
- Fixed the 13:00 dashboard FAILED state by running `scripts/peer-log.sh off` (cause below).
- Panel repo: `159a37b` tried SGR 5 blink (Ghostty 1.3.1 ignores it); `f150372` flashes the RESTART DUE TO HIGH CONTEXT line by rewriting that one row once a second between refreshes. The user confirmed it flashes.

## Open

1. **First real Ghostty-close handover in this repo**: this section is it. Check `tail ~/.config/claude-burst/handover/handover.log` shows `queue`, `write`, then `wrote ... committed <hash>`; and `git log -1 -- HANDOFF.md`.
2. **Writer model**: the user asked what `opus` means; I recommended `sonnet` to save cost. No change made. Check the dashboard field.
3. **`fcb1ef1` unpushed**: another session's compaction commit. Ask the user before pushing.

## Things that will bite you

- **Peer-log armed during the working day causes an outage.** `lsof` per accepted connection took ~1 s under load (not 40-70 ms), accept is serial, the :17777 listen queue backed up (`netstat -Lan | grep 17777` showed qlen 19), healthz timed out, pf-heal called it BROKEN and its reload made it worse. Gateway logs still showed 200s, which is the tell. Only arm it overnight.
- `claude -p --allowedTools a b "prompt"`: the flag is variadic and eats the prompt (error "No deferred tool marker found"). write.sh uses `--allowedTools=a,b` and the prompt on stdin.
- Installed handover scripts are generated: edit `internal/handover/scripts/`, not `~/.config/claude-burst/handover/`; `GetStatus` rewrites them when they differ.
- `scripts/deploy.sh` builds the working tree. Another session had untracked Go files in `internal/router/`, so I deployed from `git worktree add --detach <dir> HEAD`. Do the same while other sessions are mid-change.
- Models sometimes write em dashes despite the instructions; write.sh strips them from HANDOFF.md after the run.

---

# Handover, 2026-09-29: overflow pruning, cache accounting, graceful restart

Written at session close (28 Sep evening to 29 Sep midday). **Verify before acting**: true at
the time, nothing keeps it true. All times local (SAST). No em or en dashes anywhere, by the
user's standing instruction (see the memory `feedback-no-em-en-dashes`).

## State right now

- Gateway healthy, route PRIMARY, no forced or per-model overflow windows (the Sonnet test
  force was cleared with Back to primary at ~12:03).
- **Peer-log is ARMED and the user chose to leave it armed "for a while"**
  (`CLAUDE_BURST_LOG_TLS_PEERS=1`, since 28 Sep 22:48). Do not disarm without asking. It had
  caught nothing for #3 by 29 Sep midday: zero `unknown certificate` errors since arming; the
  last storm was 22 Sep. It costs 40-70 ms per connection. When a storm fires, read it with
  `scripts/peer-log.sh status`. `EOF` handshake errors around deploys are restarts, not the
  storm.
- `main` is 4 commits ahead of origin, none pushed (the user asks for pushes explicitly each
  time; ask first):
  - `b1afafc` and the handover commit after it: this session. Part 1 of proxy-side compaction,
    not wired in, no behaviour change.
  - `88a779a` (12:51) and `29d0c39` (13:08): **another Claude Code session**
    (`session_012L1QrE1i9wux83dQ5c9YXy`) working in this repo at the same time. It added
    "Session handover": SessionStart/SessionEnd hooks that brief from HANDOFF.md and have a
    forked session rewrite and commit HANDOFF.md on close, with a dashboard section. It
    **deployed** (installed binary 13:09), so the running gateway includes it. Its
    `internal/handover/scripts/write.sh` contains an em or en dash, against the user's
    rule; left for that session or the user rather than edited mid-flight.
  - Because that hook rewrites HANDOFF.md at session end, this section may be edited by it.
- **In progress when the session paused: proxy-side compaction of primary sessions.** See the
  section of that name below; it is the next thing to pick up.
- Live `config.json` edited by hand this session (backed up first): Opus 5.5 pricing
  (`4 / 20`, `cache_read_per_mtok 0.2`) and Fable 5.1 `cache_read_per_mtok 0.25`;
  `ExitTimeOut 60` added to the LaunchAgent plist.

## What shipped (in order)

| commit | what |
|---|---|
| `d851c80` | `install.sh` fix: `fb7126e` had spliced `apply_keep_awake` into the `uninstall() {` line, so `./install.sh uninstall` did not exist |
| `f1cec3e` | Issue #2: non-streaming responses now record usage (closed #2, replied to the reporter) |
| `29aa154` | Cached-token accounting on every route; overflow pruning (stub old tool results, stepped cut-off, cap huge results); Context & cache panel with switches |
| `62fd296` | Token shunting removed from the dashboard (backend kept: `install.sh uninstall` still runs `shunt disable`) |
| `710d45e` | Graceful restart: on SIGTERM the gateway keeps serving and exits when no reply is streaming (max 50 s) |
| `a5f5000` | Opus 5.5 priced; cache reads at Opus 5.5 $0.20 (0.05x) and Fable 5.1 $0.25 (0.025x), not the 0.1x default |
| `2fc834e` | Share-saved figure, re-run counter, and per-model force (`/api/force` with `model`) |
| `d92034c` | Upstream error body logged and put in the metrics note |
| `ca0ff52` | Shape (roles, block types, tool ids; never content) of a request the secondary rejects |
| `7bb91f2` | Orphaned `tool_result` sent to the secondary as user text (fixed Together `invalid_tool_messages`) |
| `18eb1a5` | Every em and en dash removed (290 in 14 files) |
| `1c7a6fb`, `3f629eb` | Saved view on the daily chart; `pruned_usd` recorded per request |

Every one was deployed with `scripts/deploy.sh` and the full suite passed first.

## Pruning: what is known

- Only the **openai-compatible secondary** is pruned. The primary never is: it is a
  flat-rate subscription at ~98% cache hit, and rewriting its history would break the cache
  and burn the limit faster. Bedrock is excluded for the same reason. The user asked why it
  "only optimises overflow traffic"; that is the reason, by design.
- Baseline before shipping: 30 days, 255 overflow requests, ~96k input vs ~700 output tokens
  each, $40.63.
- **One real test run** (forced `claude-sonnet-5` only, headless `claude -p`, 30 turns,
  reading 25 Go files, ~$2.67): 12 requests pruned, 80 results stubbed, 11 capped, ~276k
  tokens not sent (~16% of the session's input, ~$0.39), **1 re-run** of a stubbed call, and
  the final answers were correct, including facts from files read early and since stubbed.
- The panel's failure-rate comparison for the last 7 days is **polluted** by the pre-fix 400s
  (see next section): it reads 21% pruned vs 46% unpruned failures. It corrects itself as those
  days roll out of the window.

## Open

1. **GLM writes tool calls as text.** Two re-runs of the same test (11:57, 12:00) stopped after
   3 turns: GLM answered with `<tool_call>Read ... </arg_value>` in plain text instead of a
   structured tool call, and once invented a `config.yaml`. Claude Code sees end of turn and
   stops. The first run (11:41) made 30 structured calls without it. Unknown whether it is
   GLM being inconsistent or something in the translated request; the orphan fix (`7bb91f2`)
   landed between the good run and the bad ones, so **test with it reverted before blaming
   GLM**. Candidate mitigation if it is GLM: detect `<tool_call>` text in the response and
   convert it to a `tool_use` block. Not started.
2. **Before `7bb91f2`, about half of all tool-using overflow requests were failing** (20 of
   41 in the test) with Together `invalid_tool_messages`. Claude Code sends some requests that
   start with a `tool_result` whose `tool_use` is not included (seen as
   `user[tool_result:call_x] system[text]`). This was silently hurting every real overflow
   session before today; worth checking the month's secondary error rate against this.
3. **Clean pruning numbers need more overflow traffic.** One run is one data point. Watch the
   Context & cache verdict and the Saved view; red means raise `keep_recent` first.
4. Issue #1 (direct-port timeouts): retitled, relabelled `known-limitation`, the `no state`
   and packet-capture results posted. Nothing changed in code.
5. Issue #3 (TLS storm): opened this session from the #1 thread. No storm since 22 Sep.
6. The Tokens view of the daily chart counts uncached input only, so the primary looks tiny
   there (cache reads are ~98% of its context). Not changed; the cache card has the real
   figure.
7. Minor: the "Recent requests" table is mostly heartbeats and event logging (629 of 784
   rows in an hour), which have no cost by nature. Offered a "model calls only" filter; the
   user has not answered.

## In progress: proxy-side compaction of primary sessions (experimental)

**What the user asked for:** auto compact at 400k tokens of context, a warning at 300k, and at
most one compaction per session per time window (default 1 hour). Offered three routes; the
user chose "proxy compaction (experimental)" over "block the prompt once" and "warnings only".
Their follow-up ideas, and why neither works: appending `/compact` to a request (it is a Claude
Code client command; in a request it is just text) and having the proxy send its own request
(yes: that is the design below).

**Constraints, sourced** (claude-code-guide agent over code.claude.com docs, and the claude-api
skill's `shared/model-migration.md` Breaking change 3):
- Claude Code's auto-compact threshold is not configurable (not documented), and no hook or
  proxy can run `/compact`. Hooks can only show `systemMessage` / add `additionalContext`.
- Server-side compaction (`compact-2026-01-12`) returns a block the client must send back;
  Claude Code is not documented to do that, so it was rejected.
- Opus 5.5 binds thinking blocks to the unedited history (enforced for accounts created on or
  after 2026-08-31; older accounts record it and restart the cache). Documented safe client
  shape: keep-tail compaction with thinking **stripped from retained turns**; never compact
  mid tool round.

**Design (agreed in principle, stated to the user):**
1. Per session (`x-claude-code-session-id`), track the context the last primary response
   reported: `input + cache_read + cache_write` (from `writeMetric`).
2. At 300k: warn (log line + dashboard). At 400k, if no compaction for that session in the
   window: in the background, send one summary request through the primary provider with the
   inbound auth headers, the same model, system and tools, messages `[0, p0)` where `p0` is
   the last plain user prompt, plus the Anthropic-recommended summarisation prompt (quoted in
   `shared/model-migration.md` near line 1840; its last sentence, "Do not call any tools...",
   is load-bearing because tools stay in the request). Stream it and keep the text inside
   `<summary>`. Require `p0`'s prefix to be at least ~30% of the bytes, else skip and log.
3. Apply the swap first on a request that **ends in a plain user prompt** (`swapAt` = its
   message count), then on every later request of that session:
   `rewriteWithSummary(msgs, summary, p0, swapAt)`. Before applying, check
   `prefixHash(msgs, p0)` still matches; if not (`/clear`, `/compact`, rewind), drop the state.
4. Config block, **off by default**: enabled, warn_at_tokens 300000, compact_at_tokens
   400000, window_minutes 60. Admin toggle with explanation in the Context & cache panel,
   plus a per-session table (context size, compacted at, state).
5. Metrics: a row for the summary call (note "compaction summary") and a field on rewritten
   requests so the dashboard can show compactions and tokens no longer sent.

**Done (`b1afafc`, tested):** `promptBoundaries`, `prefixHash`, `rewriteWithSummary` (carries
the first message's `<system-reminder>` blocks, where CLAUDE.md lives, verbatim),
`summaryFromText`, with tests in `internal/router/compact_test.go`.

**Not done:** the per-session tracker (a half-written test for it was removed so the tree
builds), the summary call, the hook point (in `handle()` after `reqModel := requestModel(body)`,
before routing, skipping `count_tokens`), config, admin UI, metrics fields, deploy.

**Must test before enabling for real:** with `compact_at_tokens` set low (say 60k) on a
throwaway headless session (`claude -p` in a scratch clone, as the pruning test did), never
the user's live sessions. Check the swap produces no 400s, the model continues sensibly, and
the next turns' `cache_read_tokens` fall. In-memory state is lost on restart (the session then
re-compacts after the window); persisting it is a follow-up.

**Why this pays, unlike pruning the primary:** it rewrites the history once per window instead
of every step. One summary call (400k cached read ~$0.08, a few thousand output tokens) plus
one cache write of the smaller new prefix, then every later turn reads ~350k fewer tokens
(~$0.07 per turn at Opus 5.5 rates), so it pays back within about 7-10 turns.

## Asked and answered: is there any context optimisation for the primary?

At the time of asking: no, deliberately (and the user then asked for compaction, above). This session only improved the primary's **measurement** (cached tokens
recorded, Opus 5.5 priced). Requests to the primary pass through unchanged. Reasoning, in
API-equivalent dollars for Opus 5.5 ($4 input): the primary reads ~98% of its context from
cache at $0.20/MTok (0.05x), and any rewrite of history re-writes the whole context to cache
at 1.25x. With the pruning cut-off moving every ~10 tool results, each step costs about
1.2 x context and saves about 0.5 x removed tokens over the next 10 turns, so it only breaks
even when pruning removes over 70% of the context. The test removed 16%. How subscription
limits count cache reads is not known, so this is the dollar case, not a limits proof.

What does shrink the primary's context is session hygiene, not the proxy: `/clear` between
unrelated tasks, `/compact` in long sessions, subagents for noisy exploration, rewind rather
than correct. Offered, not started: a view ranking sessions by context size (the new
`cache_read_tokens` field makes it possible) to show where `/clear` or `/compact` would help
most.

## Things that will bite you

- **launchd caps an agent's exit timeout at 60 s** whatever the plist says (measured: plist
  150, `launchctl print` 60). The drain deadline is 50 s to fit. Check `launchctl print`,
  never the plist.
- A restart before `710d45e` cut every streaming reply on the machine; one of this session's
  deploys cut the user's other session mid-answer. Deploys drain now, but still ask before
  restarting while the user has sessions running.
- `pruned_usd` only exists from 29 Sep 12:15; the Saved view says "not priced" before that.
- Cache-read prices are model-specific (Opus 5.5 $0.20, Fable 5.1 $0.25). An unpriced cache
  read falls back to 0.1x input for Claude models and to the full input rate for anything
  else. Take prices from the claude-api skill, never from memory.
- `/api/force` without `model` moves **every** session, including the one doing the testing.
  Use the per-model force (`claude-sonnet-5`) for experiments.

---

# Handover, 2026-09-28: Burst breaks on network reconnect

Written at session close. **Verify before acting**, true at the time, nothing keeps it true.
All times local (SAST).

## State right now

- **Transparent mode is installed and live.** Reinstalled 10:37 via
  `~/.config/claude-burst/install-transparent.command` after the user rolled back at 10:18.
  Watchdog confirmed healthy at 10:38:42; `https://api.anthropic.com/healthz` answers from the
  gateway; local CA trusted in the System keychain; `rolled-back` marker absent.
- **pf-heal is the new version:** running, heartbeat fresh, `install-pf-heal.sh status`
  reports no STALE, installed plist has the WatchPaths below.
- Pushed: `d636d70` (the fix) and `1a9f3e4` (test fix). Working tree clean.

## What happened

User on a phone hotspot walked away from the laptop; on return Claude Code showed
`502 local network unavailable (DNS is failing ...)` and the dashboard showed
`FAILED · transparent proxy ... dial tcp 127.0.0.1:443: connect: connection refused`.
Two different things, from `claude-burst.log` and `/var/log/claude-burst-pf.log`:

1. **10:06-10:16 the network really was gone.** Snapshots show no IPv4 uplink (the hotspot's
   `172.20.10.2` disappears) and `www.apple.com` failing in 1-2 ms. The 502 and "not failing
   over" were **correct**, the secondary is behind the same dead network. Nothing to fix.
2. **On every reconnect the redirect broke.** Six times 09:30-10:18 pf-heal logged
   `BROKEN ... (rdr rule: loaded, main ruleset: referenced, gateway on :17777: listening)`,
   and one `reload-anchor` (which also flushes this anchor's states, 10 and 123 cleared)
   healed it every time. The outage length was just pf-heal's **120 s** timer: up to ~2 min
   of machine-wide "connection refused" per reconnect. At 10:18:04 it had healed; the user's
   rollback ran 14 s later without knowing.

## The fix (`d636d70`)

- LaunchDaemon also fires on `WatchPaths`: `/var/run/resolv.conf` (configd rewrites it on
  every network change, its mtime was 10:17, the reconnect) and `/etc/pf.conf`.
  `StartInterval` 120 → 30 as backstop (a healthy cycle is one local curl).
- After a network change, `pf-heal.sh` keeps cycling for 90 s (`run_with_settle`), because pf
  breaks a few seconds *after* the change and one probe at the moment of the change passes.
  Change detection is the marker's mtime vs `/etc/claude-burst/pf-heal.network-seen`.
- `BROKEN` lines now include `pf: enabled|DISABLED|unknown`.
- Self-tests: pf-heal 26/26 (new: break appears after the change and heals in the same run;
  no settle without a change; no probing while rolled back), install-pf-heal 14/14.

`1a9f3e4`: `TestStateReportsRejectedModelsAndWhereTheyGo` had failed since `fee840f` (default
Fable fallback moved to `claude-opus-5-5`, test hardcoded `claude-opus-5`), which meant
`deploy.sh` refused every deploy. Test now reads `config.Default()`.

## Open

1. **Not yet proven on a real disconnect.** Next hotspot drop, expect in
   `/var/log/claude-burst-pf.log`: `network changed -- watching the intercept for 90s`, then
   `BROKEN` / `HEALED` within seconds (or nothing, if it did not break). If a break still sits
   for ~30 s+, the WatchPaths trigger did not fire, check the plist actually loaded
   (`sudo launchctl print system/ninja.andrewbaker.claude-burst-pfheal`).
2. **Mechanism still unknown.** Why is 443 refused with the rule loaded and referenced?
   Candidates: pf disabled by whatever reloads it (Zscaler/CrowdStrike), or stale states.
   The new `pf:` field in the next `BROKEN` line answers the first. Do not guess, see
   `claude_burst_pf_anchor_loss` and the direct-port investigation's four wrong guesses.
   The fix heals regardless of which it is.
3. **During a real outage Claude Code still shows the 502.** Correct behaviour; no hold/retry
   in the gateway would survive a 10-minute outage. Only revisit if short blips (<30 s) turn
   out to be common.

## Unrelated machine change this session

Removed 5 stale `Claude Traffic Light` hooks from `~/.claude/settings.json` (app was
uninstalled; every prompt/stop threw `Cannot find module .../set-status.js`). Backup:
`~/.claude/settings.json.bak-20260928-114932-traffic-light`. The other two hooks
(panel session hook, cost alert) untouched.

---

# Update 2026-09-21 (evening): the 18:04 failure, and what is still open

The 502 "resolve api.anthropic.com over DoH ... no such host" was **the machine's network**,
not a Burst bug: the gateway's own network snapshot shows no uplink and `www.apple.com` failing
in 1 ms at 16:36, 17:59 and 18:04-18:23, with the interface flapping between a hotspot
(`172.20.10.2`), a LAN (`192.168.0.90`), link-local (`169.254.x`) and `192.168.89.x`. Three
Burst behaviours made it worse and are fixed in the commit after this note (retry a dead pooled
connection once, do not fail over into a dead network, short self-releasing outage windows).

**Correction, same evening:** pf-heal is not missing instrumentation -- it already has
heartbeat-based liveness (`/etc/claude-burst/pf-heal.heartbeat`), the same pattern as the
gateway watchdog, surfaced live on the dashboard's Guards card. The gap is that nobody checked
it or `/var/log/claude-burst-pf.log` before `scripts/rollback.sh` ran at 18:25:07 and erased
whatever they would have shown. Codified in ROLLBACK.md's ordering rules (#5): check pf-heal's
own state before rolling back for a broken redirect, not after.

**Update, 2026-09-22 morning:** pf-heal bailed out again -- "GIVING UP after 4 failed
repairs" -- but NOT the same bug. Its own log showed `transparent-root.sh reload-anchor`
verifying the real path OK all 4 times, moments before pf-heal's own single-shot recheck of
the exact same URL said FAIL, all 4 times. Root cause: `intercept_path_healthy()` was one
curl call with no retry, against a path the tool's own comments already document as
unreliable single-shot (`probe_direct_retry`'s 2026-09-03 measurement: 1 success in 10 for
the analogous direct-port check). Fixed: both `intercept_path_healthy` (pf-heal.sh) and the
equivalent, more dangerous check inside `do_reload_anchor` (transparent-root.sh, where a
false FAIL reverts a just-installed good anchor) now retry with a budget, same pattern as
`probe_direct_retry` already used for the direct port. `pf-heal.sh --self-test` verified
(20/20, unchanged); `transparent-root.sh --self-test` verified (text-edit paths only --
`do_reload_anchor` needs root and has no automated coverage). Both retry functions were also
exercised standalone against a stubbed curl that fails twice then succeeds, and one that
never succeeds, confirming recovery and budget behaviour. This was NOT investigated as a
repeat of `claude_burst_direct_port_investigation.md` (the open, four-wrong-guesses direct-
port mystery) -- it's a different, narrower bug, evidenced directly from the log rather than
inferred.

**A second, real gap that same rollback exposed:** it also silently re-enabled token shunting
(disabled earlier that day) and reinstalled its guard hook, because `claude-burst shunt disable`
writes `config.json`/`settings.json` directly and never updates the backup rollback restores
from. Fixed: `internal/backup` now updates `*.latest.bak` from inside `config.Save` and
`claudesettings.Write` themselves, from what was just written, right after every successful
write -- so it is never older than the config actually in force. Codified in ROLLBACK.md's
ordering rules (#6). The first implementation (snapshot BEFORE the write, matching
`scripts/backup-config.sh`'s own convention) does NOT fix this -- a before-write snapshot of
the old value is still the old value, and a later unrelated rollback still restores it; a test
written against that version failed, which is what caught the mistake.

---

# Update 2026-09-21 (later): token shunting is now OFF

Read this before the handover below. Shunting was switched off with
`claude-burst shunt disable` because it delegated nothing in real use (12 blocks in real projects, 0 shunts;
Claude reads in windows, which the guard allows). **The two "not done" asks below (toggle
switch, README audit) are moot** unless someone decides to keep the feature. Reasoning and
evidence: `DECISION-token-shunting-off.md`. Open Claude Code sessions need a restart to drop
the hook.

---

# Handover, 2026-09-21: token shunting

Written when the session closed. **Verify before acting**, everything here was true at the
time and nothing keeps it true. Two older handoffs follow it: 2026-09-20 (failover fixes) and
2026-09-08 (issue #1, TLS storm). Neither is superseded by this one.

## Start here: what is NOT done

Two asks were in flight when the session ended.

### 1. Make the shunt master control a toggle switch, not a checkbox, NOT STARTED

`internal/admin/admin.html`, the "Token shunting" section. Today it is a plain checkbox:

```html
<label style="display:flex;gap:10px;align-items:center;cursor:pointer;font-weight:700;font-size:15px">
  <input type="checkbox" id="shuntMaster" style="width:auto;margin:0;transform:scale(1.25)">
  Token shunting <span id="shuntMasterState" class="pill"></span>
</label>
```

- The JS only uses `.checked` and `onchange` (`$("shuntMaster").onchange` posts
  `{read:on, write:on}`; `renderShunt` sets `.checked` unless `shuntBusy`). **Restyle, do not
  rewrite**: a `.switch` class with `appearance:none`, a 44x24 pill track, a `::after` thumb, the
  track using `var(--ok)` when `:checked`, a `:focus-visible` ring, and `role="switch"` with
  `aria-checked` kept in step in `renderShunt` and the handler. Remove the inline `scale(1.25)`.
- Gotcha: a global rule gives every `input` `min-width:210px` and padding. `input[type="checkbox"]
  {min-width:0}` already exists for checkboxes; the switch needs `min-width:0; padding:0; border`
  set explicitly. Use the theme variables so dark mode works.
- The two part switches (Bulk read, Code write) are still checkboxes. The ask said "the shunt", so
  they were left alone; say so, and offer to convert them for consistency.
- Verify visually. The browser extension was disconnected at the end, so this has never been seen
  in Chrome. Panel logic can be checked without a browser: extract the functions from the
  `<script>` in `admin.html` and run them in Node with stub `$`/`esc`/`num` (worked well for the
  activity table). Screenshot click coordinates are in the screenshot's frame (1453 wide), not CSS
  pixels, and the first click after a page load is sometimes ignored.

### 2. README audit against what the product does now, PARTLY DONE

Asked: "did you rewrite the readme to reflect what the product currently does?" The honest answer
was: reframed around Together AI + shunting, but never audited end to end. The audit found:

- **Accurate (checked by script):** every `claude-burst <command>` and `--flag` in the README's
  code blocks exists in the CLI (the only two hits, `--self-test` and `claude-burst hosts`, are a
  script flag and an /etc/hosts marker); Go 1.23 matches `go.mod`; every internal anchor resolves.
- **Stale, still to write:**
  - *Forcing the secondary, and the admin UI* describes an older dashboard. Add: the readiness
    ring (checks: traffic reaching gateway, gateway watchdog, pf redirect guard, secondary ready,
    error rate), the rail (Observe / Control / Setup), the daily activity chart (7d/14d/30d),
    the **Token shunting** panel (master switch, threshold, cards, recent activity with project and
    session, LOOP rows), the *Try another Claude model before the secondary* toggle, the header
    **Install Transparent Proxy** button (only while Claude Burst is not in use), **Reinstall**
    (only while it is), key reveal gated by Touch ID, and that *Recent requests* now includes
    "Token Shunt" rows (merged for display only, never into `metrics.jsonl`).
  - *What is logged* names two files; `shunt.jsonl` is a third (documented only under Token
    shunting). Add a pointer.
  - *Commands*: `claude-burst stats` now also prints a `shunt:` line.
  - *Uninstall*: now removes the shunt hook and skill and switches shunting off in `config.json`
    (a reinstall needs `claude-burst shunt enable`); the purge hint lists the Together, OpenRouter
    and Bedrock Keychain services. **The code was fixed and tested; this paragraph was not
    updated.**
- **Deliberate, worth a sentence in the docs:** `claude-burst disable` / `enable` do **not** touch
  the shunt hook. `deploy.sh` calls them around the swap in base-url mode, so tying shunting to them
  would silently switch the guard off on every deploy. Only `shunt enable|disable` and
  `./install.sh uninstall` change it.

## Where things stand (verify, do not trust)

- **git:** HEAD `b73e332` (uninstall fix) plus this handover commit; `origin/main` is at
  `1b1793b`, so **the last two commits are unpushed**. Push only when asked.
- **Deployed:** gateway `https://127.0.0.1:17777` (transparent mode), admin `127.0.0.1:7788`.
  Built from `37b365b`; no non-test Go has changed since, so the running binary matches HEAD's Go
  code. `install.sh` is a script, so its fix needs no deploy.
- **Shunting is ON here:** read + write, 350 lines, worker `zai-org/GLM-5.3` on Together, hook and
  skill installed. Last look (30 days, includes the test runs): 5 delegated reads, 13 refused
  direct reads, about 32.7k tokens kept out of context (an estimate), $0.08 worker cost.
- **Off switch, immediate** (the guard reads `config.json` on every call): the panel's master
  switch, or `claude-burst shunt disable`. Restart Claude Code only to unload the skill.
  **A restart is also what loads the skill** in sessions that started before shunting was enabled.

## What exists (all on origin unless noted)

`claude-burst shunt enable|disable|status|doctor|log` plus the hook-run `guard` and the
Claude-run `read` / `write`. A `PreToolUse` hook refuses whole-file `Read` and plain
`cat|head|tail|less|more` at 350+ lines and points at `shunt read`; windowed reads always pass;
credential-looking files are never sent to the worker; it fails **open** and says so
(`GUARD-ERR`). Worker = the configured openai-compatible secondary (Bedrock cannot be one).
Code: `internal/shunt/`, `cmd/claude-burst/shunt.go`, `internal/admin/shunt.go`. Every event in
`~/.config/claude-burst/shunt.jsonl` carries session id, project, file, tool, and a failure
`stage`; a session refused the same file 3+ times with no answered read is tagged **LOOP**.
`claude-burst shunt log --problems` is the first thing to run if it seems not to be working.

## Tests, and how to run them

```bash
go vet ./... && go test ./... -race -count=1            # the CI gate; deploy.sh runs the same
go test ./internal/integration -run TestShunt -v         # 7 end-to-end tests, real binary, fake worker
go test ./internal/integration -run TestInstallScript    # real install.sh uninstall, launchctl stubbed
CLAUDE_BURST_LIVE_SHUNT=1 go test ./internal/integration -run TestLiveShunt -v -timeout 10m   # real Claude Code + real worker, ~ a few cents
TOGETHER_API_KEY=$(security find-generic-password -s claude-burst-together -w) \
  go test ./internal/router -run TestLiveSecondary -v    # real Together translation
```

All of these passed on 2026-09-21. The key behaviours were mutation-checked (making the guard never
block, skipping the readiness check on enable, zeroing the repeat counter, dropping the failure
log, silencing guard errors each fail the suite). The live tests' events land in the real log under
projects named `shunt-live-check-*`.

## Things that will bite you

- **`deploy.sh` builds the working tree** and runs `go test ./... -race`. A data race in a test
  refused a deploy once (plain `go test` had passed). Keep the tree clean or it ships code no
  commit holds.
- **Deploying changes every open Claude Code session's hook on its next call**: the guard is a
  subprocess of the installed binary. It also restarts the gateway (a blip).
- **A blocked model may not follow the redirect.** One live run answered with `grep` and never
  touched the worker, which is legitimate. The natural test asserts only what holds either way; the
  other test tells the model to follow the refusal. The loop that started the logging work (a
  `wporg-ready` session refused six times, no worker call, session then unknowable) was never
  identified; the log would name it now.
- `shunt.jsonl` is append-ordered and `Recent()` returns file order newest-first; tests that append
  out of time order get confusing results.
- Throwaway instances used ports 27777/27788 with a scratch `HOME`. The real gateway is 17777.
- `BLOG.md` is a dated post that says it is kept as written. Leave it.

# Session handoff, 2026-09-20

A point-in-time snapshot, written at 14:20 local. **Verify before acting.**

## Verify state first

```bash
curl -s http://127.0.0.1:7788/api/state | python3 -m json.tool | head -40   # the running gateway's own answer
~/.local/bin/claude-burst status                                              # adds "refused:" lines per model
grep -a "replaying on the subscription\|dropped server-only tools" ~/.config/claude-burst/claude-burst.log | tail
```

## What was fixed (both deployed, both on origin/main)

1. **`7d661cb` - failover to GLM died with `400 Invalid JSON data: missing field
   \`parameters\``.** Claude Code declares Anthropic's *server-side* tools
   (`web_search_20250305` …) with a name and `type` but no `input_schema`. We turned them
   into OpenAI functions with no `parameters`, and Together rejects the **whole request**
   for one such tool. Now dropped (logged as `dropped server-only tools: …`), schemas with
   `type` left implicit are normalised, and `tools`/`tool_choice` are omitted when nothing
   survives. Confirmed against the live endpoint before and after.
2. **`6ca48b5` - one Fable rejection sent every model to the paid secondary for two days.**
   The overflow window was account-wide. Now `State.ModelOverflow` scopes a rejection to
   the model that was refused; Anthropic's claim headers name a *bucket*, never the models
   it covers, so we do not guess. On top: `fallback_chain` (default fable → opus) replays a
   refused request as another Claude model **on the subscription** before spending money.
   Dashboard toggle in *Actions* ("Try another Claude model before the secondary") turns it
   off live and shows which models are refused and where their traffic goes;
   `claude-burst status` prints `refused:` lines. Design and rationale: README, "Limits are
   per model, and are never inferred".

Behaviour worth remembering:
- A **forced** window (`force-secondary`, dashboard button) stays account-wide and
  **bypasses the chain** on purpose, its only job is to exercise the secondary.
- A pre-scoping state file's account-wide window is **dropped on load** (logged). That is
  what happened to the `seven_day_overage_included` window that was armed until 09-22.
- The toggle (`DowngradeDisabled`) survives `ClearOverflow` and `ForceOverflow`; both used
  to replace the whole `State` struct and silently reset it.

## NOT yet verified against real traffic

- **The chain has never fired for real.** The window was dropped at deploy, so nothing has
  been refused since. First real Fable refusal should log
  `replaying on the subscription as "claude-opus-5"` and show a `refused:` line in
  `claude-burst status`. **If a Fable refusal goes to GLM instead, that is a bug**, pull the
  log lines around it.
- **The dropped-tools line has not appeared in real traffic** (count 0 at 14:20) and no
  failover to Together has happened since the fix. The fix is proven by the unit tests and a
  live request, not by an organic failover.
- I did not run `transparent-root.sh status` (needs sudo). End-to-end evidence that the
  redirect works: `/v1/messages` 200s in the gateway log at 14:13.

## What went wrong along the way

- **`scripts/rollback.sh` was run by hand at 12:38**, which removed the machine-wide
  redirect. Claude Code then talked straight to Anthropic, so the Fable limit hit with the
  gateway bypassed and nothing to fail over. Fixed by re-running
  `sudo scripts/transparent-root.sh install --host api.anthropic.com --gateway-port 17777`.
  The self-heal watchdog had stood down (`rolled-back` marker) and only spoke up after the
  deploy cleared it, a rollback silences the thing that would have noticed it.
- **Why tests missed the `parameters` bug:** every translation test asserted on JSON this
  package produced, i.e. our *belief* about what the endpoint accepts, never the endpoint.
  No fixture had a schema-less tool either. `internal/router/provider_openai_live_test.go`
  now asks Together for real; it needs a key so it is opt-in:
  `TOGETHER_API_KEY=$(security find-generic-password -s claude-burst-together -w) go test ./internal/router/ -run TestLiveSecondary -v`
  Run it after any change to `translateAnthropicRequest`.

## Token shunting

Moved. It has its own handover at the very top of this file and that one is current; this
section used to describe it and had gone stale within a day (it still said the session-aware
logging was undeployed).

## Small things

- The 12:44 deploy printed `ninja.andrewbaker.claude-burst is not loaded in launchd` and
  recovered; the 13:43 deploy did not, and `launchctl print` shows it loaded now.
- `scripts/check-interception.sh` is zsh-only: `bash` prints `print: command not found` and
  a bad-substitution error. Run it by path or with `zsh`. (Its verdict needs the inspecting
  network; off it, it says INCONCLUSIVE.)
- Still open from before, untouched today: issue #1 (direct connections to the rdr target
  port; best lead is the reply direction) and the TLS handshake storm. Both are in the
  2026-09-08 handoff below and in `INVESTIGATION-TLS-STORM.md`.

---

# Previous handoff, 2026-09-08 (still accurate for issue #1 and the TLS storm)

A point-in-time snapshot, written at the end of a long session. **Everything below
was true at 11:00 on 2026-09-08 and nothing keeps it true.** Verify before acting;
this repo's own history is mostly the story of documents that stopped matching the
machine (see the top of ROLLBACK.md, which had described the wrong intercept mode
for eight days).

## Verify state first, one command

```bash
curl -s http://127.0.0.1:7788/api/state | python3 -m json.tool | head -30
```

That is the running gateway's own answer. `claude-burst status` reads config on disk,
which is a different question.

## What the machine was doing

- **Transparent intercept mode, installed and live.** Gateway `0.2.0` on
  `127.0.0.1:17777` serving HTTPS, `/etc/hosts` redirect present, pf rdr
  `443 -> 17777` loaded, local CA trusted in both the Claude Code bundle and the
  System keychain, `ANTHROPIC_BASE_URL` unset so Remote Control still works.
- **Port 17777, not the default 7777.** Moved chasing issue #1; the move did not fix
  it (see below) and the shipped default went back to 7777 while this machine stayed
  put. `config.json` names it explicitly, so nothing infers it.
- Primary `oauth-passthrough`; secondary `openai-compatible` -> Together AI
  (`zai-org/GLM-5.3`), key in Keychain `claude-burst-together`.
- Both guards armed and heartbeating: pf self-heal daemon (root) and the gateway
  watchdog (user agent).

### The one thing that will mislead you

**Do not health-check the gateway by connecting to its port directly.** While the
redirect is installed, direct connections to the gateway port fail roughly nineteen
times in twenty. The gateway is fine. Probe the real path:

```bash
curl -sk https://api.anthropic.com/healthz     # a body containing "overflow" means it came from the gateway
```

The occasional direct success is what makes this expensive, one lucky probe is
enough to convince you the port is fine and send you looking elsewhere.

## Open threads

### 1. Issue #1, direct connections to the rdr's target port (OPEN, best lead yet)

Root cause is **not** identified, but the layer is. `scripts/diagnose-direct-port.sh`
(read-only, safe mid-incident) measured across 20 failing probes:

```
state-insert   11810 -> 12100   +290    <- pf's state-insertion-FAILURE counter
state-mismatch   338 ->   338     +0
every filter and block drop reason: 0, unchanged
```

pf is not filtering these packets; it cannot insert state for them. An anomalous
state with the gateway port on both sides shows up in the same capture:

```
ALL tcp 127.0.0.1:17777 <- 127.0.0.1:17777   TIME_WAIT:TIME_WAIT
```

**Both the experiment and the packet capture have now been run.**

The `no state` rule (hypothesis 4) is ruled out: direct 1/15 before, 0/15 after, with the
rule `quick` and actually evaluated. Update (c).

`scripts/capture-direct-port-packets.sh` then answered what four hypotheses never asked.
Across 10 failing probes the SYN reaches lo0 every time, 11 retransmissions each, and
**nothing comes back at all**: no RST, no SYN-ACK, zero packets. A third burst with no
port filter confirmed nothing came back under any pair of ports either. The control down
`:443`, same run, was 10/10 clean. **pf swallows it; the investigation stays on pf.**

The 1-in-10 success is the lead. Its first SYN-ACK left the gateway addressed to
`127.0.0.1:443` instead of the client's port, pf reverse-translating the reply of a
connection that was never forward-translated, got a RST, and only survived because the
*retransmitted* SYN-ACK escaped that. So the failure is in the **reply** direction, not
the SYN. That is the first account consistent with all of it: zero filter counters, the
`state-insert` rise (11/probe here vs ~14 measured, same shape), and the anomalous
`17777 <- 17777` state. Full detail in INVESTIGATION-TLS-STORM.md, update (d).

**Four hypotheses, four wrong, so this is a hypothesis, not a fix.** It is the
best-supported one yet and that is exactly what the last four felt like. The next step is
a test of the reply-direction account, and it should revert unconditionally like
experiment-nostate-rule.sh did.

### 2. TLS handshake storm, recurred, cause unknown (OPEN)

Root-caused and fixed 2026-09-03 (Claude Desktop's updater vs. an untrusted CA), held
for three clean days, then returned 2026-09-07 23:20 through 2026-09-08 07:05 at about
a tenth of the old rate, and apparently from a **different** client: Claude Desktop's
log shows no TLS-worded error after 2026-09-03. Structural suspicion recorded in
INVESTIGATION-TLS-STORM.md: `rollback.sh` step 1b removes the System-keychain trust and
only `install-proxy.sh` step 5 restores it, so every rollback reopens the hole.
`CLAUDE_BURST_LOG_TLS_PEERS=1` identifies the client and needs no root.

## Four wrong answers, do not re-propose them

Issue #1 attracted four confident wrong diagnoses. All are recorded as ruled out, with
evidence, in INVESTIGATION-TLS-STORM.md:

1. **A security product blocklists port 7777.** Wrong. 7777 answers 10/10 with a plain
   listener once it is no longer the rdr target. Moving the port relocated the symptom.
2. **A missing filter `pass` rule.** Wrong twice over: nothing is blocking (no block
   rules in the main ruleset or the com.apple anchor), and our anchor is referenced by
   `rdr-anchor` only, so a filter rule in it is loaded and never evaluated.
3. **State mismatch.** Predicted explicitly; `state-mismatch` did not move at all.
4. **A `no state` filter rule**, to skip the insertion that fails. Ruled out by
   experiment 2026-09-08, update (c): 1/15 before, 0/15 after, with the rule `quick`
   and actually evaluated. A filter rule cannot decline the insertion that is failing,
   so the failure is not on the filter path.

The pattern behind all four: reasoning from a mechanism instead of measuring, and in
case 1 varying a thing (the port number) that was perfectly confounded with the thing
that mattered (being the rdr target). The read-only diagnostic settled in one run what
two rounds of theorising did not, and #4 cost nothing only because it was written as a
reverting experiment rather than a commit.

## What changed this session (all pushed to origin/main)

Docs audited against the machine, not against each other: ROLLBACK.md's TL;DR
(described the wrong mode entirely), README (8 corrections, the largest being
`metered_failover` documented as one counter when it is two), BLOG.md (dated and given
a postscript rather than rewritten), INVESTIGATION-TLS-STORM.md (said FIXED while
rejections were still happening).

Three real bugs found by that auditing, all fixed, tested and deployed:

- **Control-plane traffic was failing over to the paid secondary**, and a single dropped
  Remote Control heartbeat could arm an overflow window that routed inference to a paid
  provider. `e49bcaf`.
- **`reset` and `force-secondary` never reached the running gateway**, they mutated
  their own in-process copy and reported success. In a real overflow window that means
  being told you are off the paid secondary while billing continues. `61eee30`.
- **`install-proxy.sh` printed one port and passed another** to the root helper. `9aa478f`.

Plus: every prober now reads `cfg.Listen` instead of assuming 7777, and pf-heal's
self-test no longer reads the real `/etc/claude-burst/transparent.state` (it had been
passing for the wrong reason).

## Conventions worth knowing

- Run scripts **by path** (`scripts/rollback.sh`), never `bash scripts/...` - several are
  zsh-only and fail at parse time under bash, doing nothing.
- Never push without being asked. Commit locally and wait.
- Pasting into an interactive zsh: `#` is **not** a comment there, and parentheses in a
  pasted line get glob-expanded (`(stops...)` produced `unknown sort specifier` this
  session). Send bare commands.
- `deploy.sh` handles build, test, atomic swap, health check and rollback. Use it rather
  than copying binaries.
