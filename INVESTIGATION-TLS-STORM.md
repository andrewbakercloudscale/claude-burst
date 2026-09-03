# Open investigation: TLS handshake storm + port-7777 timeout

Status: TLS handshake storm **ROOT-CAUSED AND FIXED 2026-09-03** (see update at the bottom).
Issue #1 (port-7777 timeout) is still **OPEN**.

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
  `~/.config/claude-burst/health-check-failures.log` on every health-check failure. Has one
  real capture from 2026-08-31T17:10:39Z. Does **not** capture pfctl output (needs a
  password, can't run unattended).
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
