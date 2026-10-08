# Codex through Burst

[Back to the README](../README.md)

Claude Burst also carries OpenAI's **Codex** (the ChatGPT app's Codex and the `codex` CLI), signed in with ChatGPT. Its model calls pass through a second gateway on this Mac on their way to `chatgpt.com`, so the dashboard's **Codex** tab can show what they cost in tokens, how full each session's context is, and how much of the ChatGPT plan's limits is used.

## Turn it on

Dashboard, **Codex** tab, **Route Codex through Burst**. Or:

```bash
claude-burst codex enable    # route Codex through Burst
claude-burst codex status
claude-burst codex disable   # straight to ChatGPT again
```

Then **restart Codex**: quit and reopen the ChatGPT app, and restart any `codex` running in a terminal. Codex reads its config when a session starts.

## How it works

- `enable` adds a model provider at the **top** of `~/.codex/config.toml` (or `$CODEX_HOME/config.toml`), between `# BEGIN claude-burst` and `# END claude-burst`, after saving a copy of the file to `~/.config/claude-burst/backups/`. Nothing outside the markers is touched.
- The provider points Codex at `http://127.0.0.1:7779/backend-api/codex` with `requires_openai_auth = true`, so Codex keeps its ChatGPT sign-in and sends its own token with every request.
- The gateway forwards each request to `chatgpt.com` **unchanged**, unless [Automask](automask.md) covers ChatGPT and found something to mask in a turn, or an item was removed in the context inspector, and reads the reply on its way back: the turn's tokens (from `response.completed`), each model's context window (from the model list), and the plan usage ChatGPT reports in its `x-codex-*` headers.
- Only model calls (`/models`, `/responses`) go through Burst. Codex's sign-in, plugins and cloud tasks go to ChatGPT directly.
- Each turn is recorded, metadata only, in `~/.config/claude-burst/codex-metrics.jsonl`, apart from Claude Code's `metrics.jsonl`: no Claude Code total ever includes a Codex turn. The gateway log gets one `codex: turn ...` line per turn. Every other request gets one `codex: request GET /path status=200 ms=... client="..."` line, naming the Codex client that sent it, and a WebSocket one `codex: websocket ...` line when it opens.
- **Everything that is HTTP is passed on**, whatever its method or path: a request, a streamed reply, a WebSocket, and HTTP/2 without TLS. A reply ChatGPT refuses (a 401 on the model list, say) goes back to Codex as it came and is logged as `codex: GET ... answered 401 by ChatGPT`.
- **What is not HTTP is forwarded too, unread.** A connection that is not HTTP at all (a TLS handshake sent to the plain port, another protocol) is carried to `chatgpt.com` byte for byte and its answer carried back, so it is ChatGPT that answers and never Burst. A TLS handshake goes on as it is; anything else travels inside Burst's own TLS connection, as a request would. It is logged as `level=error codex: not HTTP, on 127.0.0.1:7779 from 127.0.0.1:<port>: forwarded to chatgpt.com:443 as it came ...` with what it began with (the first line only, never headers or a query), the bytes each way and ChatGPT's answer. A forwarded connection goes straight to the host: a proxy set in the environment is not used for it.
- **The one thing Burst still answers itself** is a connection that starts like HTTP and then does not parse (a broken header, headers over 1 MB): Go's server answers `400` before any of Burst's code runs. That is `level=error codex: refused a connection to the Codex port ...`, with both ports.
- **Both are rows in the Codex requests table** beside the turns (slot `not http` or `refused`, status in red), and so is a request that is not a model call but that ChatGPT refused, with the Codex client that sent it. The latest 50 are kept, across restarts, apart from the turns: no count, error rate or check includes them. The Codex tab's check **Only HTTP at the port** goes amber for an hour. Search the log for `codex: not HTTP` or `codex: refused`.
- The listener runs whenever Burst runs, routed or not: a Codex session keeps sending to the port it started with until it exits.
- If `config.toml` already sets its own `model_provider`, Burst refuses rather than override it, and says which.

Checked against codex-cli 0.159.2. The other route, `chatgpt_base_url`, was rejected: Codex requires it to be HTTPS and it also carries plugin, MCP and account calls.

## The Codex tab

![The Codex tab's overview: its checks, the routing state and its buttons, 14-day turns, sessions, tokens and errors, and the ChatGPT plan's weekly window](screenshots/codex.png)

