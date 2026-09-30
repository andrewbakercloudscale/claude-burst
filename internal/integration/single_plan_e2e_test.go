package integration_test

// Single plan, end to end: a real gateway and admin server behind real
// listeners, with only Anthropic behind them. Someone with Claude
// Enterprise alone must get working requests, Anthropic's own limit
// responses, and a dashboard that says so, through the same wiring
// production uses.

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/andrewbakercloudscale/claude-burst/internal/config"
)

func singlePlanHarness(t *testing.T, secondary config.RouteConfig) *harness {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("AWS_BEARER_TOKEN_BEDROCK", "")
	primary := newUpstream(t, "primary")
	cfg := config.Default()
	cfg.KeychainService = "claude-burst-single-plan-e2e-no-such-item"
	cfg.Primary = config.RouteConfig{Provider: "oauth-passthrough", BaseURL: primary.srv.URL, FailoverStrategy: "subscription-limit"}
	cfg.Secondary = secondary
	cfg.ResolveRoutes()
	if err := config.Save(cfg); err != nil {
		t.Fatal(err)
	}
	return newHarnessFromConfig(t, cfg, primary, nil)
}

func TestSinglePlanEndToEnd(t *testing.T) {
	for name, sec := range map[string]config.RouteConfig{
		"secondary none":     {Provider: config.ProviderNone},
		"no secondary block": {},
	} {
		t.Run(name, func(t *testing.T) {
			h := singlePlanHarness(t, sec)

			if status, by := h.claudeCodeRequest(); status != http.StatusOK || by != "primary" {
				t.Fatalf("ordinary request: %d from %q", status, by)
			}

			// A real subscription limit: Anthropic's 429, from Anthropic.
			h.primary.rejected.Store(true)
			h.primary.status.Store(http.StatusTooManyRequests)
			if status, by := h.claudeCodeRequest(); status != http.StatusTooManyRequests || by != "primary" {
				t.Fatalf("limited request: %d from %q, want Anthropic's own 429", status, by)
			}

			// The limit lifts: the very next request is served.
			h.primary.rejected.Store(false)
			h.primary.status.Store(http.StatusOK)
			if status, by := h.claudeCodeRequest(); status != http.StatusOK || by != "primary" {
				t.Fatalf("after the limit lifted: %d from %q", status, by)
			}

			// Force is refused, and the dashboard reports no secondary.
			if status, _ := h.adminPost(t, "/api/force", map[string]int{"minutes": 15}); status != http.StatusBadRequest {
				t.Fatalf("force on a single plan: %d, want 400", status)
			}
			resp, err := http.Get(h.adminSrv.URL + "/api/state")
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			var st struct {
				SecondaryReady bool `json:"secondary_ready"`
			}
			if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
				t.Fatal(err)
			}
			if st.SecondaryReady {
				t.Fatal("state says a secondary is ready on a single plan")
			}
			if n := h.primary.requests.Load(); n != 3 {
				t.Fatalf("Anthropic saw %d requests, want all 3", n)
			}
		})
	}
}
