# Known limitations

[Back to the README](../README.md)

## 1. No failover on ordinary throttling

Anthropic uses HTTP 429 for several different conditions. Claude Burst deliberately refuses to interpret a bare 429 as Max exhaustion. This avoids turning a temporary capacity throttle into unexpected spend on the secondary.

## 2. API-equivalent cost is not Anthropic's internal cost

The metrics estimate answers: "What would these observed input/output tokens cost at the configured public API rates?" It does not estimate Anthropic's marginal inference cost, gross margin, internal transfer pricing, or the economic value of prompt caching unless you extend the metric model to account for cache buckets.

## 3. Consumer versus commercial governance remains different

A local data-loss-prevention layer can reduce what leaves the machine, but it does not make a consumer Max account contractually or operationally identical to Claude for Work, the Claude API, or Bedrock. Review your organization's legal, procurement, retention, audit and account-management requirements before rolling consumer subscriptions out to employees.

## 4. No caller authentication on the local gateway

The gateway has no login: any local process can send it a request. It cannot force an overflow window open (only genuine subscription-limit or sustained-failure signals do that), but it can ride an already-open one, and it can drive ordinary traffic through your credential, as any local process could by reading that credential itself.

What stops a web page from doing the same through your browser is an Origin and Host guard: the gateway refuses a request that carries a browser `Origin` header, or whose `Host` names anything but the gateway's own loopback address or the intercepted host. Before that guard, a page could send `POST /v1/messages` with a simple content type, which needs no CORS preflight. Don't bind `listen` to anything but `127.0.0.1`.

## 5. Upstream error text (including the request path/query) is logged and metered failure detail is not size-bounded

Transport-error and non-failover-error log lines include the upstream `error.Error()` string, which can contain the request URL (path and query, not host credentials, Go's `url.Error` redacts userinfo). Prompts and response bodies are never included per the metadata-only design, but treat `claude-burst.log` as containing request metadata, not as fully opaque.

## 6. Transparent intercept mode is machine-wide, and TLS interception is assumed benign

The `/etc/hosts` entry transparent mode installs affects every process on the Mac, not just
Claude Code, see [the trade-off table](transparent-mode.md#what-it-costs). Separately, the design assumes that TLS
interception does not itself break Remote Control. That is well supported (Claude Code is
widely run behind corporate inspecting proxies, and documents `NODE_EXTRA_CA_CERTS` for
exactly that) but is not something this project can prove. `scripts/check-interception.sh`
settles it on a network that actually inspects TLS: it distinguishes *intercepted* from
*bypassed* from *not enrolled*, which a bare certificate-issuer check cannot.

## 7. Lid-closed keep-awake changes a machine-wide power setting

`keep_awake_lid_closed` sets `pmset SleepDisabled`, which applies to the whole Mac, not just Claude Code. In the default `ac` mode a root LaunchDaemon turns it off when you unplug; if that daemon is stopped, the last value stays, so a Mac unplugged while it is down will not sleep with the lid shut. `claude-burst status` and `scripts/lid-awake-root.sh status` show the daemon and the live value. In `always` mode a closed laptop on battery never sleeps: heat and a flat battery in a bag. Unplugging while the lid is already shut has not been verified to sleep the Mac immediately rather than at its next wake check.
