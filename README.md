# Claude Burst: the missing control plane for Claude Code (and Codex)

> **Broken?** Open the [**support console**](#support-console) at http://127.0.0.1:7789/: what happened, the log around it, and Restart and Repair buttons, up even when the gateway is not. Or paste one line into Terminal: [**Repair**](#repair-burst) fixes the common problems, [**Diagnose**](#diagnose-burst) copies a report to the clipboard. Also: [**Update and reinstall**](#update-and-reinstall-burst) · [**Bypass Burst**](#bypass-burst)

The ops layer for running Claude Code all day on a Mac, and now OpenAI's Codex too: subscription-first routing with overflow to GLM/OpenRouter/Bedrock, pauseless compaction, session coordination and handover across parallel sessions, lid-shut keep-awake with automatic hotspot join for Remote Control, a dashboard for cost, health and guards, and an audit of every alert and action.

**Now supports Codex.** Codex (the CLI and the ChatGPT desktop app, signed in with ChatGPT) can route through Burst with one click on the dashboard's Codex tab or `claude-burst codex enable`: tokens per turn, each session's context against its window, the ChatGPT plan's limits, a path trace and a test turn. Requests reach ChatGPT unchanged. See [Codex](docs/codex.md).

> **What it changes on your Mac**
>
> | Change | When | Undo |
> |---|---|---|
> | Entries in `~/.claude/settings.json`: hooks for the features you switch on, and `ANTHROPIC_BASE_URL` in base-url mode | always (hooks only for features you turn on) | [Claude Code settings and hooks](ROLLBACK.md#claude-code-settings-and-hooks-claude) |
> | A LaunchAgent that runs the gateway, one for the support console, plus the binary in `~/.local/bin` | always | [Always installed](ROLLBACK.md#always-installed) |
> | Burst's block at the top of `~/.codex/config.toml` | only when you route Codex through Burst | `claude-burst codex disable` |
> | An `/etc/hosts` entry, a pf redirect and a trusted root CA (name-constrained to `api.anthropic.com`, in the System keychain) | transparent mode only, which you choose | [Transparent mode](ROLLBACK.md#transparent-mode-machine-wide-root) |
> | `pmset disablesleep` and a root LaunchDaemon | lid keep-awake only, off by default | [Lid shut and hotspot](ROLLBACK.md#lid-shut-and-hotspot) |
> | Root LaunchDaemon and user LaunchAgent guards | when you arm them | [Guards](ROLLBACK.md#guards) |
>
> `./install.sh uninstall` removes all of it and checks that it is gone ([Uninstall](#uninstall)). What this means for your security: [Trust and risk](#trust-and-risk).

Claude Burst is a local gateway that sits between Claude Code and Anthropic. **It is for** people who use Claude Code heavily on a Mac, on a Pro, Max or Enterprise plan (or a metered API key), and want to keep working through limits, long sessions and closed lids. Claude Code talks to it exactly as it talks to Anthropic: nothing in your workflow changes, and **Revert to normal Claude** on the dashboard takes it out of the path in one click. It is young (macOS only, experimental): try it on a non-critical development account first.

![Dashboard overview: health checks, routing, requests, sessions, tokens and spend, and daily activity](docs/screenshots/overview.png)

## Fix or update Burst: copy-paste scripts

### Support console

http://127.0.0.1:7789/ is a small page served by its own process, so it answers when the gateway and its dashboard do not: which part is down, the latest log lines, the audit (every on-screen alert and every action taken from the dashboard or the console, each with the log lines around it), and buttons to Restart the gateway, Repair, write a Diagnostic report or Turn Burst off. Turning Burst off leaves it running, so it is also where you turn Burst back on. The same audit is under General, Audit on the dashboard.

### Repair Burst

One line. Syncs with GitHub, stops anything stale holding Burst's ports, reinstalls in the mode Burst was in, arms the gateway watchdog, puts the CA back into Claude Code's trust bundle, finds Claude Code sessions started before the CA changed (they time out until restarted) and offers to stop them, then checks a request gets through and opens the dashboard:

```bash
curl -fsSL https://raw.githubusercontent.com/andrewbakercloudscale/claude-burst/main/scripts/repair.sh | bash
```

It asks before stopping any session; stopped ones resume with `claude --continue`.

### Diagnose Burst

One line. Changes nothing, needs no password, redacts secrets, and copies the report to the clipboard to paste wherever you are getting help:

```bash
curl -fsSL https://raw.githubusercontent.com/andrewbakercloudscale/claude-burst/main/scripts/diagnose.sh | bash
```

Paste either block into Terminal as a whole. Each writes a script to your home folder, makes it runnable and runs it; next time just run `~/burst-update.sh` or `~/burst-bypass.sh`. Both ask for your password when they touch `/etc/hosts`.

### Update and reinstall Burst

Newest release, transparent mode with the `/etc/hosts` redirect and pf rule, and anything left holding Burst's ports 7777, 17777, 7788 and 7779 stopped first, then the dashboard opens. Fixes a Mac where Burst is broken, stale or half installed:

```sh
cat > ~/burst-update.sh <<'EOF'
#!/bin/zsh
# Update Claude Burst and reinstall it in transparent mode. Safe to rerun.
set -euo pipefail
STEP="finding the checkout"
trap 'rc=$?; (( rc )) && print -u2 "burst-update FAILED (exit $rc) while $STEP. Paste everything above when asking for help."' EXIT
REPO="${CLAUDE_BURST_REPO:-}"
if [[ -z "$REPO" ]]; then
  # Any checkout up to three folders down whose remote is Claude Burst.
  for d in $(find ~ -maxdepth 4 -type d -name .git -not -path '*/Library/*' 2>/dev/null); do
    if git -C "${d%/.git}" remote -v 2>/dev/null | grep -q 'claude-burst'; then REPO="${d%/.git}"; break; fi
  done
fi
if [[ -z "$REPO" ]]; then
  REPO=~/claude-burst
  git clone https://github.com/andrewbakercloudscale/claude-burst.git "$REPO"
fi
cd "$REPO"
echo "== updating $REPO"; STEP="updating $REPO"
git fetch --tags --quiet origin
if git symbolic-ref -q HEAD >/dev/null; then
  git pull --ff-only --quiet
else
  git -c advice.detachedHead=false checkout --quiet "$(git tag --sort=-v:refname | grep '^v' | head -1)"
fi
git log --oneline -1
echo "== freeing Burst's ports"; STEP="freeing the ports"
GW=$(launchctl print "gui/$UID/ninja.andrewbaker.claude-burst" 2>/dev/null | awk '$1 == "pid" {print $3; exit}' || true)
for port in 7777 17777 7788 7779; do
  for pid in $(lsof -nP -t -iTCP:$port -sTCP:LISTEN 2>/dev/null); do
    [[ "$pid" == "$GW" ]] && continue
    echo "port $port: stopping pid $pid ($(ps -o comm= -p $pid))"
    kill $pid 2>/dev/null || sudo kill $pid
    sleep 1
    kill -0 $pid 2>/dev/null && { kill -9 $pid 2>/dev/null || sudo kill -9 $pid; }
  done
done
rm -f ~/.config/claude-burst/rolled-back ~/.config/claude-burst/rolled-back.noted
echo "== installing"; STEP="installing (install.sh)"
CLAUDE_BURST_MODE=transparent CLAUDE_BURST_FORCE=1 ./install.sh
echo
~/.local/bin/claude-burst status 2>&1 | head -20 || true
echo
echo "Done. A Claude Code session open from before that now fails: restart it with claude --resume (keeps its history)."
# The dashboard, once it answers.
ADMIN=$(python3 -c 'import json,os;print(json.load(open(os.path.expanduser("~/.config/claude-burst/config.json"))).get("admin_listen") or "127.0.0.1:7788")' 2>/dev/null || echo 127.0.0.1:7788)
for i in {1..30}; do curl -s -m 2 -o /dev/null "http://$ADMIN/" && break; sleep 1; done
open "http://$ADMIN/"
EOF
chmod +x ~/burst-update.sh && ~/burst-update.sh
```

### Bypass Burst

Claude Code talks to Anthropic directly; sessions already open keep working:

```sh
cat > ~/burst-bypass.sh <<'EOF'
#!/bin/zsh
# Take Claude Burst out of the path. Undo with ~/burst-update.sh or: claude-burst enable
if [[ -x ~/.local/bin/burst-off ]]; then exec ~/.local/bin/burst-off; fi
# Installs older than v0.11 have no burst-off: the same steps by hand.
BIN=~/.local/bin/claude-burst
CFG=~/.config/claude-burst
mkdir -p $CFG && date > $CFG/rolled-back   # the watchdog leaves a stopped gateway alone
[[ -x $BIN ]] && $BIN passthrough --detach 2>/dev/null
if grep -q '# BEGIN claude-burst' /etc/hosts; then   # the redirect first: it affects every app
  if [[ -x /usr/local/libexec/claude-burst/transparent-root.sh ]]; then
    sudo /usr/local/libexec/claude-burst/transparent-root.sh remove
  else
    sudo sed -i '' '/# BEGIN claude-burst/,/# END claude-burst/d' /etc/hosts
    sudo dscacheutil -flushcache; sudo killall -HUP mDNSResponder
  fi
fi
python3 - <<'PY'
import json, os
p = os.path.expanduser("~/.claude/settings.json")
c = os.path.expanduser("~/.config/claude-burst/config.json")
try:
    s = json.load(open(p))
except Exception:
    raise SystemExit
env = s.get("env", {})
url = env.get("ANTHROPIC_BASE_URL", "")
if url.startswith(("http://127.0.0.1:", "https://127.0.0.1:", "http://localhost:")):
    try:
        adopted = json.load(open(c)).get("adopted_base_url", "")
    except Exception:
        adopted = ""
    if adopted:
        env["ANTHROPIC_BASE_URL"] = adopted
    else:
        env.pop("ANTHROPIC_BASE_URL")
    json.dump(s, open(p, "w"), indent=2)
    print("settings.json: ANTHROPIC_BASE_URL no longer points at Burst")
PY
launchctl bootout "gui/$UID/ninja.andrewbaker.claude-burst" 2>/dev/null
KEEP=$(cat $CFG/passthrough.pid 2>/dev/null)
for port in 7777 17777 7788 7779; do
  for pid in $(lsof -nP -t -iTCP:$port -sTCP:LISTEN 2>/dev/null); do
    [[ "$pid" == "$KEEP" ]] && continue
    echo "port $port: stopping pid $pid ($(ps -o comm= -p $pid))"
    kill $pid 2>/dev/null || sudo kill $pid
  done
done
code=$(curl -s -m 8 -o /dev/null -w '%{http_code} %{remote_ip}' https://api.anthropic.com/)
echo "api.anthropic.com answers directly: $code"
echo "Burst is off. A Claude Code session that still fails: restart it with claude --resume (keeps its history)."
EOF
chmod +x ~/burst-bypass.sh && ~/burst-bypass.sh
```

With v0.11 or later, `burst-off` (on your PATH) does the same as the bypass script, and `scripts/rollback.sh` does it from a checkout.## Quickstart

Needs macOS with Go 1.23+, the Xcode Command Line Tools and Claude Code already logged in; see [Requirements](#requirements).

```bash
# 1. Clone the newest release tag from the Releases page (v0.14.0 at the time of writing)
git clone --branch v0.14.0 https://github.com/andrewbakercloudscale/claude-burst.git
cd claude-burst

# 2. Build, install and start the gateway. It asks once for transparent mode (recommended:
#    Remote Control keeps working; needs your password) or base-url (no password, RC off).
#    CLAUDE_BURST_MODE=transparent|base-url ./install.sh skips the question.
./install.sh

# 3. Optional: a secondary to overflow to when a limit is hit. Skip it for a single plan.
claude-burst configure --secondary openai-compatible \
  --secondary-base-url https://api.together.xyz/v1 --secondary-model zai-org/GLM-5.3
TOGETHER_API_KEY='your-key' claude-burst keychain-set --provider together
launchctl kickstart -k gui/$UID/ninja.andrewbaker.claude-burst   # routing is read at startup

# 4. Restart Claude Code, then check
claude-burst status
curl -s http://127.0.0.1:7777/healthz        # base-url mode only
open http://127.0.0.1:7788                   # the dashboard

# Undo
./install.sh uninstall
```

If macOS asks whether "claude-burst" may access your Desktop (or Documents) folder after every update, run `scripts/signing-setup.sh` once: it signs builds with a local certificate so one Allow lasts. The dashboard's **Permissions** section shows whether the running build is signed and what macOS has allowed under Desktop, Documents and Downloads, and has buttons to set up signing and open the Privacy settings. The installer and `scripts/deploy.sh`, when run at a Terminal, press its **Check folder access** for you straight after starting the gateway, so macOS asks while you are there: click Allow on each. While one sits unanswered, the gateway's update check waits behind it and times out.

OpenRouter, Bedrock and a metered API key are in [Providers](docs/providers.md). To keep Claude Code's Remote Control, switch to [transparent mode](docs/transparent-mode.md) from the dashboard's **Install** section once this works.

## What it does

**Keep working past a limit.** Your Claude login stays the primary credential. Burst watches Anthropic's own subscription rate-limit headers, and only when Anthropic says a model's allowance is actually exhausted does it send *that model's* requests elsewhere: first to other Claude models on your own plan (Fable to Opus), then to a secondary you pay for (Together AI, OpenRouter, any OpenAI-compatible endpoint, or Amazon Bedrock). It returns to the subscription when the reset time arrives. A bare 429 never triggers it, and overflow requests are pruned of old tool output before they are sent. See [Routing and failover](docs/routing.md) and [Providers](docs/providers.md).

**Long sessions without the pause** (Leading Edge, off by default). Every turn resends the whole conversation, so a turn at 400k tokens uses your limits about four times as fast as one at 100k, and Claude Code only compacts near the end of its 1M window, stopping the session while it does. Burst compacts much earlier, in the background, and swaps the summary in on your next prompt; `/compact-async` does it on demand. In two days of real use (one person, long Opus sessions) it saved about $26 a day of **API-equivalent** value, net of the summaries' own cost. On a subscription that is not money back: your bill does not change, your limits last longer. See [Pauseless compaction](docs/compaction.md).

**Codex too.** OpenAI's Codex, signed in with ChatGPT, can go through Burst as well: one button on the dashboard's Codex tab (or `claude-burst codex enable`). Requests reach ChatGPT unchanged; the Codex tab shows tokens, each session's context against its window, and the ChatGPT plan's limits. See [Codex](docs/codex.md).

**Several sessions, one working tree** (Leading Edge, off by default). Several Claude Code sessions, and their background subagents, can edit the same repository without overwriting, sweeping up or shipping each other's uncommitted work, and nobody waits: the first editor of a file commits it, and is asked to commit other sessions' changes first. See [Session coordination](docs/coordination.md).

**Session handover.** In a repository that opts in, each session reads `HANDOFF.md` when it starts and writes it when it closes, so the next one picks up where the last left off. Gitignore it to keep the notes local. See [Session handover](docs/handover.md).

**Lid shut, offline.** Close the lid and Claude Code keeps running, still reachable from your phone through Remote Control, and when the internet drops Burst joins the phone hotspot you picked. See [Lid shut, hotspot and notifications](docs/lid-and-hotspot.md).

**See everything.** The dashboard on `http://127.0.0.1:7788` shows health checks, routing, spend by model and by repository, compaction savings, who is editing what, and a **Needs attention** list. A terminal usage panel shows the same beside each session, and the `burst-band` mod shows it inside the session itself: Burst's route, the context Burst really sends and its compaction state above the prompt, a context bar of what that context is made of (`/context-bar` hides it), the panel's summary in `/burst`, and Burst's alerts and Pauseless Compaction's lines as toasts (each shown once: an alert toasted in the session is not also a Ghostty pop-up, and a compaction line is not repeated under the prompt). Where the usage panel's sidebar is installed, the sidebar's own ctx bar becomes Burst's context bar and lists standing problems, so the band stands aside and Burst's route and any standing problem sit in Claude Code's status line instead (`/burst band` switches between the two) (Claude Code 2.1.287 or later; mods do not draw on Remote Control). See [The dashboard](docs/dashboard.md).

**Keep Remote Control.** Claude Code turns Remote Control off whenever `ANTHROPIC_BASE_URL` names anything but Anthropic. Transparent mode leaves that variable alone and redirects at DNS instead, with a local certificate. See [Transparent intercept mode](docs/transparent-mode.md).

**One plan only** (Claude Enterprise, or a single Pro/Max subscription)? No secondary is needed. Compaction, fallback models on your own plan, handover, coordination and the dashboard all work as they are, and with nowhere to overflow to, Anthropic's own responses, a limit included, reach Claude Code unchanged. **No subscription?** A metered Anthropic API key works as the primary; failover then waits for sustained failures rather than a limit header. See [Providers](docs/providers.md#no-subscription-setup-metered-api-key-primary).

Every setting is listed in [Configuration](docs/configuration.md).

## Trust and risk

Claude Burst is a man-in-the-middle for your Claude traffic by design. This is what that means, and how to take each part away.

**The local CA (transparent mode only).** Burst generates its own certificate authority on your Mac. The CA and its private key live in `~/.config/claude-burst/ca/` (the key is mode 0600 in a 0700 directory) and never leave the machine. The CA is name-constrained to the intercepted host, `api.anthropic.com`, so it cannot vouch for any other site even where it is trusted. The dashboard's transparent install trusts it in two places: Claude Code's `NODE_EXTRA_CA_CERTS` bundle, and system-wide in the **System keychain**, so other apps that reach `api.anthropic.com` (such as Claude Desktop's updater) do not fail their handshakes. Installs made before the name constraint regenerate the CA and re-trust it once. Remove the system-wide trust with `sudo scripts/untrust-ca-systemwide.sh`, the bundle entry with `claude-burst disable`, and the CA itself by deleting `~/.config/claude-burst/ca/`.

**The redirect is machine-wide.** In transparent mode `/etc/hosts` and a pf rule send every process's `api.anthropic.com` traffic to the gateway, so if the gateway is down, nothing on the Mac reaches Anthropic. Two guards watch for that, and the pf guard removes the redirect rather than leave the Mac cut off. `sudo scripts/transparent-root.sh remove` undoes it; `scripts/rollback.sh` undoes everything even when the gateway is dead ([Emergency recovery](ROLLBACK.md#emergency-recovery-without-this-machines-help)).

**The gateway has no login.** It listens on loopback only (`127.0.0.1:7777`, dashboard on `127.0.0.1:7788`). Any local process can talk to it, as it could talk to Anthropic with your credential. What keeps web pages out is an Origin and Host guard on the gateway, and on the dashboard a `Host` check plus a custom header on every change, which a cross-origin page cannot send without a preflight the server never answers. Anything needing root opens in Terminal for you to read and approve; the dashboard never escalates by itself. Details in [The dashboard](docs/dashboard.md#the-local-admin-ui) and [Known limitations](docs/limitations.md).

**Where your requests go.** Subscription requests go to Anthropic unchanged, with Claude Code's own login. Overflow requests go to the secondary you configured, with the whole (pruned) conversation, under that provider's terms. Pauseless compaction and the handover writer send requests Claude Code did not send itself: summaries, on your subscription, with your session's login, which count against your limits like any other request.

**What is stored.** The logs (`claude-burst.log`, `metrics.jsonl`) are metadata only: no prompts, code, tool inputs or model output. Compaction summaries are conversation content, stored in `~/.config/claude-burst/compaction-state.json` at mode 0600 and removed once a session has gone 48 hours without a request. Handover writes into `HANDOFF.md` only in repositories that opt in. API keys are in the macOS login Keychain. See [What is logged](docs/logging.md).

**Removing it.** `./install.sh uninstall` removes every part and verifies it, keeping `~/.config/claude-burst` unless you pass `--purge` (see [Uninstall](#uninstall)). [ROLLBACK.md](ROLLBACK.md) lists each change and its own undo, so you can remove one part and keep the rest. To report a vulnerability, see [SECURITY.md](SECURITY.md).

## Requirements

- **macOS** on Apple Silicon or Intel. Developed and tested on macOS 26 (26.5); expected to need macOS 14.4 or later, since the hotspot watcher is built around a `networksetup` change in 14.4. Older versions are untested.
- **Go 1.23 or later.** There is no prebuilt binary in the repository; `install.sh` builds one.
- **Xcode Command Line Tools** (`xcode-select --install`), for `git` and `python3`, which the installer and scripts use.
- **zsh**, the macOS default shell: the installer and scripts are zsh.
- **Claude Code**, logged into the account you want to use (subscription mode), **or** a metered Anthropic API key.
- **Ghostty** is assumed by the lid-shut feature (it turns off Ghostty's App Nap), by the handover's window-close handling and by the usage panel's split. Everything else works in any terminal.
- **Optional:** a key for a secondary (Together AI, OpenRouter, another OpenAI-compatible endpoint, or Amazon Bedrock). See [Providers](docs/providers.md).

`./install.sh` builds `claude-burst` into `~/.local/bin`, writes the initial configuration, adds `~/.local/bin` to `~/.zprofile` if needed, sets `ANTHROPIC_BASE_URL=http://127.0.0.1:7777` in `~/.claude/settings.json` (and adds no credential of its own, so your saved login stays in use), and starts the LaunchAgent. At the end it offers the optional [usage panel](https://github.com/andrewbakercloudscale/claudecode-cost-usage-panel), a separate repository; `CLAUDE_BURST_PANEL=yes` or `no` answers without the prompt. It also installs the `burst-band` mod from `mods/burst-band` (`CLAUDE_BURST_MOD=no` skips it), and every deploy brings the installed copy up to date.

## Commands

```text
claude-burst serve
claude-burst configure --secondary openai-compatible \
  --secondary-base-url https://api.together.xyz/v1 --secondary-model zai-org/GLM-5.3
claude-burst configure --secondary none
claude-burst keychain-set --provider together   # reads TOGETHER_API_KEY into the Keychain
claude-burst keychain-set --provider together --service my-service   # store under a custom service name
claude-burst enable
claude-burst disable                         # off; sessions already open keep working (a pass-through on the port)
claude-burst status
claude-burst reset                           # back to primary now
claude-burst force-secondary --minutes 15    # route to the secondary on purpose (testing)
claude-burst stats --days 30
claude-burst coord status                    # session coordination: who masters which file
claude-burst coord send <session> "message"  # message another session (id prefix)
claude-burst coord take <path> --session <id>      # become a file's master, or ask its master for it
claude-burst coord release <path> [--session <id>] # hand a file on, as if its master had ended
claude-burst uninstall-hooks                 # remove every Claude Code hook Burst added (./install.sh uninstall runs it)
claude-burst version

claude-burst configure --keep-awake-lid-closed true|false   # lid shut: keep Claude Code + Remote Control running
claude-burst configure --keep-awake-power ac|always         # ac (default): only while plugged in
sudo scripts/lid-awake-root.sh apply ac|always              # the root half; configure prints this
sudo scripts/lid-awake-root.sh remove
scripts/lid-awake-root.sh status

# Inside Claude Code (installed while Pauseless Compaction is on)
/compact-async                               # compact now, in the background, no pause

# Amazon Bedrock secondary (overflow only)
claude-burst configure --secondary bedrock --region us-east-1
claude-burst keychain-set                    # reads AWS_BEARER_TOKEN_BEDROCK
claude-burst configure --primary anthropic-api-key --secondary bedrock
```

## Uninstall

```bash
./install.sh uninstall           # keeps ~/.config/claude-burst
./install.sh uninstall --purge   # also deletes ~/.config/claude-burst
```

It runs in this order, and each step is the same undo script you could run by hand:

1. **Root steps, only for what is actually installed.** It lists them first, then sudo asks for your password once. A base-url install with nothing machine-wide asks for no password.
   - the pf self-heal LaunchDaemon: `sudo scripts/install-pf-heal.sh uninstall`
   - transparent mode's `/etc/hosts` redirect, pf rule and anchor: `sudo scripts/transparent-root.sh remove` (run whenever the hosts block, the pf.conf reference, the anchor file or `intercept.mode: transparent` is found)
   - the admin hostname entry in `/etc/hosts`: `sudo scripts/transparent-root.sh admin-host-remove`
   - the local CA in the System keychain: `sudo scripts/untrust-ca-systemwide.sh`
   - the lid-closed keep-awake LaunchDaemon, restoring `SleepDisabled` to its prior value: `sudo scripts/lid-awake-root.sh remove`

   If `/etc/hosts` still redirects api.anthropic.com after this, the uninstall stops here with the gateway still running, because removing the gateway then would cut the whole Mac off from Anthropic.
2. **The self-heal watchdog LaunchAgent** (`scripts/install-selfheal-watchdog.sh uninstall`), then the gateway is stopped. Both come before the hooks: the watchdog restarts the gateway, and a gateway that starts reinstalls its hooks.
3. **Claude Code settings, while the binary still exists:** `claude-burst uninstall-hooks` removes the token-shunting hook and skill, the session coordination hooks, the handover hooks, the prompt notice hooks and `~/.claude/commands/compact-async.md`, touching nothing else in `settings.json`; then `claude-burst disable` removes `ANTHROPIC_BASE_URL` (base-url mode) or the CA from Claude Code's CA bundle (transparent mode).
4. **The gateway LaunchAgent and the binary** (`~/.local/bin/claude-burst`), and Ghostty's App Nap override. With `--purge`, `~/.config/claude-burst` too.
5. **A check.** It exits non-zero, naming each leftover and the command that removes it, if `/etc/hosts` still has a claude-burst block, the pf anchor is still referenced or loaded, the pf self-heal daemon or the System-keychain CA is still there, or `~/.claude/settings.json` still mentions claude-burst. Success is printed only when every check passes. The loaded pf anchor can only be read as root, so it is checked only when sudo has a cached password from step 1.

Kept: `~/.config/claude-burst` (config, state, metrics, logs, backups; unless `--purge`), with `config.json` unchanged so a reinstall comes back with the same features on; the secondary key in the macOS Keychain (the uninstall prints the `security delete-generic-password` command for it); `/var/log/claude-burst-pf.log`; the PATH line in `~/.zprofile`; and the usage panel, which has its own uninstaller.

## Contributing and tests

`go test ./... -race` and `go vet ./...` run on every push. How to build, test and send changes, and the style rules, are in [CONTRIBUTING.md](CONTRIBUTING.md). Design notes, decisions and past investigations are under [docs/](docs/): [design proposals](docs/design/), [decisions](docs/decisions/) and [history](docs/history/).

## Terms and design notes

Anthropic's current Consumer Terms prohibit account sharing and prohibit bypassing protective measures. This project is designed around one subscription account per user and treats Anthropic's quota rejection as final for that subscription window. It does not rotate accounts, suppress quota signals or fabricate headers. When a limit is reached it first tries other Claude models on the same plan, then makes a separate, paid request through a provider you have chosen and pay for yourself: Together AI, OpenRouter or another OpenAI-compatible endpoint (usually serving a non-Claude model such as GLM), Amazon Bedrock, or a metered Anthropic API key.

Two parts of the design go beyond a plain gateway, and you should weigh them. **Transparent mode intercepts TLS** for `api.anthropic.com` on your own Mac, with a CA generated there, so Claude Code believes it is talking to Anthropic directly. Anthropic documents `ANTHROPIC_BASE_URL` gateways; DNS-level interception is this project's choice, made to keep Remote Control, not a documented mechanism. And **the gateway originates requests of its own**: pauseless compaction's summaries and the handover writer run on your subscription, with your session's login, and count against your limits.

Anthropic also documents the concept of switching Max users to metered API credits after included usage is exhausted. Claude Burst applies the same base-plus-overflow idea to a metered channel you control. This is a technical interpretation, not legal advice, and an organisation's deployment should still be reviewed against the contracts it has actually signed.

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
