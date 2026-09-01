// Package integration_test drives the gateway (internal/router) and the
// admin control panel (internal/admin) together, over real TCP listeners,
// the same way Claude Code and a browser clicking the dashboard actually do.
//
// It exists because internal/router and internal/admin are each well tested
// in isolation, but nothing exercised the seam between them: a browser click
// on "Force -> secondary" goes through admin's HTTP handler, which validates
// against config.Load() (disk) and then calls router.Server.ForceOverflow,
// whose effect is only visible on the NEXT request through router.Server's
// own HTTP entrypoint. A bug in that seam -- the two ends disagreeing about
// what's configured -- cannot show up in either package's own unit tests.
package integration_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/andrewbakercloudscale/claude-burst/internal/admin"
	"github.com/andrewbakercloudscale/claude-burst/internal/config"
	"github.com/andrewbakercloudscale/claude-burst/internal/router"
)

// upstream is a fake Anthropic-shaped backend. Every response identifies
// itself via the X-Test-Upstream header (name) so a test can tell which
// backend actually served a given client request without depending on
// response-body parsing. status/body are read atomically so a test can flip
// a running server from "healthy" to "returning a subscription-limit 429"
// mid-test, the way a real outage or quota exhaustion would.
type upstream struct {
	name   string
	srv    *httptest.Server
	status atomic.Int32
	// rejected, when true, makes every response carry the unified
	// rate-limit-rejected headers subscriptionLimitDetector looks for,
	// regardless of status.
	rejected atomic.Bool
	requests atomic.Int32
}

func newUpstream(t *testing.T, name string) *upstream {
	t.Helper()
	u := &upstream{name: name}
	u.status.Store(http.StatusOK)
	u.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.requests.Add(1)
		w.Header().Set("X-Test-Upstream", u.name)
		if u.rejected.Load() {
			w.Header().Set("anthropic-ratelimit-unified-status", "rejected")
			w.Header().Set("anthropic-ratelimit-unified-representative-claim", "five_hour")
			w.Header().Set("anthropic-ratelimit-unified-5h-reset", fmt.Sprintf("%d", time.Now().Add(time.Hour).Unix()))
		}
		status := int(u.status.Load())
		w.WriteHeader(status)
		if status >= 400 {
			_, _ = w.Write([]byte(`{"type":"error","error":{"type":"rate_limit_error","message":"synthetic upstream failure"}}`))
			return
		}
		resp := map[string]any{
			"id": "msg_test", "type": "message", "role": "assistant",
			"model":   "claude-sonnet-5",
			"content": []map[string]any{{"type": "text", "text": "hello from " + u.name}},
			"usage":   map[string]any{"input_tokens": 1, "output_tokens": 1},
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(u.srv.Close)
	return u
}

// harness wires a real router.Server and admin.Server together, each behind
// its own real net/http listener, plus two fake upstreams -- reproducing
// the actual production topology (Claude Code -> gateway :7777, browser ->
// admin :7788, gateway -> Anthropic/secondary) instead of only calling Go
// functions directly.
type harness struct {
	t         *testing.T
	primary   *upstream
	secondary *upstream
	gateway   *router.Server
	gwSrv     *httptest.Server
	adminSrv  *httptest.Server
}

// newHarness builds a harness whose on-disk config.json (what admin's
// handlers read) matches the config the gateway was actually constructed
// with -- the non-drifted, everything-restarted-cleanly case. See
// TestForceFailover_ConfigDriftLeavesGatewayWithNoLiveSecondary below for the
// deliberately-mismatched case.
func newHarness(t *testing.T) *harness {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)

	primary := newUpstream(t, "primary")
	secondary := newUpstream(t, "secondary")

	cfg := config.Default()
	cfg.Primary = config.RouteConfig{Provider: "oauth-passthrough", BaseURL: primary.srv.URL, FailoverStrategy: "subscription-limit"}
	cfg.Secondary = config.RouteConfig{Provider: "anthropic-api-key", BaseURL: secondary.srv.URL}
	cfg.ResolveRoutes()

	if err := config.Save(cfg); err != nil {
		t.Fatalf("writing config.json: %v", err)
	}

	return newHarnessFromConfig(t, cfg, primary, secondary)
}

