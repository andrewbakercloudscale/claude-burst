# What Claude Burst changes on your Mac, and how to undo it

Every change Claude Burst can make, where it lives, and the command that undoes it. Each part
can be removed on its own. To remove everything at once, use `./install.sh uninstall` (normal
case) or `scripts/rollback.sh` (emergency case, below).

**Stuck right now?** Type `/claude-burst-revert` in Claude Code: it makes no request to the
model, so it works while the gateway is down, and it opens a Terminal window that does the
rest. Or run `burst-off` in any terminal, which is what the command runs. install.sh puts
it on the PATH as a copy of `scripts/rollback.sh`, so it works even if the checkout has
moved. It also stops anything holding Burst's ports. `claude-burst enable` turns Burst back
on.

## Remove everything

```sh
./install.sh uninstall            # everything below, with one sudo prompt if anything needs root
./install.sh uninstall --purge    # the same, and delete ~/.config/claude-burst too
```

`./install.sh uninstall` removes the transparent mode redirect and pf anchor, the
System-keychain CA trust, the admin hostname entry, the pf guard and the lid keep-awake
daemon (asking for sudo only when one of them is there), then the gateway watchdog, every
Claude Code hook Burst added, the LaunchAgent and the binary. It then checks that each one is
gone and exits non-zero, naming what is left and the command that removes it, if anything
remains. If `/etc/hosts` still redirects `api.anthropic.com` after the root steps, it stops
with the gateway still running, because removing the gateway then would cut the whole Mac off
from Anthropic.

