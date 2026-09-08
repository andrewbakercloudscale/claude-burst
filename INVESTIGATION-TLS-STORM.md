# Open investigation: TLS handshake storm + port-7777 timeout

Status: TLS handshake storm **ROOT-CAUSED 2026-09-03**, and the fix held for three days —
but certificate rejections **came back on 2026-09-07**, at about a tenth of the old rate and
apparently from a different client. See the 2026-09-08 update on that before treating it as
closed.

Issue #1 (the direct-port timeout) is **CHARACTERISED 2026-09-08**, after one wrong answer
that got as far as being published here and on the issue. It is not the port number. **The pf
rdr rule makes its own target port unreachable for direct connections**, whichever port that
is — so it follows the redirect around and changing ports does not help. It was never
intermittent: it reproduces on demand, and only while transparent mode is installed. See the
two updates at the bottom, in order.

## Issue #1 (original): direct 127.0.0.1:7777 timeout right after restart

Tracked at https://github.com/andrewbakercloudscale/claude-burst/issues/1

- Right after `launchctl kickstart -k`, direct `curl`/`nc` to `127.0.0.1:7777` sometimes times
  out at the raw TCP level for up to ~15s, then resolves on its own. The real traffic path
  (443 → pf-redirected to 7777) is usually unaffected at the same moment.
- Caused `deploy.sh` to falsely roll back healthy builds (now mitigated by checking both
  schemes and not touching CA trust mid-deploy — see `deploy.sh` steps 3 and 6).
- Ruled out: macOS Application Firewall, other pf-touching software, the claude-burst pf
  anchor's own rule (only redirects :443, never matches a direct :7777 connection), slow
  gateway startup dependency (DoH resolver does no I/O until first dial).
- **Still needed to settle it:** `sudo pfctl -a claude-burst -sr` and `-ss` (rules + state
  table) captured at the exact moment of a reproduction, to check for stale NAT/state
  entries surviving repeated anchor reload/restart cycles. Needs an interactive terminal
  with root — owner was about to run this when the session was paused.