func newHarnessFromConfig(t *testing.T, cfg config.Config, primary, secondary *upstream) *harness {
	t.Helper()
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")
	metricsPath := filepath.Join(dir, "metrics.jsonl")

	gw, err := router.New(cfg, statePath, metricsPath, log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatalf("router.New: %v", err)
	}

	gwSrv := httptest.NewServer(http.HandlerFunc(gw.ServeHTTP))
	t.Cleanup(gwSrv.Close)

	a := admin.New(gw, metricsPath, "test", "", "/path/to/transparent-root.sh")
	adminSrv := httptest.NewServer(a.Handler())
	t.Cleanup(adminSrv.Close)

	return &harness{t: t, primary: primary, secondary: secondary, gateway: gw, gwSrv: gwSrv, adminSrv: adminSrv}
}

// claudeCodeRequest simulates one inference call the way Claude Code makes
// it: a POST to /v1/messages carrying the headers Claude Code always sends.
// It returns the HTTP status and the X-Test-Upstream header identifying
// which fake backend actually served it -- "" if neither did (a proxy-level
// error).
func (h *harness) claudeCodeRequest() (status int, servedBy string) {
	h.t.Helper()
	body := []byte(`{"model":"claude-sonnet-5","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)
	req, err := http.NewRequest(http.MethodPost, h.gwSrv.URL+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		h.t.Fatalf("building request: %v", err)
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("authorization", "Bearer test-oauth-token")
	req.Header.Set("x-claude-code-session-id", "test-session")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatalf("gateway request: %v", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode, resp.Header.Get("X-Test-Upstream")
}

// adminPost simulates a browser click: POST to one of the admin mutation
// endpoints ("/api/force", "/api/reset", ...) with the same header the
// dashboard's own JS sends (see admin.html's `H` constant) -- without it,
// admin.go's mutating() guard rejects the request the way it would reject a
// cross-origin page that can't complete the preflight.
func (h *harness) adminPost(t *testing.T, path string, body any) (status int, respBody map[string]any) {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal admin request body: %v", err)
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(http.MethodPost, h.adminSrv.URL+path, rd)
	if err != nil {
		t.Fatalf("building admin request: %v", err)
	}
	req.Header.Set("X-Claude-Burst-Admin", "1")
	req.Header.Set("content-type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("admin request %s: %v", path, err)
	}
	defer resp.Body.Close()
	var v map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&v)
	return resp.StatusCode, v
}

// TestForceFailoverThenFailBack is the scenario in the bug report, driven
// end to end exactly the way the dashboard buttons drive it: click "Force ->
// secondary", send inference traffic (repeatedly, standing in for the user
// hitting retry), click "Clear overflow", and confirm traffic goes back to
// the primary. Every step goes through the real HTTP handlers, not direct
// Go calls, so a wiring bug between admin and router shows up here even
// though each package's own unit tests are green.
func TestForceFailoverThenFailBack(t *testing.T) {
	h := newHarness(t)

	if status, servedBy := h.claudeCodeRequest(); status != http.StatusOK || servedBy != "primary" {
		t.Fatalf("before any failover: status=%d servedBy=%q, want 200/primary", status, servedBy)
	}

	if status, body := h.adminPost(t, "/api/force", map[string]int{"minutes": 15}); status != http.StatusOK {
		t.Fatalf("POST /api/force: status=%d body=%v", status, body)
	}

	// "even when I retry" from the bug report: several requests in a row,
	// not just one, must all reach the secondary and succeed.
	for i := 0; i < 3; i++ {
		status, servedBy := h.claudeCodeRequest()
		if status != http.StatusOK {
			t.Fatalf("retry %d after forcing secondary: status=%d (want 200)", i, status)
		}
		if servedBy != "secondary" {
			t.Fatalf("retry %d after forcing secondary: served by %q, want secondary", i, servedBy)
		}
	}

	if status, body := h.adminPost(t, "/api/reset", nil); status != http.StatusOK {
		t.Fatalf("POST /api/reset: status=%d body=%v", status, body)
	}

	if status, servedBy := h.claudeCodeRequest(); status != http.StatusOK || servedBy != "primary" {
		t.Fatalf("after clearing overflow: status=%d servedBy=%q, want 200/primary", status, servedBy)
	}
}

// TestForceFailoverRejectedWithoutSecondary confirms the admin button fails
// loudly (400, clear message) rather than silently when there is nothing to
// fail over to -- the one case admin's own unit tests already cover in
// isolation; kept here as the end-to-end baseline the drift test below is
// contrasted against.
func TestForceFailoverRejectedWithoutSecondary(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	primary := newUpstream(t, "primary")

	cfg := config.Default()
	cfg.Primary = config.RouteConfig{Provider: "oauth-passthrough", BaseURL: primary.srv.URL, FailoverStrategy: "subscription-limit"}
	cfg.Secondary = config.RouteConfig{Provider: config.ProviderNone}
	cfg.ResolveRoutes()
	if err := config.Save(cfg); err != nil {
		t.Fatalf("writing config.json: %v", err)
	}

	h := newHarnessFromConfig(t, cfg, primary, nil)
	status, body := h.adminPost(t, "/api/force", map[string]int{"minutes": 15})
	if status != http.StatusBadRequest {
		t.Fatalf("status=%d body=%v, want 400", status, body)
	}
}

// TestForceFailover_ConfigDriftLeavesGatewayWithNoLiveSecondary is the
// regression test for the actual root cause behind the bug report: a real
// build's "Force -> secondary" button doing nothing, and staying broken
// across retries.
//
// The gap: handleForce (admin.go) used to validate against config.Load() --
// read fresh from disk on every call -- while the real routing decision
// uses router.Server.secondary, a Provider built ONCE from whatever config
// was passed to router.New() at process start (see handleConfig's own
// response: "the gateway reads config at startup -- restart it for this to
// take effect"). So a secondary added to config.json (by hand, or by the
// dashboard's "Save" button) without restarting the gateway process used to
// pass handleForce's validation -- disk said a secondary existed -- and
// ForceOverflow happily armed the overflow window, even though the running
// gateway's own s.secondary was still nil. The next inference request then
// routed into a nil Provider, panicking into a bare 500 that persisted
// across every retry, since ForceOverflow's state.json keeps the overflow
// window armed regardless of how many times the client tries again.
//
// Fix: handleForce now checks router.Server.HasSecondary() (the live
// wiring) instead of a freshly re-read config file, and router.go's handle()
// additionally refuses to route into a nil secondary at all -- defense in
// depth for the same state surviving a restart into a config that removed
// the secondary. This test pins both: the button now REJECTS instead of
// falsely succeeding, the secondary upstream is never touched, and ordinary
// traffic keeps working normally throughout.
func TestForceFailover_ConfigDriftLeavesGatewayWithNoLiveSecondary(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	primary := newUpstream(t, "primary")
	secondary := newUpstream(t, "secondary")

	// The gateway process starts with NO secondary configured.
	startupCfg := config.Default()
	startupCfg.Primary = config.RouteConfig{Provider: "oauth-passthrough", BaseURL: primary.srv.URL, FailoverStrategy: "subscription-limit"}
	startupCfg.Secondary = config.RouteConfig{Provider: config.ProviderNone}
	startupCfg.ResolveRoutes()

	h := newHarnessFromConfig(t, startupCfg, primary, secondary)

	// Now simulate "added a secondary via the dashboard's Save button (or by
	// hand), but never restarted claude-burst" -- config.json on disk gets a
	// secondary; the already-running gateway process does not.
	diskCfg := startupCfg
	diskCfg.Secondary = config.RouteConfig{Provider: "anthropic-api-key", BaseURL: secondary.srv.URL}
	if err := config.Save(diskCfg); err != nil {
		t.Fatalf("writing drifted config.json: %v", err)
	}

	status, body := h.adminPost(t, "/api/force", map[string]int{"minutes": 15})
	if status != http.StatusBadRequest {
		t.Fatalf("POST /api/force with drifted config: status=%d body=%v, want 400 (must validate against the live gateway, not disk config)", status, body)
	}

	// Ordinary traffic must be completely unaffected: no overflow window was
	// armed, so this still goes straight to the primary.
	if status, servedBy := h.claudeCodeRequest(); status != http.StatusOK || servedBy != "primary" {
		t.Fatalf("after a rejected force: status=%d servedBy=%q, want 200/primary", status, servedBy)
	}
	if secondary.requests.Load() != 0 {
		t.Fatalf("secondary upstream received %d requests it should never have gotten", secondary.requests.Load())
	}
}

// TestOverflowActiveWithNilSecondary_RouterRejectsCleanly is the router-level
// half of the same fix: even if an overflow window ends up armed with no
// live secondary Provider -- e.g. state.json survives a restart into a
// config that removed the secondary, not just the admin-drift path above --
// the gateway must return a clear error instead of panicking a nil Provider
// into a bare, undiagnosable 500.
func TestOverflowActiveWithNilSecondary_RouterRejectsCleanly(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	primary := newUpstream(t, "primary")

	cfg := config.Default()
	cfg.Primary = config.RouteConfig{Provider: "oauth-passthrough", BaseURL: primary.srv.URL, FailoverStrategy: "subscription-limit"}
	cfg.Secondary = config.RouteConfig{Provider: config.ProviderNone}
	cfg.ResolveRoutes()
	if err := config.Save(cfg); err != nil {
		t.Fatalf("writing config.json: %v", err)
	}

	h := newHarnessFromConfig(t, cfg, primary, nil)
	h.gateway.ForceOverflow(0, "test: simulating stale overflow state with no live secondary")

	status, servedBy := h.claudeCodeRequest()
	if status != http.StatusBadGateway {
		t.Fatalf("inference request during overflow with a nil secondary: status=%d servedBy=%q, want 502", status, servedBy)
	}
}

// TestGenuineSubscriptionLimitTriggersAutomaticFailover exercises the other
// path into overflow -- Anthropic itself reporting plan exhaustion, not a
// forced admin action -- end to end, confirming the failed request is
// transparently replayed to the secondary within the SAME client call (no
// visible error, no manual retry needed), and that the overflow window then
// keeps routing subsequent requests straight to the secondary.
func TestGenuineSubscriptionLimitTriggersAutomaticFailover(t *testing.T) {
	h := newHarness(t)
	h.primary.rejected.Store(true)
	h.primary.status.Store(http.StatusTooManyRequests)

	status, servedBy := h.claudeCodeRequest()
	if status != http.StatusOK || servedBy != "secondary" {
		t.Fatalf("first request during genuine exhaustion: status=%d servedBy=%q, want 200/secondary (transparent replay)", status, servedBy)
	}

	// Primary "recovers", but the overflow window from the genuine signal
	// should still be honored without needing another 429 to re-arm it.
	h.primary.rejected.Store(false)
	h.primary.status.Store(http.StatusOK)
	if status, servedBy := h.claudeCodeRequest(); status != http.StatusOK || servedBy != "secondary" {
		t.Fatalf("second request within the overflow window: status=%d servedBy=%q, want 200/secondary", status, servedBy)
	}
}
