package router

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/andrewbakercloudscale/claude-burst/internal/config"
)

// subscriptionLimitDetector triggers failover only when Anthropic's own
// subscription-specific headers/body say the included Max/Pro allowance is
// exhausted. A bare transport error, or any ordinary upstream error that
// isn't a recognized subscription-limit signal, never triggers failover
// under this strategy -- this preserves the original oauth-passthrough
// behavior exactly.
type subscriptionLimitDetector struct{}

func (subscriptionLimitDetector) OnResponse(status int, h http.Header, body []byte) FailoverDecision {
	ok, claim, resetAt, reason := subscriptionLimit(status, h, body)
	if !ok {
		return FailoverDecision{}
	}
	return FailoverDecision{Failover: true, ResetAt: resetAt, Claim: claim, Reason: reason}
}

func (subscriptionLimitDetector) OnError(error) FailoverDecision { return FailoverDecision{} }
func (subscriptionLimitDetector) OnSuccess()                     {}

func subscriptionLimit(status int, h http.Header, body []byte) (bool, string, int64, string) {
	statusVal := strings.ToLower(h.Get("anthropic-ratelimit-unified-status"))
	claim := h.Get("anthropic-ratelimit-unified-representative-claim")
	if isRejected(statusVal) {
		return true, claim, resetFromHeaders(h, claim), "anthropic-ratelimit-unified-status=" + statusVal
	}
	for _, c := range []struct{ claim, header string }{
		{"five_hour", "anthropic-ratelimit-unified-5h-status"},
		{"seven_day", "anthropic-ratelimit-unified-7d-status"},
		{"seven_day_opus", "anthropic-ratelimit-unified-7d-opus-status"},
		{"seven_day_sonnet", "anthropic-ratelimit-unified-7d-sonnet-status"},
	} {
		if isRejected(strings.ToLower(h.Get(c.header))) {
			return true, c.claim, resetFromHeaders(h, c.claim), c.header + " rejected"
		}
	}
	// Conservative fallback: generic 429s are NOT treated as plan exhaustion.
	if status == http.StatusTooManyRequests {
		b := strings.ToLower(string(body))
		phrases := []string{"hit your session limit", "hit your weekly limit", "usage limit", "plan limit"}
		for _, p := range phrases {
			if strings.Contains(b, p) {
				return true, claim, resetFromHeaders(h, claim), "429 body matched subscription limit"
			}
		}
	}
	return false, "", 0, ""
}

func isRejected(v string) bool { return v == "rejected" || v == "rate_limited" }

func resetFromHeaders(h http.Header, claim string) int64 {
	keys := []string{"anthropic-ratelimit-unified-reset"}
	switch claim {
	case "five_hour":
		keys = append([]string{"anthropic-ratelimit-unified-5h-reset"}, keys...)
	case "seven_day":
		keys = append([]string{"anthropic-ratelimit-unified-7d-reset"}, keys...)
	case "seven_day_opus":
		keys = append([]string{"anthropic-ratelimit-unified-7d-opus-reset"}, keys...)
	case "seven_day_sonnet":
		keys = append([]string{"anthropic-ratelimit-unified-7d-sonnet-reset"}, keys...)
	}
	for _, k := range keys {
		if v := strings.TrimSpace(h.Get(k)); v != "" {
			if n, err := strconv.ParseInt(v, 10, 64); err == nil {
				return n
			}
			if t, err := time.Parse(time.RFC3339, v); err == nil {
				return t.Unix()
			}
		}
	}
	return 0
}

// combinedDetector layers a metered sliding-window failure count on top of
// subscription-limit detection. A genuine Anthropic subscription-exhaustion
// signal still fails over immediately (via sub), so normal Max/Pro overflow
// behavior is unchanged; in addition, a run of ordinary 429/5xx responses or
// transport errors (timeouts, connection refused) -- an Anthropic outage,
// not plan exhaustion -- fails over once minFailures are seen inside the
// trailing window. This is what "subscription-limit+metered-failures" buys
// over plain "subscription-limit": resilience to Anthropic being down, not
// just to the included allowance running out.
type combinedDetector struct {
	sub     subscriptionLimitDetector
	metered *meteredFailureDetector
}

func newCombinedDetector(mf config.MeteredFailoverConfig) *combinedDetector {
	return &combinedDetector{metered: newMeteredFailureDetector(mf.WindowSeconds, mf.MinFailures)}
}

func (d *combinedDetector) OnResponse(status int, h http.Header, body []byte) FailoverDecision {
	if dec := d.sub.OnResponse(status, h, body); dec.Failover {
		return dec
	}
	return d.metered.OnResponse(status, h, body)
}

func (d *combinedDetector) OnError(err error) FailoverDecision {
	// Subscription-limit signals only ever arrive on a response; a transport
	// error (timeout, connection refused) has no headers/body to inspect, so
	// only the metered leg can ever fire here.
	return d.metered.OnError(err)
}

func (d *combinedDetector) OnSuccess() { d.metered.OnSuccess() }

// noFailoverDetector never triggers failover. Used for the secondary slot
// (which must never itself fail over further -- a failure never chains past
// two hops) and for a primary explicitly configured with failover_strategy
// "none".
type noFailoverDetector struct{}

