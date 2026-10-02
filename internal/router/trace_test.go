package router

import (
	"context"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/andrewbakercloudscale/claude-burst/internal/config"
)

// The dashboard's test message: a registered token in TraceHeader collects
// the routing decision and every upstream hop, and the header itself never
// reaches a provider.
func TestTraceRecordsRouteAndHopsAndNeverForwardsTheHeader(t *testing.T) {
	var leaked atomic.Int64
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(TraceHeader) != "" {
			leaked.Add(1)
		}
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`{"type":"message","usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer up.Close()
	cfg := config.Default()
	cfg.AnthropicBaseURL = up.URL
	cfg.Intercept.Host = "api.anthropic.com"
	dir := t.TempDir()
	s2, err := New(cfg, filepath.Join(dir, "state.json"), filepath.Join(dir, "metrics.jsonl"), log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatal(err)
	}

	tok := s2.BeginTrace()
	req := claudeCodeShaped("api.anthropic.com")
	req.Header.Set(TraceHeader, tok)
	req.Header.Set("Authorization", "Bearer trace-token")
	rr := httptest.NewRecorder()
	s2.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rr.Code, rr.Body)
	}
	tr, ok := s2.EndTrace(tok)
	if !ok || tr.RequestID == "" || tr.Slot != "primary" || tr.Reason == "" {
		t.Fatalf("trace: ok=%v %+v", ok, tr)
	}
	if len(tr.Hops) != 1 || tr.Hops[0].Slot != "primary" || tr.Hops[0].Status != 200 || tr.Hops[0].Destination == "" {
		t.Fatalf("hops: %+v", tr.Hops)
	}
	if leaked.Load() != 0 {
		t.Fatal("TraceHeader reached the provider")
	}
	if h, _ := s2.ClientCredential(); h != nil {
		t.Fatal("the test message's own credential must not be kept as Claude Code's")
	}
	if _, ok := s2.EndTrace(tok); ok {
		t.Fatal("a trace is collected once")
	}

	// An unregistered token is stripped too, and records nothing.
	req = claudeCodeShaped("api.anthropic.com")
	req.Header.Set(TraceHeader, "not-registered")
	req.Header.Set("Authorization", "Bearer from-claude-code")
	req.Header.Set("anthropic-beta", "claude-code-20250219,oauth-2025-04-20,context-1m-2025-08-07")
	s2.ServeHTTP(httptest.NewRecorder(), req)
	if leaked.Load() != 0 {
		t.Fatal("an unregistered TraceHeader reached the provider")
	}
	h, at := s2.ClientCredential()
	if h.Get("Authorization") != "Bearer from-claude-code" || h.Get("Anthropic-Beta") != "oauth-2025-04-20" || at.IsZero() {
		t.Fatalf("Claude Code's credential headers must be kept for the test message: %v", h)
	}
	if v := h.Values("Anthropic-Beta"); len(v) != 1 {
		t.Fatalf("only the oauth beta is kept, not Claude Code's feature betas: %v", v)
	}
	if h.Get("User-Agent") != "" {
		t.Fatal("only credential headers are kept")
	}
}

// A token whose request never arrived (it died at DNS, pf or TLS) reports
// ok=false, which the dashboard reads as "never reached the gateway".
func TestTraceNotReached(t *testing.T) {
	s, _, _ := guardServer(t, nil)
	tok := s.BeginTrace()
	if _, ok := s.EndTrace(tok); ok {
		t.Fatal("no request carried the token")
	}
}

// Outside transparent mode there is nothing intercepted to resolve.
func TestResolveUpstreamOutsideTransparentMode(t *testing.T) {
	s, _, _ := guardServer(t, nil)
	if _, _, ok, _ := s.ResolveUpstream(context.Background()); ok {
		t.Fatal("base-url mode has no intercept resolver")
	}
	s.resolver = newInterceptResolver("api.anthropic.com", "", "160.79.104.10")
	host, addrs, ok, err := s.ResolveUpstream(context.Background())
	if !ok || err != nil || host != "api.anthropic.com" || len(addrs) != 1 || addrs[0] != "160.79.104.10" {
		t.Fatalf("pinned: %s %v %v %v", host, addrs, ok, err)
	}
}
