# What is logged

[Back to the README](../README.md)

Claude Burst writes to two files under `~/.config/claude-burst/` - both rotate, so the pair is really up to 20 `claude-burst.log[.N]` files and 6 `metrics.jsonl[.N]` files, and both are metadata-only: **prompts, source code, tool inputs and model outputs are never written to the logs.** The proxy necessarily handles the request body in memory so it can replay a rejected request to the secondary, but it does not persist it.

Three features do keep conversation-derived content on disk, each because it is the point of the feature:

- **Pauseless compaction** stores each session's summary in `~/.config/claude-burst/compaction-state.json`, mode 0600, removed once the session has gone 48 hours without a request. See [Pauseless compaction](compaction.md).
- **Session handover**, when you opt a repository in, writes a summary of the session into that repository's `HANDOFF.md`. See [Session handover](handover.md).
- **Session coordination** logs file paths and session ids, not file contents, to `~/.config/claude-burst/coord/coord.log`. See [Session coordination](coordination.md).

## `metrics.jsonl` - structured, one line per request

(Also note the model fields: `model` is what **served** the request and drives cost;
`requested_model` is what Claude Code asked for. They differ exactly when a remapping
provider, Bedrock's modelMap, or openai-compatible fixed/consistent failover, was
involved. Pricing uses the served model, so a request GLM served is never costed at
Claude Opus rates.)

- timestamp and a short request id (also present in `claude-burst.log`, so a line in one file can always be matched to the other)
- Claude Code session and agent identifiers
- the slot (`primary` or `secondary`) and the route that served it, `anthropic` for oauth-passthrough, `anthropic-api-key`, `bedrock`, or an openai-compatible secondary's vendor label (`together`, `openrouter`, whatever `keychain_service` names)
- `destination`: the actual outbound URL (scheme, host and path; no query). The slot label says which slot was *chosen*; this says where the request physically went, which is what settles "did that really go to the secondary?"
- model
- HTTP status
- latency
- input/output token usage where exposed in the SSE stream
- estimated API-equivalent cost using the prices in `config.json`. A model with **no** `pricing` entry costs `0` - so the event also carries `pricing_unknown: true`, and `stats` counts it separately, because a zero meaning "not priced" must not read as a zero meaning "free". Third-party secondary models are not in the default pricing table: add yours to `pricing` or its spend will not be counted
- subscription limit claim and reset timestamp when failover occurs
- a short note on what happened (e.g. "subscription limit detected; request replayed to secondary", "keychain load failed: ...")

## `claude-burst.log` - plain text, for debugging

Every request gets a `start` line and a matching `done` line (same request id, final HTTP status, duration), so you can always see what the gateway did even for requests that never made it into `metrics.jsonl`. In addition, every failure path logs which stage it failed at and why:

- reading or size-limiting the request body
- building the outbound request to Anthropic or the secondary
- the upstream HTTP call itself (network errors)
- loading the secondary's key from the environment/Keychain
- mapping a model name for the secondary (Bedrock `model_map`, or the openai-compatible target)
- reading or writing the local overflow-state file
- writing a metrics event
- a panic anywhere in the request path, recovered, logged with a stack trace, and turned into a `500` instead of crashing the gateway or hanging Claude Code's connection

A metrics-write failure (disk full, permissions, etc.) is logged but never fails the request itself, by the time metrics would be written, Claude Code has already been served.
