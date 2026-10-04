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
- The gateway forwards each request to `chatgpt.com` **unchanged** and reads the reply on its way back: the turn's tokens (from `response.completed`), each model's context window (from the model list), and the plan usage ChatGPT reports in its `x-codex-*` headers.
- Only model calls (`/models`, `/responses`) go through Burst. Codex's sign-in, plugins and cloud tasks go to ChatGPT directly.
- Each turn is recorded, metadata only, in `~/.config/claude-burst/codex-metrics.jsonl`, apart from Claude Code's `metrics.jsonl`: no Claude Code total ever includes a Codex turn. The gateway log gets one `codex: turn ...` line per turn.
- The listener runs whenever Burst runs, routed or not: a Codex session keeps sending to the port it started with until it exits.
- If `config.toml` already sets its own `model_provider`, Burst refuses rather than override it, and says which.

Checked against codex-cli 0.159.2. The other route, `chatgpt_base_url`, was rejected: Codex requires it to be HTTPS and it also carries plugin, MCP and account calls.

## The Codex tab

- **Checks**: Codex's own meter, as the Claude tab's is Claude Code's, with the same rule (the worst failing check sets the colour) and a fix button on each failing check:
  - **Codex goes through Burst**: Burst's block is in `~/.codex/config.toml` (red if that file does not parse; amber if Codex goes straight to ChatGPT).
  - **Burst's Codex port answering**: red when Codex is routed to a port nothing answers on.
  - **ChatGPT answering**: the newest turn ChatGPT answered, while under 10 minutes old; red on a 5xx, no answer, a refused sign-in (401/403) or the plan's limit (429). A 499 is Codex hanging up and does not count.
  - **ChatGPT plan headroom**: amber at 90% of any plan window.
  - **Gateway watchdog**: one gateway serves Codex and Claude Code.
  - **Error rate**: amber at 2% of Codex turns over 14 days.
- **Overview**: whether Codex goes through Burst, the gateway's listener (with the reason if it is not listening), the last request, 14-day turns, sessions, tokens and errors, and the **ChatGPT plan limits** as bars (for example the weekly window, percent used, when it resets).
- **Context**: each recent session's context after its latest turn against its model's window, as a bar: green, amber from 60%, red from 85%. Codex compacts on its own as it nears the limit.
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
- Compaction, coordination, automask and the band are Claude Code features and do not apply to Codex.
