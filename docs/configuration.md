# Configuration

[Back to the README](../README.md)

Everything lives in `~/.config/claude-burst/config.json`. You rarely need to edit it by hand: most keys are set from the dashboard or with `claude-burst configure`, and the table below says which. The file is read at startup unless a row says it is read live; the dashboard shows a banner, with **Restart the gateway now**, when something saved is not yet in use.

## Every top-level key

| Key | Default | What it does | Set by |
|---|---|---|---|
| `listen` | `127.0.0.1:7777` | The gateway's address. Keep it on loopback. | `configure --listen`, then reinstall |
| `admin_listen` | `127.0.0.1:7788` | The dashboard's address; `off` (empty) disables it. A separate listener from `listen`, so admin routes are never reachable on the intercepted hostname. | `configure --admin-listen` |
| `admin_hostname` | empty | An optional friendly name for the dashboard, see [A friendlier admin URL](dashboard.md#a-friendlier-admin-url). | `configure --admin-hostname` |
| `reset_grace_seconds` | `10` | Extra seconds after Anthropic's reset time before trying the subscription again. | Dashboard, Advanced |
| `unknown_reset_seconds` | `300` | How long an overflow window lasts when Anthropic gives no reset time. | Dashboard, Advanced |
| `response_header_timeout_seconds` | `60` | How long to wait for a response to *start* before treating the upstream as failed. | Dashboard, Advanced |
| `max_request_mb` | `128` | Largest request body the gateway accepts. | Dashboard, Advanced |
| `primary` | `oauth-passthrough` to `api.anthropic.com` | The primary route, see below. | `configure --primary`, `--failover-strategy`; Dashboard, Routing |
| `adopted_base_url` | unset | The `ANTHROPIC_BASE_URL` Claude Code had before `enable` (an enterprise gateway such as Portkey). It became `primary.base_url`; `disable`, the dashboard Revert and `rollback.sh` put it back. Written by `enable`. | Not edited by hand |
| `secondary` | Bedrock slot with no key | The overflow route, see below and [Providers](providers.md). | `configure --secondary ...`; Dashboard, Secondary |
| `metered_failover` | 60 s window, 3 HTTP failures, 1 transport failure | Thresholds for the metered failover strategies, see below. | `configure --metered-window-seconds`, `--metered-min-failures`; Dashboard, Failover & pricing |
| `fallback_chain` | Fable to Opus | Other Claude models to try on your own plan before the secondary, see [Routing](routing.md#limits-are-per-model-and-are-never-inferred). | Dashboard, Failover & pricing |
| `pricing` | Anthropic models built in | Per-million-token prices, keyed by the model that served the request (`input_per_mtok`, `output_per_mtok`, optional `cache_read_per_mtok`, `cache_write_per_mtok`). | Dashboard, Failover & pricing |
| `intercept` | `mode: base-url` | How Claude Code is pointed at the gateway, see below and [Transparent mode](transparent-mode.md). | `configure --intercept-mode`, `--intercept-host`; Dashboard, Install |
| `secondary_pruning` | on | Trims old tool output from overflow requests: `disabled`, `no_stub`, `no_cap`, `keep_recent` (10), `step` (10), `stub_min_bytes` (1 KB), `max_tool_result_bytes` (40 KB). See [Overflow pruning](routing.md#overflow-pruning). | Dashboard, Context & cache |
| `primary_compaction` | off | Pauseless compaction: `enabled`, `compact_at_tokens` (300k), `warn_at_percent` (80, a percentage of `compact_at_tokens`), `window_minutes` (30), `no_prompt_notice`, `mid_turn`, `repo_overrides` (a list of `repo` (the repository's full path) with `compact_at_tokens` or `off`). See [Pauseless compaction](compaction.md). | Dashboard, Pauseless Compaction |
| `session_coordination` | off | `enabled`, `master_idle_minutes` (15), `nudge_minutes` (10). See [Session coordination](coordination.md). | Dashboard, Session coordination |
| `keep_awake_lid_closed` | `false` | Keep the Mac awake with the lid shut, see below. | `configure --keep-awake-lid-closed`; Dashboard, This Mac |
| `keep_awake_lid_closed_power` | `ac` | `ac` (plugged in only) or `always`. | `configure --keep-awake-power`; Dashboard, This Mac |
| `keep_awake_idle_minutes` | `0` (no limit) | Only stay awake this long after Claude Code was last used. | `configure --keep-awake-idle-minutes`; Dashboard, This Mac |
| `notify` | all off | Retired: Burst no longer sends macOS notifications; kept so older files still load. See [Notifications](lid-and-hotspot.md#notifications). | none |
| `alert_daily_spend_usd` | 0 (off) | An on-screen alert once today's API-equivalent spend passes this many dollars, once a day. Read live. See [Gateway alerts on screen](dashboard.md#gateway-alerts-on-screen). | Dashboard, Session options |
| `hotspot` | off | `ssid`, `when` (`lid-closed` or `always`), `check_seconds` (5), `offline_checks` (2), `retry_seconds` (60), `give_up_minutes` (30). Read live. See [Join a hotspot](lid-and-hotspot.md#join-a-hotspot-when-offline). | Dashboard, This Mac |
| `anthropic_base_url`, `bedrock_base_url`, `keychain_service`, `model_map` | see below | Legacy flat fields, still read: they are turned into `primary`/`secondary` when those blocks are absent. | Older configs only |

Two other things are not in `config.json`:

- **Session handover** settings are in `~/.config/claude-burst/handover.json`, owned by the dashboard's Session handover section (see [Session handover](handover.md)).
- **TLS peer logging**, a diagnostic that names the process behind each incoming connection, is the environment variable `CLAUDE_BURST_LOG_TLS_PEERS=1` on the gateway's LaunchAgent, switched with `scripts/peer-log.sh on|off`. It costs 40-70 ms per connection, so leave it off.

Model IDs change over time. With Together AI or OpenRouter keep `secondary.model` (and any `model_map`) aligned with a model the endpoint actually serves; with Bedrock keep `model_map` aligned with the Claude models enabled in your account.

## An example, and the routing keys in detail

Legacy flat fields (`anthropic_base_url`, `bedrock_base_url`, `model_map`, `keychain_service`) are still read and still work unchanged, they're synthesized into `primary`/`secondary` automatically. New setups can also configure `primary`/`secondary` directly:

```json
{
  "listen": "127.0.0.1:7777",
  "reset_grace_seconds": 10,
  "unknown_reset_seconds": 300,
  "response_header_timeout_seconds": 60,
  "max_request_mb": 128,
  "keep_awake_lid_closed": false,
  "keep_awake_lid_closed_power": "ac",
  "notify": { "failover": true, "compaction": false, "guards": true },
  "hotspot": { "ssid": "My Phone", "when": "lid-closed" },
  "intercept": { "mode": "transparent", "host": "api.anthropic.com" },
  "primary": {
    "provider": "oauth-passthrough",
    "base_url": "https://api.anthropic.com",
    "failover_strategy": "subscription-limit"
  },
  "secondary": {
    "provider": "openai-compatible",
    "base_url": "https://api.together.xyz/v1",
    "model": "zai-org/GLM-5.3",
    "keychain_service": "claude-burst-together"
  },
  "pricing": {
    "zai-org/GLM-5.3": { "input_per_mtok": 1.4, "output_per_mtok": 4.4 }
  },
  "metered_failover": {
    "window_seconds": 60,
    "min_failures": 3
  }
}
```

An Amazon Bedrock secondary instead has `"provider": "bedrock"`, the `bedrock-runtime` `base_url`, `"keychain_service": "claude-burst-bedrock"` and a required `model_map` from every Claude model to its Bedrock id, see [Amazon Bedrock notes](providers.md#amazon-bedrock-notes).

- `primary.provider` / `secondary.provider`: `oauth-passthrough` (subscription OAuth passthrough), `anthropic-api-key` (metered, no-subscription), `openai-compatible` (Together AI, OpenRouter, ...), or `bedrock`. Neither slot is tied to a specific vendor, either can hold any of them, though `configure --primary` only accepts the first three, so a non-Anthropic primary means editing `config.json`. `none` is valid for the secondary only.
- `primary.failover_strategy`: `subscription-limit` (only Anthropic's own subscription-exhaustion headers trigger failover, a bare 429 never does), `metered-failures` (a sliding-window failure count triggers failover, since every route is metered and a single blip shouldn't move traffic), `subscription-limit+metered-failures` (both: genuine subscription exhaustion fails over immediately as above, *and* a sustained run of 429/5xx responses or transport errors/timeouts, an Anthropic outage, not plan exhaustion, fails over once the relevant `metered_failover` threshold is reached, which is a different number for HTTP failures than for transport failures; see below), or `none` (never fail over). A subscription (`oauth-passthrough`) primary defaults to `subscription-limit` alone, which by design does **not** react to a bare 500 or a timeout, set `subscription-limit+metered-failures` (`claude-burst configure --failover-strategy subscription-limit+metered-failures`) if you also want overflow on an Anthropic outage.
- `metered_failover.window_seconds` / `min_failures` / `transport_error_min_failures`: for the metered strategies, how many upstream failures inside a trailing window before failing over. **Two counters, not one**, because the two signals differ in strength. An HTTP failure (429 or 5xx) means Anthropic answered and could be a passing blip, so it takes `min_failures` (default 3) within `window_seconds` (default 60). A transport failure, Anthropic could not be reached at all, takes `transport_error_min_failures`, which defaults to **1**, so a real outage does not sit retrying against a dead primary. Any success resets both. Other 4xx errors (bad key, malformed request) never count, since routing to the secondary wouldn't fix them; neither do failures that are unambiguously *this machine's* fault, DNS resolution failure, "network unreachable", "no route to host" - because the secondary is equally unreachable through a dead local network, and counting them turns walking out of WiFi range into a paid overflow window. Nor does a request the **client** cancelled: the outbound call carries Claude Code's own request context, so interrupting a turn cancels the upstream call too, and with `transport_error_min_failures` at 1 a single Esc used to arm a 300-second overflow window and bill the next few minutes of inference to the paid secondary (observed live 2026-09-08). Cancellation is excluded, and a cancelled request is never replayed to the secondary, nobody is waiting for the answer. A *deadline* that expires still counts, since that is a genuinely stalled upstream.
- `metered_failover.hotspot_transport_multiplier` (default `2`, `1` turns it off): while the failing connection went out through an iPhone Personal Hotspot (`172.20.10.x`), `transport_error_min_failures` is multiplied by this, and the log says `on a phone hotspot: needs N failures`. On a phone the mobile uplink is the likeliest thing to have dropped, and the secondary sits behind it too: on 2026-10-02 one broken pipe on the hotspot sent six turns to the paid secondary. HTTP 429/5xx answers are Anthropic speaking and are not scaled.
- `pricing`: per-million-token rates, keyed by the model that actually served the request. A third-party model is **not** in the defaults (the same GLM id costs different amounts through Together, OpenRouter and Z.ai), so add yours or its spend is reported as unpriced rather than free.
- `keep_awake_lid_closed` (default `false`) / `keep_awake_lid_closed_power` (`ac` default, or `always`): keep the Mac, and so Claude Code in Ghostty and Remote Control, running with the lid shut, plugged in only, or on battery too. Changing the file alone does nothing to the machine: apply with `configure --keep-awake-lid-closed` or `./install.sh`, and `claude-burst status` reports any drift. See [Keeping Claude Code working with the lid shut](lid-and-hotspot.md).
- `notify.failover` / `notify.compaction` / `notify.guards`: retired, ignored. Burst's notifications are the usage panel's on-screen alerts, see [Notifications](lid-and-hotspot.md#notifications).
- `hotspot.ssid` (empty: off) / `hotspot.when` (`lid-closed` default, or `always`): join that network when this Mac is offline, see [Join a hotspot when offline](lid-and-hotspot.md#join-a-hotspot-when-offline). Read live.
- `response_header_timeout_seconds`: bounds how long the gateway waits for a response to *start* before treating the upstream as failed (doesn't affect how long an already-started stream can run).

`./install.sh` re-applies `keep_awake_lid_closed` from `config.json` on every run. When it is `false` (the default) the installer touches no power settings and asks for no password; when `true` it asks for sudo once to apply the chosen power mode.


### The `intercept` block

- `mode`: `base-url` (the default: sets `ANTHROPIC_BASE_URL` in `~/.claude/settings.json`, no root, but Claude Code turns Remote Control off) or `transparent` (redirects at `/etc/hosts` and terminates TLS locally, so Remote Control keeps working). See [Transparent mode](transparent-mode.md).
- `host` (`api.anthropic.com`): the one hostname intercepted. The local CA is limited to this name.
- `tls_port` (`443`): the port Claude Code connects to.
- `ca_dir` (`~/.config/claude-burst/ca`): where the local CA and its key live.
- `ca_bundle` (`$NODE_EXTRA_CA_CERTS`, else `~/.claude/certs/node-extra-ca-certs.pem`): the bundle Claude Code trusts; the CA is appended inside a marked block and never replaces what is there.
- `resolver_doh` (`https://cloudflare-dns.com/dns-query`) and `upstream_addr` (empty): how the gateway finds the real Anthropic while `/etc/hosts` points the name at itself, see [How it avoids calling itself](transparent-mode.md#how-it-avoids-calling-itself).
