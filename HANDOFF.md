# Update 2026-09-21 (evening): the 18:04 failure, and what is still open

The 502 "resolve api.anthropic.com over DoH ... no such host" was **the machine's network**,
not a Burst bug: the gateway's own network snapshot shows no uplink and `www.apple.com` failing
in 1 ms at 16:36, 17:59 and 18:04-18:23, with the interface flapping between a hotspot
(`172.20.10.2`), a LAN (`192.168.0.90`), link-local (`169.254.x`) and `192.168.89.x`. Three
Burst behaviours made it worse and are fixed in the commit after this note (retry a dead pooled
connection once, do not fail over into a dead network, short self-releasing outage windows).

**Still open:** the pf-heal daemon healed the redirect three times that afternoon (17:35, 17:47,
17:59, ~1 s each, every one on a network change) and then logged nothing after 17:59:29, while
at 18:24 the redirect was refused again. Whether it died, hung, or was not triggered during the
18:04-18:24 churn is unknown: `scripts/rollback.sh` ran at 18:25:07 and stopped everything before
it could be inspected. If Burst is re-enabled, check `/var/log/claude-burst-pf.log` and the
daemon's state *first*, before trusting the redirect after a network change.

---

# Update 2026-09-21 (later): token shunting is now OFF

Read this before the handover below. Shunting was switched off with
`claude-burst shunt disable` because it delegated nothing in real use (12 blocks in real projects, 0 shunts;
Claude reads in windows, which the guard allows). **The two "not done" asks below (toggle
switch, README audit) are moot** unless someone decides to keep the feature. Reasoning and
evidence: `DECISION-token-shunting-off.md`. Open Claude Code sessions need a restart to drop
the hook.

---

# Handover — 2026-09-21: token shunting

Written when the session closed. **Verify before acting** — everything here was true at the
time and nothing keeps it true. Two older handoffs follow it: 2026-09-20 (failover fixes) and
2026-09-08 (issue #1, TLS storm). Neither is superseded by this one.

## Start here: what is NOT done

Two asks were in flight when the session ended.

### 1. Make the shunt master control a toggle switch, not a checkbox — NOT STARTED

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

### 2. README audit against what the product does now — PARTLY DONE

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
