# Backlog: Codex support

**Status: backlog, 2026-10-03.** Nothing here is built yet.

## Where things stand

**Burst never sees Codex's traffic today.** Transparent mode intercepts only
`api.anthropic.com`, and the gateway's inbound side speaks only Anthropic's
Messages API. Codex speaks OpenAI's Responses API, to `api.openai.com` with
an API key or to `chatgpt.com/backend-api/codex` with a ChatGPT login.

On this Mac `~/.codex` exists (the desktop app's state) but there is no
`codex` CLI on the PATH.

## What it would take

1. **Inbound Responses API.** A second listener path (`/v1/responses`) that
   accepts Codex's requests, the same way `/v1/messages` accepts Claude
   Code's. Streaming (SSE) has to pass through untouched.
2. **Getting the traffic.** Prefer direct mode: Codex supports custom
   `model_providers` in `~/.codex/config.toml` with a `base_url`, so pointing
   it at `http://127.0.0.1:17777/v1` needs no CA or pf. Transparent
   interception of `api.openai.com` is the fallback, and `chatgpt.com` should
   stay out of scope (it carries the ChatGPT web app too).
3. **Failover.** Primary: OpenAI with the user's key. Secondary: any
   OpenAI-compatible provider (Together, Portkey). That is a passthrough, not
   a translation, so it is simpler than Claude Code's secondary.
4. **Accounting.** Requests, tokens and cost in the same tables, with a
   client column (Claude Code, Codex) so the two are never summed blind.
5. **Session features.** Compaction, coordination and the band are Claude
   Code specific; leave them off for Codex at first.

## Open questions

- Does the user run Codex with a ChatGPT login or an API key? A ChatGPT
  login cannot be routed through a custom provider, which would leave only
  transparent mode on `chatgpt.com`.
- Is the CLI wanted, or only the desktop app?

Related: [opencode-support.md](opencode-support.md), the same inbound gap.