It keeps `~/.config/claude-burst` (configuration, metrics, logs, backups and the CA) unless you
pass `--purge`, so an accidental rerun does not wipe them. It always keeps the secondary's key
in the Keychain (it prints the `security delete-generic-password` command), the PATH line in
`~/.zprofile`, `/var/log/claude-burst-pf.log` and the usage panel. The README's
[Uninstall](README.md#uninstall) section lists the steps in order.

## Emergency recovery, without this machine's help

Everything the dashboard offers goes through a gateway that is running. When it is
not, the gateway is dead, `127.0.0.1:7788` refuses to connect, and transparent mode's
`/etc/hosts` redirect is still pointing every process on the Mac at nothing, the
dashboard cannot help you, and neither can any button on it.

That case is why the whole recovery chain is committed here and nothing in it needs
this project to be installed, built, or working:

```sh
git clone https://github.com/andrewbakercloudscale/claude-burst.git
cd claude-burst
scripts/rollback.sh          # asks for sudo; undoes every part, verifies the result
```

`rollback.sh` removes the `/etc/hosts` redirect and pf rule first (widest blast radius
first), then the System-keychain CA trust, then removes only Claude Burst's own entries
from `settings.json` (a loopback `ANTHROPIC_BASE_URL` or proxy) and the CA bundle (its
marked block), keeping everything else, such as your hooks, a Portkey or corporate gateway
and your employer's CAs. A copy of each is kept first. It restores Burst's own `config.json`
from the latest backup, stops the gateway, and **verifies
`api.anthropic.com` resolves off-box before it claims success**. It is idempotent: safe
when nothing was installed, when half an install succeeded, and twice in a row. It is also
what the dashboard's **Revert to normal Claude** button runs.

If you cannot even clone, the two commands that matter are short enough to type:

```sh
sudo sed -i '' '/# BEGIN claude-burst hosts/,/# END claude-burst hosts/d' /etc/hosts
sudo dscacheutil -flushcache
```

That alone restores Anthropic access machine-wide; everything else is tidy-up.

## Each part, and its undo

### Always installed

| What | Where | Undo |
|---|---|---|
| The binary | `~/.local/bin/claude-burst` | `rm ~/.local/bin/claude-burst` |
| A `PATH` line | `~/.zprofile`, the line ending `# claude-burst` | delete that line |
| The gateway LaunchAgent | `~/Library/LaunchAgents/ninja.andrewbaker.claude-burst.plist` | `launchctl bootout gui/$UID/ninja.andrewbaker.claude-burst`, then delete the plist |
| The support console LaunchAgent (127.0.0.1:7789) | `~/Library/LaunchAgents/ninja.andrewbaker.claude-burst-console.plist` | `scripts/install-console.sh uninstall`. `burst-off` leaves it running on purpose: it is how you turn Burst back on |
| Codex routing | Burst's marked block at the top of `~/.codex/config.toml`, only when routed | `claude-burst codex disable` (or `scripts/codex-unroute.sh`) |
| Configuration, state, logs, metrics, the audit (`audit.jsonl`), backups | `~/.config/claude-burst/` | `rm -rf ~/.config/claude-burst` (this also deletes the CA) |
| The secondary's API key | macOS login Keychain, service `claude-burst-<provider>` | `security delete-generic-password -s claude-burst-together` (or `-openrouter`, `-bedrock`) |

### Claude Code settings and hooks (`~/.claude/`)

| What | When | Undo |
|---|---|---|
| `ANTHROPIC_BASE_URL=http://127.0.0.1:7777` in `settings.json` | base-url mode | `claude-burst disable` |
| Prompt notice hooks (`UserPromptSubmit`, `PostToolUse`) | Pauseless compaction's notices are on | switch the notices off in the dashboard |
| `~/.claude/commands/compact-async.md` | Pauseless compaction is on | switch compaction off (a file of your own with that name is never touched) |
| Handover hooks (`SessionStart`, `SessionEnd`) | Session handover installed | **Remove** in the dashboard's Session handover section |
| Coordination hooks (seven) | Session coordination is on | switch it off in the dashboard |

`claude-burst uninstall-hooks` removes every hook, skill and command Burst put in `~/.claude`
at once, touching nothing else in `settings.json`; `./install.sh uninstall` runs it. Every write
to `settings.json` is backed up first, under `~/.config/claude-burst/backups/`.

### Transparent mode (machine-wide, root)

| What | Where | Undo |
|---|---|---|
| Hosts redirect | `/etc/hosts`, between `# BEGIN claude-burst hosts` and `# END claude-burst hosts` | `sudo scripts/transparent-root.sh remove` |
| pf redirect | `/etc/pf.anchors/claude-burst`, and marker blocks in `/etc/pf.conf` | the same command (it also restores pf's previous enabled state) |
| Local CA and its key | `~/.config/claude-burst/ca/` | `rm -rf ~/.config/claude-burst/ca` after the two lines below |
| CA trusted by Claude Code | a `# BEGIN claude-burst CA` block in the `NODE_EXTRA_CA_CERTS` bundle (default `~/.claude/certs/node-extra-ca-certs.pem`) | `claude-burst disable` (other certificates in the bundle are left untouched) |
| CA trusted system-wide | System keychain, `claude-burst local CA` | `sudo scripts/untrust-ca-systemwide.sh`, or delete it in Keychain Access |
| Pre-install copies | `/etc/claude-burst/hosts.pre-install.bak`, `/etc/claude-burst/pf.conf.pre-install.bak` | `sudo rm -rf /etc/claude-burst` once the above are gone |
| Dashboard hostname (optional) | a separate `/etc/hosts` block | `sudo scripts/transparent-root.sh admin-host-remove` |

To check without changing anything: `sudo scripts/transparent-root.sh status`. Watch for
`live rdr rule : MISSING` while the hosts entry is present: that is the one genuinely bad
state (DNS redirects, nothing listens), and every process on the Mac that talks to Anthropic
gets connection refused. The fix is `remove`.

If the script is unavailable, restore `/etc/hosts` by hand:

```sh
sudo cp /etc/claude-burst/hosts.pre-install.bak /etc/hosts
sudo dscacheutil -flushcache
sudo killall -HUP mDNSResponder
```

Or edit `/etc/hosts` and delete everything between `# BEGIN claude-burst hosts` and
`# END claude-burst hosts` inclusive.

**The CA bundle can hold other people's certificates.** On a machine behind a corporate
proxy, the `NODE_EXTRA_CA_CERTS` bundle holds the employer's CAs too. Burst only ever appends
its own marked block and only ever removes that block. If this file is lost, corporate TLS
can break machine-wide, so treat it as the most sensitive file Burst goes near.

### Guards

| What | Runs as | Undo |
|---|---|---|
| Gateway watchdog, `ninja.andrewbaker.claude-burst-selfheal` | you (LaunchAgent) | `scripts/install-selfheal-watchdog.sh uninstall` |
| pf redirect guard, `ninja.andrewbaker.claude-burst-pfheal` | root (LaunchDaemon), log `/var/log/claude-burst-pf.log` | `sudo scripts/install-pf-heal.sh uninstall` |

### Lid shut and hotspot

| What | Where | Undo |
|---|---|---|
| `pmset disablesleep 1` | machine-wide power setting | `sudo scripts/lid-awake-root.sh remove` (restores the previous value) |
| Lid keep-awake daemon, `ninja.andrewbaker.claude-burst-lidawake` | root LaunchDaemon, copy in `/usr/local/libexec/claude-burst`, log `/var/log/claude-burst-lidawake.log` | the same command |
| Ghostty App Nap off | `defaults` key `NSAppSleepDisabled` for `com.mitchellh.ghostty` | `defaults delete com.mitchellh.ghostty NSAppSleepDisabled` |
| Hotspot password | login Keychain, service `claude-burst-hotspot` | `security delete-generic-password -s claude-burst-hotspot` |

### The usage panel

The optional usage panel is a separate project with its own uninstaller,
`claude-panel-uninstall.sh`, which the dashboard's **Remove** button runs. It is not removed
by `./install.sh uninstall`.

### Going back to the previous binary only

`scripts/deploy.sh` backs up the previous binary and rolls it back automatically on a failed
health check. By hand:

```sh
cp ~/.config/claude-burst/backups/claude-burst-bin.latest.bak ~/.local/bin/claude-burst
launchctl kickstart -k gui/$UID/ninja.andrewbaker.claude-burst
```

## Run these scripts by path, not `bash script.sh`

Every script here is `#!/bin/zsh` and several use zsh-only syntax. Invoke by path
(`scripts/rollback.sh`) and the shebang picks the right shell. `rollback.sh`, `deploy.sh`
and `watchdog.sh` work under bash too, because someone reaching for them in an emergency
will type `bash` as readily as `zsh`; `transparent-root.sh` does not, and bash fails at parse
time.

## Ordering rules that must not be broken

Learned the hard way on 2026-08-30, when an `enable` ran before its gateway was listening
and killed a live session with `Connection refused`. The installer, the dashboard's Install
button (`internal/admin/install.go`) and `scripts/install-proxy.sh` all follow these.

1. **Back up first.** `scripts/backup-config.sh` before any enable, disable or configure.
2. **The gateway must be healthy before anything points traffic at it.**
   `transparent-root.sh install` refuses to run unless `/healthz` answers, and verifies the
   pf redirect works *before* it touches `/etc/hosts`.
3. **`/etc/hosts` is thrown last and undone first.** It is the machine-wide switch, so it is
   the last thing enabled and the first thing removed.
4. **Arm the watchdog** immediately after enabling.
5. **Before running `rollback.sh` for a broken redirect, check the pf guard's own state
   first**: the dashboard's Guards card, `sudo scripts/pf-heal.sh --check` and
   `/var/log/claude-burst-pf.log`. A rollback erases the evidence of why the redirect broke.
6. **`rollback.sh`'s restore point is kept current automatically.** Every `config.json` and
   `settings.json` write also updates `backups/*.latest.bak` from what was just written, so a
   rollback never restores a configuration older than the one in force.
   `scripts/backup-config.sh` is still worth running before a risky change: it also covers
   the CA bundle and `/etc/hosts`.
