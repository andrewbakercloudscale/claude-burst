# Proposal: OpenCode support

**Status: proposal, 2026-10-01.** A plan, not a feature: nothing here is built yet.

## Where things stand

**What Burst does for OpenCode today:** only the Finder shortcut ("Launch
OpenCode in Ghostty", kept awake with caffeinate) and, if installed, the
OpenCode usage panel split. Nothing in the gateway.

**How OpenCode is set up on this Mac** (OpenCode 1.18.19, Homebrew,
`~/.config/opencode/`):

- every model is on **Together AI** through `@ai-sdk/openai-compatible`,
  `baseURL: https://api.together.xyz/v1`, key from `TOGETHER_API_KEY`;
- three profiles: `opencode.jsonc` (GLM-5.2, gpt-oss-20b small),
  `opencode.max.jsonc` (GLM-5.2, Kimi-K3, MiniMax-M3), `opencode.cheap.jsonc`
  (DeepSeek-V4-Flash, gpt-oss);
- no Anthropic provider, and `@opencode-ai/plugin` 1.18.19 installed.

So **Burst never sees OpenCode's traffic.** Transparent mode intercepts only
`api.anthropic.com`, and the gateway's inbound side speaks only Anthropic's
Messages API. Its OpenAI-compatible code (`provider_openai.go`) is outbound:
it translates Claude Code's Anthropic requests for the secondary.

Using a Claude Pro/Max login from OpenCode is out of scope: Anthropic does not
allow third-party tools to use subscription logins, and the README already
rules that pattern out for Burst.

## What "support" should mean

In order of value for how OpenCode is used here:

1. **See its spend.** OpenCode's Together spend shows up in Activity, Spend
   by model and Spend by repository beside Claude Code's, with sessions and
   repositories attributed.
2. **Coordinate with Claude Code sessions.** An OpenCode session editing the
   same repository takes part in session coordination: masters, the
   commit-before-idle rule, no sweeping `git add -A`. Today it is invisible,
   so it can clobber or sweep up Claude Code's work and the reverse.
3. **Cut its cost.** Overflow pruning (stub old tool output, cap huge
   results) already exists for OpenAI-compatible requests; applied to
   OpenCode's own requests it saves the same share of input.
4. **Pauseless compaction for OpenCode sessions.** Most complex; last.

Failover and the Anthropic subscription do not apply: OpenCode already runs
on the paid provider.

## Todo

### Phase 0: find out (no code)

- [ ] Point a test OpenCode profile's `baseURL` at a logging stub and record
      exactly what it sends: paths (`/chat/completions` only?), headers (any
      session or conversation id? user agent), streaming format, tool-call
      shape, and how long histories get.
- [ ] Check whether OpenCode sends a stable per-session identifier. If not,
      key sessions on a hash of the first messages, as compaction already
      does for subagents (`conversationID`).
- [ ] Check how the repository can be known: the request carries no cwd, so
      either OpenCode's session database (`~/.local/share/opencode/
      opencode.db`) maps sessions to directories, as Claude Code's
      transcripts do, or a plugin adds a header.
- [ ] Read the `@opencode-ai/plugin` hook list: which events exist before
      and after a tool runs, at session start and at idle, and whether a
      hook can refuse a tool call (needed for coordination).

### Phase 1: an OpenAI-compatible inbound endpoint (spend)

- [ ] Add `/openai/v1/chat/completions` (and `/openai/v1/models`) to the
      gateway: pass the request through to the configured OpenAI-compatible
      upstream unchanged, stream the answer back, and record tokens and cost
      per model in `metrics.jsonl` with `client: "opencode"`.
- [ ] Reuse the secondary's Keychain key, so OpenCode's config only changes
      `baseURL` (to `http://127.0.0.1:<port>/openai/v1`); no key in its file.
- [ ] Session and repository attribution from phase 0's findings.
- [ ] Dashboard: OpenCode rows in Activity and both spend tables, with a
      client filter (Claude Code, OpenCode, all).
- [ ] An "Use Burst for OpenCode" switch that edits the `baseURL` of the
      chosen OpenCode profiles, with a backup, and puts it back when off.
- [ ] Fail safe: if the gateway is down, OpenCode errors. Decide between a
      watchdog-style revert of `baseURL` and simply documenting it, since
      unlike Claude Code nothing else on the Mac depends on it.

### Phase 2: session coordination for OpenCode

- [ ] An OpenCode plugin (shipped with Burst, installed by the switch) that
      calls `claude-burst coord <hook>` with the same JSON shape Claude
      Code's hooks use: session start, before an edit or write, before a
      shell command, after a tool, and at idle.
- [ ] Map OpenCode's tool names (edit, write, bash, patch) onto the
      coordinator's Edit/Write/Bash handling, including patch tools that
      touch several files at once.
- [ ] Commit-before-idle ([orphaned-work.md](orphaned-work.md), rule 1) for OpenCode,
      through its idle event, if a plugin can keep the session going;
      otherwise a notice plus layer 2's orphan record.
- [ ] Show OpenCode sessions in the dashboard's "Sessions taking part",
      labelled by client.

### Phase 3: pruning

- [ ] Run the existing pruning (stub, cap) on the inbound OpenAI requests,
      behind its own switch, and show OpenCode's share in Context & cache.
- [ ] Check that Together's prompt cache still hits: pruning moves in steps
      precisely so the prefix stays byte-identical, but OpenCode's own
      request layout may differ from the translated Claude Code requests the
      step size was tuned on.

### Phase 4: compaction (later, maybe)

- [ ] Port Pauseless Compaction to OpenAI chat messages: plain-prompt
      boundaries, summary request on the same model, prefix hash, swap at
      the next user message. Mid-turn swapping is a separate question for
      each provider.
- [ ] Only worth doing if phase 1's numbers show OpenCode sessions regularly
      reaching large contexts on the expensive models.

## Risks and open questions

- **OpenCode changes fast** (1.18.x). The plugin API and request shapes
  should be pinned in tests against recorded traffic, so an upgrade that
  changes them fails a test rather than silently dropping spend or
  coordination.
- **Two clients, one coordinator.** Claude Code and OpenCode hooks must
  agree on session ids and state; a shared JSON schema with the client named
  in each session keeps them apart.
- **The usage panel** already has an OpenCode launcher
  (`opencode-panel-launch.sh`); check what it reads so Burst's metrics and
  the panel do not disagree about the same session.
- **Scope check before phase 1**: if OpenCode is used rarely, phase 2
  (coordination) may be worth more than phase 1 (spend), because the
  clobbering risk exists whenever both run in one repository.
