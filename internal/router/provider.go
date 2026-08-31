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

// ServeModeler is implemented by providers that serve a request with a
// DIFFERENT upstream model than the one Claude Code asked for -- the
// openai-compatible provider translating to a fixed or mapped third-party
// model, and Bedrock with its modelMap.
//
// Prepare() cannot simply return the upstream model for these, because
// forward() also passes Prepare's model into TranslateResponse, where it is
// echoed back to Claude Code in the message envelope's "model" field. Claude
// Code asked for claude-opus-5 and must see claude-opus-5 there, not
// zai-org/GLM-5.3. So the requested model travels the existing path, and the
// model that actually served -- the one that belongs in metrics.jsonl, in the
// admin UI, and in cost lookups -- comes from here.
//
// Without this, secondary rows are labelled and priced as the requested Claude
// model: an admin page showing "claude-opus-5, $2.08" for a request GLM
// actually served, at prices GLM does not charge.
type ServeModeler interface {
	// ServeModel returns the upstream model that serves a request whose
	// requested model is the argument. Must be deterministic for the same
	// input, since it is evaluated twice per request (Prepare and metrics).
	ServeModel(requestedModel string) string
}

// Translator is implemented by a Provider whose upstream speaks a different
// wire protocol than Anthropic's Messages API (e.g. an OpenAI-compatible
// chat-completions endpoint). When a Provider also implements Translator,
// forward() in router.go calls TranslateResponse instead of the generic
// byte-for-byte Server.relay -- the three Anthropic-wire providers
// (oauth-passthrough, anthropic-api-key, bedrock) implement neither this nor
// any equivalent, since a raw relay is already correct for them.
type Translator interface {
	TranslateResponse(w http.ResponseWriter, resp *http.Response, model string) (tokenUsage, error)
}
