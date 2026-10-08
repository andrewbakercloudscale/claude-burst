# What is logged

[Back to the README](../README.md)

Claude Burst's two main logs are under `~/.config/claude-burst/`: `claude-burst.log` (text) and `metrics.jsonl` (one record a request). Both rotate, so the pair is really up to 20 `claude-burst.log[.N]` files and 6 `metrics.jsonl[.N]` files, and both are metadata-only: **prompts, source code, tool inputs and model outputs are never written to the logs.** The proxy necessarily handles the request body in memory so it can replay a rejected request to the secondary, but it does not persist it. [Every file it writes](#every-file-and-its-limit) is listed below.

Two things enforce "metadata-only" where text from outside could get in:

- **An upstream error** is logged as its `type` and `message` only, 300 characters at most. The rest of an error body can echo the request back.
- **A changed system reminder** (the context Claude Code keeps in the first message) is named by line number, length and a short hash, never quoted.
- **URLs** are logged without their query string, in the text log and in a metrics `note`.

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
- input/output token usage where exposed in the SSE stream, with `cache_read_tokens` and `cache_write_tokens` apart: `input_tokens` is uncached input only, and on a long session the cached part is nearly all of it
- what was left out of the request: `pruned_bytes`, `pruned_tool_results`, `truncated_tool_results`, `pruned_usd` (secondary pruning) and the `compacted_*` fields (pauseless compaction)
- estimated API-equivalent cost using the prices in `config.json`. A model with **no** `pricing` entry costs `0` - so the event also carries `pricing_unknown: true`, and `stats` counts it separately, because a zero meaning "not priced" must not read as a zero meaning "free". Third-party secondary models are not in the default pricing table: add yours to `pricing` or its spend will not be counted
- subscription limit claim and reset timestamp when failover occurs
- a short note on what happened (e.g. "subscription limit detected; request replayed to secondary", "keychain load failed: ...")

## `claude-burst.log` - plain text, for debugging

Every line starts the same way:

```
2026-10-07T11:30:05.123+02:00 level=warn req=17036d1f8242148f failover route=anthropic ...
```

- **The time** is local, to the millisecond, with its zone, so a pasted line says what clock it is on and it sorts with the times in `metrics.jsonl` and `audit.jsonl`. Lines written before 7 Oct 2026 start `2026/10/07 11:30:05`, local with no zone; the dashboard reads both.
- **`level=`** is `info`, `warn` or `error`. `grep -v level=info claude-burst.log` is everything that went wrong or changed course: failovers, windows opening and closing, the network going, a refused swap, a panic, a start after a crash.
- **`req=`** is the request id, the same one in `metrics.jsonl` (`request_id`). Codex turns carry theirs too (`codex: turn req=...`, matching `codex-metrics.jsonl`).

Every model request (`/v1/messages`) gets a `start` line and a matching `done` line (same request id, final HTTP status, duration), so you can always see what the gateway did even for requests that never made it into `metrics.jsonl`. Health checks, heartbeats and the other calls that are not a model request are counted while they succeed, and one `quiet:` line a minute says how many; one that fails or takes over 10 seconds gets its `done` line. They were 56% of the log.

A line that would repeat for every request of an outage (the network is down, the network is unchanged) is written when it starts and then once a minute, ending `[and N more like this since the last such line]`.

In addition, every failure path logs which stage it failed at and why:

- reading or size-limiting the request body
- building the outbound request to Anthropic or the secondary
- the upstream HTTP call itself (network errors)
- loading the secondary's key from the environment/Keychain
- mapping a model name for the secondary (Bedrock `model_map`, or the openai-compatible target)
- reading or writing the local overflow-state file
- writing a metrics event
- a panic anywhere in the request path, recovered, logged with a stack trace, and turned into a `500` instead of crashing the gateway or hanging Claude Code's connection

A metrics-write failure (disk full, permissions, etc.) is logged but never fails the request itself, by the time metrics would be written, Claude Code has already been served.

## A crash

A Go process that dies of a panic or a runtime fault writes its stack to stderr, which launchd appends to `launchd.err.log`. Each start of the gateway writes a dated line there (`=== claude-burst 0.20.11 started 2026-10-07 11:11:15 +02:00 pid 51299 ===`), so the stack above it can be dated. If the run before ended in a crash, the new one says so in `claude-burst.log` (`error stage=startup the previous run ended in a crash: ...`) and in the audit, which the support console shows.

## Every file and its limit

All under `~/.config/claude-burst/` unless a path is given.

| File | What | Limit |
|---|---|---|
| `claude-burst.log` | The gateway's text log | 20 files of 10MB |
| `metrics.jsonl` | One record a request | 6 files of 10MB |
| `codex-metrics.jsonl` | One record a Codex turn | 6 files of 10MB |
| `audit.jsonl` | Alerts shown and actions taken | 2MB and one older file |
| `launchd.err.log`, `launchd.out.log` | The gateway's stderr and stdout: TLS handshake errors, a crash's stack | Cut back to the last 1MB at a start once over 4MB |
| `console.err.log`, `console.out.log` | The support console's | The same |
| `coord/coord.log` | Session coordination: file paths and session ids | 2MB and one older file |
| `passthrough.log` | The pass-through that stands in while Burst is off | 2MB and one older file |
| `self-heal.log`, `watchdog.log`, `health-check-failures.log` | The watchdogs, and what a failed health check looked like | 2MB and one older file each |
| `hotspot.log` | Hotspot joins | 400 lines |
| `compaction-outcomes.jsonl` | Summaries dropped or ended, for the learner | None: a line of about 100 bytes for each such event |
| `automask-state.json`, `automask.key` | Automask's counts, its last 50 masks, and each session's masks filed under a keyed hash of the value: no value | Rewritten whole when a new value is masked; a session's entries go after a day idle |
| `compaction-threads.json` | Which conversation each of the latest 512 replies belongs to, so a restart does not lose a session's place: ids and sizes, no content | Rewritten whole, about 100KB at most |
| `/var/log/claude-burst-pf.log` | The pf redirect guard (root) | One older file |

If the text log cannot rotate (permissions, a full disk) it goes on in the file it has and says so once on stderr; it used to stop without a word.
