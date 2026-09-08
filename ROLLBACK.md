# Rollback notes — transparent intercept mode

Written 2026-08-30. Covers every change made while building the optional
transparent intercept mode, and how to undo each one independently.

## TL;DR — what is on my machine right now?

Updated 2026-09-08. **Transparent mode is installed and live** — the opposite of what
this section said on 2026-08-31, when only base-url mode had ever been enabled. Every
line below was verified against the machine, not inferred from config.

**Deployed and live:**
- Gateway binary `0.2.0` at `~/.local/bin/claude-burst`, running under LaunchAgent
  `ninja.andrewbaker.claude-burst`, serving **HTTPS** on `127.0.0.1:17777`. Note the port:
  this machine was moved off 7777 on 2026-09-08 chasing issue #1, the move did not fix it
  (see INVESTIGATION-TLS-STORM.md update (b)), and the shipped default went back to 7777
  while this machine stayed on 17777. `config.json` names it explicitly, so nothing infers it.
- **Do not health-check the gateway by connecting to its port directly.** While the pf
  redirect is installed, the rdr rule makes its own target port very nearly unreachable — a
  direct `curl https://127.0.0.1:17777/healthz` times out roughly nineteen times in twenty,
  with the gateway perfectly healthy. Note the *roughly*: an occasional probe succeeds, which
  is precisely what makes this so good at wasting a morning. Probe the real path instead:
  `curl -sk https://api.anthropic.com/healthz`, which answers from the gateway and whose body
  contains `"overflow"`.
- **Admin UI on <http://127.0.0.1:7788>** (loopback only, no login).
- Primary `oauth-passthrough` → `api.anthropic.com`, failover strategy
  `subscription-limit+metered-failures`. Secondary `openai-compatible` → Together AI
  (`zai-org/GLM-5.3`), key in Keychain service `claude-burst-together`.

**Transparent intercept mode — installed, all four parts present:**
- `intercept.mode` is `transparent`, host `api.anthropic.com`.
- `/etc/hosts` has the `# BEGIN claude-burst hosts` block redirecting
  `api.anthropic.com` → `127.0.0.1`.
- pf: `/etc/pf.anchors/claude-burst` holds the `rdr pass … port 443 -> … port 17777`
  rule, and `/etc/pf.conf` carries both the `rdr-anchor` and `load anchor` marker blocks.
- Local CA in `~/.config/claude-burst/ca/` (CA `claude-burst local CA`, valid to
  2036-08-28; leaf for `api.anthropic.com`, valid to 2027-10-02). Trusted **twice**, and
  the two are removed separately: inside the `# BEGIN claude-burst CA` block of
  `~/.claude/certs/node-extra-ca-certs.pem` (13 certificates, 12 of them the
  corporate ones — see *The CA bundle* below), and as a trusted root in the **System
  keychain** (`scripts/trust-ca-systemwide.sh`, added because Claude Desktop's updater
  has no idea about `NODE_EXTRA_CA_CERTS` — see INVESTIGATION-TLS-STORM.md).
- `~/.claude/settings.json` has **no** `ANTHROPIC_BASE_URL`, which is the whole point:
  Remote Control keeps working. Do not set it while this mode is installed.
- Live path confirmed, not just configured: the dashboard's **Test connection** reports
  `https://api.anthropic.com/healthz` resolving to this gateway.

**Both guards armed and beating:**
- `ninja.andrewbaker.claude-burst-pfheal` — root LaunchDaemon, guards the pf rdr rule,
  log `/var/log/claude-burst-pf.log` (empty, which is the good case).
- `ninja.andrewbaker.claude-burst-selfheal` — user LaunchAgent, guards the gateway
  process, log `~/.config/claude-burst/self-heal.log`.
- Both write a heartbeat every ~2 min; the dashboard reads the heartbeat, never
  launchd's opinion. No `rolled-back` marker is present, so neither is standing down.

**If Claude Code — or this Mac — is broken and you want out fast:**

```sh
scripts/rollback.sh                       # /etc/hosts + pf FIRST, then System-keychain
                                          # CA trust, then settings.json/config.json/CA
                                          # bundle from backup; verifies direct reach
```
Then restart Claude Code. Idempotent, and safe when nothing was installed. It is
also what the dashboard's **Revert** button now runs. If you only want the
machine-wide half gone: `sudo scripts/transparent-root.sh remove`.

