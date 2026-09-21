# Claude Burst

**Your Claude subscription as the engine. Together AI's GLM as cheap overflow — and as a worker that keeps the boring work out of Claude's context.**

Claude Burst is a Mac-only local gateway for Claude Code. One inexpensive metered model does two jobs:

1. **Overflow.** It keeps your normal Claude Pro/Max login as the primary credential, watches Anthropic's own subscription rate-limit headers, and only when Anthropic says a model's allowance is actually exhausted does it send *that model's* requests to a secondary — this README works through **Together AI serving GLM** — then returns to the subscription when the reset timestamp arrives.
2. **Token shunting.** The same secondary reads large files and generates boilerplate *instead of* Claude, so your allowance is spent on judgment rather than I/O. A hook refuses whole-file reads above a threshold and redirects them to the worker. See [Token shunting](#token-shunting-keep-the-boring-work-out-of-claudes-context).

The secondary is a pluggable slot. **Together AI** and **OpenRouter** (or any other OpenAI-compatible chat-completions endpoint) can do both jobs. **Amazon Bedrock** is supported for overflow only and cannot be a shunt worker. A direct Anthropic API key is available if you would rather stay on Anthropic's own billing. See [Together AI, OpenRouter or any OpenAI-compatible secondary](#together-ai-openrouter-or-any-openai-compatible-secondary).

This is an experimental MVP. Test it on a non-critical development account before any broader rollout.

**No Claude subscription?** Claude Burst also supports a direct, metered Anthropic API key as the primary route instead of subscription passthrough (see [No-subscription setup](#no-subscription-setup-metered-api-key-primary) below). In that mode there's no included allowance to burst from, so failover to the secondary is triggered by sustained failures instead of subscription-exhaustion headers — both routes are metered, so a single transient error doesn't flip traffic to a second paid provider.

## Supported providers

Primary and secondary are independent, pluggable slots (`internal/router/provider.go`) — nothing here is tied to one vendor:

| Slot | Options |
| --- | --- |
| **Primary** | `oauth-passthrough` — your existing Claude Pro/Max subscription login (the default, and the setup this whole README describes first) · `anthropic-api-key` — a direct, metered Anthropic API key, for accounts with no subscription |
| **Secondary** | `openai-compatible` — **Together AI** (the worked example, serving GLM), OpenRouter, or any other OpenAI-compatible chat-completions endpoint · `bedrock` — Amazon Bedrock · `none` — disable overflow entirely |

What each secondary can do:

| Secondary | Overflow | Token-shunting worker |
| --- | :---: | :---: |
| Together AI, OpenRouter, any OpenAI-compatible endpoint | yes | **yes** |
| Amazon Bedrock | yes | no |

Bedrock speaks Anthropic's Messages format natively and is relayed byte-for-byte. The OpenAI-compatible providers go through a translator in both directions. The shunt worker makes one plain chat-completions call, which is why only that family can serve it.

See [Together AI, OpenRouter or any OpenAI-compatible secondary](#together-ai-openrouter-or-any-openai-compatible-secondary) for the worked examples, and [Configuration](#configuration) for every field.

**Keeping Remote Control.** Pointing Claude Code at any local gateway normally costs you its Remote Control feature — Claude Code disables Remote Control the moment `ANTHROPIC_BASE_URL` names anything other than `api.anthropic.com`, and the default setup below sets exactly that variable. Claude Burst's `transparent` intercept mode solves this by never touching `ANTHROPIC_BASE_URL` at all: instead of using that config mechanism, it gets into the path a level lower, at DNS, so Claude Code's own settings never change and it believes it is still talking to `api.anthropic.com` directly. See [Keeping Remote Control: transparent intercept mode](#keeping-remote-control-transparent-intercept-mode-optional) below.

## Why this exists

Anthropic exposes materially different commercial models for access to the same Claude model families:

- Claude Max is a fixed monthly subscription with rolling usage limits.
- Claude API is metered by token.
- Together AI, OpenRouter and Amazon Bedrock all offer metered access to Claude-family or comparable models outside Anthropic's own billing.

Anthropic's Claude Code gateway documentation explicitly supports `ANTHROPIC_BASE_URL` with an existing claude.ai subscription login. Setting only the base URL keeps the subscription credential active and the subscription's usage limits and billing continue to apply. Claude Burst uses that supported gateway mechanism and respects the subscription limit rather than trying to evade it.

## Token shunting: keep the boring work out of Claude's context

> **Switched off on 2026-09-21, and not recommended.** In its first day it blocked 12 direct
> reads in real plugin projects and delegated **none** of them: Claude read the files in windows
> instead, which the guard deliberately allows. Full reasoning and the evidence:
> [DECISION-token-shunting-off.md](DECISION-token-shunting-off.md), including how it compares
> with the article it was built from (the design matches; the workload and the test did not).
> What follows documents how it works, for anyone re-enabling it.


Most of what a coding agent does is I/O, not judgment: reading 25,000 tokens of source to produce 300 tokens of understanding. `claude-burst shunt` hands that to your **configured openai-compatible secondary** — Together AI serving GLM in the worked example — so Opus/Fable never carry it. It is the [Spotify technique](https://andrewbaker.ninja/2026/09/17/shunting-the-boring-work-how-spotify-cut-claude-code-token-usage-by-90-and-how-to-do-the-same-without-their-plugin/) built into the gateway you already run, reusing the secondary's Keychain key and base URL.

**Prerequisite:** an openai-compatible secondary (Bedrock cannot be a worker). With Together AI:

```bash
claude-burst configure --secondary openai-compatible \
  --secondary-base-url https://api.together.xyz/v1 \
  --secondary-model zai-org/GLM-5.3
TOGETHER_API_KEY='your-key' claude-burst keychain-set --provider together
```

Then:

```bash
claude-burst shunt enable            # both parts; or --read / --write for one
claude-burst shunt doctor            # proves the worker sees a whole prompt
claude-burst shunt status            # what is on, and what it has saved
claude-burst shunt disable --write   # switch one part off; disable alone removes everything
```

The dashboard has a **Token shunting** panel (rail → Control) with the same two switches, the threshold, and what it has saved. It shows the hook, skill and worker separately and turns red when a switch is on over something that is not actually in place (for example the hook wiped from `settings.json`), with a one-click repair.

Restart Claude Code after enabling. Four parts, installed for you:

| Part | What it does |
| --- | --- |
| **Guard** (`PreToolUse` hook, `Read\|Bash`; installed only while `read` is on) | Refuses a whole-file `Read`, or a plain `cat`/`head`/`tail`/`less`/`more`, on a file of **350+ lines** (`shunt.min_lines`) and tells Claude to use the worker instead. Windowed reads (`offset`/`limit`) always pass — they are the escape hatch. |
| **`shunt read`** | `--question Q file...` → a terse answer with `path:line` citations. Files over 6,000 lines are chunked and read concurrently. Needs `read` on. |
| **`shunt write`** | `--spec S --ref example --out path` → generates a file **straight to disk**, never through Claude's context. Keeps the previous file as `.bak`, refuses truncated, empty, `null` or refusal output, writes atomically. Needs `write` on. |
| **Skill** (`~/.claude/skills/claude-burst-shunt`) | Tells Claude when to delegate and when not to (edits, debugging, concurrency, security review). Describes only what is switched on. |

**Why a hook and not just instructions:** an agent under pressure reads the file directly unless something refuses it. The guard is that something.

**What it will not do**

- **Never sends credential-looking files** (`.env`, `*.pem`, `*.key`, `id_rsa*`, `.aws/`, `.ssh/`, `*.tfstate`, …) to the worker, and refuses to write them. A direct read is allowed for those, as before.
- **Fails open.** If the guard cannot decide (bad input, unreadable config) it allows the read. A wrongly allowed read only costs tokens; a wrongly refused one would leave Claude unable to look at a file.
- **Leaves `.claude/` alone** and will not write inside it or `.git/`.
- **Needs an openai-compatible secondary.** Bedrock is a different wire protocol and is not a worker.
- **Your files go to that provider** whenever a read is delegated. That is the trade; use `shunt.model`/a self-hosted `secondary.base_url` if it matters.

**Honest numbers.** Spotify's ~90% is the reduction in frontier tokens on *bulk-read* work, not on a whole session; an independent rebuild measured ~60% fewer frontier tokens, ~33% lower total cost and ~65% more elapsed time (a delegated read costs 10-30 s). Below the threshold a delegation costs more than it saves — that is why the default is 350 lines. `claude-burst stats` and `status` report reads, writes, refused direct reads, an *estimated* tokens-kept-out figure (~4 bytes/token) and the worker's own cost. A worker model with no `pricing` entry is reported as unpriced rather than free. Measure over a couple of weeks before quoting it.

```json
"shunt": { "read": true, "write": true, "min_lines": 350, "chunk_lines": 6000, "timeout_seconds": 120, "model": "" }
```

**Seeing what is going on.** Every refusal, delegation and failure is recorded in `~/.config/claude-burst/shunt.jsonl` with **the Claude Code session id, the project directory, the file and the tool** (`Read`, `Bash cat`, ...), and no file contents, questions, specs or answers.

```bash
claude-burst shunt log                  # plain text, oldest first: time, TAG, what happened, [project · session]
claude-burst shunt log --problems       # only what needs a look
claude-burst shunt log --session aaaa1  # one session (id or prefix)
claude-burst shunt log --json           # raw events
claude-burst shunt status               # totals, plus the last problems
```

One shunt is **one row**. The guard hook and `shunt read` are separate processes that log separately — a *redirect* when a direct read is blocked, and the delegated read a few seconds later — and the dashboard and `shunt log` fold the pair into a single `SHUNTED` row (raw events stay in `shunt.jsonl`, and `shunt log --json` prints them unfolded). Tags: **`SHUNTED`** (the read went to the worker instead of Claude; if Claude tried a direct read first the row says how many times), `WRITE` (generated straight to disk), **`REDIRECTED`** (a direct read was blocked and Claude has not yet followed up — grey, and turns into `SHUNTED` or `NO-SHUNT`), **`NO-SHUNT`** (a direct read was blocked but no delegated read followed: usually Claude read a window of the file or moved on — amber, not a failure), **`LOOP`** (the same session was blocked three or more times with no answer in between: it is retrying instead of running `shunt read`; the third and later attempts also tell the model so), `SHUNT-FAIL` / `WRITE-FAIL` (**with the stage it failed at**: `disabled`, `args`, `worker_init`, `worker_call`, `validate`, `write_file`), and `GUARD-ERR` (the guard could not decide and **allowed** the call; it fails open, and says so). Only the last three kinds and `LOOP` are red. Every exit path logs, including the ones that used to leave no trace: the feature being off, no worker key, bad arguments. The dashboard's *Recent activity* table shows the same tags with project and session columns, and turns the card red only while a session is looping.

**How it is tested.** Each piece has unit tests (the guard's rules, the worker, code-write validation, the `settings.json` hook editor, the dashboard endpoints). On top of those, `internal/integration/shunt_e2e_test.go` builds the real binary and drives it the way Claude Code does — the hook's stdin/exit-code protocol, a `settings.json` that already holds someone else's hook, and a fake openai-compatible worker — through enable, blocking and passing the right calls, a delegated read (a credentials file never reaches the worker), a generated file, the log, and disable restoring `settings.json` exactly. It also checks that enabling refuses without a worker, that a failing worker is reported and logged, and that the guard fails open on a broken config. Breaking the guard, or skipping the worker check on enable, makes it fail.

**Live proof on your machine.** Those tests fake Claude Code. `internal/integration/shunt_live_test.go` uses the real thing: it runs `claude -p` on a small model against a 600-line file, then reads `shunt.jsonl` for *that session's own* events. It checks that a real whole-file `Read` is blocked, that the refusal reaches the model, that the log names the session, project and file, that the delegated `shunt read` runs with the same session id (so `CLAUDE_CODE_SESSION_ID` really is in Claude's Bash environment), that a real call to your worker is billed and recorded, that nothing loops, and that the answer is right. It uses the **installed** binary and your real config, so it answers "is what I have deployed working?", and it costs a few cents, so it is opt-in:

```bash
CLAUDE_BURST_LIVE_SHUNT=1 go test ./internal/integration/ -run TestLiveShunt -v -timeout 10m
```

Its events appear in your real log under a project named `shunt-live-check-*`. It refuses to pass on a machine where shunting is not fully enabled. A model that is blocked is free to answer another way (it sometimes uses `grep`), so one test lets the model choose and asserts only what holds either way, and the other tells it to follow the refusal's instructions and proves the whole redirect.

## Routing behaviour

1. Claude Code sends `/v1/messages` to `http://127.0.0.1:7777`.
2. Claude Burst forwards the request to `https://api.anthropic.com` unchanged, including the user's saved Claude subscription OAuth credential and required beta headers.
3. Successful responses stream straight back to Claude Code.
4. Generic `429` responses do **not** trigger overflow.
5. Overflow activates only when Anthropic's subscription headers indicate a rejected unified limit, for example `anthropic-ratelimit-unified-status: rejected`, or when an explicit subscription-limit error is returned.
6. Claude Burst reads Anthropic's reset timestamp and persists it against **the model that was refused**, not the account.
7. The rejected request is replayed down that model's `fallback_chain` first — another Claude model, still on the subscription, still free.
8. Only when every rung has a rejection window of its own does the request go to the configured secondary — Together AI, OpenRouter, any other OpenAI-compatible endpoint, or Amazon Bedrock — using a credential stored in macOS Keychain.
9. Later requests for that model skip straight to the rung (or the secondary) until the reset time plus a small safety grace period; other models are untouched.
10. The first request after that time goes back to Anthropic Max automatically.

### Limits are per model, and are never inferred

Anthropic's claim headers name the *bucket* that was exhausted (`five_hour`,
`seven_day_opus`, `seven_day_overage_included`, …) but nothing in the response states which
**models** that bucket covers. Claude Burst does not guess: only the model that was actually
refused gets a window. If a limit really is account-wide, the next model discovers that for
itself on its first request — one rejection, which bills nothing.

Guessing the other way is what cost real money. Until 2026-09-20 any reported limit armed
one account-wide window, so a single refused Fable request sent **every** model to the paid
secondary for the next two days while Opus was answering normally.

`fallback_chain` in `config.json` is the ordered list of models to try on the subscription
before spending anything:

```json
"fallback_chain": {
  "claude-fable-5-1": ["claude-opus-5"],
  "claude-fable-5":   ["claude-opus-5"]
}
```

Those two are the shipped default. `claude-opus-5` → `claude-sonnet-5` works the same way
but is left for you to add deliberately — it is a much larger capability drop than a cost
saving justifies by default. A rung is skipped when it is inside a window of its own, and a
chain that names its own key is ignored rather than retrying the model that was just refused.

The dashboard's **Actions** section has a *Try another Claude model before the secondary*
toggle that turns the whole thing off live, without a config edit or a restart, and shows
which models are currently refused and where their traffic is actually going. `claude-burst
status` prints the same thing as `refused:` lines.

The forced window (`claude-burst force-secondary`, or **Force → secondary** on the
dashboard) is still account-wide and deliberately bypasses the chain: its only purpose is to
exercise the secondary, and quietly serving a different Claude model instead would defeat
the only test that path ever gets.

Only `/v1/messages` participates in any of this. Everything else — `count_tokens`, and
Claude Code's control-plane traffic such as Remote Control's long-poll and settings fetch —
always goes to the primary, never fails over, and does not feed the failover detector in
either direction. There is nowhere correct to send those: an OpenAI-compatible endpoint has
no equivalent of a Remote Control long-poll, and translating a `count_tokens` body would
bill a full generation to answer "how many tokens is this". Just as importantly, a dropped
long-poll is not evidence that inference is failing and must not be able to open a paid
overflow window, and a healthy long-poll is not evidence that it has recovered.

Claude Burst does not rotate Max accounts, suppress quota signals, fabricate headers, or attempt to extend the Max allowance. The subscription limit remains authoritative.

## What is logged

Claude Burst writes to two files under `~/.config/claude-burst/` — both rotate, so the pair is really up to 20 `claude-burst.log[.N]` files and 6 `metrics.jsonl[.N]` files — and both are metadata-only: **prompts, source code, tool inputs and model outputs are never written to disk.** The proxy necessarily handles the request body in memory so it can replay a rejected request to the secondary, but it does not persist it.

### `metrics.jsonl` — structured, one line per request

(Also note the model fields: `model` is what **served** the request and drives cost;
`requested_model` is what Claude Code asked for. They differ exactly when a remapping
provider — Bedrock's modelMap, or openai-compatible fixed/consistent failover — was
involved. Pricing uses the served model, so a request GLM served is never costed at
Claude Opus rates.)

- timestamp and a short request id (also present in `claude-burst.log`, so a line in one file can always be matched to the other)
- Claude Code session and agent identifiers
- the slot (`primary` or `secondary`) and the route that served it — `anthropic` for oauth-passthrough, `anthropic-api-key`, `bedrock`, or an openai-compatible secondary's vendor label (`together`, `openrouter`, whatever `keychain_service` names)
- `destination`: the actual outbound URL (scheme, host and path; no query). The slot label says which slot was *chosen*; this says where the request physically went, which is what settles "did that really go to the secondary?"
- model
- HTTP status
- latency
- input/output token usage where exposed in the SSE stream
- estimated API-equivalent cost using the prices in `config.json`. A model with **no** `pricing` entry costs `0` — so the event also carries `pricing_unknown: true`, and `stats` counts it separately, because a zero meaning "not priced" must not read as a zero meaning "free". Third-party secondary models are not in the default pricing table: add yours to `pricing` or its spend will not be counted
- subscription limit claim and reset timestamp when failover occurs
- a short note on what happened (e.g. "subscription limit detected; request replayed to secondary", "keychain load failed: ...")

### `claude-burst.log` — plain text, for debugging

Every request gets a `start` line and a matching `done` line (same request id, final HTTP status, duration), so you can always see what the gateway did even for requests that never made it into `metrics.jsonl`. In addition, every failure path logs which stage it failed at and why:

- reading or size-limiting the request body
- building the outbound request to Anthropic or the secondary
- the upstream HTTP call itself (network errors)
- loading the secondary's key from the environment/Keychain
- mapping a model name for the secondary (Bedrock `model_map`, or the openai-compatible target)
- reading or writing the local overflow-state file
- writing a metrics event
- a panic anywhere in the request path — recovered, logged with a stack trace, and turned into a `500` instead of crashing the gateway or hanging Claude Code's connection

A metrics-write failure (disk full, permissions, etc.) is logged but never fails the request itself — by the time metrics would be written, Claude Code has already been served.

## Requirements

- macOS on Apple Silicon or Intel
- Go 1.23+ (there's no prebuilt binary in the repo; `install.sh` builds one locally)
- Either: Claude Code already installed and logged into the intended Pro/Max account (subscription mode), **or** a metered Anthropic API key (no-subscription mode)
- A credential for whichever secondary you pick — one of:

| Secondary | Credential | Provider setting | Overflow | Shunt worker |
|---|---|---|:---:|:---:|
| **Together AI** (worked example) | a Together API key | `--secondary openai-compatible` | yes | yes |
| **OpenRouter** | an OpenRouter API key | `--secondary openai-compatible` | yes | yes |
| **Amazon Bedrock** | Bedrock access + an API key in `AWS_BEARER_TOKEN_BEDROCK` | `--secondary bedrock` | yes | no |
| *(none)* | — | `--secondary none` | no | no |

The secondary is a pluggable slot (`internal/router/provider.go`), not a hardcoded vendor. The two families differ in two ways worth knowing up front. Bedrock speaks Anthropic's Messages wire format natively, so its responses are relayed byte-for-byte, while Together AI and OpenRouter go through the OpenAI-compatible translator (`internal/router/provider_openai.go`) in both directions, streaming included — both paths are tested. And only the OpenAI-compatible family can be a [token-shunting](#token-shunting-keep-the-boring-work-out-of-claudes-context) worker. Pick on price, model availability and who you would rather have a billing relationship with.

## Install

```bash
git clone https://github.com/andrewbakercloudscale/claude-burst.git
cd claude-burst
```

Run `./install.sh`, then point the secondary at your provider. Together AI is the recommended path — it does overflow and can also be the shunt worker:

```bash
# Together AI (recommended)
./install.sh
claude-burst configure --secondary openai-compatible \
  --secondary-base-url https://api.together.xyz/v1 \
  --secondary-model zai-org/GLM-5.3
TOGETHER_API_KEY='your-key' claude-burst keychain-set --provider together
claude-burst shunt enable          # optional: token shunting, see above
```

```bash
# OpenRouter
./install.sh
claude-burst configure --secondary openai-compatible \
  --secondary-base-url https://openrouter.ai/api/v1 \
  --secondary-model anthropic/claude-sonnet-4.5
OPENROUTER_API_KEY='your-key' claude-burst keychain-set --provider openrouter
```

```bash
# Amazon Bedrock (overflow only — cannot be a shunt worker)
export AWS_REGION=us-east-1
export AWS_BEARER_TOKEN_BEDROCK='your-bedrock-api-key'
./install.sh
```

**Why the Together and OpenRouter blocks run `configure` after the installer.** `install.sh` itself only knows Bedrock: it runs `configure --region` and stores `AWS_BEARER_TOKEN_BEDROCK` if that is set. That is its historical default, not a recommendation. Until you run `configure --secondary openai-compatible` the secondary is a Bedrock slot with no key behind it, so overflow would have nowhere to go.

See [Together AI, OpenRouter or any OpenAI-compatible secondary](#together-ai-openrouter-or-any-openai-compatible-secondary) for the full detail, including what the translator does.

The installer:

- builds `claude-burst` locally (`go build`) and installs it into `~/.local/bin/claude-burst`
- stores a Bedrock key in macOS Keychain if `AWS_BEARER_TOKEN_BEDROCK` is set; for Together AI, OpenRouter or any other endpoint use `claude-burst keychain-set --provider <name>` as shown above (see [Commands](#commands))
- writes the initial configuration
- updates `~/.claude/settings.json` with only `ANTHROPIC_BASE_URL=http://127.0.0.1:7777`
- does **not** add an Anthropic credential of its own — in subscription mode this keeps the saved Max login active; in no-subscription mode, Claude Code's own `ANTHROPIC_API_KEY` (set separately, see below) is what gets forwarded
- creates and starts a macOS LaunchAgent
- adds `~/.local/bin` to `~/.zprofile` if required

Restart Claude Code after installation.

### No-subscription setup (metered API key primary)

If you don't have a Claude Pro/Max subscription, use a direct Anthropic API key as the primary route instead of subscription passthrough:

```bash
./install.sh
claude-burst configure --primary anthropic-api-key \
  --secondary openai-compatible \
  --secondary-base-url https://api.together.xyz/v1 \
  --secondary-model zai-org/GLM-5.3
TOGETHER_API_KEY='your-key' claude-burst keychain-set --provider together
claude-burst enable
```

Then set `ANTHROPIC_API_KEY` in Claude Code's own settings env (e.g. the `env` block in `~/.claude/settings.json`, alongside `ANTHROPIC_BASE_URL`) — **not** in claude-burst's config. The gateway never stores or injects an Anthropic credential itself; it only forwards whatever auth header Claude Code already sent, exactly like subscription mode does with the OAuth header.

In this mode, both the primary (metered Anthropic API) and the secondary (Together AI) cost money per token, so failover isn't triggered by a single rate-limit response — see [`metered_failover`](#configuration) below. (Amazon Bedrock works here too: `--secondary bedrock --region us-east-1` with `AWS_BEARER_TOKEN_BEDROCK` stored via `claude-burst keychain-set`.)

## Verify

```bash
claude-burst status
curl -s http://127.0.0.1:7777/healthz          # base-url mode only — see below
claude-burst stats --days 30
```

**In transparent mode that `curl` will almost certainly time out, and that is expected.**
While the pf redirect is installed, direct connections to the gateway's own port nearly
always fail — the rule makes its own target port unreachable, whichever port it targets.
Measured at 0/20 and 1/15 in separate runs, so roughly one attempt in twenty does get
through: enough that a single lucky probe can convince you the port is fine, not enough to
build a health check on (issue #1, and `INVESTIGATION-TLS-STORM.md` for the measurements).
Probe the real path instead, which is what the gateway's own health checks do:

```bash
curl -sk https://api.anthropic.com/healthz     # answers from the gateway, not Anthropic
```

A JSON body containing `"overflow"` means it came from Claude Burst. Anthropic's real
`/healthz` does not return that, so this cannot produce a false positive.

You can also check Claude Code's own `/status` and `/usage` views to confirm that it is still using the intended subscription before a failover occurs.

## Commands

```text
claude-burst serve
claude-burst configure --secondary openai-compatible \
  --secondary-base-url https://api.together.xyz/v1 --secondary-model zai-org/GLM-5.3
claude-burst configure --secondary none
claude-burst keychain-set --provider together   # reads TOGETHER_API_KEY into the Keychain
claude-burst keychain-set --provider together --service my-service   # store under a custom service name
claude-burst enable
claude-burst disable
claude-burst status
claude-burst reset                           # back to primary now
claude-burst force-secondary --minutes 15    # route to the secondary on purpose (testing)
claude-burst stats --days 30
claude-burst version

claude-burst shunt enable [--read] [--write] # token shunting: default both
claude-burst shunt disable [--read] [--write]
claude-burst shunt status
claude-burst shunt doctor [--quick]          # does the worker see a whole prompt?
claude-burst shunt log [--problems]          # what happened, with project and session

# Amazon Bedrock secondary (overflow only)
claude-burst configure --secondary bedrock --region us-east-1
claude-burst keychain-set                    # reads AWS_BEARER_TOKEN_BEDROCK
claude-burst configure --primary anthropic-api-key --secondary bedrock
```

`shunt guard`, `shunt read` and `shunt write` also exist, but they are what the hook and Claude run, not commands you type.

## Configuration

Configuration lives at `~/.config/claude-burst/config.json`. Legacy flat fields (`anthropic_base_url`, `bedrock_base_url`, `model_map`, `keychain_service`) are still read and still work unchanged — they're synthesized into `primary`/`secondary` automatically. New setups can also configure `primary`/`secondary` directly:

```json
{
  "listen": "127.0.0.1:7777",
  "reset_grace_seconds": 10,
  "unknown_reset_seconds": 300,
  "response_header_timeout_seconds": 60,
  "max_request_mb": 128,
  "primary": {
    "provider": "oauth-passthrough",
    "base_url": "https://api.anthropic.com",
    "failover_strategy": "subscription-limit"
  },
  "secondary": {
    "provider": "openai-compatible",
    "base_url": "https://api.together.xyz/v1",
    "model": "zai-org/GLM-5.3",
    "keychain_service": "claude-burst-together"
  },
  "pricing": {
    "zai-org/GLM-5.3": { "input_per_mtok": 1.4, "output_per_mtok": 4.4 }
  },
  "shunt": {
    "read": true,
    "write": true,
    "min_lines": 350
  },
  "metered_failover": {
    "window_seconds": 60,
    "min_failures": 3
  }
}
```

An Amazon Bedrock secondary instead has `"provider": "bedrock"`, the `bedrock-runtime` `base_url`, `"keychain_service": "claude-burst-bedrock"` and a required `model_map` from every Claude model to its Bedrock id — see [Amazon Bedrock notes](#amazon-bedrock-notes).

- `primary.provider` / `secondary.provider`: `oauth-passthrough` (subscription OAuth passthrough), `anthropic-api-key` (metered, no-subscription), `openai-compatible` (Together AI, OpenRouter, ...), or `bedrock`. Neither slot is tied to a specific vendor — either can hold any of them, though `configure --primary` only accepts the first three, so a non-Anthropic primary means editing `config.json`. `none` is valid for the secondary only.
- `primary.failover_strategy`: `subscription-limit` (only Anthropic's own subscription-exhaustion headers trigger failover — a bare 429 never does), `metered-failures` (a sliding-window failure count triggers failover, since every route is metered and a single blip shouldn't move traffic), `subscription-limit+metered-failures` (both: genuine subscription exhaustion fails over immediately as above, *and* a sustained run of 429/5xx responses or transport errors/timeouts — an Anthropic outage, not plan exhaustion — fails over once the relevant `metered_failover` threshold is reached, which is a different number for HTTP failures than for transport failures; see below), or `none` (never fail over). A subscription (`oauth-passthrough`) primary defaults to `subscription-limit` alone, which by design does **not** react to a bare 500 or a timeout — set `subscription-limit+metered-failures` (`claude-burst configure --failover-strategy subscription-limit+metered-failures`) if you also want overflow on an Anthropic outage.
- `metered_failover.window_seconds` / `min_failures` / `transport_error_min_failures`: for the metered strategies, how many upstream failures inside a trailing window before failing over. **Two counters, not one**, because the two signals differ in strength. An HTTP failure (429 or 5xx) means Anthropic answered and could be a passing blip, so it takes `min_failures` (default 3) within `window_seconds` (default 60). A transport failure — Anthropic could not be reached at all — takes `transport_error_min_failures`, which defaults to **1**, so a real outage does not sit retrying against a dead primary. Any success resets both. Other 4xx errors (bad key, malformed request) never count, since routing to the secondary wouldn't fix them; neither do failures that are unambiguously *this machine's* fault — DNS resolution failure, "network unreachable", "no route to host" — because the secondary is equally unreachable through a dead local network, and counting them turns walking out of WiFi range into a paid overflow window. Nor does a request the **client** cancelled: the outbound call carries Claude Code's own request context, so interrupting a turn cancels the upstream call too, and with `transport_error_min_failures` at 1 a single Esc used to arm a 300-second overflow window and bill the next few minutes of inference to the paid secondary (observed live 2026-09-08). Cancellation is excluded, and a cancelled request is never replayed to the secondary — nobody is waiting for the answer. A *deadline* that expires still counts, since that is a genuinely stalled upstream.
- `pricing`: per-million-token rates, keyed by the model that actually served the request. A third-party model is **not** in the defaults (the same GLM id costs different amounts through Together, OpenRouter and Z.ai), so add yours or its spend is reported as unpriced rather than free. This also prices token-shunt worker calls.
- `shunt.read` / `shunt.write` / `shunt.min_lines` / `shunt.chunk_lines` / `shunt.timeout_seconds` / `shunt.model`: token shunting — see [Token shunting](#token-shunting-keep-the-boring-work-out-of-claudes-context). `shunt.model` overrides the worker model; empty means the secondary's own model.
- `response_header_timeout_seconds`: bounds how long the gateway waits for a response to *start* before treating the upstream as failed (doesn't affect how long an already-started stream can run).

Model IDs change over time. With Together AI or OpenRouter keep `secondary.model` (and any `model_map`) aligned with a model the endpoint actually serves; with Bedrock keep `model_map` aligned with the Claude models enabled in your account.

## Important limitations

### 1. No failover on ordinary throttling

Anthropic uses HTTP 429 for several different conditions. Claude Burst deliberately refuses to interpret a bare 429 as Max exhaustion. This avoids turning a temporary capacity throttle into unexpected spend on the secondary.

### 2. API-equivalent cost is not Anthropic's internal cost

The metrics estimate answers: "What would these observed input/output tokens cost at the configured public API rates?" It does not estimate Anthropic's marginal inference cost, gross margin, internal transfer pricing, or the economic value of prompt caching unless you extend the metric model to account for cache buckets.

### 3. Consumer versus commercial governance remains different

A local data-loss-prevention layer can reduce what leaves the machine, but it does not make a consumer Max account contractually or operationally identical to Claude for Work, the Claude API, or Bedrock. Review your organization's legal, procurement, retention, audit and account-management requirements before rolling consumer subscriptions out to employees.

### 4. No caller authentication on the local gateway

Any local process — including a browser tab, since `POST /v1/messages` with a simple content type needs no CORS preflight — can send the gateway a request. It cannot force an overflow window open (only genuine subscription-limit/sustained-failure signals do that), but it can ride an already-open one, and it can drive ordinary (non-overflow) traffic through your credential. Don't bind `listen` to anything but `127.0.0.1`.

### 5. Upstream error text (including the request path/query) is logged and metered failure detail is not size-bounded

Transport-error and non-failover-error log lines include the upstream `error.Error()` string, which can contain the request URL (path and query, not host credentials — Go's `url.Error` redacts userinfo). Prompts and response bodies are never included per the metadata-only design, but treat `claude-burst.log` as containing request metadata, not as fully opaque.

### 6. Transparent intercept mode is machine-wide, and TLS interception is assumed benign

The `/etc/hosts` entry transparent mode installs affects every process on the Mac, not just
Claude Code — see the trade-off table above. Separately, the design assumes that TLS
interception does not itself break Remote Control. That is well supported (Claude Code is
widely run behind corporate inspecting proxies, and documents `NODE_EXTRA_CA_CERTS` for
exactly that) but is not something this project can prove. `scripts/check-interception.sh`
settles it on a network that actually inspects TLS: it distinguishes *intercepted* from
*bypassed* from *not enrolled*, which a bare certificate-issuer check cannot.

### 7. Token shunting sends file contents to the secondary provider

Whenever a read is delegated the whole file goes to the secondary (Together AI in the worked example), a third party with its own retention terms. Credential-looking files (`.env`, `*.pem`, `*.key`, `id_rsa*`, `.aws/`, `.ssh/`, `*.tfstate`, ...) are never sent, by a name-based heuristic that errs toward refusing; it is a safety net, not a data-loss-prevention control. A worker's answer is derived from file contents, which can contain text that looks like instructions, so the installed skill tells Claude to treat it as data. Turn it off from the dashboard's master switch or with `claude-burst shunt disable`; the guard reads `config.json` on every call, so off takes effect immediately.

## Together AI, OpenRouter or any OpenAI-compatible secondary

A secondary can be any OpenAI-compatible chat-completions endpoint, not one named vendor. **Together AI serving GLM 5.3 is the one this project runs and has verified live**, including a genuine streaming tool call. `provider: "openai-compatible"` plus a `base_url` and `model` is the entire integration surface; nothing about the vendor is hardcoded anywhere in the request path. This is also the only family that can be a [token-shunting](#token-shunting-keep-the-boring-work-out-of-claudes-context) worker. Together AI and OpenRouter (which fronts many different model providers behind one OpenAI-compatible API) are shown below, but the same `base_url`/`model` shape works for any other OpenAI-compatible endpoint. Unlike `bedrock` and the two Anthropic-passthrough providers, which all speak Anthropic's Messages wire format natively and only need `Server.relay` to stream the response back byte-for-byte, this provider (`internal/router/provider_openai.go`) does real bidirectional translation: request body shape (`system`/`messages`/`tools`, including splitting Anthropic's nested `tool_result` blocks into OpenAI's sibling `tool` messages), non-streaming and **streaming** response shape (OpenAI's `delta`-based SSE chunks translated live into Anthropic's `message_start`/`content_block_start`/`content_block_delta`/`content_block_stop`/`message_delta`/`message_stop` event sequence, including parallel tool calls), and tool-call schema (`tool_use` blocks ↔ `tool_calls`).

Not translated (dropped, not an error): Anthropic's server-side tools (`web_search_20250305` and friends — declared with a name and a `type` but no `input_schema`, because Anthropic's own API runs them; a logged line names any that were dropped), images/documents in message content, Anthropic extended-thinking (`thinking`/`redacted_thinking`) blocks in history, and prompt-caching `cache_control` hints — none have a meaningful equivalent on a generic OpenAI-compatible endpoint, and Claude Code's ordinary coding-agent traffic is overwhelmingly text + tool-use.

Configure it as `secondary` in `config.json`. Two failover modes, chosen just by whether `model_map` is present:

**Fixed failover** — every Claude model (sonnet, opus, haiku) fails over to the same target model:
```json
{
  "secondary": {
    "provider": "openai-compatible",
    "base_url": "https://api.together.xyz/v1",
    "model": "zai-org/GLM-5.3"
  }
}
```

**Consistent failover** — each Claude model can fail over to a *different* target (e.g. opus to a stronger/pricier model, sonnet/haiku to a cheaper one), via `model_map`. `model` is still required as the fallback target for any Claude model with no explicit entry:
```json
{
  "secondary": {
    "provider": "openai-compatible",
    "base_url": "https://api.together.xyz/v1",
    "model": "zai-org/GLM-5.3",
    "model_map": {
      "claude-opus-5": "zai-org/GLM-5.3-Big"
    }
  }
}
```
Unlike Bedrock's `model_map` (which errors on a Claude model with no entry), an unmapped model here silently falls back to `model` rather than failing the request — there's always a usable target.

### Worked example: OpenRouter instead of Together AI

Same shape, different endpoint and model:

```json
{
  "secondary": {
    "provider": "openai-compatible",
    "base_url": "https://openrouter.ai/api/v1",
    "model": "z-ai/glm-5.3"
  }
}
```

```bash
claude-burst configure --secondary openai-compatible \
  --secondary-base-url https://openrouter.ai/api/v1 \
  --secondary-model z-ai/glm-5.3 \
  --secondary-keychain-service claude-burst-openrouter
claude-burst keychain-set --provider openrouter   # reads OPENROUTER_API_KEY
```

### Credential storage and naming

`claude-burst keychain-set --provider <label>` stores whatever `<label>_API_KEY` is set in the environment (uppercased, hyphens become underscores) into a macOS Keychain service named `claude-burst-<label>` by default — `--provider together` reads `TOGETHER_API_KEY` into `claude-burst-together`, `--provider openrouter` reads `OPENROUTER_API_KEY` into `claude-burst-openrouter`, and so on for any other vendor. Nothing here is a hardcoded allowlist; `<label>` can be anything. At request time, the gateway derives the same identity back out of whichever keychain service `secondary.keychain_service` actually names, so the two directions always agree without a second place to keep in sync (`internal/router.EnvVarForProvider` / `openAICompatibleIdentity`). Use `--secondary-keychain-service` on `configure` if you want a service name other than the `claude-burst-<label>` default (for example, to run two different OpenAI-compatible secondaries side by side under distinct names), and the matching `--service` on `keychain-set` to store the key under that same name. `keychain-set` never infers the service from whichever secondary happens to be configured: it used to, and the first time two OpenAI-compatible providers existed side by side it overwrote one provider's stored key with the other's. Storing several providers' keys and swapping which is active is now just independent config edits.

**Dual-account (`/login` personal + work) OAuth failover was investigated and explicitly rejected**, in favor of the above. It would have required reading and independently refreshing a live Claude Code OAuth credential via an undocumented endpoint (`https://platform.claude.com/v1/oauth/token`) — exactly the pattern this README's design principles (and the source blog post) call out as why other third-party tools have been blocked by Anthropic. Not planned.

## Amazon Bedrock notes

Bedrock is supported as an overflow secondary (`--secondary bedrock`), not as a token-shunting worker. Its `model_map` is required: every Claude model needs an entry, and one without it fails the request rather than falling back. What is specific to it:

### Bedrock feature compatibility

Claude Code's Anthropic endpoint can send beta features that a third-party/cloud endpoint may not support. Claude Burst strips only the OAuth-specific `oauth-*` beta value before Bedrock and leaves the remaining Claude Code beta capabilities intact. If Bedrock rejects a feature that Anthropic accepts, the response is returned to Claude Code rather than silently weakening the request.

### Bedrock API key authentication only (as of v0.2.0)

The gateway reads `AWS_BEARER_TOKEN_BEDROCK` and stores it in macOS Keychain. It does not yet implement AWS SSO, role assumption, `awsAuthRefresh`, or SigV4 signing. Those should be added before a large enterprise rollout.

### The Bedrock key is briefly visible in local process listings

`claude-burst keychain-set` passes the key to `/usr/bin/security` as a command-line argument, so it's visible in `ps` output to other local processes for the duration of that one call. Reading it from stdin instead would close this, but `security`'s interactive password prompt doesn't reliably accept piped stdin in non-terminal contexts, so this hasn't been changed yet.

## Keeping Remote Control: transparent intercept mode (optional)

Claude Code disables **Remote Control** whenever `ANTHROPIC_BASE_URL` names a host other
than `api.anthropic.com` — a check on the literal variable value, not on where the traffic
ends up ([docs](https://code.claude.com/docs/en/remote-control.md)). The default `base-url`
mode sets exactly that variable, so enabling the gateway costs you that feature.

`transparent` mode leaves the variable unset and gets into the path at the DNS layer
instead: `/etc/hosts` points the hostname at the gateway, which terminates TLS with a
locally generated CA. Claude Code believes it reached Anthropic directly, so Remote Control
keeps working.

```bash
claude-burst configure --intercept-mode transparent
claude-burst enable        # generates the CA, trusts it, prints the one sudo step left
sudo scripts/transparent-root.sh install
```

Undo, at any time, idempotent, safe even if nothing was installed:

```bash
sudo scripts/transparent-root.sh remove
```

### What it costs

`transparent` is the recommended mode and `base-url` is the fallback. The reason is the first row:
Claude Code turns Remote Control **off** whenever `ANTHROPIC_BASE_URL` names a non-Anthropic host, so
base-url mode buys its simplicity by disabling the feature transparent mode exists to preserve. Choose
base-url when you cannot or would rather not touch system files.

| | `transparent` — recommended | `base-url` — fallback |
|---|---|---|
| Remote Control | **works** | disabled |
| root required | once, for `/etc/hosts` + pf | no |
| certificates | local CA, added to `NODE_EXTRA_CA_CERTS` | none |
| blast radius | every process on the machine | this user's Claude Code |
| guards needed | gateway watchdog + pf redirect guard | gateway watchdog |

Note that the code's own default is still `base-url`: it is what an unconfigured install falls back to,
because it is the only mode that needs no privileges and cannot half-install. That is a safe starting
point, not a recommendation — pick transparent deliberately, from the dashboard or with
`claude-burst configure --intercept-mode transparent`.

The last row is the real trade. While the `/etc/hosts` entry exists, *everything* on the
Mac that talks to that hostname goes through the gateway — so if the gateway is down,
Anthropic is unreachable machine-wide, not just in one session. `transparent-root.sh
install` therefore refuses to run unless `/healthz` answers, and verifies the pf redirect
works *before* touching `/etc/hosts`; `remove` undoes `/etc/hosts` first.

### How it avoids calling itself

Once `/etc/hosts` maps the hostname to the gateway, that mapping applies to the gateway's
own upstream requests too — it would call itself, forever. Go consults `/etc/hosts` in both
its cgo and pure-Go resolver modes, so `PreferGo` does not avoid this. The gateway resolves
the intercepted hostname over DNS-over-HTTPS instead (`intercept.resolver_doh`), which never
consults `/etc/hosts`, and dials the returned address while leaving TLS `ServerName` as the
real hostname so certificate verification is unchanged. Only the intercepted hostname is
treated this way. A DoH answer pointing at loopback is rejected outright — that is the
loop's exact shape. Set `intercept.upstream_addr` to pin an IP where DoH is blocked.

### Checking whether it is working

```bash
claude-burst status                          # CA trusted? hosts entry? certificate?
sudo scripts/transparent-root.sh status      # pf rule actually loaded?
curl -sk https://api.anthropic.com/healthz   # does the REAL path answer from the gateway?
```

Use the last one rather than a direct probe of the gateway port: while the redirect is
installed the gateway port very nearly never accepts direct connections (see Verify above).
The admin dashboard's **Test connection** button runs exactly this check.

If you want to see *why* for yourself, `scripts/diagnose-direct-port.sh` snapshots pf's
loaded rules, states and drop counters around a burst of failing probes and names the
counter that moved. It is read-only — every `pfctl` call is a `-s` show — so it is safe to
run mid-incident:

```bash
sudo scripts/diagnose-direct-port.sh          # writes a timestamped log you can attach to a bug
```

On this machine it shows `state-insert` climbing by ~14 per probe while every filter and
block counter stays at zero: pf is failing to *insert state* for these connections, not
filtering them. See `INVESTIGATION-TLS-STORM.md`.

Watch for `live rdr rule : MISSING` while the hosts entry is present. That is the bad
state — DNS redirects but nothing listens — and the fix is `remove`.

### Guards

Two background jobs keep this working while nobody is watching, and **the dashboard shows whether each
is armed, when it last checked and what it has caught** — with a button to install either:

| Guard | Runs as | Covers |
|---|---|---|
| **Gateway watchdog** (`install-selfheal-watchdog.sh`) | you | the gateway's process dying. Checks it has a *pid*, not merely that launchd still has the job registered — launchd goes on answering for a job whose process has exited |
| **pf redirect guard** (`install-pf-heal.sh`) | root | traffic not reaching the gateway, whatever the cause. Probes the real path end to end rather than any one component |

Both are needed in transparent mode; base-url mode needs only the watchdog.

### Guarding the pf rule

The rdr rule is the one part of this that something else on your Mac can take away.
On 2026-09-07 it vanished from the loaded ruleset while `/etc/hosts` stayed, and every
process on the machine got `connection refused` for `api.anthropic.com` for hours. The
anchor file and the `pf.conf` reference were both still perfectly intact — only the
*loaded* ruleset had lost the rule — so every check that read configuration said OK.
Other pf-owning software (VPN and endpoint-security clients, in this case Zscaler and
CrowdStrike) reloads pf on network change and on wake; a `load anchor` line does not
guarantee the rule stays loaded.

So arm the healer. The dashboard's **pf redirect guard** panel says whether it is armed,
when it last checked, and what it has caught — and arms it for you. `install-proxy.sh`
also does it during a transparent install. By hand:

```bash
sudo scripts/install-pf-heal.sh          # root LaunchDaemon, every 2 minutes
scripts/install-pf-heal.sh status        # armed? what has it caught?
tail -f /var/log/claude-burst-pf.log     # world-readable, no sudo needed
```

Each cycle it does nothing at all unless the hosts block is present *and* the rdr rule
is gone. Then it logs the outage, runs `transparent-root.sh reload-anchor`, and
notifies you. If four consecutive repairs fail it **removes the redirect** and says so:
with it gone, everything reaches Anthropic directly and burst is merely out of the path,
which beats a Mac that cannot reach Anthropic at all.

It needs root — reloading a pf anchor does — which is why it is a LaunchDaemon rather
than part of the existing user-level `self-heal-watchdog.sh`. That watchdog detected this
exact failure three times and could only post a notification.

Drive every branch of its decision tree without root, and without breaking anything:

```bash
scripts/pf-heal.sh --self-test
```

See [ROLLBACK.md](ROLLBACK.md) for undoing every part of this independently.

## Emergency recovery, without this machine's help

Everything the dashboard offers goes through a gateway that is running. When it is
not — the gateway is dead, `127.0.0.1:7788` refuses to connect, and transparent mode's
`/etc/hosts` redirect is still pointing every process on the Mac at nothing — the
dashboard cannot help you, and neither can any button on it.

That case is why the whole recovery chain is committed here and nothing in it needs
this project to be installed, built, or working:

```bash
git clone https://github.com/andrewbakercloudscale/claude-burst.git
cd claude-burst
./scripts/rollback.sh          # asks for sudo; undoes every part, verifies the result
```

`rollback.sh` removes the `/etc/hosts` redirect and pf rule first (widest blast radius
first), then the System-keychain CA trust, then restores `settings.json`, `config.json`
and the CA bundle from the latest backup, stops the gateway, and **verifies
`api.anthropic.com` resolves off-box before it claims success**. It is idempotent: safe
when nothing was installed, when half an install succeeded, and twice in a row.

If you cannot even clone, the two commands that matter are short enough to type:

```bash
sudo sed -i '' '/# BEGIN claude-burst hosts/,/# END claude-burst hosts/d' /etc/hosts
sudo dscacheutil -flushcache
```

That alone restores Anthropic access machine-wide; everything else is tidy-up.

## Forcing the secondary, and the admin UI

A subscription primary only fails over on genuine exhaustion signals, which cannot be
provoked on demand — so the secondary path stays unexercised until the day it is needed,
which is the worst possible moment to find out it is misconfigured. To exercise it:

```bash
claude-burst force-secondary --minutes 15   # inference goes to the secondary
claude-burst reset                          # back to the primary immediately
```

The forced state is recorded with `limit_claim: "forced"`, so neither the metrics nor
`status` ever imply Anthropic reported a limit it did not.

A local control panel runs alongside the gateway on `127.0.0.1:7788` (disable with
`claude-burst configure --admin-listen off`). It shows routing state, usage, the last 50
requests, and the last 20 upstream responses **with their headers** — the
`anthropic-ratelimit-*` ones are what actually decide failover, so overflow behaviour
becomes debuggable rather than mysterious. It also says whether burst is in the path at
all — and **Test connection** proves it live rather than reading config off disk, which is
not the same question.

What the buttons do: force or clear overflow; change the secondary model, failover strategy
and intercept mode; restart the gateway (config is only read at startup); install either
mode; arm either guard and read its log; read the gateway's own log. Anything needing root
or affecting the whole machine is **not** done in-process — the button writes a script and
opens it in Terminal, where you answer the sudo prompt and watch every command. That
includes **Revert**, which runs `scripts/rollback.sh`: it undoes the machine-wide redirect
too, not just Claude Code's endpoint, because a revert button that cannot undo the widest
change it is offered for is not a revert button.

It binds loopback and has no login, which is not by itself safe: a malicious page can point
a hostname it controls at `127.0.0.1` and drive the UI from your own browser. Two defences
apply to every request — the `Host` header must name loopback, and mutating requests must
carry a custom header, which forces a CORS preflight the server never answers. Response
headers are held in memory only and filtered through an allowlist; request rows are
metadata only, never prompt or response content.

### A friendlier admin URL

```bash
sudo scripts/transparent-root.sh admin-host cloudscale-claudeburst.test
claude-burst configure --admin-hostname cloudscale-claudeburst.test
launchctl kickstart -k gui/$UID/ninja.andrewbaker.claude-burst
```

Then the panel is at <http://cloudscale-claudeburst.test:7788>. Use a **dotted** name —
browsers treat a single-label name as a search term, and `.test` is reserved by RFC 6761 so
it can never collide with a real domain. Undo with
`sudo scripts/transparent-root.sh admin-host-remove` and
`claude-burst configure --admin-hostname off`.

This is a separate `/etc/hosts` block from transparent mode's, so removing one never
disturbs the other.

Be aware of the trade. The `Host` header check is a DNS-rebinding defence, and a hostname a
hostile page can guess and navigate to weakens it. What still holds: mutating requests
require a custom header, so a cross-origin page needs a preflight this server never answers,
and no CORS headers are ever returned, so responses cannot be read cross-origin. A hostile
page could therefore fire read-only requests but not see the answers.

## Testing

```bash
go test ./... -race
go vet ./...
```

CI runs both on every push and pull request (see `.github/workflows/test.yml`).

The tests include a simulated Anthropic subscription rejection that verifies the same request is replayed to the secondary, the model is remapped, the OAuth beta is removed from a Bedrock call, and the overflow reset state is persisted (the suite covers both Bedrock and the OpenAI-compatible translator), plus an equivalent suite for the metered-failures strategy (sustained-failure threshold, window expiry, success reset, and the no-subscription primary forwarding its own auth header unchanged).

Token shunting has its own tests, including an end-to-end one that builds the binary and drives it through the hook protocol — see [How it is tested](#token-shunting-keep-the-boring-work-out-of-claudes-context).

## Uninstall

```bash
./install.sh uninstall
```

This removes the LaunchAgent and the binary and intentionally keeps metrics, configuration and the Keychain secret so a rerun of the installer does not silently wipe them.

## Terms and design notes

Anthropic's current Consumer Terms prohibit account sharing and prohibit bypassing protective measures. This project is designed around one subscription account per user and treats Anthropic's quota rejection as final for that subscription window. It then makes a separate, paid request through Amazon Bedrock rather than trying to defeat the Max limit.

Anthropic also documents the concept of switching Max users to metered API credits after included usage is exhausted. Claude Burst applies the same base-plus-overflow idea to an AWS Bedrock channel controlled by the organization. This is a technical interpretation, not legal advice, and an enterprise deployment should still be reviewed against the contracts your organization has actually signed.

## Official references

- Anthropic Max plan: https://support.claude.com/en/articles/11049741-what-is-the-max-plan
- Claude Code with Pro/Max: https://support.claude.com/en/articles/11145838-use-claude-code-with-your-pro-or-max-plan
- Claude pricing: https://claude.com/pricing
- Claude Code LLM gateways: https://code.claude.com/docs/en/llm-gateway
- Gateway protocol: https://code.claude.com/docs/en/llm-gateway-protocol
- Claude Code errors and usage limits: https://code.claude.com/docs/en/errors
- Anthropic Consumer Terms: https://www.anthropic.com/legal/consumer-terms
- Claude Code on Amazon Bedrock: https://code.claude.com/docs/en/amazon-bedrock
- Amazon Bedrock Anthropic Messages API: https://docs.aws.amazon.com/bedrock/latest/userguide/inference-messages-api.html
- Amazon Bedrock API keys: https://docs.aws.amazon.com/bedrock/latest/userguide/api-keys.html

## License

MIT. See `LICENSE`.
