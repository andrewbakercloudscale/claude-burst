# Session handoff — 2026-09-20

A point-in-time snapshot, written at 14:20 local. **Verify before acting.**

## Verify state first

```bash
curl -s http://127.0.0.1:7788/api/state | python3 -m json.tool | head -40   # the running gateway's own answer
~/.local/bin/claude-burst status                                              # adds "refused:" lines per model
grep -a "replaying on the subscription\|dropped server-only tools" ~/.config/claude-burst/claude-burst.log | tail
```

## What was fixed (both deployed, both on origin/main)

1. **`7d661cb` — failover to GLM died with `400 Invalid JSON data: missing field
   \`parameters\``.** Claude Code declares Anthropic's *server-side* tools
   (`web_search_20250305` …) with a name and `type` but no `input_schema`. We turned them
   into OpenAI functions with no `parameters`, and Together rejects the **whole request**
   for one such tool. Now dropped (logged as `dropped server-only tools: …`), schemas with
   `type` left implicit are normalised, and `tools`/`tool_choice` are omitted when nothing
   survives. Confirmed against the live endpoint before and after.
2. **`6ca48b5` — one Fable rejection sent every model to the paid secondary for two days.**
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
  **bypasses the chain** on purpose — its only job is to exercise the secondary.
- A pre-scoping state file's account-wide window is **dropped on load** (logged). That is
  what happened to the `seven_day_overage_included` window that was armed until 09-22.
- The toggle (`DowngradeDisabled`) survives `ClearOverflow` and `ForceOverflow`; both used
  to replace the whole `State` struct and silently reset it.

## NOT yet verified against real traffic

- **The chain has never fired for real.** The window was dropped at deploy, so nothing has
  been refused since. First real Fable refusal should log
  `replaying on the subscription as "claude-opus-5"` and show a `refused:` line in
  `claude-burst status`. **If a Fable refusal goes to GLM instead, that is a bug** — pull the
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
  deploy cleared it — a rollback silences the thing that would have noticed it.
- **Why tests missed the `parameters` bug:** every translation test asserted on JSON this
  package produced, i.e. our *belief* about what the endpoint accepts, never the endpoint.
  No fixture had a schema-less tool either. `internal/router/provider_openai_live_test.go`
  now asks Together for real; it needs a key so it is opt-in:
  `TOGETHER_API_KEY=$(security find-generic-password -s claude-burst-together -w) go test ./internal/router/ -run TestLiveSecondary -v`
  Run it after any change to `translateAnthropicRequest`.

## Token shunting — state at handover (the "other session" above)

**Shipped, deployed (14:19) and on origin:** `49caaf3`, `0af15ec`, `11d9b6c`. **Turned on** for
this machine: read + write, threshold 350 lines, worker = the Together secondary
(`zai-org/GLM-5.3`), guard hook installed in `~/.claude/settings.json`, skill installed.
**Not deployed:** everything committed after `11d9b6c` (the two commits below).

What it is: `claude-burst shunt` hands whole-file reads and boilerplate generation to the
openai-compatible secondary so Opus/Fable never carry them. A `PreToolUse` hook refuses
whole-file `Read` / plain `cat|head|tail|less|more` at 350+ lines and points at
`claude-burst shunt read`; `shunt write` generates a file straight to disk. Read and write
are separate switches. Dashboard: **Token shunting** panel (master switch, threshold, cards,
recent activity) plus "Token Shunt" rows in *Recent requests*. README has the design and
the honest numbers ("Token shunting" section). Code: `internal/shunt/`,
`cmd/claude-burst/shunt.go`, `internal/admin/shunt.go`.

### What the live log showed (the reason for the logging work)

Six direct reads were **refused between 14:21 and 14:24** (`Bash`, 26,719 B x4 and 44,428 B
x2), and **no worker call followed**. 26,719 B is `wporg-ready/shared/php-parse.php`.
So a session hit the block and **retried the same `cat` instead of running `shunt read`**.

