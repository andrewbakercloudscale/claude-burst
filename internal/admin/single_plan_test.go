package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Single plan (Claude Enterprise, or any one subscription, and no second
// provider) on the dashboard: a working setup, not a fault.

// The Secondary check. No secondary, and the keyless Bedrock that a config
// with no secondary block resolves to, both read as a green "Single plan";
// only a secondary someone chose, whose key is missing, warns.
func TestSecondaryCheckTreatsASinglePlanAsHealthy(t *testing.T) {
	type check struct {
		OK       bool   `json:"ok"`
		Critical bool   `json:"critical"`
		Label    string `json:"label"`
		Detail   string `json:"detail"`
	}
	var got map[string]check
	runPageJS(t, []string{"secondaryCheck"}, `
out({
  missing: secondaryCheck(undefined),
  none: secondaryCheck({provider: "none"}),
  keylessBedrock: secondaryCheck({provider: "bedrock", key_env_var: "AWS_BEARER_TOKEN_BEDROCK", key_present: false}),
  keylessTogether: secondaryCheck({provider: "openai-compatible", model: "GLM", key_env_var: "TOGETHER_API_KEY", key_present: false}),
  ready: secondaryCheck({provider: "openai-compatible", model: "GLM", key_env_var: "TOGETHER_API_KEY", key_present: true}),
});`, &got)

	for _, k := range []string{"missing", "none", "keylessBedrock"} {
		c := got[k]
		if !c.OK || c.Critical || c.Label != "Single plan" || !strings.Contains(c.Detail, "unchanged") {
			t.Errorf("%s: want a green Single plan check, got %+v", k, c)
		}
	}
	if c := got["keylessTogether"]; c.OK || c.Critical || !strings.Contains(c.Detail, "no API key") {
		t.Errorf("a chosen secondary with no key must warn (not critical): %+v", c)
	}
	if c := got["ready"]; !c.OK || c.Label != "Secondary ready" {
		t.Errorf("ready: %+v", c)
	}
}

// /api/state tells the page there is no secondary to force traffic onto,
// and /api/force refuses, rather than arming a window that sends every
// request nowhere.
func TestSinglePlanStateAndForce(t *testing.T) {
	t.Setenv("AWS_BEARER_TOKEN_BEDROCK", "")
	s := newTestServer(t) // config.Default(): no secondary block, no key
	h := s.Handler()

	req := httptest.NewRequest(http.MethodGet, "http://x/api/state", nil)
	req.Host = "127.0.0.1:7788"
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	var st struct {
		SecondaryReady *bool `json:"secondary_ready"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &st); err != nil || st.SecondaryReady == nil {
		t.Fatalf("state has no secondary_ready: %s", rr.Body.String())
	}
	if *st.SecondaryReady {
		t.Fatal("a keyless default secondary must report not ready")
	}

	req = httptest.NewRequest(http.MethodPost, "http://x/api/force", strings.NewReader(`{"minutes":15}`))
	req.Host = "127.0.0.1:7788"
	req.Header.Set("X-Claude-Burst-Admin", "1")
	req.Header.Set("Content-Type", "application/json")
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), "nothing to fail over to") {
		t.Fatalf("force on a single plan: %d %s", rr.Code, rr.Body.String())
	}
}

// The Force button is disabled from the same flag.
func TestForceButtonFollowsSecondaryReady(t *testing.T) {
	if !strings.Contains(string(indexHTML), `$("force").disabled = !s.secondary_ready;`) {
		t.Fatal("the Force button must be disabled when the running gateway has no usable secondary")
	}
}
