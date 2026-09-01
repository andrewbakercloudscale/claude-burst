package router

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/andrewbakercloudscale/claude-burst/internal/config"
)

// newTestServer builds a Server wired to httptest primary/bedrock upstreams
// (either may be nil to leave that route unset), with its own temp state and
// metrics files, and returns it along with a buffer capturing everything
// written through s.logger so tests can assert on log content.
func newTestServer(t *testing.T, primaryURL, bedrockURL string) (*Server, *bytes.Buffer) {
	t.Helper()
	cfg := config.Default()
	if primaryURL != "" {
		cfg.AnthropicBaseURL = primaryURL
	}
	if bedrockURL != "" {
		cfg.BedrockBaseURL = bedrockURL
	}
	dir := t.TempDir()
	var logBuf bytes.Buffer
	s, err := New(cfg, filepath.Join(dir, "state.json"), filepath.Join(dir, "metrics.jsonl"), log.New(&logBuf, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	return s, &logBuf
}

func TestSubscriptionLimitFromUnifiedHeader(t *testing.T) {
	h := http.Header{}
	h.Set("anthropic-ratelimit-unified-status", "rejected")
	h.Set("anthropic-ratelimit-unified-representative-claim", "five_hour")
	h.Set("anthropic-ratelimit-unified-5h-reset", "1800000000")
	ok, claim, reset, _ := subscriptionLimit(429, h, []byte(`{"type":"error"}`))
	if !ok || claim != "five_hour" || reset != 1800000000 {
		t.Fatalf("got ok=%v claim=%q reset=%d", ok, claim, reset)
	}
}

func TestGeneric429DoesNotFailOver(t *testing.T) {
	h := http.Header{}
	ok, _, _, _ := subscriptionLimit(429, h, []byte(`{"error":{"type":"rate_limit_error","message":"Server is temporarily limiting requests"}}`))
	if ok {
		t.Fatal("generic 429 must not trigger paid overflow")
	}
}

func TestRewriteModel(t *testing.T) {
	b, model, err := rewriteModel([]byte(`{"model":"claude-sonnet-5","messages":[]}`), map[string]string{"claude-sonnet-5": "global.anthropic.claude-sonnet-5"})
	if err != nil {
		t.Fatal(err)
	}
	if model != "global.anthropic.claude-sonnet-5" {
		t.Fatalf("wrong model %q", model)
	}
	if requestModel(b) != model {
		t.Fatalf("body model not rewritten: %s", b)
	}
}

func TestStripOAuthBeta(t *testing.T) {
	h := http.Header{}
	h.Set("anthropic-beta", "oauth-2025-04-20,claude-code-20250219,context-management-2025-06-27")
	stripOAuthBeta(h)
	got := h.Get("anthropic-beta")
	if got != "claude-code-20250219,context-management-2025-06-27" {
		t.Fatalf("got %q", got)
	}
}

func TestFailoverReplaysRequestToBedrock(t *testing.T) {
	t.Setenv("AWS_BEARER_TOKEN_BEDROCK", "test-bedrock-key")

	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("anthropic-ratelimit-unified-status", "rejected")
		w.Header().Set("anthropic-ratelimit-unified-representative-claim", "five_hour")
		w.Header().Set("anthropic-ratelimit-unified-reset", strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10))
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"rate_limit_error","message":"limit"}}`))
	}))
	defer primary.Close()

	bedrockCalled := false
	bedrock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bedrockCalled = true
		if got := r.Header.Get("Authorization"); got != "Bearer test-bedrock-key" {
			t.Fatalf("bad auth %q", got)
		}
		if strings.Contains(r.Header.Get("anthropic-beta"), "oauth-") {
			t.Fatal("oauth beta leaked to Bedrock")
		}
		b, _ := io.ReadAll(r.Body)
		if requestModel(b) != "global.anthropic.claude-sonnet-5" {
			t.Fatalf("model not mapped: %s", b)
		}
		w.Header().Set("content-type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer bedrock.Close()

	cfg := config.Default()
	cfg.AnthropicBaseURL = primary.URL
	cfg.BedrockBaseURL = bedrock.URL
	dir := t.TempDir()
	s, err := New(cfg, filepath.Join(dir, "state.json"), filepath.Join(dir, "metrics.jsonl"), log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatal(err)
	}

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "http://local/v1/messages?beta=true", strings.NewReader(`{"model":"claude-sonnet-5","messages":[]}`))
	req.Header.Set("Authorization", "Bearer oauth-token")
	req.Header.Set("anthropic-beta", "oauth-2025-04-20,claude-code-20250219")
	s.ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if !bedrockCalled {
		t.Fatal("Bedrock was not called")
	}
	if !s.inOverflow(time.Now()) {
		t.Fatal("overflow state was not activated")
	}
}

// TestEveryRequestIsLoggedStartAndDone verifies the top-level ServeHTTP
// wrapper always writes a start line and a matching done line (with status
// and duration) for every request, success or not, so the text log alone is
// enough to answer "what did the gateway just do" without cross-referencing
// metrics.jsonl.
func TestEveryRequestIsLoggedStartAndDone(t *testing.T) {
	s, logBuf := newTestServer(t, "", "")

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "http://local/healthz", nil)
	s.ServeHTTP(rr, req)

	logs := logBuf.String()
	if !strings.Contains(logs, `start method="GET" path="/healthz"`) {
		t.Fatalf("missing start log line, got:\n%s", logs)
	}
	if !strings.Contains(logs, `done method="GET" path="/healthz" status=200`) {
		t.Fatalf("missing done log line with status, got:\n%s", logs)
	}
	// Both lines for the same request must share a request id.
	var startID, doneID string
	for _, line := range strings.Split(logs, "\n") {
		if strings.Contains(line, " start ") {
			startID = strings.Fields(line)[0]
		}
		if strings.Contains(line, " done ") {
			doneID = strings.Fields(line)[0]
		}
	}
	if startID == "" || startID != doneID {
		t.Fatalf("start/done request ids did not match: start=%q done=%q", startID, doneID)
	}
}

// TestBodyTooLargeIsLoggedAndRejected verifies oversized request bodies are
// rejected with 413 and that the rejection reason is logged, since this is
// exactly the kind of failure an operator needs visible in the log rather
// than silently dropped.
func TestBodyTooLargeIsLoggedAndRejected(t *testing.T) {
	s, logBuf := newTestServer(t, "", "")
	s.cfg.MaxRequestMB = 0
	if s.cfg.MaxRequestMB <= 0 {
		s.cfg.MaxRequestMB = 1 // config.Load would normally coerce this; set directly for the test
	}
	s.cfg.MaxRequestMB = 1

	oversized := strings.Repeat("a", 2*1024*1024)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "http://local/v1/messages", strings.NewReader(oversized))
	s.ServeHTTP(rr, req)

	if rr.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(logBuf.String(), "stage=read_body reason=too_large") {
		t.Fatalf("missing too_large log line, got:\n%s", logBuf.String())
	}
}

// TestKeychainMissingKeyLogsAndReturns503 verifies that when Anthropic
// rejects the subscription allowance but no Bedrock credential is available
// (the common misconfiguration: forgot `claude-burst keychain-set`), the
// gateway returns a clear 503 rather than hanging, and the failure stage is
// logged so it is diagnosable from the log file alone.
func TestKeychainMissingKeyLogsAndReturns503(t *testing.T) {
	t.Setenv("AWS_BEARER_TOKEN_BEDROCK", "") // ensure no key is available on this path

	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("anthropic-ratelimit-unified-status", "rejected")
		w.Header().Set("anthropic-ratelimit-unified-representative-claim", "five_hour")
		w.Header().Set("anthropic-ratelimit-unified-reset", strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10))
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"rate_limit_error","message":"limit"}}`))
	}))
	defer primary.Close()

	s, logBuf := newTestServer(t, primary.URL, "http://127.0.0.1:0")

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "http://local/v1/messages", strings.NewReader(`{"model":"claude-sonnet-5","messages":[]}`))
	s.ServeHTTP(rr, req)

	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(logBuf.String(), "stage=keychain_load") {
		t.Fatalf("missing keychain_load error log, got:\n%s", logBuf.String())
	}
}