- The mechanism is fine: from that directory the guard blocks the file at 931 lines and
  `claude-burst shunt read -q ... shared/php-parse.php` returned an accurate, cited answer
  in 8.5 s. That call is in the log too (a real read logged at ~14:29, mine, not a session's).
- **Cause not confirmed and the session is unknown**: the deployed binary logs no session,
  project or file for a refusal. That is the gap.
- `CLAUDE_CODE_SESSION_ID` is in the Bash tool's environment and the hook payload carries
  `session_id` (same id the gateway puts in `metrics.jsonl`), so both halves can record it.

### Tests and docs (added after the first handover commit)

- **`internal/integration/shunt_e2e_test.go`** builds the real binary and drives it through the
  hook's stdin/exit-code protocol against a fake worker in a throwaway `HOME`: enable, guard
  block/allow matrix, delegated read (a `.env` never reaches the worker), generated file + `.bak`,
  log, disable restoring `settings.json` exactly, no-worker enable refused, failing worker
  logged, guard fails open on broken config. **Mutation-checked**: making the guard never block,
  and skipping the readiness check on enable, each fail it. Run: `go test ./internal/integration -run TestShunt -v`.
- **README rewritten around Together AI + token shunting** (Bedrock is now "overflow only" and
  lives in its own *Amazon Bedrock notes* section; limitations renumbered; all internal anchors
  checked). `--help` and `config.example.json` follow. `install.sh` is still Bedrock-only (it
  runs `configure --region`); the README says so and shows the `configure` step that replaces it.
  `BLOG.md` was left alone on purpose — it is a dated post that says it is kept as written.

### Committed but NOT deployed

1. **Refusal message rewritten** (`guard.go`): says "Do NOT retry", offers
   `sed -n 'START,ENDp'` as well as `Read` offset/limit; a test asserts the sed window it
   recommends is itself allowed. Tested, complete.
2. **Logging foundation** (`log.go`, `events.go`): `Event` gained `session_id, cwd, tool,
   path, paths, stage, lines, threshold, repeat`; `events.go` has `StageError`/`StageOf`,
   `Event.Describe()`, `Tag()`, `IsProblem()`, `RepeatCount()` (refusals of one file by one
   session in 10 min with no answer in between). The guard now writes `cwd` on refusals and
   the panel shows the project. **The rest of this is not wired yet — see below.**

### TODO: "make logging tell us clearly what's going on, include sessions"

None of this is done; the pieces above exist so it is mostly plumbing.

1. **Guard** (`cmd/claude-burst/shunt.go` `shuntGuard`): add `SessionID` to `HookInput`
   (`json:"session_id"`) and `Tool` ("Read" / "Bash cat") to `Decision`; log the full refusal
   (session, cwd, path, tool, lines, threshold, `Repeat: RepeatCount(...)`); when repeat >= 1
   prefix the message with "refusal #N of this file, run the shunt read command now".
   **Log `guard_error` events** instead of the silent `return`s (bad stdin JSON, config load).
2. **`shunt read` / `write`**: every exit path must log. Today they `fatal()` **before**
   logging when the feature is off, no worker (wrong provider / no key), or args are bad, so
   those failures leave no trace. Wrap errors with the stage constants (`StageWorkerCall` in
   `Complete`, `StageValidate` in `CodeWrite`, ...), take the session from
   `CLAUDE_CODE_SESSION_ID`, use `flag.ContinueOnError` so a bad flag is logged, and check
   arguments before `NewWorker`.
3. **`claude-burst shunt log`** `[-n N] [--problems] [--session ID] [--json]`: plain-text lines
   from `Tag()` + `Describe()` + project + 8-char session. `shunt status` should list recent
   problems (failures, guard errors, `Repeat >= 2` "LOOP" refusals).
4. **Dashboard**: `shuntActivityRow` needs session/project/tool/path/stage/repeat and a
   server-built `detail` (from `Describe`, so wording lives in one place); add Session and
   Project columns; add `repeat_refusals_1h` to state and a warning line in the panel; put the
   short session in the note of the shunt rows in *Recent requests*.
5. README: say `shunt.jsonl` records file **paths**, project directory and session id but still
   never contents, questions or answers.
6. Tests for each, then a real end-to-end with the built binary: disabled, no worker, bad
   args, guard fed garbage on stdin — each must show up in `shunt log`.

### Before you deploy

- `deploy.sh` builds the **working tree**. It is clean as of this handover; keep it that way
  or the next deploy ships code no commit holds.
- A deploy restarts the gateway (blip for live sessions). The **guard runs as a subprocess of
  the installed binary**, so a deploy also changes what every open Claude Code session's hook
  does on its next call, with no restart of those sessions.
- Off switch, immediate (the guard reads config on every call): dashboard master toggle or
  `claude-burst shunt disable`. Restart Claude Code only to unload the skill.
- Scratch test instances used ports 27777/27788 with a throwaway `HOME`; the real gateway is
  `https://127.0.0.1:17777` with the admin panel on `127.0.0.1:7788`.

Verify:

```bash
claude-burst shunt status
tail -5 ~/.config/claude-burst/shunt.jsonl
curl -s http://127.0.0.1:7788/api/shunt-activity | python3 -m json.tool | head -30
```

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

# Previous handoff — 2026-09-08 (still accurate for issue #1 and the TLS storm)

A point-in-time snapshot, written at the end of a long session. **Everything below
was true at 11:00 on 2026-09-08 and nothing keeps it true.** Verify before acting;
this repo's own history is mostly the story of documents that stopped matching the
machine (see the top of ROLLBACK.md, which had described the wrong intercept mode
for eight days).

## Verify state first — one command

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

The occasional direct success is what makes this expensive — one lucky probe is
enough to convince you the port is fine and send you looking elsewhere.

## Open threads

### 1. Issue #1 — direct connections to the rdr's target port (OPEN, best lead yet)

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
Across 10 failing probes the SYN reaches lo0 every time — 11 retransmissions each — and
**nothing comes back at all**: no RST, no SYN-ACK, zero packets. A third burst with no
port filter confirmed nothing came back under any pair of ports either. The control down
`:443`, same run, was 10/10 clean. **pf swallows it; the investigation stays on pf.**

The 1-in-10 success is the lead. Its first SYN-ACK left the gateway addressed to
`127.0.0.1:443` instead of the client's port — pf reverse-translating the reply of a
connection that was never forward-translated — got a RST, and only survived because the
*retransmitted* SYN-ACK escaped that. So the failure is in the **reply** direction, not
the SYN. That is the first account consistent with all of it: zero filter counters, the
`state-insert` rise (11/probe here vs ~14 measured, same shape), and the anomalous
`17777 <- 17777` state. Full detail in INVESTIGATION-TLS-STORM.md, update (d).

**Four hypotheses, four wrong — so this is a hypothesis, not a fix.** It is the
best-supported one yet and that is exactly what the last four felt like. The next step is
a test of the reply-direction account, and it should revert unconditionally like
experiment-nostate-rule.sh did.

### 2. TLS handshake storm — recurred, cause unknown (OPEN)

Root-caused and fixed 2026-09-03 (Claude Desktop's updater vs. an untrusted CA), held
for three clean days, then returned 2026-09-07 23:20 through 2026-09-08 07:05 at about
a tenth of the old rate — and apparently from a **different** client: Claude Desktop's
log shows no TLS-worded error after 2026-09-03. Structural suspicion recorded in
INVESTIGATION-TLS-STORM.md: `rollback.sh` step 1b removes the System-keychain trust and
only `install-proxy.sh` step 5 restores it, so every rollback reopens the hole.
`CLAUDE_BURST_LOG_TLS_PEERS=1` identifies the client and needs no root.

## Four wrong answers — do not re-propose them

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
- **`reset` and `force-secondary` never reached the running gateway** — they mutated
  their own in-process copy and reported success. In a real overflow window that means
  being told you are off the paid secondary while billing continues. `61eee30`.
- **`install-proxy.sh` printed one port and passed another** to the root helper. `9aa478f`.

Plus: every prober now reads `cfg.Listen` instead of assuming 7777, and pf-heal's
self-test no longer reads the real `/etc/claude-burst/transparent.state` (it had been
passing for the wrong reason).

## Conventions worth knowing

- Run scripts **by path** (`scripts/rollback.sh`), never `bash scripts/...` — several are
  zsh-only and fail at parse time under bash, doing nothing.
- Never push without being asked. Commit locally and wait.
- Pasting into an interactive zsh: `#` is **not** a comment there, and parentheses in a
  pasted line get glob-expanded (`(stops...)` produced `unknown sort specifier` this
  session). Send bare commands.
- `deploy.sh` handles build, test, atomic swap, health check and rollback. Use it rather
  than copying binaries.