![Codex insights: tokens per day split into cached input, uncached input and output, latency, error rate and cached share tiles, and each model's share of the tokens](screenshots/codex-insights.png)

![Codex context by session: each session's context after its latest turn as a bar against its model's window. Example repository names](screenshots/codex-context.png)

![Codex context inspector: a bar of the context by part (tool outputs, reasoning, replies, instructions, tools, AGENTS.md), one flagged tool output, and the items with a Remove button. Illustration: an example session](screenshots/codex-inspector.png)

| Section | What it answers |
| --- | --- |
| Checks and overview | Is Codex going through Burst, is ChatGPT answering, and how much of the plan is used? |
| Insights | Turns, tokens, latency and errors per day, and which models the tokens go to |
| Context | How full each session's window is |
| Context inspector | What a session's context is made of, with Remove and Restore |
| Requests | The recent model calls, one a row |

- **Checks**: Codex's own meter, as the Claude tab's is Claude Code's, with the same rule (the worst failing check sets the colour) and a fix button on each failing check:
  - **Codex goes through Burst**: Burst's block is in `~/.codex/config.toml` (red if that file does not parse; amber if Codex goes straight to ChatGPT).
  - **Burst's Codex port answering**: red when Codex is routed to a port nothing answers on.
  - **ChatGPT answering**: the newest turn ChatGPT answered, while under 10 minutes old; red on a 5xx, no answer, a refused sign-in (401/403) or the plan's limit (429). A 499 is Codex hanging up and does not count.
  - **ChatGPT plan headroom**: amber at 90% of any plan window.
  - **Gateway watchdog**: one gateway serves Codex and Claude Code.
  - **Error rate**: amber at 2% of Codex turns over 14 days.
- **Overview**: whether Codex goes through Burst, the gateway's listener (with the reason if it is not listening), the last request, 14-day turns, sessions, tokens and errors, and the **ChatGPT plan limits** as bars (for example the weekly window, percent used, when it resets).
- **Insights**: what the Claude tab's Daily activity and Analytics are for Claude Code, over 7, 14 or 30 days.
  - **Daily activity**: a bar per day, as **Tokens** (cached input, uncached input and output, stacked) or **Turns** (answered and failed). Days older than the log are hatched as "no data", never drawn as empty.
  - **Tiles**: median and p95 latency (answered turns only), error rate (amber at 2%, red at 5%), the share of input read from cache (amber under half: Codex is sending most of each conversation uncached, which uses the plan fastest), tokens per turn, turns per session, the largest context any turn reached, the busiest day and the busiest hour of the day.
  - **By model**: each model's share of the tokens, with its turns, tokens per turn, cached share, output, median latency and failed turns.
  - Tokens only: there is no API-equivalent price for Codex's models. The same figures as JSON: `curl -s 'http://127.0.0.1:7788/api/codex/insights?days=14'`.
- **Context**: each recent session's context after its latest turn against its model's window, as a bar: green, amber from 60%, red from 85%. Codex compacts on its own as it nears the limit.
- **Context inspector**: with the same bar of the context by part as [Claude Code's](dashboard.md#context-inspector). A Codex session's context item by item, as Burst last forwarded it: Codex's instructions, skills, the developer reminders, AGENTS.md, your prompts, Codex's replies and tool calls, every tool output, and the encrypted reasoning (size only). **Remove** leaves AGENTS.md, skills, a reminder or a tool output out of every later turn of that session, a one-line note in its place; **Restore** puts it back. See [the dashboard](dashboard.md) for how removal works.
- **Requests**: the recent model calls, with uncached input, cached input and output apart.

When ChatGPT refuses a turn for the plan's usage limit, an on-screen alert says so with the reset time, and clears on the next accepted turn.

## Undo and recovery

Every way out removes Burst's block, through the same `scripts/codex-unroute.sh`:

- **Send Codex straight to ChatGPT** on the Codex tab, or `claude-burst codex disable`.
- `burst-off` and `scripts/rollback.sh` (everything off).
- `./install.sh uninstall`.
- [Repair](../README.md#repair-burst) checks the Codex port answers when Codex is routed here, and offers to unroute it if not.

By hand: delete the lines from `# BEGIN claude-burst` to `# END claude-burst` in `~/.codex/config.toml`. Restart Codex afterwards.

[Diagnose](../README.md#diagnose-burst) reports Codex's routing, the block, the port, a forwarding probe, Codex's processes and its recent turns.

## Not yet

- No failover for Codex: when the ChatGPT plan is out, Codex stops as it would without Burst (the alert says when it resets). A secondary needs Responses API translation.
- No API-equivalent price for Codex models yet: tokens only.
- Compaction, coordination and the band are Claude Code features and do not apply to Codex. [Automask](automask.md) does: it masks a Codex turn's instructions, messages and tool output with the same rules as Claude Code's.
