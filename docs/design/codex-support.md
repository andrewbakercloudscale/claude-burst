# Backlog: Codex support

**Status: built 2026-10-04** as a pass-through with accounting; see [../codex.md](../codex.md). Failover for Codex is still backlog. This page is the design history.

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
   interception is the other route (see below).
3. **Failover.** Primary: the user's ChatGPT plan (see below). Secondary: an
   OpenAI-compatible provider (Together, Portkey) with its own key.
4. **Accounting.** Requests, tokens and cost in the same tables, with a
   client column (Claude Code, Codex) so the two are never summed blind.
5. **Session features.** Compaction, coordination and the band are Claude
   Code specific; leave them off for Codex at first.

## Decided: ChatGPT login (2026-10-03)

The user signs in to Codex with ChatGPT, so the traffic is
`chatgpt.com/backend-api/codex/responses` with the ChatGPT token, not
`api.openai.com`. That changes step 2:

- **First try `chatgpt_base_url`.** Codex's config has a key for the ChatGPT
  backend's address. If it accepts `http://127.0.0.1:17777/backend-api/`,
  Burst gets only Codex's calls and forwards them to `chatgpt.com` with the
  token untouched. Verify against the installed Codex before building on it.
- **Fallback: transparent on `chatgpt.com`.** Every `chatgpt.com` request
  then passes through Burst, including the ChatGPT website. Only
  `/backend-api/codex/` is routed or counted; everything else is a blind
  passthrough. The user is fine with website traffic passing through
  (2026-10-03), so this is an equal option, not a last resort: pick
  whichever covers both the CLI and the desktop app.
- **Failover target.** When the ChatGPT plan is rate limited (429 or a
  usage limit message), the secondary is an OpenAI-compatible provider with
  its own key. That needs Responses to Chat Completions translation unless
  the provider speaks Responses.
- **Cost.** ChatGPT plan calls show tokens and an API equivalent, the same
  way Claude subscription calls do.

## Open questions

- Is the CLI wanted, or only the desktop app? Both read `~/.codex/config.toml`.
- Does the desktop app honour `chatgpt_base_url`?

Related: [opencode-support.md](opencode-support.md), the same inbound gap.
