# Transparent intercept mode: keeping Remote Control

[Back to the README](../README.md)

Claude Code disables **Remote Control** whenever `ANTHROPIC_BASE_URL` names a host other
than `api.anthropic.com` - a check on the literal variable value, not on where the traffic
ends up ([docs](https://code.claude.com/docs/en/remote-control.md)). The `base-url`
mode sets exactly that variable, so enabling the gateway costs you that feature.

`transparent` mode leaves the variable unset and gets into the path at the DNS layer
instead: `/etc/hosts` points the hostname at the gateway, which terminates TLS with a
locally generated CA. Claude Code believes it reached Anthropic directly, so Remote Control
keeps working.

```bash
claude-burst configure --intercept-mode transparent
claude-burst enable        # generates the CA, trusts it, prints the one sudo step left
sudo scripts/transparent-root.sh install
```

The dashboard's **Install** (Setup menu) does the same in the right order, and also trusts the
CA in the System keychain (`scripts/trust-ca-systemwide.sh`), so other apps that talk to
`api.anthropic.com`, such as Claude Desktop's updater, accept the gateway's certificate
instead of failing their handshakes. The CA is name-constrained: it can only vouch for the
intercepted host, so even trusted system-wide it cannot be used to impersonate any other site.
Installs made before the constraint existed regenerate the CA and re-trust it once. See
[Trust and risk](../README.md#trust-and-risk).

Undo, at any time, idempotent, safe even if nothing was installed:

```bash
sudo scripts/transparent-root.sh remove      # /etc/hosts and pf
sudo scripts/untrust-ca-systemwide.sh        # the System-keychain trust
```

## What it costs

`transparent` is the recommended mode and `base-url` is the fallback. The reason is the first row:
Claude Code turns Remote Control **off** whenever `ANTHROPIC_BASE_URL` names a non-Anthropic host, so
base-url mode buys its simplicity by disabling the feature transparent mode exists to preserve. Choose
base-url when you cannot or would rather not touch system files.

| | `transparent` - recommended | `base-url` - fallback |
|---|---|---|
| Remote Control | **works** | disabled |
| root required | once, for `/etc/hosts` + pf | no |
| certificates | local CA, name-constrained to the intercepted host, trusted by Claude Code (`NODE_EXTRA_CA_CERTS`) and in the System keychain | none |
| blast radius | every process on the machine | this user's Claude Code |
| guards needed | gateway watchdog + pf redirect guard | gateway watchdog |

`install.sh` sets up transparent mode unless you answer no, Claude Code already goes through another
gateway (base-url adopts it), or there is no terminal to ask for the password on. If the transparent
steps cannot finish and nothing was redirected, it falls back to base-url so Burst still works. The
code's own default for an unset mode is still `base-url`, since it needs no privileges.

The last row is the real trade. While the `/etc/hosts` entry exists, *everything* on the
Mac that talks to that hostname goes through the gateway, so if the gateway is down,
Anthropic is unreachable machine-wide, not just in one session. `transparent-root.sh
install` therefore refuses to run unless `/healthz` answers, and verifies the pf redirect
works *before* touching `/etc/hosts`; `remove` undoes `/etc/hosts` first.

## How it avoids calling itself

Once `/etc/hosts` maps the hostname to the gateway, that mapping applies to the gateway's
own upstream requests too, it would call itself, forever. Go consults `/etc/hosts` in both
its cgo and pure-Go resolver modes, so `PreferGo` does not avoid this. The gateway resolves
the intercepted hostname over DNS-over-HTTPS instead (`intercept.resolver_doh`), which never
consults `/etc/hosts`, and dials the returned address while leaving TLS `ServerName` as the
real hostname so certificate verification is unchanged. Only the intercepted hostname is
treated this way. A DoH answer pointing at loopback is rejected outright, that is the
loop's exact shape. Set `intercept.upstream_addr` to pin an IP where DoH is blocked.

## Checking whether it is working

```bash
claude-burst status                          # CA trusted? hosts entry? certificate?
sudo scripts/transparent-root.sh status      # pf rule actually loaded?
curl -sk https://api.anthropic.com/healthz   # does the REAL path answer from the gateway?
```

Use the last one rather than a direct probe of the gateway port. **A direct
`curl http://127.0.0.1:7777/healthz` will almost certainly time out in this mode, and that is
expected.** While the pf redirect is installed, direct connections to the gateway's own port
nearly always fail: the rule makes its own target port unreachable, whichever port it targets.
Measured at 0/20 and 1/15 in separate runs, so roughly one attempt in twenty does get
through: enough that a single lucky probe can convince you the port is fine, not enough to
build a health check on ([issue #1](https://github.com/andrewbakercloudscale/claude-burst/issues/1),
and [the investigation notes](history/investigation-tls-storm.md) for the measurements).

A JSON body containing `"overflow"` from `https://api.anthropic.com/healthz` means it came
from Claude Burst. Anthropic's real `/healthz` does not return that, so this cannot produce a
false positive. The admin dashboard's **Test connection** button runs exactly this check.

If you want to see *why* for yourself, `scripts/diagnose-direct-port.sh` snapshots pf's
loaded rules, states and drop counters around a burst of failing probes and names the
counter that moved. It is read-only, every `pfctl` call is a `-s` show, so it is safe to
run mid-incident:

```bash
sudo scripts/diagnose-direct-port.sh          # writes a timestamped log you can attach to a bug
```

On this machine it shows `state-insert` climbing by ~14 per probe while every filter and
block counter stays at zero: pf is failing to *insert state* for these connections, not
filtering them. See [the investigation notes](history/investigation-tls-storm.md).

Watch for `live rdr rule : MISSING` while the hosts entry is present. That is the bad
state, DNS redirects but nothing listens, and the fix is `remove`.

## Guards

Two background jobs keep this working while nobody is watching, and **the dashboard shows whether each
is armed, when it last checked and what it has caught**, with a button to install either:

| Guard | Runs as | Covers |
|---|---|---|
| **Gateway watchdog** (`install-selfheal-watchdog.sh`) | you | the gateway's process dying. Checks it has a *pid*, not merely that launchd still has the job registered, launchd goes on answering for a job whose process has exited |
| **pf redirect guard** (`install-pf-heal.sh`) | root | traffic not reaching the gateway, whatever the cause. Probes the real path end to end rather than any one component |

Both are needed in transparent mode; base-url mode needs only the watchdog.

## Guarding the pf rule

The rdr rule is the one part of this that something else on your Mac can take away.
On 2026-09-07 it vanished from the loaded ruleset while `/etc/hosts` stayed, and every
process on the machine got `connection refused` for `api.anthropic.com` for hours. The
anchor file and the `pf.conf` reference were both still perfectly intact, only the
*loaded* ruleset had lost the rule, so every check that read configuration said OK.
Other pf-owning software (VPN and endpoint-security clients, in this case Zscaler and
CrowdStrike) reloads pf on network change and on wake; a `load anchor` line does not
guarantee the rule stays loaded.

So arm the healer. The dashboard's **pf redirect guard** panel says whether it is armed,
when it last checked, and what it has caught, and arms it for you. `install-proxy.sh`
also does it during a transparent install. By hand:

```bash
sudo scripts/install-pf-heal.sh          # root LaunchDaemon, every 30s + on network change
scripts/install-pf-heal.sh status        # armed? what has it caught?
tail -f /var/log/claude-burst-pf.log     # world-readable, no sudo needed
```

Each cycle it does nothing at all unless the hosts block is present *and* the rdr rule
is gone. Then it logs the outage, runs `transparent-root.sh reload-anchor`, and
notifies you. If four consecutive repairs fail it **removes the redirect** and says so:
with it gone, everything reaches Anthropic directly and burst is merely out of the path,
which beats a Mac that cannot reach Anthropic at all.

It needs root, reloading a pf anchor does, which is why it is a LaunchDaemon rather
than part of the existing user-level `self-heal-watchdog.sh`. That watchdog detected this
exact failure three times and could only post a notification.

Drive every branch of its decision tree without root, and without breaking anything:

```bash
scripts/pf-heal.sh --self-test
```

See [ROLLBACK.md](../ROLLBACK.md) for undoing every part of this independently.