Because transparent mode IS installed, the redirect is machine-wide: while it is in
place and the gateway is down, *every* process on this Mac fails to reach
`api.anthropic.com`, not just Claude Code. If you cannot even clone the repo:

```sh
sudo sed -i '' '/# BEGIN claude-burst hosts/,/# END claude-burst hosts/d' /etc/hosts
sudo dscacheutil -flushcache
```

**To go back to the previous binary only:**

```sh
cp ~/.config/claude-burst/backups/claude-burst-bin.latest.bak ~/.local/bin/claude-burst
launchctl kickstart -k gui/$UID/ninja.andrewbaker.claude-burst
```

## Run these scripts by path, not `bash script.sh`

Every script here is `#!/bin/zsh` and several use zsh-only syntax. Invoking one as
`bash scripts/rollback.sh` does **not** fall back gracefully:

- `rollback.sh`, `deploy.sh`, `watchdog.sh` used zsh's `${0:A:h}` to find their own
  directory. Under bash that is an unbound-variable error on line 2, so the script exits
  having done nothing. These now use a POSIX form and work under either shell.
- `transparent-root.sh` uses `${(@f)}` and `print -r` and cannot be made bash-compatible
  without rewriting it. Bash fails at *parse* time, so it cannot even self-correct by
  re-execing under zsh.

An earlier version of this document told you to run `bash scripts/rollback.sh` in an
emergency. That command would have silently done nothing. Invoke by path
(`scripts/rollback.sh`) and the shebang picks the right shell.

## Why any of this exists

Claude Code disables Remote Control whenever `ANTHROPIC_BASE_URL` names a host
other than `api.anthropic.com` — a check on the literal variable value, not on
where traffic ends up (documented, since v2.1.196). So enabling the gateway
costs `/remote-control`.

Transparent mode leaves that variable unset and gets into the path at the DNS
layer instead: `/etc/hosts` points `api.anthropic.com` at the gateway, which
terminates TLS with its own CA. Claude Code believes it reached Anthropic, so
Remote Control keeps working.

**It is opt-in.** `intercept.mode` defaults to `base-url`, which is the
original behaviour. A config with no `intercept` block behaves exactly as it
did before any of this existed.

## What changed, and how to undo each piece

### 1. Committed and pushed — `8a96ea9`

| file | change |
|---|---|
| `internal/config/config.go` | `InterceptConfig` block, defaults, validation |
| `internal/tlsca/` *(new)* | local CA + leaf; CA-bundle marker-block editing |
| `internal/router/resolver.go` *(new)* | DoH dialer (prevents the gateway calling itself) |
| `internal/router/router.go` | wires the resolver; separate client for long-polls |
| `scripts/check-interception.sh` *(new)* | corporate-TLS-inspection check |

**Inert.** Every new path is gated on `cfg.Intercept.Transparent()`, which is
false unless explicitly configured. `serve`, `enable` and `disable` are not yet
wired to any of it.

Undo:
```sh
git revert 8a96ea9      # keeps history
# or, to erase it entirely (rewrites a pushed commit):
git reset --hard 3b94faf && git push --force-with-lease origin main
```

### 2. Later commits

| commit | what |
|---|---|
| `8fcb727` | `transparent-root.sh` root helper; rollback + backup cover `/etc/hosts` and the CA bundle |
| `ca7eb6c` | transparent mode wired into `serve`/`enable`/`disable`/`status`; two CLI bug fixes |
| `a45eafe` | admin UI on `127.0.0.1:7788` |
| `16c7610` | `force-secondary`; fixes `--secondary none` never having worked |

All are inert with respect to transparent mode until `intercept.mode` is set. The admin UI
is the one user-visible change from deploying, and it binds loopback only.

### 3. The pf spike — already reverted

`pf-spike.sh` (in the session scratchpad, not in the repo) loaded a temporary
rdr rule into a sub-anchor of `com.apple/*` to prove the redirect works on
macOS 26.5.2. It flushed the anchor and released pf's enable token on exit, and
its own output confirmed `pf now: Status: Disabled` — the state it started in.
**Nothing to undo.** `/etc/pf.conf` was never touched.

