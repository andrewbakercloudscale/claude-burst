# Routing and failover

[Back to the README](../README.md)

## Why this exists

Anthropic exposes materially different commercial models for access to the same Claude model families:

- Claude Max is a fixed monthly subscription with rolling usage limits.
- Claude API is metered by token.
- Together AI, OpenRouter and Amazon Bedrock all offer metered access to Claude-family or comparable models outside Anthropic's own billing.

Anthropic's Claude Code gateway documentation explicitly supports `ANTHROPIC_BASE_URL` with an existing claude.ai subscription login. Setting only the base URL keeps the subscription credential active and the subscription's usage limits and billing continue to apply. Claude Burst uses that supported gateway mechanism and respects the subscription limit rather than trying to evade it.

## Routing behaviour

1. Claude Code sends `/v1/messages` to `http://127.0.0.1:7777`.
2. Claude Burst forwards the request to `https://api.anthropic.com` unchanged, including the user's saved Claude subscription OAuth credential and required beta headers.
3. Successful responses stream straight back to Claude Code.
4. Generic `429` responses do **not** trigger overflow.
5. Overflow activates only on a `429` whose subscription headers indicate a rejected unified limit (a `400` or `529` sent while a window shows rejected, as on overage, is that error and never fails over), for example `anthropic-ratelimit-unified-status: rejected`, or when an explicit subscription-limit error is returned.
6. Claude Burst reads Anthropic's reset timestamp and persists it against **the model that was refused**, not the account.
7. The rejected request is replayed down that model's `fallback_chain` first, another Claude model, still on the subscription, still free.
8. Only when every rung has a rejection window of its own does the request go to the configured secondary, Together AI, OpenRouter, any other OpenAI-compatible endpoint, or Amazon Bedrock, using a credential stored in macOS Keychain.
9. Later requests for that model skip straight to the rung (or the secondary) until the reset time plus a small safety grace period; other models are untouched.
10. The first request after that time goes back to Anthropic Max automatically.

## When the network itself is the problem

A laptop changing WiFi looks like an Anthropic outage from the inside, and failing over does
not help: the secondary is behind the same network. These rules keep it from being treated as
one (the first three from the 2026-09-21 evening, when a hotspot-to-LAN switch put a healthy primary's
traffic behind a secondary that could not answer for five minutes):

- **A dead pooled connection is retried once, on a fresh one, with or without a secondary.** A
  `write: broken pipe` means the kept-alive connection died and the server never saw the
  request. An HTTP/2 connection that never sends headers back (`timeout awaiting response
  headers`) or a local address that vanished (`can't assign requested address`) is the same
  network switch seen from the other side; the request may have run, so it is resent once and
  never more (at worst one turn twice on the subscription, against a paid failover). Read-side
  resets are *not* retried. With no secondary configured this one resend is all Burst does
  before handing the error to Claude Code, whose own retries show on screen.
- **Anthropic gets about 30 seconds before anything fails over.** Errors that prove nothing was
  sent (a connect or TLS handshake timeout, a failed lookup) are retried at 2, 4, 8 and 16
  seconds; then the request goes down the model's `fallback_chain` on the subscription; only
  then to the secondary.
- **A request nobody is waiting for never fails over.** If Claude Code gave up while the retries
  ran, the request ends there: no window, no alert, nothing sent to a paid provider.
- **Silence while DNS is down does not fail over.** If the far side did not answer (a timeout,
  not a refused or reset connection) *and* the control lookup of `www.apple.com` fails, the
  request gets a fast, explicit 502 instead of waiting on a second dead host.
- **On a phone hotspot, a transport failure needs more evidence.** A phone's mobile data drops
  out for seconds at a time, which looks exactly like Anthropic being unreachable. While the Mac
  is on a hotspot, transport failures must repeat before they open an overflow window, rather
  than the single failure that is enough on a fixed network.
- **An outage window is short and releases itself.** A window armed by failures (as opposed to
  a rate limit) lasts `metered_failover.window_seconds` (60 s), not the 5-minute unknown-reset
  default, and ends the moment the secondary also fails at the transport level.

## Limits are per model, and are never inferred

Anthropic's claim headers name the *bucket* that was exhausted (`five_hour`,
`seven_day_opus`, `seven_day_overage_included`, …) but nothing in the response states which
**models** that bucket covers. Claude Burst does not guess: only the model that was actually
refused gets a window. If a limit really is account-wide, the next model discovers that for
itself on its first request, one rejection, which bills nothing.

Guessing the other way is what cost real money. Until 2026-09-20 any reported limit armed
one account-wide window, so a single refused Fable request sent **every** model to the paid
secondary for the next two days while Opus was answering normally.

`fallback_chain` in `config.json` is the ordered list of models to try on the subscription
before spending anything:

```json
"fallback_chain": {
  "claude-fable-5-1": ["claude-opus-5-5"],
  "claude-fable-5":   ["claude-opus-5-5"]
}
```

Those two are the shipped default. `claude-opus-5` → `claude-sonnet-5` works the same way
but is left for you to add deliberately, it is a much larger capability drop than a cost
saving justifies by default. A rung is skipped when it is inside a window of its own, and a
chain that names its own key is ignored rather than retrying the model that was just refused.

The dashboard's **Actions** section has a *Try another Claude model before the secondary*
toggle that turns the whole thing off live, without a config edit or a restart, and shows
which models are currently refused and where their traffic is actually going. `claude-burst
status` prints the same thing as `refused:` lines.

The forced window (`claude-burst force-secondary`, or **Force secondary** on the
dashboard) is still account-wide and deliberately bypasses the chain: its only purpose is to
exercise the secondary, and quietly serving a different Claude model instead would defeat
the only test that path ever gets.

Only `/v1/messages` participates in any of this. Everything else, `count_tokens`, and
Claude Code's control-plane traffic such as Remote Control's long-poll and settings fetch -
always goes to the primary, never fails over, and does not feed the failover detector in
either direction. There is nowhere correct to send those: an OpenAI-compatible endpoint has
no equivalent of a Remote Control long-poll, and translating a `count_tokens` body would
bill a full generation to answer "how many tokens is this". Just as importantly, a dropped
long-poll is not evidence that inference is failing and must not be able to open a paid
overflow window, and a healthy long-poll is not evidence that it has recovered.

Claude Burst does not rotate Max accounts, suppress quota signals, fabricate headers, or attempt to extend the Max allowance. The subscription limit remains authoritative.

## Overflow pruning

Every overflow request resends the whole conversation to a metered provider, and most of it is old tool output. Before it is sent, tool results older than the most recent 10 are replaced with a one-line note, and any single result over 40 KB keeps only its start and end. The subscription is never pruned: Anthropic caches its context, and rewriting it on every request would break the cache. The dashboard's **Context & cache** panel has the switches, what was not sent, the cache hit rate per route, and a verdict that turns red if pruned requests fail more often than unpruned ones. The settings are `secondary_pruning` in [Configuration](configuration.md).

## Forcing the secondary

A subscription primary only fails over on genuine exhaustion signals, which cannot be
provoked on demand, so the secondary path stays unexercised until the day it is needed,
which is the worst possible moment to find out it is misconfigured. To exercise it:

```bash
claude-burst force-secondary --minutes 15   # inference goes to the secondary
claude-burst reset                          # back to the primary immediately
```

The forced state is recorded with `limit_claim: "forced"`, so neither the metrics nor
`status` ever imply Anthropic reported a limit it did not.