- Diagnostic tooling already in place: `scripts/health-diagnostics.sh` auto-captures curl
  verbose (both schemes), `lsof -iTCP:7777`, `launchctl list`, log tails to
  `~/.config/claude-burst/health-check-failures.log` on every health-check failure. It now
  holds **nine** captures spanning 2026-08-31T17:08:45Z to 2026-09-03T16:52Z — six of them
  in matched new-binary/rollback-binary pairs, which is this issue's signature: the health
  check failed, deploy.sh rolled a working build back, and the rollback binary then failed
  the same check. Nobody has read them since the first one. Does **not** capture pfctl
  output (needs a password, can't run unattended).
- `cmd/claude-burst/main.go`'s `serve()` logs a confirmed, timestamped listener-bind
  success/failure (`net.Listen` split from `Serve`) to help distinguish "slow to bind" from
  "bound fine, external layer is the problem".

## Related finding: TLS handshake-error storm (2026-08-31 session, ROOT-CAUSED 2026-09-03)

`launchd.err.log` repeatedly logs:
```
http: TLS handshake error from 127.0.0.1:PORT: remote error: tls: unknown certificate
```
— meaning some client keeps connecting and rejecting the gateway's certificate.

**Timeline:**
- First seen: correlated with a Claude Code session that had started ~3 min earlier,
  consistent with a process that cached `NODE_EXTRA_CA_CERTS` at startup and then had the
  CA bundle change underneath it during an enable/disable cycle. Backoff pattern looked
  exponential (4s, 6s, 8s, 13s, 17s, 26s, 30s...).
- Prior recap named two specific stale sessions as the cause: a tmux session started
  Thu 2026-08-27 05:38 (`raspberry-pis/andrewbakerninja`), and a `claude-panel-setup`
  session started 19:03:43 — both predate the CA bundle's 16:59 settle time.
- **2026-08-31 ~19:26–20:05 SAST: killed both.** Tmux session via `tmux kill-session` +
  killing the orphaned `claude`/mcp-server child; `claude-panel-setup` had already exited
  on its own. **The storm did not stop** — kept firing every ~20-30s afterward. So
  "restart the stale sessions" was not a complete fix, or there's an additional culprit
  still unaccounted for.

**Ruled out as the client, this session:**
- Claude.app (desktop, Electron) — `lsof` showed its live connections going directly to
  real Anthropic/GCP IPs over the LAN interface, not through `127.0.0.1`/`:7777`.
- `ShipIt` (Claude desktop's Squirrel auto-updater) — `OnDemand`, not running.
- Any LaunchAgent with a matching ~20-30s `StartInterval`, or a crontab entry.
- An internal self-check ticker in `cmd/claude-burst/main.go` (grepped, none found).

**Other facts gathered:**
- `/etc/hosts` only redirects `api.anthropic.com` → `127.0.0.1` — nothing else is
  intercepted, so whatever's retrying is specifically resolving that hostname.
- `claude-burst.log` (application-layer request log) had **zero entries for ~2 hours**
  (last successful request 18:05:29) while the TLS storm kept firing in the same window —
  whatever's retrying never gets past the TLS layer at all, so it never shows up as an
  HTTP request in the app log.
- Tried to catch the client process live via a tight `lsof -iTCP -n -P` polling loop
  (0.3–0.7s interval, ~90s), filtering on `->127.0.0.1:7777` — caught nothing. The
  loopback TLS-reject-and-close cycle is almost certainly faster than shell-loop polling
  resolution.
- Tried unified logging (`log show --predicate ...`) for cert/TLS/7777-related events in
  the same window — nothing surfaced; the client isn't logging its own TLS failures via
  `os_log` in a greppable way.

**Next step (needs root, owner has terminal access):**
```bash
sudo dtrace -n 'syscall::connect:entry /((struct sockaddr_in *)copyin(arg1,arg2))->sin_port == htons(7777)/ { printf("%s [%d]", execname, pid); }'
# leave running ~1 min while the storm fires (every 20-30s) — will name the actual client process
```
Run this alongside the `pfctl -a claude-burst -sr`/`-ss` capture issue #1 needs — both
require root and the storm is reproducing continuously right now, not just at restart, so
one sitting can gather both.

Full write-up also posted as a comment on issue #1:
https://github.com/andrewbakercloudscale/claude-burst/issues/1#issuecomment-5482450428

See also: memory `claude_burst_direct_port_investigation.md` and
`claude_burst_ops_safety.md` for the broader deploy-safety context (backup/rollback
tooling, why transparent mode's deploy path avoids touching CA trust).

## Update 2026-09-02: still firing, and the cadence is not what it looked like

Two days after the session above was paused, the storm is **still going**, unprompted and
with no intervening fix: 2723 handshake rejections logged in total, spanning
2026-08-31 11:49:07 to 2026-09-02 20:04:01, of which **937 are from today alone**.
`launchd.err.log` has reached 281KB. So this is not a transient tied to that session's
enable/disable cycle, and it survived every restart and session-kill tried on 2026-08-31.

**The interval is not fixed.** Reading only the tail of a burst suggests a ~30s poller,
which is misleading — that is just where the ramp plateaus. Measuring the gaps between the
last 21 events gives:

```
0, 0, 0, 0, 1, 2, 2, 4, 5, 8, 10, 11, 18, 20, 26, 30, 31   seconds   (one burst)
2774, 3447, 3806                                            seconds   (~46, ~57, ~63 min)
```

That is an exponential-backoff retry ramp — matching the "4s, 6s, 8s, 13s, 17s, 26s, 30s"
pattern noted on 2026-08-31 — that climbs to ~31s, gives up, and then is **re-triggered
roughly once an hour**.

Two consequences for the investigation:

- **Stop looking for a fixed-interval job.** A `StartInterval`/cron entry would produce
  evenly spaced connections, which is not what the log shows. The earlier "no LaunchAgent
  with a matching 20-30s interval" finding was therefore never evidence of much; the thing
  to look for is a client that wakes roughly hourly and then retries with backoff until it
  gives up.
- **The quiet gaps are the opportunity.** Earlier attempts to catch the client with a tight
  `lsof` loop failed partly because they ran blind. The end of a ~45-60 minute gap predicts
  the next burst, and the first retries of a burst are only 0-2s apart, so a capture armed
  just before a predicted burst has far better odds than continuous polling.

The `dtrace` one-liner below is still the definitive answer and still needs root; the above
only narrows where to point it and when to run it.

## Update 2026-09-03: root-caused, and fixed

`dtrace`'s `syscall` provider turned out to be fully blocked by SIP on this machine --
`probe description syscall::connect:entry does not match any probes. System Integrity
Protection is on` -- so the one-liner above was never runnable here. An earlier untargeted
`lsof`-polling attempt also failed, separately, because "the loopback TLS-reject-and-close
cycle is almost certainly faster than shell-loop polling resolution."

Built a different approach instead: `cmd/claude-burst/peerlog.go`, an opt-in
(`CLAUDE_BURST_LOG_TLS_PEERS=1`) listener wrapper that identifies the calling process via a
port-scoped `lsof` lookup *synchronously inside `Accept()`*, before the connection can reach
the TLS handshake that would otherwise close it first. An async version (`go
l.identify(...)`) still lost the race against the fastest-failing connections in a real
burst; making it synchronous (accepting serialized connection acceptance during the capture
window -- fine for a low-traffic diagnostic run, never enabled otherwise) removed the race
by construction.

Running it caught several benign connections (`curl` health-checks, Claude Code CLI itself
-- both succeeding, since they trust the gateway's CA), but the actual match came from
**correlating timestamps against Claude Desktop's own log** (`~/Library/Logs/Claude/main.log`)
rather than from a caught failure directly: its `[updater]` component has logged `Auto-update
error: A TLS error caused the secure connection to fail` roughly **hourly since 2026-08-31
20:19:22** -- matching this investigation's own "re-triggered roughly once an hour" finding
exactly, and matching a live-captured storm burst (`2026-09-03 16:05:20`) to the second.

**Root cause:** Claude Desktop's own auto-updater checks `api.anthropic.com` periodically.
Transparent mode's `/etc/hosts` redirect catches that traffic like any other, sending it to
the gateway -- whose certificate only `claude-burst enable` trusts, and only for Claude Code
CLI (via `NODE_EXTRA_CA_CERTS`). Claude Desktop has never heard of this CA, so its TLS client
rejects the gateway's certificate every time, producing exactly the "unknown certificate"
storm. Claude.app itself was independently confirmed live (via `lsof` on its own PIDs) to
never touch the redirect for its real traffic -- its connections go straight from the LAN
interface to Anthropic's real IPs -- so this is specifically the updater's background check,
not the app's normal operation. It is not only noise: a live MCP-filesystem startup failure
for Claude Desktop's Cowork feature (`Couldn't start ... Connection closed`, 2026-09-03
15:05:30) landed in the middle of a storm burst, strongly suggesting that startup path also
touches `api.anthropic.com` and was collateral damage from the same untrusted-certificate
rejection.

**Fix, not a workaround:** `scripts/trust-ca-systemwide.sh` imports the gateway's CA into the
macOS System keychain as a trusted root, so any process's TLS stack accepts it -- not just
Claude Code CLI's. This is what transparent mode's redirect actually needed to be silent for
machine-wide traffic, which was always the design intent (see ROLLBACK.md: transparent mode
already accepts machine-wide blast radius as its known cost). `scripts/untrust-ca-systemwide.sh`
undoes it. Both are now wired into `install-proxy.sh` (step 5) and `rollback.sh` (step 1b)
respectively, as explicit, clearly-labeled root steps -- never silently escalated, same as
every other machine-wide change in this repo.

Issue #1 (the direct `:7777` timeout) is unrelated and remains open.

## Update 2026-09-08: the storm came back, smaller, and probably from something else

Counting `unknown certificate` rejections in `launchd.err.log` per day:

```
965   2026-08-31
751   2026-09-01
1395  2026-09-02
747   2026-09-03   <- trust-ca-systemwide.sh landed
  0   2026-09-04
  0   2026-09-05
  0   2026-09-06
 86   2026-09-07   <- first at 23:20:18
 34   2026-09-08   <- last at 07:05:35
```

So the fix was real: three clean days, from ~750/day to nothing. It is the recurrence that
needs explaining, and the honest reading is that **it is not the same client**. Claude
Desktop's own `~/Library/Logs/Claude/main.log` has logged no TLS-worded updater error since
`2026-09-03 16:05:20` — the identification that root-caused this in the first place. Its
later failures (through `2026-09-07 22:12:08`) say *"Could not connect to the server"*,
which is the pf outage of that evening (connection refused, nothing listening), a different
fault with a different signature. Meanwhile the gateway went on rejecting certificates from
some client that leaves no trace in that log at all.

**The most likely mechanism, and it is structural rather than mysterious.** The fix is a
System-keychain trust that `scripts/rollback.sh` deliberately removes at step 1b, via
`untrust-ca-systemwide.sh`, and that only `install-proxy.sh` step 5 puts back. So every
rollback re-opens the exact hole this investigation closed, and any window in which the
`/etc/hosts` redirect is installed while System trust is not reproduces the storm by
construction. The evening of 2026-09-07 and the morning of 2026-09-08 contained several
such cycles. That is the same shape as the state `interceptActive` already
calls "worse than not intercepting" — traffic arrives and is rejected — one layer down,
and nothing currently checks for it.

**Verified good right now**, so this is a window problem and not a persistent regression:

```
$ security verify-cert -c ~/.config/claude-burst/ca/leaf-cert.pem -p ssl -s api.anthropic.com
...certificate verification successful.        # rc=0
```

Note that presence and trust are different questions, and `trust-ca-systemwide.sh`'s own
verification (line 49) asks the weaker one — `security find-certificate -c "claude-burst
local CA"` succeeds for a certificate sitting in the keychain with no trust at all. Use
`verify-cert` above, which exercises the decision a TLS client actually makes. (Checked:
`dump-trust-settings -d` reporting `Number of trust settings : 0` is **not** the smoking gun
it looks like — Zscaler, Capitec and Intune's roots all report 0 as well. That is what a
default-trust root looks like.)

**Next step, and it no longer needs root.** `dtrace` is still blocked by SIP, but the
purpose-built replacement from the 2026-09-03 update is still in the binary and still the
right tool:

```bash
CLAUDE_BURST_LOG_TLS_PEERS=1   # then restart the gateway and wait for a burst
```

It identifies the calling process synchronously inside `Accept()`, before the handshake that
would otherwise close the connection first. Point it at the next recurrence and name the
client, rather than inferring it from timestamps a second time.

## Update 2026-09-08 (a): issue #1 is the port number — **WRONG, see (b) below**

> **Retracted.** The conclusion of this section — that port 7777 specifically is blocked — is
> wrong, and update (b) below shows why. The capture analysis and the measurements in it are
> sound and worth keeping; only the interpretation was bad. Left in place rather than deleted
> so the mistake and its correction stay legible.

Nine captures had accumulated in `health-check-failures.log` while this document said it was
waiting for one. Reading them settles the issue, and the first thing they show is that **four
of the nine are not this bug at all**:

| captures | direct probe | gateway | real path | what it actually was |
|---|---|---|---|---|
| 1–5 (08-31 ×2, 09-01 ×2, 09-03 07:38) | **timeout, ~3000ms** | LISTENING, 8+ ESTABLISHED | **HTTP 200 in 0.03–0.12s** | issue #1 |
| 6–9 (09-03 12:50, 12:56, 15:45 ×2) | **refused, 0ms** | `Could not find service` | HTTP 404 from the *real* Anthropic | a half-installed proxy, fixed in `2b2cdf4` |

Captures 6–9 have been inflating this issue's evidence pile with a different failure that has
its own fix. Captures 1–5 all predate the real-path health fallback (`75da3de`, 09-03 11:04),
which is why the check failed at all while the gateway was demonstrably fine.

### It reproduces on demand, and it is not intermittent

Measured 2026-09-08 with the gateway healthy and serving live traffic:

```
gateway  DIRECT     https://127.0.0.1:7777        1/15     curl
gateway  DIRECT     https://localhost:7777        0/15
admin    DIRECT     http://127.0.0.1:7788        15/15
gateway  REAL PATH  https://api.anthropic.com    15/15     pf rdr -> the SAME socket
nc -> 127.0.0.1:7777    0/10           nc -> 127.0.0.1:7788   10/10
plain python listener on 127.0.0.1:7801               10/10
```

Nothing appears in `claude-burst.log` for the failed attempts: the SYN never reaches the
process.

### What that rules out

Not the gateway process — its own admin listener on 7788 answers 15/15. Not the listening
socket — traffic arriving at **that exact socket** through the pf rdr from :443 succeeds
15/15 while direct connections to it fail. Not loopback, not curl (`nc` agrees), not the
client. Not pf state and not the source port: both were measured out on 2026-09-03 (see
`deploy.sh`'s note — 0/10 before an anchor state flush, 1/10 after, and 0/10 from three
different source-port ranges).

What is left is the destination port number, on new flows only. 7778 — the adjacent port —
is clean at 10/10, as are 8777, 9777, 17777 and 18777, so this is an exact-match entry
rather than a range.

### The likely mechanism, and why pfctl never showed it

Three network system extensions are active on this Mac, and they filter new flows **above**
pf, which is exactly why the pf-anchor state flush correctly found nothing:

```
com.forcepoint.ne          Forcepoint Neo NE     [activated enabled]
com.crowdstrike.falcon     Falcon Sensor         [activated enabled]
io.tailscale.ipn...        Tailscale NE          [activated enabled]
```

7777 is a long-standing backdoor/trojan port on enterprise blocklists. A Forcepoint or Falcon
policy dropping it fits every observation: destination-port-specific, new-flows-only, SYN
dropped rather than refused (hence a timeout, never `connection refused`), invisible to
`pfctl`, and unaffected by anything this project can configure.

Confirming *which* product would need `sudo pfctl -sa` plus vendor policy, but that is now
optional: it does not change what to do.

### The fix: stop using 7777

The gateway's default listen port is now **17777** (`internal/config.Default`). Real traffic
is unaffected either way — the pf rule redirects `443 -> the gateway port` — so this only
ever mattered to the direct probes, which is precisely what was falsely rolling back healthy
builds and waking the watchdog.

Changing it required removing the hardcoded `7777` from every prober first. `gateway_healthy`,
the diagnostics dump, `connectivity-test.sh`, `pf-heal.sh`'s fallback, `install.sh`'s summary
and the dtrace capture all had the port baked in, so `listen` was a setting you could change
and then watch every health check keep testing the old port — reporting a healthy gateway as
dead, which is the thing deploy.sh and watchdog.sh act on. They all read `listen` from
`config.json` now.

**Existing installs keep 7777** — `config.json` names the port explicitly, and `Load` only
applies the default when the field is empty. Moving an installed gateway means changing
`listen` *and* regenerating the pf anchor, which needs root:

```bash
sudo scripts/transparent-root.sh remove          # hosts first: traffic goes direct, the safe state
claude-burst configure --listen 127.0.0.1:17777
scripts/install-proxy.sh                         # restarts, waits healthy, reinstalls hosts+pf on the new port
```

The ordering is ROLLBACK.md's rule 3 and matters: between the gateway moving to a new port and
pf being regenerated, the old rdr would point `443` at a port nothing is listening on — the
machine-wide-refused state. Removing the hosts redirect first makes that window harmless.

## Update 2026-09-08 (b): it is the rdr target, not the port — correcting (a)

Update (a) concluded that a security product blocks destination port 7777, and recommended
moving the gateway to 17777. The move was carried out. The result disproves it.

```
before the redirect was installed:  17777 direct   15/15    (not yet the rdr target)
after  the redirect was installed:  17777 direct    0/15    (now the rdr target) — timeout
with 7777 no longer the target:      7777 direct   10/10    (plain python listener)
```

**7777 was never blocked.** A plain listener on it answers 10/10 right now. What actually
happens is that **the pf rdr rule makes its own target port unreachable for direct
connections** — the symptom follows the redirect to whatever port it points at.

### How the wrong answer survived so long

Every control in update (a) was consistent with this too, and was misread. The ports tested
clean — 7778, 8777, 9777, 17777, 18777, 7801 — were all tested with a listener that was *not*
the rdr target, and the only port that ever failed was the one that was. The variable being
changed (the port number) was perfectly confounded with the variable that mattered (being the
rdr target). The experiment that would have killed the theory in ten seconds — a plain
listener on 7777 while something else was the rdr target — was never run, because 7777 was
believed to be poisoned and therefore untestable.

The evidence against was also already in this repo. `transparent-root.sh` records pf states
with the gateway port on *both* sides, and `deploy.sh`'s note that "only the destination port
matters" is the same observation phrased as a conclusion about the number rather than about
the role.

### What this means operationally

- The `no rdr on lo0 ... port <gateway>` exemption line in the anchor does not do what it
  intends on macOS loopback. That is the bug, and it is ours, not a security vendor's.
- **The direct probe cannot be trusted whenever transparent mode is installed** — by design,
  not intermittently. `gateway_healthy`'s real-path probe is not a fallback in that mode, it
  is the only correct check, and it is why deploys have been succeeding throughout.
- Changing the port achieves nothing and the default is back to **7777**. The de-hardcoding
  that the port move forced is kept: `cfg.Listen` is a real setting and every prober now reads
  it, which was independently worth doing.

### Measured 2026-09-08: pf fails to INSERT STATE

`scripts/diagnose-direct-port.sh` (read-only) snapshots pf's counters around a burst of
failing probes. 20 direct probes, 10 real-path probes as control:

```
counter                 before       after     delta
match                 29333280    29337527     +4247
state-mismatch             338         338        +0
state-insert             11810       12100      +290   <-- the drop reason
```

Every filter and block drop reason stayed at **zero**. `state-insert` is pf's
state-insertion-*failure* counter, and +290 is about 14 per probe — one per SYN
retransmission across each 3-second attempt.

Two candidate fixes died on this output before costing anything:

- **A security product blocklisting the port.** Already disproved by the port move; the
  counters confirm it, since a filter drop would have moved a block counter.
- **An explicit filter `pass` rule.** Nothing is blocking — no block rules exist in the
  main ruleset or in `com.apple` — so a plain `pass` has nothing to override. Worse, our
  anchor is referenced by `rdr-anchor` only, with no filter `anchor` line, so the rule would
  have been loaded and never evaluated: a fix that looks applied and does nothing.
- **State *mismatch*.** Predicted, and flatly wrong: that counter did not move at all.

### Still open

Why state insertion fails. The anomalous state is visible in the same capture and has the
gateway port on **both** sides:

```
ALL tcp 127.0.0.1:17777 -> 127.0.0.1:443     TIME_WAIT:TIME_WAIT
ALL tcp 127.0.0.1:17777 <- 127.0.0.1:17777   TIME_WAIT:TIME_WAIT
```

The refined candidate — untested, and the fourth on this issue, so it gets an experiment
rather than a commit — is a filter rule that explicitly does NOT create state, which would
need a filter `anchor "claude-burst"` line in `/etc/pf.conf` that does not currently exist:

```
pass quick on lo0 inet proto tcp from any to 127.0.0.1 port <gateway> no state
```

Note this is *not* the plain `pass` ruled out above: the point is `no state`, bypassing the
insertion that is failing, rather than overriding a block that does not exist.

## Update 2026-09-08 (c): the `no state` rule is ruled out too — RUN, not reasoned

`sudo scripts/experiment-nostate-rule.sh` was run at 10:53 SAST. The candidate above was
applied for real — including the filter `anchor "claude-burst"` line `/etc/pf.conf` lacks,
so this time the rule was actually *evaluated* rather than loaded and skipped — measured,
and reverted.

```
real path (precondition)      5/5
direct 127.0.0.1:17777 before 1/15
  rules live in the anchor while applied:
    no rdr  on lo0 inet proto tcp from any to 127.0.0.1 port = 17777
    rdr pass on lo0 inet proto tcp from any to 127.0.0.1 port = 443 -> 127.0.0.1 port 17777
    pass quick on lo0 inet proto tcp from any to 127.0.0.1 port = 17777 no state
real path (mid-experiment)    5/5   <- the candidate did not break the machine-wide path
direct 127.0.0.1:17777 after  0/15
```

**Verdict: no improvement.** 1/15 and 0/15 are the same number — the baseline rate is
about 1 in 20 by itself, so a single success on either side is noise, not signal. Both
files were restored from `/etc/claude-burst/experiment-20260908-105312` and the real path
re-verified at 5/5 after the revert. Nothing left behind.

This is the **fourth** hypothesis on issue #1 and the fourth wrong one. What is different
about this one is only that it cost fifteen minutes and no machine-wide reconfiguration,
because it was written as a reverting experiment instead of a commit.

### What it eliminates

The rule was `quick`, in an anchor that was genuinely evaluated this time, matching the
exact tuple — and direct connections still failed. So the failing state insertion is **not
the one the filter rule would have created**. Skipping filter-state creation changes
nothing, which means the insertion that fails belongs to some other path: most plausibly
the translation machinery around the `rdr`/`no rdr` pair, which a filter rule cannot opt
out of. That is a narrowing, not an answer.

### Do not propose next

A fifth mechanism argued from how pf ought to behave. Four for four now, and the one
diagnostic that was run settled more than the two rounds of theorising that preceded it.
The next step on this issue is a **measurement that distinguishes**, not another candidate
fix — e.g. `tcpdump -ni lo0` across a burst of failing direct probes, which answers a
question none of the four hypotheses addressed: whether the SYN reaches the socket at all,
and whether what comes back is a RST, and from where.

## Update 2026-09-08 (d): the SYN is swallowed inside pf — measured, not argued

`scripts/capture-direct-port-packets.sh` was run (unprivileged — see the note at the
end). Three bursts, one of them with **no port filter at all** so nothing could hide.

### Per-probe, the failing path

10 direct probes to `127.0.0.1:17777`, every client source port listed:

```
src port 52587  SYNs sent=2    packets received back=18     <- the 1 lucky success
src port 52588  SYNs sent=11   packets received back=0
src port 52590  SYNs sent=11   packets received back=0
src port 52591  SYNs sent=11   packets received back=0
src port 52595  SYNs sent=11   packets received back=0
src port 52602  SYNs sent=11   packets received back=0
src port 52603  SYNs sent=11   packets received back=0
src port 52606  SYNs sent=11   packets received back=0
src port 52608  SYNs sent=11   packets received back=0
src port 52614  SYNs sent=11   packets received back=0
```

**The SYN reaches lo0 every time. Nothing whatsoever comes back.** Not a RST, not a
SYN-ACK, not an ICMP — zero packets. The control burst down the working `:443` path,
captured the same way in the same run, was 10/10 with one SYN and one SYN-ACK each.

That is the first of the four outcomes the script was written to distinguish: **pf
swallowed it, and the investigation stays on pf.** It is not the socket refusing (that
would be a RST from `:17777`), not a synthesised RST from elsewhere, and not a packet
that never left the client.

### The obvious objection, closed

A reply whose source port pf had *also* rewritten would not have matched a
`tcp port 17777` filter, and would have looked like silence. So a third burst captured
**every TCP packet on lo0**, no port filter. Across 6 failing probes (66 SYNs), the only
SYN-ACKs on the interface were unrelated traffic — `:443` from another connection,
`:7788` (our own admin server), `:9000`. **Nothing came back from the gateway in any
form, under any pair of ports.**

### 11 SYNs per probe, against a counter that said ~14

`diagnose-direct-port.sh` measured `state-insert` rising 290 across 20 probes, ~14.5
each. This capture shows 11 SYNs per failing probe. Same order, same shape — one
insertion failure per SYN retransmission — with the difference explained by curl's
retransmission schedule inside a 3-second timeout. **The counter belongs to these
packets**, which is what makes the earlier measurement safe to keep building on.

### What the one success reveals about the mechanism

The lucky probe (1 in 10, matching the known ~1-in-20 baseline) is the most informative
packet sequence in this whole investigation:

```
1. 127.0.0.1.52587 > 127.0.0.1.17777: [S]      client SYN, correctly addressed
2. 127.0.0.1.17777 > 127.0.0.1.443:   [S.]     the gateway's SYN-ACK, sent to :443
3. 127.0.0.1.443   > 127.0.0.1.17777: [R]      RST -- nothing holds that connection
4. 127.0.0.1.17777 > 127.0.0.1.52587: [S.]     retransmitted SYN-ACK, addressed right
   ... handshake completes, request succeeds
```

Packet 2 is pf **reverse-translating the reply of a connection that was never forward-
translated**: the destination port went from 52587 to 443, which is exactly the rdr rule's
mapping run backwards. The connection only survived because the retransmitted SYN-ACK
(packet 4) escaped that treatment.

So the failure is the reply direction being mangled or dropped by the rdr's reverse
translation, not the SYN being filtered on the way in. That is consistent with every
earlier measurement — the zero filter/block counters, the `state-insert` rise, and the
anomalous `127.0.0.1:17777 <- 127.0.0.1:17777` state — and it is the first account that
explains all of them at once.

**Still a mechanism inferred from one packet trace, not a fix.** It is the best-supported
hypothesis this issue has had, and it has been wrong four times before. The next step is
to test that account, not to act on it.

### Note on running it

The script no longer demands root. macOS grants BPF access through the `access_bpf`
group, which this account has, and refusing to capture without a password that is not
needed is how a diagnostic goes unrun. The capability is tested rather than assumed, and
when the run is unprivileged the pf rule listing prints **UNKNOWN** rather than nothing —
"no output" must never read as "no rdr rule".

## Update 2026-09-08 (e): attribution armed and proven, storm dormant

There is no Wireshark or tshark on this machine and no packet-capture MCP connected —
and neither would have answered this anyway. A capture names a **port**; the question is
which **process**. The join is port to PID, and `cmd/claude-burst/peerlog.go` already
does it correctly: `lsof` scoped to the exact ephemeral port, run synchronously the
instant `Accept()` returns, so the connection cannot reach the TLS handshake and be torn
down before its owner is known. It had simply never been switched on.

`scripts/peer-log.sh on|off|status` now arms it, and it is **armed**.

### Proven working, on live traffic

```
peer-log: connection from 127.0.0.1:54343 -- lsof: ... curl 15160 ... 127.0.0.1:54343->127.0.0.1:443
peer-log: connection from 127.0.0.1:54341 -- lsof: ... 2.1.263 33360 ... 127.0.0.1:54341->127.0.0.1:443
```

The second is this Claude Code session itself (`2.1.263` is the version-named binary) —
a false lead, identified rather than chased.

### Two traps this hit, both caught by verification rather than assumption

- **`launchctl kickstart -k` does not re-read the plist.** It restarts the process using
  launchd's cached job definition, so the edited `EnvironmentVariables` block never
  reached it. The plist said armed, the plist *was* armed, and `ps eww` on the running
  process showed the variable absent. `bootout` + `bootstrap` is what makes launchd
  re-read the file. The script now does that, and confirms by looking for the startup
  banner rather than trusting the edit.
- **The peer-log lines and the TLS errors are in different files.** The gateway's own
  logger (`main.go:157`) writes to a rotating `claude-burst.log`; Go's `http.Server`
  writes handshake errors to stderr, which launchd puts in `launchd.err.log`. The first
  version of the status join read only the latter and would have reported "none yet"
  forever — the exact failure shape this repo keeps rediscovering.

### What the timing says, and what it does not

The storm is **dormant**: last `unknown certificate` at 07:05:35, nothing in the four and
a half hours since. Burst clusters start roughly hourly overnight — 23:47, 00:55, 01:53,
02:11, 03:58, 04:59, 07:01 — with a tighter ~15-minute rhythm inside the 02:00 hour that
may be a second client. Errors arrive in **pairs on adjacent ports**, so whatever it is
opens two connections at once.

The hourly shape suggests a scheduled task, but `crontab` is empty and no launchd agent
that runs hourly touches Anthropic at all (the hourly ones are Google, Zoom and GoToMeeting
updaters, which have no reason to resolve `api.anthropic.com` — and only `api.anthropic.com`
is in `/etc/hosts`). **That is a narrowing, not a name.** After four wrong answers on
issue #1, the pattern is not going to be named from a schedule that merely rhymes.

### The cost of leaving it armed

`identify()` is synchronous, so `Accept()` blocks for one `lsof` — measured at 40-70ms
here — and connections are accepted one at a time while it runs. That is real, and it is
why the code calls itself diagnostic-only. Since the bursts are overnight, the sensible
window costs nothing. **Disarm with `scripts/peer-log.sh off` once a burst is captured.**
