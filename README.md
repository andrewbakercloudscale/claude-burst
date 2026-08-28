# Claude Burst

**Claude Max as included capacity. Amazon Bedrock as paid overflow.**

Claude Burst is a Mac-only local gateway for Claude Code. It keeps your normal Claude Pro/Max subscription login as the primary credential, observes Anthropic's authoritative subscription rate-limit headers, and only switches inference to Amazon Bedrock when Anthropic says the subscription allowance is actually exhausted. When the reset timestamp arrives, it automatically returns to the subscription.

This is an experimental MVP. Test it on a non-critical development account before any broader rollout.

## Why this exists

Anthropic exposes materially different commercial models for access to the same Claude model families:

- Claude Max is a fixed monthly subscription with rolling usage limits.
- Claude API is metered by token.
- Amazon Bedrock provides metered access to Claude through AWS.

Anthropic's Claude Code gateway documentation explicitly supports `ANTHROPIC_BASE_URL` with an existing claude.ai subscription login. Setting only the base URL keeps the subscription credential active and the subscription's usage limits and billing continue to apply. Claude Burst uses that supported gateway mechanism and respects the subscription limit rather than trying to evade it.

## Routing behaviour

1. Claude Code sends `/v1/messages` to `http://127.0.0.1:7777`.
2. Claude Burst forwards the request to `https://api.anthropic.com` unchanged, including the user's saved Claude subscription OAuth credential and required beta headers.
3. Successful responses stream straight back to Claude Code.
4. Generic `429` responses do **not** trigger overflow.
5. Overflow activates only when Anthropic's subscription headers indicate a rejected unified limit, for example `anthropic-ratelimit-unified-status: rejected`, or when an explicit subscription-limit error is returned.
6. Claude Burst reads Anthropic's reset timestamp and persists it locally.
7. The rejected request is replayed to the Amazon Bedrock Anthropic Messages endpoint using a Bedrock API key stored in macOS Keychain.
8. Future inference requests use Bedrock until the reset time plus a small safety grace period.
9. The first request after that time goes back to Anthropic Max automatically.

Claude Burst does not rotate Max accounts, suppress quota signals, fabricate headers, or attempt to extend the Max allowance. The subscription limit remains authoritative.

## What is logged

Claude Burst writes to two files under `~/.config/claude-burst/`, and both are metadata-only: **prompts, source code, tool inputs and model outputs are never written to disk.** The proxy necessarily handles the request body in memory so it can replay a rejected request to Bedrock, but it does not persist it.

### `metrics.jsonl` — structured, one line per request

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
- Claude Code already installed and logged into the intended Pro/Max account
- Amazon Bedrock access to the Claude models you want to use
- A Bedrock API key in `AWS_BEARER_TOKEN_BEDROCK`

The MVP uses Amazon Bedrock's Anthropic-compatible Messages API with a Bedrock API key because that preserves the Anthropic request/stream format and keeps the router small. IAM/SigV4/SSO credential support is a sensible next step.

## Install

```bash
unzip claude-burst.zip
cd claude-burst

export AWS_REGION=us-east-1
export AWS_BEARER_TOKEN_BEDROCK='your-bedrock-api-key'

./install.sh
```

The installer:

- installs the correct prebuilt Mac binary into `~/.local/bin/claude-burst`
- stores the Bedrock key in macOS Keychain when `AWS_BEARER_TOKEN_BEDROCK` is present
- writes the initial configuration
- updates `~/.claude/settings.json` with only `ANTHROPIC_BASE_URL=http://127.0.0.1:7777`
- does **not** add an Anthropic API key or gateway credential, so the saved Max login stays active
- creates and starts a macOS LaunchAgent
- adds `~/.local/bin` to `~/.zprofile` if required

Restart Claude Code after installation.

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
claude-burst keychain-set
claude-burst enable
claude-burst disable
claude-burst status
claude-burst reset
claude-burst stats --days 30
claude-burst version
```

## Configuration

Configuration lives at `~/.config/claude-burst/config.json`.

The important fields are:

```json
{
  "listen": "127.0.0.1:7777",
  "anthropic_base_url": "https://api.anthropic.com",
  "bedrock_base_url": "https://bedrock-runtime.us-east-1.amazonaws.com/anthropic",
  "reset_grace_seconds": 10,
  "unknown_reset_seconds": 300,
  "max_request_mb": 128,
  "model_map": {
    "claude-sonnet-5": "global.anthropic.claude-sonnet-5",
    "claude-opus-5": "global.anthropic.claude-opus-5"
  }
}
```

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

## Testing

```bash
go test ./...
go vet ./...
```

The tests include a simulated Anthropic subscription rejection that verifies the same request is replayed to Bedrock, the model is remapped, the OAuth beta is removed from the Bedrock call, and the overflow reset state is persisted.

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
