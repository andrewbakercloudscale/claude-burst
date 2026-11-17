# Claude Desktop

[Back to the README](../README.md)

Burst works for people who use Claude Code through Claude Desktop (`/Applications/Claude.app`, its **Code** tab and **Cowork**) rather than the `claude` command in a terminal. This page says what Desktop does differently, what Burst needs for it, and how to check that Desktop's sessions really go through Burst.

## What Claude Desktop runs

Found on 17 Nov 2026 from Claude Desktop 5.11492.5 and its own log (`~/Library/Logs/Claude/main.log`):

- **The Code tab runs its own Claude Code**, downloaded by Desktop, not the one on your `PATH`: `~/Library/Application Support/Claude/claude-code/<version>/<hash>/claude.app/Contents/MacOS/claude`. It updates on its own schedule.
- **Its transcripts land where the terminal's do**, in `~/.claude/projects`, marked `"entrypoint": "claude-desktop"`, and its requests say so in their User-Agent (`claude-cli/2.1.325 (external, claude-desktop, ...)`). Burst names their sessions and repositories the same way as the terminal's. A session started without a folder runs in `~/Library/Application Support/Claude/scratch-workspaces/...`; Burst groups those as **(Claude Desktop scratch)** rather than listing each as a repository. A session in a worktree Desktop made (`<repo>/.claude/worktrees/<name>`) is counted under its repository, and that repository's own **Compact at** applies to it.
- **It reads `~/.claude/settings.json`**, so base-url mode's `ANTHROPIC_BASE_URL` reaches it. **Cowork does not**: it runs the same Claude Code with `CLAUDE_CONFIG_DIR` set to a temporary folder and an `ANTHROPIC_BASE_URL` of Desktop's choosing, so only transparent mode puts Cowork through Burst.
- **It does not use your shell's `NODE_EXTRA_CA_CERTS`.** Desktop writes its own CA bundle at launch, `~/Library/Application Support/Claude/ca-bundle.pem`, from the certificate authorities the **System keychain** trusts (plus the `NODE_EXTRA_CA_CERTS` launchd gave Desktop itself, if any), and hands that bundle to every Claude Code it starts. So in transparent mode, Desktop's sessions accept the gateway's certificate only when Burst's CA is trusted system-wide, and only from the next time Desktop starts.
- **Desktop's own chat** (the Chat tab) talks to claude.ai, which Burst does not intercept. Desktop also reaches `api.anthropic.com` for its updater and some connectors (`/v1/pages/mcp`, for one); in transparent mode those reach the gateway too, which forwards a request that is not a model request to Anthropic without compaction or failover. Before the CA was trusted system-wide they failed their handshakes ([the 2026 investigation](history/investigation-tls-storm.md)).

## Setting it up

Install as the [Quickstart](../README.md#quickstart) says; there is no separate Desktop install. Then:

1. **Transparent mode** (recommended, and the only one that reaches Cowork): let the install trust the CA system-wide. The dashboard's **Install** does it; by hand it is `sudo scripts/trust-ca-systemwide.sh`. Without it, every Desktop Code and Cowork session refuses the gateway, while the terminal's `claude` works.
2. **Quit and reopen Claude Desktop** (Claude, Quit; not just closing the window). Desktop writes its CA bundle as it starts, so a Desktop that was open during the install keeps refusing the gateway until it restarts. The same goes after `claude-burst ca-rotate`.
3. In **base-url mode**, start a new Code session after the install: a running one keeps the settings it started with.

The installer needs a Terminal once, to build Burst and, for transparent mode, to ask for your password. After that nothing needs a terminal: the dashboard (`http://127.0.0.1:7788`, or Cmd-Space, "burst") and the support console have every switch.

## Checking that Desktop goes through Burst

- **The support console** (`http://127.0.0.1:7789`) shows a **Claude Desktop** row where Desktop is installed. It is red when Desktop's CA bundle lacks Burst's CA, or when Desktop's Code tab ran sessions in the last week and none of their requests reached the gateway; otherwise it says how many did.
- **`scripts/diagnose.sh --check`** has the same CA check, and its report has a Claude Desktop section: the app's version, the Claude Code builds its Code tab runs, its CA bundle, and the requests from Desktop in the gateway's log.
- **The gateway's log** ends a model request's line with `client=claude-desktop` (Code tab) or `client=cowork` when Desktop sent it; the terminal's lines are unchanged. `metrics.jsonl` records the same as `"client"` on every request.

```bash
grep -c 'client=claude-desktop' ~/.config/claude-burst/claude-burst.log
```

## Alerts without a terminal

The on-screen toasts are drawn by the usage panel, a terminal pane, and by the `burst-session` mod inside a terminal session. Claude Desktop shows neither. So where nothing on the Mac shows the toasts, Burst sends **every** alert to macOS Notification Center by default, not only the critical ones; see [Notifications](lid-and-hotspot.md#notifications). Allow **Claude Burst** when macOS asks during the install. If you missed it, or the dashboard's **Notifications** section says it is turned off, run `claude-burst app notifications`: it opens System Settings at Notifications, Claude Burst.

## What does not reach Desktop

- The usage panel and the in-session band and toasts: they are terminal features.
- Remote Control's on-screen name from the panel (`claude-panel-rc-name`).
- The mod's band and toasts: Desktop's Claude Code loads the plugins in `~/.claude` (its log says so), but the band and toasts are drawn by the terminal's interface. Whether the mod's commands, such as `/compact-async`, work in the Code tab has not been checked.