// TestBedrockModelMappingFailureLogsAndReturns502 verifies an unmapped model
// during overflow is rejected clearly (502) and logged with the offending
// model name, instead of silently sending the wrong model to Bedrock or
// failing without explanation.
func TestBedrockModelMappingFailureLogsAndReturns502(t *testing.T) {
	t.Setenv("AWS_BEARER_TOKEN_BEDROCK", "test-bedrock-key")

	s, logBuf := newTestServer(t, "", "http://127.0.0.1:0")
	s.activateOverflow(time.Now().Add(time.Hour).Unix(), "five_hour", "test setup")

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "http://local/v1/messages", strings.NewReader(`{"model":"some-unmapped-model","messages":[]}`))
	s.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadGateway {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(logBuf.String(), `stage=model_mapping route=bedrock requested_model="some-unmapped-model"`) {
		t.Fatalf("missing model_mapping error log, got:\n%s", logBuf.String())
	}
}

// TestUpstreamErrorNoFailoverIsLogged verifies that an ordinary (non
// subscription-limit) upstream error is passed through unchanged to Claude
// Code and logged, so a real Anthropic-side outage is visible in the log
// rather than being confused with a failover event.
func TestUpstreamErrorNoFailoverIsLogged(t *testing.T) {
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"api_error","message":"boom"}}`))
	}))
	defer primary.Close()

	s, logBuf := newTestServer(t, primary.URL, "")

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "http://local/v1/messages", strings.NewReader(`{"model":"claude-sonnet-5","messages":[]}`))
	s.ServeHTTP(rr, req)

	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if s.inOverflow(time.Now()) {
		t.Fatal("a plain upstream 500 must not trigger overflow")
	}
	if !strings.Contains(logBuf.String(), "upstream_error route=anthropic") {
		t.Fatalf("missing upstream_error log line, got:\n%s", logBuf.String())
	}
}

// TestMetricsWriteFailureDoesNotBreakRequest verifies that if metrics.jsonl
// cannot be written (disk full, permissions, path taken by a directory), the
// gateway still serves the request successfully to Claude Code and logs the
// metrics failure instead of losing it silently or failing the request.
func TestMetricsWriteFailureDoesNotBreakRequest(t *testing.T) {
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer primary.Close()

	cfg := config.Default()
	cfg.AnthropicBaseURL = primary.URL
	dir := t.TempDir()
	metricsPath := filepath.Join(dir, "metrics.jsonl")
	if err := os.MkdirAll(metricsPath, 0700); err != nil { // a directory where a file is expected
		t.Fatal(err)
	}
	var logBuf bytes.Buffer
	s, err := New(cfg, filepath.Join(dir, "state.json"), metricsPath, log.New(&logBuf, "", 0))
	if err != nil {
		t.Fatal(err)
	}

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "http://local/v1/messages", strings.NewReader(`{"model":"claude-sonnet-5","messages":[]}`))
	s.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("request must still succeed even though metrics failed: status=%d body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(logBuf.String(), "stage=write_metric") {
		t.Fatalf("missing write_metric error log, got:\n%s", logBuf.String())
	}
}

// panicReader is a broken io.ReadCloser that panics on Read, standing in for
// the kind of unexpected failure (e.g. a corrupted transport) the recovery
// middleware must survive.
type panicReader struct{}

func (panicReader) Read(_ []byte) (int, error) { panic("simulated panic reading request body") }
func (panicReader) Close() error               { return nil }

// TestPanicRecoveryReturns500AndLogsStack verifies that a panic anywhere in
// the request path is caught by ServeHTTP, turned into a 500 instead of
// crashing the gateway or hanging Claude Code's connection, and logged with
// a stack trace for debugging.
func TestPanicRecoveryReturns500AndLogsStack(t *testing.T) {
	s, logBuf := newTestServer(t, "", "")

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "http://local/v1/messages", nil)
	req.Body = panicReader{}

	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("panic escaped ServeHTTP instead of being recovered: %v", r)
			}
		}()
		s.ServeHTTP(rr, req)
	}()

	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	logs := logBuf.String()
	if !strings.Contains(logs, "PANIC") || !strings.Contains(logs, "simulated panic reading request body") {
		t.Fatalf("missing panic log line, got:\n%s", logs)
	}
	if !strings.Contains(logs, `done method="POST" path="/v1/messages"`) {
		t.Fatalf("done line must still be logged after a recovered panic, got:\n%s", logs)
	}
}

// TestNoSecondaryConfigured_NeverFailsOver verifies that when a config has
// no secondary route at all (e.g. BedrockBaseURL explicitly cleared), a
// subscription-limit rejection on the primary is passed straight through to
// the client and overflow is never activated -- there is nothing to fail
// over to.
// TestValidateBaseURLRejectsGarbage is a regression test: url.Parse alone
// accepts almost any string (empty, a bare host with no scheme, a relative
// path), silently deferring the failure to request time instead of startup.
// validateBaseURL must catch these at configuration/construction time.
func TestValidateBaseURLRejectsGarbage(t *testing.T) {
	for _, raw := range []string{"", "not-a-url-at-all", "/just-a-path", "ftp://example.com"} {
		if _, err := validateBaseURL("test", raw); err == nil {
			t.Fatalf("expected validateBaseURL to reject %q, got nil error", raw)
		}
	}
	if _, err := validateBaseURL("test", "https://example.com/anthropic"); err != nil {
		t.Fatalf("expected a valid https URL to pass, got %v", err)
	}
}

// TestBuildForwardRequestRejectsPathTraversal is a regression test: a
// request path with enough ".." segments to escape the configured base path
// must not reach the credentialed upstream at an unintended path on the
// same host.
func TestBuildForwardRequestRejectsPathTraversal(t *testing.T) {
	base, err := url.Parse("https://bedrock-runtime.us-east-1.amazonaws.com/anthropic")
	if err != nil {
		t.Fatal(err)
	}
	in := httptest.NewRequest(http.MethodPost, "http://local/v1/messages/../../../secret-admin-api", nil)
	if _, err := buildForwardRequest(context.Background(), base, in, nil, true); err == nil {
		t.Fatal("expected a path-traversal request to be rejected, got nil error")
	}

	// A normal, non-escaping path must still work.
	in = httptest.NewRequest(http.MethodPost, "http://local/v1/messages", nil)
	req, err := buildForwardRequest(context.Background(), base, in, nil, true)
	if err != nil {
		t.Fatalf("unexpected error for a normal path: %v", err)
	}
	if req.URL.Path != "/anthropic/v1/messages" {
		t.Fatalf("got path %q", req.URL.Path)
	}
}

func TestNoSecondaryConfigured_NeverFailsOver(t *testing.T) {
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("anthropic-ratelimit-unified-status", "rejected")
		w.Header().Set("anthropic-ratelimit-unified-representative-claim", "five_hour")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"type":"error"}`))
	}))
	defer primary.Close()

	cfg := config.Default()
	cfg.AnthropicBaseURL = primary.URL
	cfg.BedrockBaseURL = "" // explicitly no secondary
	dir := t.TempDir()
	s, err := New(cfg, filepath.Join(dir, "state.json"), filepath.Join(dir, "metrics.jsonl"), log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	if s.secondary != nil {
		t.Fatal("expected no secondary provider to be configured")
	}

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "http://local/v1/messages", strings.NewReader(`{"model":"claude-sonnet-5","messages":[]}`))
	s.ServeHTTP(rr, req)

	if rr.Code != http.StatusTooManyRequests {
		t.Fatalf("expected the subscription-limit response passed straight through, got %d", rr.Code)
	}
	if s.inOverflow(time.Now()) {
		t.Fatal("overflow must never activate when no secondary is configured")
	}
	if s.HasSecondary() {
		t.Fatal("HasSecondary must report false when no secondary is configured")
	}
}

