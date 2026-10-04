# Providers: primary and secondary

[Back to the README](../README.md)

Primary and secondary are independent, pluggable slots (`internal/router/provider.go`), nothing here is tied to one vendor:

| Slot | Options |
| --- | --- |
| **Primary** | `oauth-passthrough` - your existing Claude Pro/Max subscription login (the default) · `anthropic-api-key` - a direct, metered Anthropic API key, for accounts with no subscription |
| **Secondary** | `openai-compatible` - **Together AI** (the worked example, serving GLM), OpenRouter, or any other OpenAI-compatible chat-completions endpoint · `bedrock` - Amazon Bedrock · `none` - disable overflow entirely |

See [Together AI, OpenRouter or any OpenAI-compatible secondary](#together-ai-openrouter-or-any-openai-compatible-secondary) for the worked examples, and [Configuration](configuration.md) for every field.

| Secondary | Credential | Provider setting | Overflow |
|---|---|---|:---:|
| **Together AI** (worked example) | a Together API key | `--secondary openai-compatible` | yes |
| **OpenRouter** | an OpenRouter API key | `--secondary openai-compatible` | yes |
| **Amazon Bedrock** | Bedrock access + an API key in `AWS_BEARER_TOKEN_BEDROCK` | `--secondary bedrock` | yes |
| *(none)* | - | `--secondary none` | no |

The two families differ in one way worth knowing up front. Bedrock speaks Anthropic's Messages wire format natively, so its responses are relayed byte-for-byte, while Together AI and OpenRouter go through the OpenAI-compatible translator (`internal/router/provider_openai.go`) in both directions, streaming included, both paths are tested. Pick on price, model availability and who you would rather have a billing relationship with.

## Enterprise: a gateway such as Portkey as the primary

On a company machine Claude Code often already goes through a gateway:
`ANTHROPIC_BASE_URL` points at it (Portkey, for example) and
`ANTHROPIC_CUSTOM_HEADERS` carries its routing headers. Such a machine usually
has no secondary, and needs none: the secondary is optional.

- Use base-url mode (`install.sh` picks it for you here). `enable` adopts the gateway: its URL becomes
  `primary.base_url`, Claude Code is pointed at Claude Burst, and every header
  Claude Code sends, its credential and the custom headers included, is passed
  to the gateway unchanged.
- `disable`, the dashboard's Revert and `scripts/rollback.sh` put the original
  URL back, so the machine is left as it was found.
- Transparent mode refuses: Claude Code never contacts `api.anthropic.com`
  there, so a redirect would catch nothing.
- If the URL is set in managed settings
  (`/Library/Application Support/ClaudeCode/managed-settings.json`), `enable`
  refuses: those override `~/.claude/settings.json`, so Claude Burst cannot sit
  in front.
- Outbound, the gateway honours `HTTPS_PROXY` (unless it points at this Mac)
  and trusts the certificates in the `NODE_EXTRA_CA_CERTS` bundle, so an
  internal gateway signed by a company CA is reachable.

## Setting up a secondary

Run `./install.sh`, then point the secondary at your provider (or leave it unset for a single plan):

```bash
# Together AI (recommended)
./install.sh
claude-burst configure --secondary openai-compatible \
  --secondary-base-url https://api.together.xyz/v1 \
  --secondary-model zai-org/GLM-5.3
TOGETHER_API_KEY='your-key' claude-burst keychain-set --provider together
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
# Amazon Bedrock
export AWS_REGION=us-east-1
export AWS_BEARER_TOKEN_BEDROCK='your-bedrock-api-key'
./install.sh
```

**Why the Together and OpenRouter blocks run `configure` after the installer.** `install.sh` picks no secondary for you. The only key it stores itself is a Bedrock one, and only when `AWS_BEARER_TOKEN_BEDROCK` is set; that is a leftover of the project's first version, not a recommendation. Until you run `configure --secondary openai-compatible` (or pick a secondary on the dashboard), Burst runs on your single plan and overflow has nowhere to go.

See [Together AI, OpenRouter or any OpenAI-compatible secondary](#together-ai-openrouter-or-any-openai-compatible-secondary) for the full detail, including what the translator does.

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

Then set `ANTHROPIC_API_KEY` in Claude Code's own settings env (e.g. the `env` block in `~/.claude/settings.json`, alongside `ANTHROPIC_BASE_URL`), **not** in claude-burst's config. The gateway never stores or injects an Anthropic credential itself; it only forwards whatever auth header Claude Code already sent, exactly like subscription mode does with the OAuth header.

In this mode, both the primary (metered Anthropic API) and the secondary (Together AI) cost money per token, so failover isn't triggered by a single rate-limit response, see [`metered_failover`](configuration.md#an-example-and-the-routing-keys-in-detail). (Amazon Bedrock works here too: `--secondary bedrock --region us-east-1` with `AWS_BEARER_TOKEN_BEDROCK` stored via `claude-burst keychain-set`.)

## Together AI, OpenRouter or any OpenAI-compatible secondary

A secondary can be any OpenAI-compatible chat-completions endpoint, not one named vendor. **Together AI serving GLM 5.3 is the one this project runs and has verified live**, including a genuine streaming tool call. `provider: "openai-compatible"` plus a `base_url` and `model` is the entire integration surface; nothing about the vendor is hardcoded anywhere in the request path. Together AI and OpenRouter (which fronts many different model providers behind one OpenAI-compatible API) are shown below, but the same `base_url`/`model` shape works for any other OpenAI-compatible endpoint. Unlike `bedrock` and the two Anthropic-passthrough providers, which all speak Anthropic's Messages wire format natively and only need `Server.relay` to stream the response back byte-for-byte, this provider (`internal/router/provider_openai.go`) does real bidirectional translation: request body shape (`system`/`messages`/`tools`, including splitting Anthropic's nested `tool_result` blocks into OpenAI's sibling `tool` messages), non-streaming and **streaming** response shape (OpenAI's `delta`-based SSE chunks translated live into Anthropic's `message_start`/`content_block_start`/`content_block_delta`/`content_block_stop`/`message_delta`/`message_stop` event sequence, including parallel tool calls), and tool-call schema (`tool_use` blocks ↔ `tool_calls`).

Not translated (dropped, not an error): Anthropic's server-side tools (`web_search_20250305` and friends, declared with a name and a `type` but no `input_schema`, because Anthropic's own API runs them; a logged line names any that were dropped), images/documents in message content, Anthropic extended-thinking (`thinking`/`redacted_thinking`) blocks in history, and prompt-caching `cache_control` hints, none have a meaningful equivalent on a generic OpenAI-compatible endpoint, and Claude Code's ordinary coding-agent traffic is overwhelmingly text + tool-use.

Configure it as `secondary` in `config.json`. Two failover modes, chosen just by whether `model_map` is present:

**Fixed failover**, every Claude model (sonnet, opus, haiku) fails over to the same target model:
```json
{
  "secondary": {
    "provider": "openai-compatible",
    "base_url": "https://api.together.xyz/v1",
    "model": "zai-org/GLM-5.3"
  }
}
```

**Consistent failover**, each Claude model can fail over to a *different* target (e.g. opus to a stronger/pricier model, sonnet/haiku to a cheaper one), via `model_map`. `model` is still required as the fallback target for any Claude model with no explicit entry:
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
Unlike Bedrock's `model_map` (which errors on a Claude model with no entry), an unmapped model here silently falls back to `model` rather than failing the request, there's always a usable target.

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

`claude-burst keychain-set --provider <label>` stores whatever `<label>_API_KEY` is set in the environment (uppercased, hyphens become underscores) into a macOS Keychain service named `claude-burst-<label>` by default, `--provider together` reads `TOGETHER_API_KEY` into `claude-burst-together`, `--provider openrouter` reads `OPENROUTER_API_KEY` into `claude-burst-openrouter`, and so on for any other vendor. Nothing here is a hardcoded allowlist; `<label>` can be anything. At request time, the gateway derives the same identity back out of whichever keychain service `secondary.keychain_service` actually names, so the two directions always agree without a second place to keep in sync (`internal/router.EnvVarForProvider` / `openAICompatibleIdentity`). Use `--secondary-keychain-service` on `configure` if you want a service name other than the `claude-burst-<label>` default (for example, to run two different OpenAI-compatible secondaries side by side under distinct names), and the matching `--service` on `keychain-set` to store the key under that same name. `keychain-set` never infers the service from whichever secondary happens to be configured: it used to, and the first time two OpenAI-compatible providers existed side by side it overwrote one provider's stored key with the other's. Storing several providers' keys and swapping which is active is now just independent config edits.

**Dual-account (`/login` personal + work) OAuth failover was investigated and explicitly rejected**, in favor of the above. It would have required reading and independently refreshing a live Claude Code OAuth credential via an undocumented endpoint (`https://platform.claude.com/v1/oauth/token`), exactly the pattern this project's design principles (and the [original blog post](history/blog-two-prices-for-the-same-model.md)) call out as why other third-party tools have been blocked by Anthropic. Not planned.

## Amazon Bedrock notes

Bedrock is supported as an overflow secondary (`--secondary bedrock`). Its `model_map` is required: every Claude model needs an entry, and one without it fails the request rather than falling back. What is specific to it:

### Bedrock feature compatibility

Claude Code's Anthropic endpoint can send beta features that a third-party/cloud endpoint may not support. Claude Burst strips only the OAuth-specific `oauth-*` beta value before Bedrock and leaves the remaining Claude Code beta capabilities intact. If Bedrock rejects a feature that Anthropic accepts, the response is returned to Claude Code rather than silently weakening the request.

### Bedrock API key authentication only (as of v0.2.0)

The gateway reads `AWS_BEARER_TOKEN_BEDROCK` and stores it in macOS Keychain. It does not yet implement AWS SSO, role assumption, `awsAuthRefresh`, or SigV4 signing. Those should be added before a large enterprise rollout.

### The Bedrock key is briefly visible in local process listings

`claude-burst keychain-set` passes the key to `/usr/bin/security` as a command-line argument, so it's visible in `ps` output to other local processes for the duration of that one call. Reading it from stdin instead would close this, but `security`'s interactive password prompt doesn't reliably accept piped stdin in non-terminal contexts, so this hasn't been changed yet.