## Rolling back each piece of transparent mode

All of this is **in play** — transparent mode is installed on this machine (see
the TL;DR). This section was written while it was still hypothetical, so that
the recovery path existed before the thing it recovers; it is now the live
undo procedure, not a contingency. Each piece below can be removed on its own,
and `scripts/rollback.sh` does the lot in the safe order.

### `/etc/hosts` and pf

```sh
sudo scripts/transparent-root.sh remove
```
Removes the `/etc/hosts` block, the `/etc/pf.conf` anchor references and
`/etc/pf.anchors/claude-burst`, reloads the pf ruleset, restores pf's previous
enabled/disabled state (it records whether it was the one that enabled pf), and
flushes the DNS cache. Idempotent; safe when nothing was installed.

Pre-install copies are also kept at `/etc/claude-burst/hosts.pre-install.bak`
and `/etc/claude-burst/pf.conf.pre-install.bak`.

To check without changing anything:
```sh
sudo scripts/transparent-root.sh status
```
Watch for `live rdr rule : MISSING` while the hosts entry is present — that is
the one genuinely bad state (DNS redirects, nothing listens), and it means
every process on the Mac that talks to Anthropic will get connection refused.
The fix is `remove`.

### Manual `/etc/hosts` repair, if the script is unavailable

```sh
sudo cp /etc/claude-burst/hosts.pre-install.bak /etc/hosts
sudo dscacheutil -flushcache
sudo killall -HUP mDNSResponder
```
Or edit `/etc/hosts` and delete everything between
`# BEGIN claude-burst hosts` and `# END claude-burst hosts` inclusive.

### The CA bundle

`~/.claude/certs/node-extra-ca-certs.pem` holds **13 certificates**: 12 corporate
ones, including Zscaler and Capitec internal CAs, plus claude-burst's own. Ours
sits inside a `# BEGIN claude-burst CA` / `# END claude-burst CA` block, appended
by transparent mode, which never rewrites the rest. To remove by hand, delete
that block. `scripts/rollback.sh` restores the whole file from the backup taken
by `backup-config.sh`.

If this file is ever lost, corporate TLS breaks machine-wide, not just Claude
Code — treat it as the most sensitive file this tool goes near.

### The binary

`scripts/deploy.sh` already backs up the previous binary and rolls it back
automatically on a failed health check. Manual restore:
```sh
cp ~/.config/claude-burst/backups/claude-burst-bin.latest.bak ~/.local/bin/claude-burst
launchctl kickstart -k gui/$UID/ninja.andrewbaker.claude-burst
```

## Ordering rules that must not be broken

Learned the hard way on 2026-08-30, when an `enable` ran before its gateway was
listening and killed a live session with `Connection refused`.

1. **Back up first.** `scripts/backup-config.sh` before any enable/disable/configure.
2. **The gateway must be healthy before anything points traffic at it.**
   `transparent-root.sh install` refuses to run unless `/healthz` answers, and
   verifies the pf redirect works *before* it touches `/etc/hosts`.
3. **`/etc/hosts` is thrown last and undone first.** It is the machine-wide
   switch, so it is the last thing enabled and the first thing removed.
4. **Arm the watchdog** immediately after enabling:
   `nohup scripts/watchdog.sh & disown`.

## Known unknowns

- **TLS interception is assumed not to break Remote Control.** The evidence is
  that it works on the Capitec/Zscaler network. That could not be verified in
  session: Zscaler was off, and all sampled hosts returned genuine issuers.
  Settle it with `scripts/check-interception.sh` while the tunnel is on —
  it distinguishes *intercepted* from *bypassed* from *not enrolled*, which a
  bare issuer check cannot.
- ~~`internal/keychain` has one failing test on this machine.~~ **Fixed in
  `850a59a`.** The test asserted against the real `claude-burst-together`
  service name, so it passed or failed depending on whether this Mac happened
  to have that credential stored — a result that depended on the developer's
  machine rather than on the code. It now uses a deliberately nonexistent
  service name. `go test ./...` is green locally and in CI.