// TestForcedOverflowWithNilSecondaryReturns502NotPanic guards the router
// half of the fix for a real bug: admin's "Force -> secondary" used to
// validate against a freshly re-read config.json rather than this process's
// actual s.secondary, so ForceOverflow could be armed (directly, or via
// state.json surviving a restart into a config with the secondary removed)
// while s.secondary was nil. Routing "secondary" straight into forward()
// with a nil Provider panics on the first interface method call; ServeHTTP's
// recover() turns that into an opaque 500 with no indication of what went
// wrong, and the overflow window then keeps every retry broken until
// something clears it. This must instead be a clear, diagnosable error.
func TestForcedOverflowWithNilSecondaryReturns502NotPanic(t *testing.T) {
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"type":"message"}`))
	}))
	defer primary.Close()

	cfg := config.Default()
	cfg.AnthropicBaseURL = primary.URL
	cfg.BedrockBaseURL = "" // explicitly no secondary
	dir := t.TempDir()
	s, err := New(cfg, filepath.Join(dir, "state.json"), filepath.Join(dir, "metrics.jsonl"), log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	if s.HasSecondary() {
		t.Fatal("expected no secondary provider to be configured")
	}

	s.ForceOverflow(0, "test: simulating a config/gateway drift")
	if !s.inOverflow(time.Now()) {
		t.Fatal("ForceOverflow should have armed the overflow window")
	}

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "http://local/v1/messages", strings.NewReader(`{"model":"claude-sonnet-5","messages":[]}`))
	s.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadGateway {
		t.Fatalf("expected a clean 502 when overflow is active but no secondary is configured, got %d (body=%s)", rr.Code, rr.Body.String())
	}
}

func TestUnknownProviderNameRejected(t *testing.T) {
	cfg := config.Default()
	cfg.Primary = config.RouteConfig{Provider: "together-ai", BaseURL: "https://api.together.xyz"}
	dir := t.TempDir()
	if _, err := New(cfg, filepath.Join(dir, "state.json"), filepath.Join(dir, "metrics.jsonl"), log.New(io.Discard, "", 0)); err == nil {
		t.Fatal("expected an error for an unknown provider name, got nil")
	}
}

func TestUnknownFailoverStrategyRejected(t *testing.T) {
	cfg := config.Default()
	cfg.Primary = config.RouteConfig{Provider: "anthropic-api-key", BaseURL: cfg.AnthropicBaseURL, FailoverStrategy: "bogus-strategy"}
	dir := t.TempDir()
	if _, err := New(cfg, filepath.Join(dir, "state.json"), filepath.Join(dir, "metrics.jsonl"), log.New(io.Discard, "", 0)); err == nil {
		t.Fatal("expected an error for an unknown failover strategy, got nil")
	}
}

// TestAnthropicAPIKeyProviderForwardsHeadersUnchanged verifies the new
// no-subscription primary mode: the gateway never holds or injects an
// Anthropic credential itself, it just forwards whatever auth header Claude
// Code already sent (here, x-api-key from a direct API key), exactly like
// oauth-passthrough does for a subscription OAuth header.
func TestAnthropicAPIKeyProviderForwardsHeadersUnchanged(t *testing.T) {
	var gotKey, gotBeta string
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("x-api-key")
		gotBeta = r.Header.Get("anthropic-beta")
		w.Header().Set("content-type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer primary.Close()

	cfg := config.Default()
	cfg.Primary = config.RouteConfig{Provider: "anthropic-api-key", BaseURL: primary.URL, FailoverStrategy: "metered-failures"}
	cfg.Secondary = config.RouteConfig{}
	cfg.BedrockBaseURL = ""
	dir := t.TempDir()
	s, err := New(cfg, filepath.Join(dir, "state.json"), filepath.Join(dir, "metrics.jsonl"), log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	if s.primary.Name() != "anthropic-api-key" {
		t.Fatalf("expected provider name anthropic-api-key, got %q", s.primary.Name())
	}

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "http://local/v1/messages", strings.NewReader(`{"model":"claude-sonnet-5","messages":[]}`))
	req.Header.Set("x-api-key", "sk-ant-test-key")
	req.Header.Set("anthropic-beta", "claude-code-20250219")
	s.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if gotKey != "sk-ant-test-key" {
		t.Fatalf("x-api-key not forwarded unchanged: got %q", gotKey)
	}
	if gotBeta != "claude-code-20250219" {
		t.Fatalf("anthropic-beta not forwarded unchanged: got %q", gotBeta)
	}
}

// TestMeteredFailoverReplaysAfterSustainedFailures is the full httptest
// analog of TestFailoverReplaysRequestToBedrock, but for the new
// anthropic-api-key + metered-failures primary: the first (minFailures-1)
// 429s must pass straight through with no Bedrock call and no overflow, and
// only the Nth failure activates overflow and replays to Bedrock.
func TestMeteredFailoverReplaysAfterSustainedFailures(t *testing.T) {
	t.Setenv("AWS_BEARER_TOKEN_BEDROCK", "test-bedrock-key")

	primaryCalls := 0
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		primaryCalls++
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"rate_limit_error","message":"rate limited"}}`))
	}))
	defer primary.Close()

	bedrockCalled := false
	bedrock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bedrockCalled = true
		w.Header().Set("content-type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer bedrock.Close()

	cfg := config.Default()
	cfg.Primary = config.RouteConfig{Provider: "anthropic-api-key", BaseURL: primary.URL, FailoverStrategy: "metered-failures"}
	cfg.Secondary = config.RouteConfig{Provider: "bedrock", BaseURL: bedrock.URL, KeychainService: cfg.KeychainService, ModelMap: cfg.ModelMap}
	cfg.MeteredFailover = config.MeteredFailoverConfig{WindowSeconds: 60, MinFailures: 3}

	dir := t.TempDir()
	s, err := New(cfg, filepath.Join(dir, "state.json"), filepath.Join(dir, "metrics.jsonl"), log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatal(err)
	}

	body := `{"model":"claude-sonnet-5","messages":[]}`
	for i := 0; i < 2; i++ {
		rr := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "http://local/v1/messages", strings.NewReader(body))
		req.Header.Set("x-api-key", "test-anthropic-key")
		s.ServeHTTP(rr, req)
		if rr.Code != http.StatusTooManyRequests {
			t.Fatalf("attempt %d: expected 429 passthrough before threshold, got %d", i, rr.Code)
		}
	}
	if bedrockCalled {
		t.Fatal("Bedrock must not be called before the failure threshold is reached")
	}
	if s.inOverflow(time.Now()) {
		t.Fatal("overflow must not activate before the failure threshold is reached")
	}

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "http://local/v1/messages", strings.NewReader(body))
	req.Header.Set("x-api-key", "test-anthropic-key")
	s.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("3rd attempt should have failed over to Bedrock: status=%d body=%s", rr.Code, rr.Body.String())
	}
	if !bedrockCalled {
		t.Fatal("Bedrock was not called after sustained primary failures")
	}
	if !s.inOverflow(time.Now()) {
		t.Fatal("overflow state was not activated after sustained failures")
	}
	if primaryCalls != 3 {
		t.Fatalf("expected exactly 3 primary calls, got %d", primaryCalls)
	}
}

