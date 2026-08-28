package router

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
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

// noFailoverDetector never triggers failover. Used for the secondary slot
// (which must never itself fail over further -- a failure never chains past
// two hops) and for a primary explicitly configured with failover_strategy
// "none".
type noFailoverDetector struct{}

func (noFailoverDetector) OnResponse(int, http.Header, []byte) FailoverDecision {
	return FailoverDecision{}
}
func (noFailoverDetector) OnError(error) FailoverDecision { return FailoverDecision{} }
func (noFailoverDetector) OnSuccess()                      {}

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
	return d.recordFailure("transport error: "+err.Error(), nil)
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
