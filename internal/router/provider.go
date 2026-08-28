package router

import (
	"context"
	"net/http"
)

// Provider builds the outbound *http.Request for one backend's wire format.
// It never decides failover; it only translates auth/model/headers for its
// vendor. Both slots (primary, secondary) are just configured Providers --
// neither slot is tied to a specific vendor.
type Provider interface {
	// Name identifies this provider in logs and in metrics.jsonl's "route"
	// field. Must stay stable across refactors: existing log/metric
	// consumers depend on the literal string "anthropic" for the
	// oauth-passthrough provider and "bedrock" for the Bedrock provider.
	Name() string

	// Prepare builds the outbound request from the inbound Claude Code
	// request and already-buffered body (nil for non-inference requests).
	// It returns the effective model name (after any provider-side
	// remapping, used for pricing/logging) and, on failure, a
	// *ProviderError describing which stage failed and what HTTP status
	// should be returned to the client.
	Prepare(ctx context.Context, in *http.Request, body []byte) (*http.Request, string, error)
}

// ProviderError carries the HTTP status and named stage a Prepare-time
// failure should produce, so the generic forward() loop in router.go never
// needs per-vendor knowledge of what a given failure means.
type ProviderError struct {
	Status int
	Stage  string
	Model  string
	Err    error
}

func (e *ProviderError) Error() string { return e.Err.Error() }
func (e *ProviderError) Unwrap() error { return e.Err }

// FailoverDecision is returned after an upstream outcome, telling the router
// whether to stop using the current provider and replay the request to the
// configured secondary.
type FailoverDecision struct {
	Failover bool
	ResetAt  int64  // unix seconds; 0 = unknown, caller applies its own fallback
	Claim    string // e.g. "five_hour", "metered_sustained_failures"
	Reason   string // human-readable, for logs/metrics
}

// FailoverDetector decides, from upstream outcomes on the primary provider,
// whether traffic should move to the secondary provider. Implementations
// must be safe for concurrent use: Claude Code issues concurrent requests
// from subagents.
type FailoverDetector interface {
	OnResponse(status int, h http.Header, body []byte) FailoverDecision
	OnError(err error) FailoverDecision
	OnSuccess()
}