// TestOpenAICompatibleFailoverTranslatesResponse is the httptest analog of
// TestFailoverReplaysRequestToBedrock for a translating secondary: a
// subscription-limit rejection on the primary must still replay to the
// secondary, but the secondary's OpenAI-shaped response must come back to
// the client correctly translated into Anthropic's Messages format, not
// relayed raw.
func TestOpenAICompatibleFailoverTranslatesResponse(t *testing.T) {
	// The env var is now derived from the configured keychain service
	// ("claude-burst-together-test" -> TOGETHER_TEST_API_KEY), not
	// hardcoded to TOGETHER_API_KEY -- see openAICompatibleIdentity.
	t.Setenv("TOGETHER_TEST_API_KEY", "test-together-key")

	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("anthropic-ratelimit-unified-status", "rejected")
		w.Header().Set("anthropic-ratelimit-unified-representative-claim", "five_hour")
		w.Header().Set("anthropic-ratelimit-unified-reset", strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10))
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"rate_limit_error","message":"limit"}}`))
	}))
	defer primary.Close()

	togetherCalled := false
	together := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		togetherCalled = true
		if got := r.Header.Get("Authorization"); got != "Bearer test-together-key" {
			t.Fatalf("bad auth %q", got)
		}
		if !strings.HasSuffix(r.URL.Path, "/chat/completions") {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		b, _ := io.ReadAll(r.Body)
		var oaiReq map[string]any
		if err := json.Unmarshal(b, &oaiReq); err != nil {
			t.Fatalf("outbound body not JSON: %v", err)
		}
		if oaiReq["model"] != "zai-org/GLM-5.3" {
			t.Fatalf("model not remapped: %v", oaiReq["model"])
		}
		w.Header().Set("content-type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"hi from GLM"},"finish_reason":"stop"}],"usage":{"prompt_tokens":4,"completion_tokens":3}}`))
	}))
	defer together.Close()

	cfg := config.Default()
	cfg.Primary = config.RouteConfig{Provider: "oauth-passthrough", BaseURL: primary.URL, FailoverStrategy: "subscription-limit"}
	cfg.Secondary = config.RouteConfig{Provider: "openai-compatible", BaseURL: together.URL, Model: "zai-org/GLM-5.3", KeychainService: "claude-burst-together-test"}
	dir := t.TempDir()
	s, err := New(cfg, filepath.Join(dir, "state.json"), filepath.Join(dir, "metrics.jsonl"), log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatal(err)
	}

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "http://local/v1/messages", strings.NewReader(`{"model":"claude-sonnet-5","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer oauth-token")
	s.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if !togetherCalled {
		t.Fatal("Together was not called")
	}
	if !s.inOverflow(time.Now()) {
		t.Fatal("overflow state was not activated")
	}

	var anthResp map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &anthResp); err != nil {
		t.Fatalf("client did not receive valid JSON: %v\nbody=%s", err, rr.Body.String())
	}
	if anthResp["type"] != "message" || anthResp["role"] != "assistant" {
		t.Fatalf("response is not Anthropic-shaped: %v", anthResp)
	}
	content, ok := anthResp["content"].([]any)
	if !ok || len(content) != 1 {
		t.Fatalf("got content=%v", anthResp["content"])
	}
	block := content[0].(map[string]any)
	if block["type"] != "text" || block["text"] != "hi from GLM" {
		t.Fatalf("got block=%v", block)
	}
}

// TestCountTokensNeverRoutesToSecondaryDuringOverflow guards against a real
// bug: /v1/messages/count_tokens matched isInference's plain prefix check, so
// during an overflow window it was sent to the openai-compatible secondary
// like any other inference call. The OpenAI translator has no concept of a
// count-only request -- it mistranslates the body into a full chat
// completion, so a token-count call silently became a real (paid-for)
// generation, then hung until the response-header timeout and 502'd because
// the reply never resembled a count response. This path must always go to
// primary, overflow or not, and never trigger failover itself.
func TestCountTokensNeverRoutesToSecondaryDuringOverflow(t *testing.T) {
	primaryCalled := false
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		primaryCalled = true
		if r.URL.Path != "/v1/messages/count_tokens" {
			t.Fatalf("unexpected path on primary: %s", r.URL.Path)
		}
		w.Header().Set("content-type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"input_tokens":42}`))
	}))
	defer primary.Close()

	secondaryCalled := false
	secondary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		secondaryCalled = true
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer secondary.Close()

	cfg := config.Default()
	cfg.Primary = config.RouteConfig{Provider: "oauth-passthrough", BaseURL: primary.URL, FailoverStrategy: "subscription-limit"}
	cfg.Secondary = config.RouteConfig{Provider: "openai-compatible", BaseURL: secondary.URL, Model: "zai-org/GLM-5.3", KeychainService: "claude-burst-together-test"}
	t.Setenv("TOGETHER_TEST_API_KEY", "test-together-key")
	dir := t.TempDir()
	s, err := New(cfg, filepath.Join(dir, "state.json"), filepath.Join(dir, "metrics.jsonl"), log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	// Simulate an active overflow window, same as a genuine subscription
	// exhaustion or a forced test would leave behind.
	s.ForceOverflow(time.Hour, "test: simulating an active overflow window")

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "http://local/v1/messages/count_tokens",
		strings.NewReader(`{"model":"claude-sonnet-5","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer oauth-token")
	s.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if !primaryCalled {
		t.Fatal("count_tokens should have been sent to primary despite the active overflow window")
	}
	if secondaryCalled {
		t.Fatal("count_tokens must never be sent to the secondary -- it has no equivalent endpoint there")
	}
}

// TestopenAICompatibleIdentityDerivation locks in the "claude-burst-<label>"
// / "<LABEL>_API_KEY" convention buildProvider relies on for every
// openai-compatible secondary -- not just Together AI, which is what this
// generalization replaced a hardcoded name/env-var pair for. Together's
// case is included specifically to prove backward compatibility: an
// existing config that only ever set (or defaulted to)
// "claude-burst-together" must derive the exact identity it had before.
func TestOpenAICompatibleIdentityDerivation(t *testing.T) {
	cases := []struct {
		keychainService string
		wantLabel       string
		wantEnvVar      string
	}{
		{"claude-burst-together", "together", "TOGETHER_API_KEY"},
		{"claude-burst-openrouter", "openrouter", "OPENROUTER_API_KEY"},
		{"claude-burst-my-custom-vendor", "my-custom-vendor", "MY_CUSTOM_VENDOR_API_KEY"},
		{"", "openai-compatible", "OPENAI_COMPATIBLE_API_KEY"},
	}
	for _, c := range cases {
		label, envVar := openAICompatibleIdentity(c.keychainService)
		if label != c.wantLabel || envVar != c.wantEnvVar {
			t.Errorf("openAICompatibleIdentity(%q) = (%q, %q), want (%q, %q)",
				c.keychainService, label, envVar, c.wantLabel, c.wantEnvVar)
		}
	}
}

// TestOpenAICompatibleSecondaryOpenRouter is the OpenRouter analog of
// TestOpenAICompatibleFailoverTranslatesResponse: same failover/translation
// path, but configured the way a real OpenRouter secondary would be (its
// own keychain service, its own env var, no model_map) to prove the
// generalized identity derivation actually drives a real request end to
// end -- not just the identity-string unit test above.
func TestOpenAICompatibleSecondaryOpenRouter(t *testing.T) {
	t.Setenv("OPENROUTER_API_KEY", "test-openrouter-key")

	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("anthropic-ratelimit-unified-status", "rejected")
		w.Header().Set("anthropic-ratelimit-unified-representative-claim", "five_hour")
		w.Header().Set("anthropic-ratelimit-unified-reset", strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10))
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"rate_limit_error","message":"limit"}}`))
	}))
	defer primary.Close()

	openrouterCalled := false
	openrouter := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		openrouterCalled = true
		if got := r.Header.Get("Authorization"); got != "Bearer test-openrouter-key" {
			t.Fatalf("bad auth %q -- env var should have been OPENROUTER_API_KEY, not TOGETHER_API_KEY", got)
		}
		if !strings.HasSuffix(r.URL.Path, "/chat/completions") {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		b, _ := io.ReadAll(r.Body)
		var oaiReq map[string]any
		if err := json.Unmarshal(b, &oaiReq); err != nil {
			t.Fatalf("outbound body not JSON: %v", err)
		}
		if oaiReq["model"] != "z-ai/glm-5.3" {
			t.Fatalf("model not passed through: %v", oaiReq["model"])
		}
		w.Header().Set("content-type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"hi from OpenRouter"},"finish_reason":"stop"}],"usage":{"prompt_tokens":4,"completion_tokens":3}}`))
	}))
	defer openrouter.Close()

	cfg := config.Default()
	cfg.Primary = config.RouteConfig{Provider: "oauth-passthrough", BaseURL: primary.URL, FailoverStrategy: "subscription-limit"}
	cfg.Secondary = config.RouteConfig{Provider: "openai-compatible", BaseURL: openrouter.URL, Model: "z-ai/glm-5.3", KeychainService: "claude-burst-openrouter"}
	dir := t.TempDir()
	s, err := New(cfg, filepath.Join(dir, "state.json"), filepath.Join(dir, "metrics.jsonl"), log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatal(err)
	}

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "http://local/v1/messages", strings.NewReader(`{"model":"claude-sonnet-5","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer oauth-token")
	s.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if !openrouterCalled {
		t.Fatal("OpenRouter was not called")
	}

	var anthResp map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &anthResp); err != nil {
		t.Fatalf("client did not receive valid JSON: %v\nbody=%s", err, rr.Body.String())
	}
	if anthResp["type"] != "message" || anthResp["role"] != "assistant" {
		t.Fatalf("response is not Anthropic-shaped: %v", anthResp)
	}
}

// TestResponseHeaderTimeoutBoundsFullyHungConnection verifies the fix for a
// gap the metered-failure detector would otherwise have: a fully hung
// upstream connection previously never produced a Go error at all (client
// Timeout was 0), so the failure would never be observed or counted. A
// bounded ResponseHeaderTimeout on the transport ensures a hung upstream
// surfaces as a prompt error instead.
func TestResponseHeaderTimeoutBoundsFullyHungConnection(t *testing.T) {
	release := make(chan struct{})
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release // never released during the test; simulates a fully hung upstream
	}))
	defer func() {
		close(release)
		primary.Close()
	}()

	cfg := config.Default()
	cfg.Primary = config.RouteConfig{Provider: "anthropic-api-key", BaseURL: primary.URL, FailoverStrategy: "metered-failures"}
	cfg.BedrockBaseURL = ""
	cfg.ResponseHeaderTimeoutSeconds = 1
	dir := t.TempDir()
	s, err := New(cfg, filepath.Join(dir, "state.json"), filepath.Join(dir, "metrics.jsonl"), log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatal(err)
	}

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "http://local/v1/messages", strings.NewReader(`{"model":"claude-sonnet-5","messages":[]}`))

	done := make(chan struct{})
	go func() {
		s.ServeHTTP(rr, req)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("request never returned; ResponseHeaderTimeout did not bound the hung connection")
	}
	if rr.Code != http.StatusBadGateway {
		t.Fatalf("expected 502 for a timed-out upstream, got %d", rr.Code)
	}
}

// TestSlowTrickleStreamStillSucceeds verifies the ResponseHeaderTimeout fix
// only bounds the wait for the response to START -- an SSE stream whose
// headers arrive immediately but whose body trickles in slowly must still
// be relayed in full, matching the original Timeout:0 behavior for
// legitimately slow (not hung) upstreams.
func TestSlowTrickleStreamStillSucceeds(t *testing.T) {
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl, _ := w.(http.Flusher)
		_, _ = io.WriteString(w, "data: {\"type\":\"ping\"}\n\n")
		if fl != nil {
			fl.Flush()
		}
		time.Sleep(300 * time.Millisecond) // slow body, but headers were sent immediately
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer primary.Close()

	cfg := config.Default()
	cfg.AnthropicBaseURL = primary.URL
	cfg.ResponseHeaderTimeoutSeconds = 1
	dir := t.TempDir()
	s, err := New(cfg, filepath.Join(dir, "state.json"), filepath.Join(dir, "metrics.jsonl"), log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatal(err)
	}

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "http://local/v1/messages", strings.NewReader(`{"model":"claude-sonnet-5","messages":[]}`))
	s.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("a slow-but-responsive stream must still succeed: status=%d body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "[DONE]") {
		t.Fatalf("full trickled body was not relayed: %s", rr.Body.String())
	}
}