func (noFailoverDetector) OnResponse(int, http.Header, []byte) FailoverDecision {
	return FailoverDecision{}
}
func (noFailoverDetector) OnError(error) FailoverDecision { return FailoverDecision{} }
func (noFailoverDetector) OnSuccess()                     {}

// meteredFailureDetector is for primary providers with no subscription-style
// quota signal (e.g. anthropic-api-key), where every request -- on the
// primary AND the secondary -- is metered and costs money. A single
// transient 429/5xx should not immediately move traffic to a second paid
// provider, so this only fires after minFailures failures inside a trailing
// windowSeconds window, and any success resets the count. Safe for
// concurrent use.
type meteredFailureDetector struct {
	mu            sync.Mutex
	windowSeconds int
	minFailures   int
	failures      []time.Time
	now           func() time.Time
}

func newMeteredFailureDetector(windowSeconds, minFailures int) *meteredFailureDetector {
	if windowSeconds <= 0 {
		windowSeconds = 60
	}
	if minFailures <= 0 {
		minFailures = 3
	}
	return &meteredFailureDetector{windowSeconds: windowSeconds, minFailures: minFailures, now: time.Now}
}

func (d *meteredFailureDetector) OnSuccess() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.failures = nil
}

func (d *meteredFailureDetector) OnResponse(status int, h http.Header, body []byte) FailoverDecision {
	if !isMeteredFailureStatus(status) {
		return FailoverDecision{}
	}
	return d.recordFailure(fmt.Sprintf("status %d", status), h)
}

func (d *meteredFailureDetector) OnError(err error) FailoverDecision {
	if isLocalConnectivityFailure(err) {
		// A DNS resolution failure or "no route to host"/"network
		// unreachable" doesn't mean Anthropic is having trouble -- it means
		// THIS MACHINE has no path to the internet at all, which the
		// secondary would be equally unreachable through. Counting these
		// toward the metered window means walking out of WiFi range floods
		// the primary with failures that all look identical to a genuine
		// Anthropic outage, and the resulting overflow window (defaulting to
		// unknown_reset_seconds, since a transport error carries no reset
		// header to read) then keeps preferring a secondary that was never
		// actually the problem for minutes after connectivity returns.
		// Reproduced live: 2026-08-31.
		return FailoverDecision{}
	}
	return d.recordFailure("transport error: "+err.Error(), nil)
}

// isLocalConnectivityFailure reports whether err specifically indicates this
// machine has no network path to attempt a connection at all, as opposed to
// a connection that was attempted and failed for a reason that could
// plausibly be Anthropic's side: connection refused (something answered and
// said no), a timeout on an already-established connection, and TLS/HTTP
// errors are all left counting as before, since ruling those out reliably
// isn't possible without guessing. This only carves out the unambiguous
// cases -- DNS resolution failure, and the two "unreachable" syscall errors
// a dial reports when the local network stack itself has no route -- rather
// than trying to classify every transport error, which would trade one kind
// of wrong guess for another.
func isLocalConnectivityFailure(err error) bool {
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return true
	}
	return errors.Is(err, syscall.ENETUNREACH) || errors.Is(err, syscall.EHOSTUNREACH)
}

func (d *meteredFailureDetector) recordFailure(detail string, h http.Header) FailoverDecision {
	d.mu.Lock()
	defer d.mu.Unlock()
	now := d.now()
	cutoff := now.Add(-time.Duration(d.windowSeconds) * time.Second)
	kept := d.failures[:0]
	for _, t := range d.failures {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	kept = append(kept, now)
	d.failures = kept
	if len(d.failures) < d.minFailures {
		return FailoverDecision{}
	}
	var reset int64
	if h != nil {
		reset = meteredResetFromHeaders(h)
	}
	return FailoverDecision{
		Failover: true,
		ResetAt:  reset,
		Claim:    "metered_sustained_failures",
		Reason:   fmt.Sprintf("%d failures within %ds (latest: %s)", len(d.failures), d.windowSeconds, detail),
	}
}

// isMeteredFailureStatus reports whether status counts toward the metered
// sliding-window failure count. Other 4xx (invalid/expired key, malformed
// request) never count: routing to the secondary provider won't fix them.
func isMeteredFailureStatus(status int) bool {
	return status == http.StatusTooManyRequests || status >= 500
}

func meteredResetFromHeaders(h http.Header) int64 {
	if v := strings.TrimSpace(h.Get("retry-after")); v != "" {
		if secs, err := strconv.ParseInt(v, 10, 64); err == nil {
			return time.Now().Add(time.Duration(secs) * time.Second).Unix()
		}
	}
	for _, k := range []string{"anthropic-ratelimit-requests-reset", "anthropic-ratelimit-tokens-reset"} {
		if v := strings.TrimSpace(h.Get(k)); v != "" {
			if n, err := strconv.ParseInt(v, 10, 64); err == nil {
				return n
			}
			if t, err := time.Parse(time.RFC3339, v); err == nil {
				return t.Unix()
			}
		}
	}
	return 0
}
