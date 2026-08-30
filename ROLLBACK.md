# Rollback notes — transparent intercept mode

Written 2026-08-30. Covers every change made while building the optional
transparent intercept mode, and how to undo each one independently.

## TL;DR — what is on my machine right now?

Updated 2026-08-31, after deploying.

**Deployed and live:**
- New gateway binary at `~/.local/bin/claude-burst`, running under LaunchAgent
  `ninja.andrewbaker.claude-burst`, healthy on `127.0.0.1:7777`.
- **Admin UI on <http://127.0.0.1:7788>** (loopback only, no login).
- `~/.claude/settings.json` has `ANTHROPIC_BASE_URL=http://127.0.0.1:7777` — the same
  `base-url` mode as before this work. Remote Control is disabled while that is set.

**Not installed, and nothing on the machine refers to it:**
- Transparent intercept mode. `intercept.mode` is unset (= `base-url`).
- `/etc/hosts` has **no** claude-burst entry; pf is **Disabled**; no CA has been
  generated; the `NODE_EXTRA_CA_CERTS` bundle is untouched.

**If Claude Code is broken and you want out fast:**

```sh
scripts/rollback.sh                       # settings.json, config.json, CA bundle
sudo scripts/transparent-root.sh remove   # /etc/hosts + pf (safe if never installed)
```
Then restart Claude Code. Both are idempotent and safe when nothing was installed.

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

## Rolling back things that are not yet in play

These become relevant only once transparent mode is actually wired up and
enabled. Listed now so the recovery path exists before the thing it recovers.

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

`~/.claude/certs/node-extra-ca-certs.pem` holds **12 certificates**, including
Zscaler and Capitec internal CAs. Transparent mode appends its own CA inside a
`# BEGIN claude-burst CA` / `# END claude-burst CA` block and never rewrites the
rest. To remove by hand, delete that block. `scripts/rollback.sh` restores the
whole file from the backup taken by `backup-config.sh`.

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
- **`internal/keychain` has one failing test on this machine** — pre-existing
  and unrelated. It expects no Together key; you have one in Keychain. It would
  pass in CI.
