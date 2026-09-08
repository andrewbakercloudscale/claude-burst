package router

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/andrewbakercloudscale/claude-burst/internal/config"
)

// probeServer builds a gateway whose secondary points at upstream.
func probeServer(t *testing.T, upstreamURL string) *Server {
	t.Helper()
	t.Setenv("PROBE_API_KEY", "test-key")
	dir := t.TempDir()
	cfg := config.Default()
	cfg.Secondary = config.RouteConfig{
		Provider: "openai-compatible", BaseURL: upstreamURL,
		Model: "zai-org/GLM-5.3", KeychainService: "claude-burst-probe",
	}
	s, err := New(cfg, filepath.Join(dir, "state.json"), filepath.Join(dir, "metrics.jsonl"), log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// TestProbeSecondaryHappyPath is the button's whole promise: a real request
// through the real Prepare/Translate path, with the reply text pulled back
// out. It also pins that the probe asks for a CLAUDE model id -- asking the
// upstream for its own id would skip the model translation, which is the
// step most likely to be misconfigured and so the main thing worth testing.
func TestProbeSecondaryHappyPath(t *testing.T) {
	var gotBody map[string]any
	var gotAuth string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"x","choices":[{"message":{"role":"assistant","content":"hello world"},
			"finish_reason":"stop"}],"usage":{"prompt_tokens":11,"completion_tokens":2}}`)
	}))
	defer up.Close()

	res, err := probeServer(t, up.URL).ProbeSecondary(context.Background())
	if err != nil {
		t.Fatalf("probe failed: %v (detail=%s)", err, res.Detail)
	}
	if res.Reply != "hello world" {
		t.Errorf("reply = %q, want %q", res.Reply, "hello world")
	}
	if res.Status != 200 {
		t.Errorf("status = %d", res.Status)
	}
	if res.ServeModel != "zai-org/GLM-5.3" {
		t.Errorf("serve_model = %q, want the upstream model", res.ServeModel)
	}
	if res.RequestedModel != probeRequestedModel {
		t.Errorf("requested_model = %q, want %q", res.RequestedModel, probeRequestedModel)
	}
	if res.InputTokens != 11 || res.OutputTokens != 2 {
		t.Errorf("tokens = %d/%d, want 11/2", res.InputTokens, res.OutputTokens)
	}
	// The upstream must have been asked for ITS model, not Claude's -- that
	// translation is the point of going through Prepare.
	if gotBody["model"] != "zai-org/GLM-5.3" {
		t.Errorf("upstream received model=%v, want the translated model", gotBody["model"])
	}
	if gotAuth != "Bearer test-key" {
		t.Errorf("upstream received Authorization=%q, want the configured key", gotAuth)
	}
	// A capped probe: an uncapped one would let a chatty provider bill for a
	// full-length response on every press of a test button.
	if mt, ok := gotBody["max_tokens"].(float64); !ok || int(mt) != probeMaxTokens {
		t.Errorf("max_tokens = %v, want %d", gotBody["max_tokens"], probeMaxTokens)
	}
}

// TestProbeSecondaryUpstreamError: the upstream's own message is the answer
// when a key is wrong, so it has to survive into the result rather than
// being flattened to "HTTP 401".
func TestProbeSecondaryUpstreamError(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":{"message":"Invalid API key provided"}}`)
	}))
	defer up.Close()

	res, err := probeServer(t, up.URL).ProbeSecondary(context.Background())
	if err == nil {
		t.Fatal("probe reported success against a 401")
	}
	if res.Status != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", res.Status)
	}
	if !strings.Contains(res.Detail, "Invalid API key provided") {
		t.Errorf("detail = %q, want the upstream's own message", res.Detail)
	}
}

// TestProbeSecondaryEmptyReplyIsNotSuccess: a 200 carrying no text is the
// shape that would render as a green tick over an empty box, which reads as
// a pass. It must be an error.
func TestProbeSecondaryEmptyReplyIsNotSuccess(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"x","choices":[{"message":{"role":"assistant","content":""},
			"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":0}}`)
	}))
	defer up.Close()

	if _, err := probeServer(t, up.URL).ProbeSecondary(context.Background()); err == nil {
		t.Fatal("a 200 with no text was reported as success")
	}
}

// TestProbeSecondaryNoneConfigured must name the restart, because the
// running gateway builds its providers at startup: a secondary added in the
// UI a moment ago is genuinely absent here, and "not configured" alone sends
// someone back to a config file that already says otherwise.
func TestProbeSecondaryNoneConfigured(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Default()
	cfg.Secondary = config.RouteConfig{Provider: config.ProviderNone}
	cfg.BedrockBaseURL = ""
	s, err := New(cfg, filepath.Join(dir, "state.json"), filepath.Join(dir, "metrics.jsonl"), log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.ProbeSecondary(context.Background())
	if err == nil {
		t.Fatal("probe succeeded with no secondary configured")
	}
	if !strings.Contains(err.Error(), "restart") {
		t.Errorf("error = %q, want it to mention restarting the gateway", err)
	}
}

// TestProbeSecondaryDoesNotTouchFailoverState: a probe answers a question
// about the secondary. If it could arm an overflow window, pressing a test
// button would silently move real traffic onto a paid provider.
func TestProbeSecondaryDoesNotTouchFailoverState(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests) // the shape that triggers failover on a real response
		_, _ = io.WriteString(w, `{"error":{"message":"rate limited"}}`)
	}))
	defer up.Close()

	s := probeServer(t, up.URL)
	before := s.Status()
	if _, err := s.ProbeSecondary(context.Background()); err == nil {
		t.Fatal("probe reported success against a 429")
	}
	after := s.Status()
	if after.OverflowUntil != before.OverflowUntil {
		t.Fatalf("the probe armed an overflow window (%d -> %d)", before.OverflowUntil, after.OverflowUntil)
	}
}
