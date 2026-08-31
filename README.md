# Claude Burst

**Claude Max as included capacity. A metered secondary as paid overflow.**

Claude Burst is a Mac-only local gateway for Claude Code. It keeps your normal Claude Pro/Max subscription login as the primary credential, observes Anthropic's authoritative subscription rate-limit headers, and only switches inference to a configured secondary when Anthropic says the subscription allowance is actually exhausted. The secondary can be Amazon Bedrock, any OpenAI-compatible chat-completions endpoint (e.g. Together AI serving GLM — see [OpenAI-compatible secondary](#openai-compatible-secondary-together-ai-openrouter-or-any-endpoint) below), or a direct Anthropic API key. When the reset timestamp arrives, it automatically returns to the subscription.

This is an experimental MVP. Test it on a non-critical development account before any broader rollout.

**No Claude subscription?** Claude Burst also supports a direct, metered Anthropic API key as the primary route instead of subscription passthrough (see [No-subscription setup](#no-subscription-setup-metered-api-key-primary) below). In that mode there's no included allowance to burst from, so failover to the secondary is triggered by sustained failures instead of subscription-exhaustion headers — both routes are metered, so a single transient error doesn't flip traffic to a second paid provider.

## Supported providers

Primary and secondary are independent, pluggable slots (`internal/router/provider.go`) — nothing here is tied to one vendor:

| Slot | Options |
| --- | --- |
| **Primary** | `oauth-passthrough` — your existing Claude Pro/Max subscription login (the default, and the setup this whole README describes first) · `anthropic-api-key` — a direct, metered Anthropic API key, for accounts with no subscription |
| **Secondary** | `bedrock` — Amazon Bedrock · `openai-compatible` — any OpenAI-compatible chat-completions endpoint, worked examples below for Together AI/GLM and OpenRouter · `none` — disable overflow entirely |

See [OpenAI-compatible secondary](#openai-compatible-secondary-together-ai-openrouter-or-any-endpoint) below for the openai-compatible worked examples, and [Configuration](#configuration) for every field.

**Keeping Remote Control.** Pointing Claude Code at any local gateway normally costs you its Remote Control feature — Claude Code disables Remote Control the moment `ANTHROPIC_BASE_URL` names anything other than `api.anthropic.com`, and the default setup below sets exactly that variable. Claude Burst's `transparent` intercept mode solves this by never touching `ANTHROPIC_BASE_URL` at all: instead of using that config mechanism, it gets into the path a level lower, at DNS, so Claude Code's own settings never change and it believes it is still talking to `api.anthropic.com` directly. See [Keeping Remote Control: transparent intercept mode](#keeping-remote-control-transparent-intercept-mode-optional) below.

## Why this exists

Anthropic exposes materially different commercial models for access to the same Claude model families:

- Claude Max is a fixed monthly subscription with rolling usage limits.
- Claude API is metered by token.
- Amazon Bedrock, and OpenAI-compatible inference providers such as Together AI, offer metered access to Claude-family or comparable models outside Anthropic's own billing.

Anthropic's Claude Code gateway documentation explicitly supports `ANTHROPIC_BASE_URL` with an existing claude.ai subscription login. Setting only the base URL keeps the subscription credential active and the subscription's usage limits and billing continue to apply. Claude Burst uses that supported gateway mechanism and respects the subscription limit rather than trying to evade it.

## Routing behaviour

1. Claude Code sends `/v1/messages` to `http://127.0.0.1:7777`.
2. Claude Burst forwards the request to `https://api.anthropic.com` unchanged, including the user's saved Claude subscription OAuth credential and required beta headers.
3. Successful responses stream straight back to Claude Code.
4. Generic `429` responses do **not** trigger overflow.
5. Overflow activates only when Anthropic's subscription headers indicate a rejected unified limit, for example `anthropic-ratelimit-unified-status: rejected`, or when an explicit subscription-limit error is returned.
6. Claude Burst reads Anthropic's reset timestamp and persists it locally.
7. The rejected request is replayed to the configured secondary — Amazon Bedrock's Anthropic Messages endpoint, or an OpenAI-compatible endpoint such as Together AI or OpenRouter — using a credential stored in macOS Keychain.
8. Future inference requests use the secondary until the reset time plus a small safety grace period.
9. The first request after that time goes back to Anthropic Max automatically.

Claude Burst does not rotate Max accounts, suppress quota signals, fabricate headers, or attempt to extend the Max allowance. The subscription limit remains authoritative.

## What is logged

Claude Burst writes to two files under `~/.config/claude-burst/`, and both are metadata-only: **prompts, source code, tool inputs and model outputs are never written to disk.** The proxy necessarily handles the request body in memory so it can replay a rejected request to Bedrock, but it does not persist it.

### `metrics.jsonl` — structured, one line per request

(Also note the model fields: `model` is what **served** the request and drives cost;
`requested_model` is what Claude Code asked for. They differ exactly when a remapping
provider — Bedrock's modelMap, or openai-compatible fixed/consistent failover — was
involved. Pricing uses the served model, so a request GLM served is never costed at
Claude Opus rates.)

- timestamp and a short request id (also present in `claude-burst.log`, so a line in one file can always be matched to the other)
- Claude Code session and agent identifiers
- selected route (`anthropic` or `bedrock`)
- model
- HTTP status
- latency
- input/output token usage where exposed in the SSE stream
- estimated API-equivalent cost using the prices in `config.json`
- subscription limit claim and reset timestamp when failover occurs
- a short note on what happened (e.g. "subscription limit detected; request replayed to Bedrock", "keychain load failed: ...")

### `claude-burst.log` — plain text, for debugging

Every request gets a `start` line and a matching `done` line (same request id, final HTTP status, duration), so you can always see what the gateway did even for requests that never made it into `metrics.jsonl`. In addition, every failure path logs which stage it failed at and why:

- reading or size-limiting the request body
- building the outbound request to Anthropic or Bedrock
- the upstream HTTP call itself (network errors)
- loading the Bedrock key from the environment/Keychain
- mapping a model name for Bedrock
- reading or writing the local overflow-state file
- writing a metrics event
- a panic anywhere in the request path — recovered, logged with a stack trace, and turned into a `500` instead of crashing the gateway or hanging Claude Code's connection

A metrics-write failure (disk full, permissions, etc.) is logged but never fails the request itself — by the time metrics would be written, Claude Code has already been served.

## Requirements

- macOS on Apple Silicon or Intel
- Go 1.23+ (there's no prebuilt binary in the repo; `install.sh` builds one locally)
- Either: Claude Code already installed and logged into the intended Pro/Max account (subscription mode), **or** a metered Anthropic API key (no-subscription mode)
- A credential for whichever secondary you pick: Amazon Bedrock access plus a Bedrock API key in `AWS_BEARER_TOKEN_BEDROCK`, **or** an API key for an OpenAI-compatible endpoint such as Together AI (see [OpenAI-compatible secondary](#openai-compatible-secondary-together-ai-openrouter-or-any-endpoint) below), **or** none at all if you're running with `--secondary none`

The secondary is a pluggable slot (`internal/router/provider.go`), not a hardcoded vendor. The install example below uses Bedrock because it needs the fewest moving parts to try first — Bedrock and the two Anthropic-passthrough providers all speak Anthropic's Messages format natively — but it is one option, not a requirement.

## Install

```bash
git clone https://github.com/andrewbakercloudscale/claude-burst.git
cd claude-burst

export AWS_REGION=us-east-1
export AWS_BEARER_TOKEN_BEDROCK='your-bedrock-api-key'

./install.sh
```

Using Together AI / GLM (or another OpenAI-compatible endpoint) as the secondary instead? Skip the `AWS_*` exports above and see [OpenAI-compatible secondary](#openai-compatible-secondary-together-ai-openrouter-or-any-endpoint) below for the equivalent quickstart.

The installer:

- builds `claude-burst` locally (`go build`) and installs it into `~/.local/bin/claude-burst`
- stores the secondary's credential in macOS Keychain when one is present (`AWS_BEARER_TOKEN_BEDROCK` for Bedrock; `claude-burst keychain-set --provider <name>` for others, see [Commands](#commands))
- writes the initial configuration
- updates `~/.claude/settings.json` with only `ANTHROPIC_BASE_URL=http://127.0.0.1:7777`
- does **not** add an Anthropic credential of its own — in subscription mode this keeps the saved Max login active; in no-subscription mode, Claude Code's own `ANTHROPIC_API_KEY` (set separately, see below) is what gets forwarded
- creates and starts a macOS LaunchAgent
- adds `~/.local/bin` to `~/.zprofile` if required

Restart Claude Code after installation.

### No-subscription setup (metered API key primary)

If you don't have a Claude Pro/Max subscription, use a direct Anthropic API key as the primary route instead of subscription passthrough:

```bash
export AWS_REGION=us-east-1
export AWS_BEARER_TOKEN_BEDROCK='your-bedrock-api-key'

./install.sh
claude-burst keychain-set
claude-burst configure --primary anthropic-api-key --secondary bedrock --region us-east-1
claude-burst enable
```

Then set `ANTHROPIC_API_KEY` in Claude Code's own settings env (e.g. the `env` block in `~/.claude/settings.json`, alongside `ANTHROPIC_BASE_URL`) — **not** in claude-burst's config. The gateway never stores or injects an Anthropic credential itself; it only forwards whatever auth header Claude Code already sent, exactly like subscription mode does with the OAuth header.

In this mode, both the primary (metered Anthropic API) and the secondary (Bedrock) cost money per token, so failover isn't triggered by a single rate-limit response — see [`metered_failover`](#configuration) below.

## Verify

```bash
claude-burst status
curl -s http://127.0.0.1:7777/healthz
claude-burst stats --days 30
```

You can also check Claude Code's own `/status` and `/usage` views to confirm that it is still using the intended subscription before a failover occurs.

## Commands

```text
claude-burst serve
claude-burst configure --region us-east-1
claude-burst configure --primary anthropic-api-key --secondary bedrock
claude-burst configure --secondary none
claude-burst keychain-set                    # Bedrock: reads AWS_BEARER_TOKEN_BEDROCK
claude-burst keychain-set --provider together # OpenAI-compatible: reads TOGETHER_API_KEY
claude-burst enable
claude-burst disable
claude-burst status
claude-burst reset                           # back to primary now
claude-burst force-secondary --minutes 15    # route to the secondary on purpose (testing)
claude-burst stats --days 30
claude-burst version
```

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
    "provider": "bedrock",
    "base_url": "https://bedrock-runtime.us-east-1.amazonaws.com/anthropic",
    "keychain_service": "claude-burst-bedrock",
    "model_map": {
      "claude-sonnet-5": "global.anthropic.claude-sonnet-5",
      "claude-opus-5": "global.anthropic.claude-opus-5"
    }
  },
  "metered_failover": {
    "window_seconds": 60,
    "min_failures": 3
  }
}
```

- `primary.provider` / `secondary.provider`: `oauth-passthrough` (subscription OAuth passthrough), `anthropic-api-key` (metered, no-subscription), or `bedrock`. Neither slot is tied to a specific vendor — either can hold either provider.
- `primary.failover_strategy`: `subscription-limit` (only Anthropic's own subscription-exhaustion headers trigger failover — a bare 429 never does), `metered-failures` (a sliding-window failure count triggers failover, since every route is metered and a single blip shouldn't move traffic), `subscription-limit+metered-failures` (both: genuine subscription exhaustion fails over immediately as above, *and* a sustained run of 429/5xx responses or transport errors/timeouts — an Anthropic outage, not plan exhaustion — fails over once `metered_failover.min_failures` are seen inside the window), or `none` (never fail over). A subscription (`oauth-passthrough`) primary defaults to `subscription-limit` alone, which by design does **not** react to a bare 500 or a timeout — set `subscription-limit+metered-failures` (`claude-burst configure --failover-strategy subscription-limit+metered-failures`) if you also want overflow on an Anthropic outage.
- `metered_failover.window_seconds` / `min_failures`: for `metered-failures`, how many upstream failures (429/5xx/transport errors) inside a trailing window before failing over. Other 4xx errors (bad key, malformed request) never count — routing to the secondary wouldn't fix them.
- `response_header_timeout_seconds`: bounds how long the gateway waits for a response to *start* before treating the upstream as failed (doesn't affect how long an already-started stream can run).

Model IDs change over time. Keep `model_map` aligned with the Claude models enabled in your Bedrock account.

## Important limitations

### 1. Bedrock feature compatibility

Claude Code's Anthropic endpoint can send beta features that a third-party/cloud endpoint may not support. Claude Burst strips only the OAuth-specific `oauth-*` beta value before Bedrock and leaves the remaining Claude Code beta capabilities intact. If Bedrock rejects a feature that Anthropic accepts, the response is returned to Claude Code rather than silently weakening the request.

### 2. Bedrock API key authentication only in v0.1

The first release reads `AWS_BEARER_TOKEN_BEDROCK` and stores it in macOS Keychain. It does not yet implement AWS SSO, role assumption, `awsAuthRefresh`, or SigV4 signing. Those should be added before a large enterprise rollout.

### 3. No failover on ordinary throttling

Anthropic uses HTTP 429 for several different conditions. Claude Burst deliberately refuses to interpret a bare 429 as Max exhaustion. This avoids turning a temporary capacity throttle into unexpected Bedrock spend.

### 4. API-equivalent cost is not Anthropic's internal cost

The metrics estimate answers: "What would these observed input/output tokens cost at the configured public API rates?" It does not estimate Anthropic's marginal inference cost, gross margin, internal transfer pricing, or the economic value of prompt caching unless you extend the metric model to account for cache buckets.

### 5. Consumer versus commercial governance remains different

A local data-loss-prevention layer can reduce what leaves the machine, but it does not make a consumer Max account contractually or operationally identical to Claude for Work, the Claude API, or Bedrock. Review your organization's legal, procurement, retention, audit and account-management requirements before rolling consumer subscriptions out to employees.

### 6. No caller authentication on the local gateway

Any local process — including a browser tab, since `POST /v1/messages` with a simple content type needs no CORS preflight — can send the gateway a request. It cannot force an overflow window open (only genuine subscription-limit/sustained-failure signals do that), but it can ride an already-open one, and it can drive ordinary (non-overflow) traffic through your credential. Don't bind `listen` to anything but `127.0.0.1`.

### 7. The Bedrock key is briefly visible in local process listings

`claude-burst keychain-set` passes the key to `/usr/bin/security` as a command-line argument, so it's visible in `ps` output to other local processes for the duration of that one call. Reading it from stdin instead would close this, but `security`'s interactive password prompt doesn't reliably accept piped stdin in non-terminal contexts, so this hasn't been changed yet.

### 8. Upstream error text (including the request path/query) is logged and metered failure detail is not size-bounded

Transport-error and non-failover-error log lines include the upstream `error.Error()` string, which can contain the request URL (path and query, not host credentials — Go's `url.Error` redacts userinfo). Prompts and response bodies are never included per the metadata-only design, but treat `claude-burst.log` as containing request metadata, not as fully opaque.

### 9. Transparent intercept mode is machine-wide, and TLS interception is assumed benign

The `/etc/hosts` entry transparent mode installs affects every process on the Mac, not just
Claude Code — see the trade-off table above. Separately, the design assumes that TLS
interception does not itself break Remote Control. That is well supported (Claude Code is
widely run behind corporate inspecting proxies, and documents `NODE_EXTRA_CA_CERTS` for
exactly that) but is not something this project can prove. `scripts/check-interception.sh`
settles it on a network that actually inspects TLS: it distinguishes *intercepted* from
*bypassed* from *not enrolled*, which a bare certificate-issuer check cannot.

## OpenAI-compatible secondary (Together AI, OpenRouter, or any endpoint)

A secondary can be any OpenAI-compatible chat-completions endpoint instead of Bedrock — not just one named vendor. `provider: "openai-compatible"` plus a `base_url` and `model` is the entire integration surface; nothing about the vendor is hardcoded anywhere in the request path. Two are shown below as concrete examples — Together AI serving GLM, and OpenRouter, which fronts many different model providers behind one OpenAI-compatible API — but the same `base_url`/`model` shape works for any other OpenAI-compatible endpoint too. Unlike `bedrock` and the two Anthropic-passthrough providers, which all speak Anthropic's Messages wire format natively and only need `Server.relay` to stream the response back byte-for-byte, this provider (`internal/router/provider_openai.go`) does real bidirectional translation: request body shape (`system`/`messages`/`tools`, including splitting Anthropic's nested `tool_result` blocks into OpenAI's sibling `tool` messages), non-streaming and **streaming** response shape (OpenAI's `delta`-based SSE chunks translated live into Anthropic's `message_start`/`content_block_start`/`content_block_delta`/`content_block_stop`/`message_delta`/`message_stop` event sequence, including parallel tool calls), and tool-call schema (`tool_use` blocks ↔ `tool_calls`). Verified against a real Together AI + GLM 5.3 endpoint, including a genuine streaming tool call.

Not translated (dropped, not an error): images/documents in message content, Anthropic extended-thinking (`thinking`/`redacted_thinking`) blocks in history, and prompt-caching `cache_control` hints — none have a meaningful equivalent on a generic OpenAI-compatible endpoint, and Claude Code's ordinary coding-agent traffic is overwhelmingly text + tool-use.

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

`claude-burst keychain-set --provider <label>` stores whatever `<label>_API_KEY` is set in the environment (uppercased, hyphens become underscores) into a macOS Keychain service named `claude-burst-<label>` by default — `--provider together` reads `TOGETHER_API_KEY` into `claude-burst-together`, `--provider openrouter` reads `OPENROUTER_API_KEY` into `claude-burst-openrouter`, and so on for any other vendor. Nothing here is a hardcoded allowlist; `<label>` can be anything. At request time, the gateway derives the same identity back out of whichever keychain service `secondary.keychain_service` actually names, so the two directions always agree without a second place to keep in sync (`internal/router.EnvVarForProvider` / `openAICompatibleIdentity`). Use `--secondary-keychain-service` on `configure` if you want a service name other than the `claude-burst-<label>` default (for example, to run two different OpenAI-compatible secondaries side by side under distinct names).

**Dual-account (`/login` personal + work) OAuth failover was investigated and explicitly rejected**, in favor of the above. It would have required reading and independently refreshing a live Claude Code OAuth credential via an undocumented endpoint (`https://platform.claude.com/v1/oauth/token`) — exactly the pattern this README's design principles (and the source blog post) call out as why other third-party tools have been blocked by Anthropic. Not planned.

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

| | `base-url` (default) | `transparent` |
|---|---|---|
| Remote Control | disabled | works |
| root required | no | once, for `/etc/hosts` + pf |
| certificates | none | local CA, added to `NODE_EXTRA_CA_CERTS` |
| blast radius | this user's Claude Code | every process on the machine |

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
```

Watch for `live rdr rule : MISSING` while the hosts entry is present. That is the bad
state — DNS redirects but nothing listens — and the fix is `remove`.

See [ROLLBACK.md](ROLLBACK.md) for undoing every part of this independently.

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
becomes debuggable rather than mysterious. It can also force/clear overflow, change the
secondary model and failover strategy, and revert Claude Code to the stock endpoint.

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

The tests include a simulated Anthropic subscription rejection that verifies the same request is replayed to Bedrock, the model is remapped, the OAuth beta is removed from the Bedrock call, and the overflow reset state is persisted, plus an equivalent suite for the metered-failures strategy (sustained-failure threshold, window expiry, success reset, and the no-subscription primary forwarding its own auth header unchanged).

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
