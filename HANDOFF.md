# Session handoff — 2026-09-08

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

**Next step is written and not yet run:**

```bash
sudo scripts/experiment-nostate-rule.sh
```

It applies `pass quick on lo0 ... port <gport> no state` plus the filter anchor line
`/etc/pf.conf` lacks, measures before and after, and reverts unconditionally — on
success, failure, error or interrupt. Read its header before running it.

### 2. TLS handshake storm — recurred, cause unknown (OPEN)

Root-caused and fixed 2026-09-03 (Claude Desktop's updater vs. an untrusted CA), held
for three clean days, then returned 2026-09-07 23:20 through 2026-09-08 07:05 at about
a tenth of the old rate — and apparently from a **different** client: Claude Desktop's
log shows no TLS-worded error after 2026-09-03. Structural suspicion recorded in
INVESTIGATION-TLS-STORM.md: `rollback.sh` step 1b removes the System-keychain trust and
only `install-proxy.sh` step 5 restores it, so every rollback reopens the hole.
`CLAUDE_BURST_LOG_TLS_PEERS=1` identifies the client and needs no root.

## Three wrong answers — do not re-propose them

Issue #1 attracted three confident wrong diagnoses in one session. All are recorded as
ruled out, with evidence, in INVESTIGATION-TLS-STORM.md:

1. **A security product blocklists port 7777.** Wrong. 7777 answers 10/10 with a plain
   listener once it is no longer the rdr target. Moving the port relocated the symptom.
2. **A missing filter `pass` rule.** Wrong twice over: nothing is blocking (no block
   rules in the main ruleset or the com.apple anchor), and our anchor is referenced by
   `rdr-anchor` only, so a filter rule in it is loaded and never evaluated.
3. **State mismatch.** Predicted explicitly; `state-mismatch` did not move at all.

The pattern behind all three: reasoning from a mechanism instead of measuring, and in
case 1 varying a thing (the port number) that was perfectly confounded with the thing
that mattered (being the rdr target). The read-only diagnostic settled in one run what
two rounds of theorising did not.

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
