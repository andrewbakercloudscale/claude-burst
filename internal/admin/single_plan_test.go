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

// The Secondary check. A secondary is optional, so every way of not having a
// usable one (none, the keyless Bedrock a config with no secondary block
// resolves to, a chosen secondary whose key is missing on this Mac) passes
// as "Secondary (optional)", with the reason as information. It never
// warns: a warning on a single-plan Mac turned the meter amber for a setup
// that works.
func TestSecondaryCheckTreatsASinglePlanAsHealthy(t *testing.T) {
	type check struct {
		OK       bool   `json:"ok"`
		Critical bool   `json:"critical"`
		Label    string `json:"label"`
		Detail   string `json:"detail"`
	}
	var got map[string]*check
	runPageJS(t, []string{"secondaryCheck"}, `
out({
  missing: secondaryCheck(undefined),
  none: secondaryCheck({provider: "none"}),
  keylessBedrock: secondaryCheck({provider: "bedrock", key_env_var: "AWS_BEARER_TOKEN_BEDROCK", key_present: false}),
  keylessTogether: secondaryCheck({provider: "openai-compatible", model: "GLM", key_env_var: "TOGETHER_API_KEY", key_present: false}),
  ready: secondaryCheck({provider: "openai-compatible", model: "GLM", key_env_var: "TOGETHER_API_KEY", key_present: true}),
});`, &got)

	for _, k := range []string{"missing", "none"} {
		if got[k] != nil {
			t.Errorf("%s: no secondary configured, so no check at all, got %+v", k, got[k])
		}
	}
	for _, k := range []string{"keylessBedrock", "keylessTogether"} {
		c := got[k]
		if c == nil {
			t.Fatalf("%s: a configured secondary keeps its check", k)
		}
		if !c.OK || c.Critical || c.Label != "Secondary (optional)" || !strings.HasPrefix(c.Detail, "not in use") {
			t.Errorf("%s: want a passing, informational Secondary (optional) check, got %+v", k, c)
		}
	}
	if c := got["keylessTogether"]; !strings.Contains(c.Detail, "openai-compatible configured, no API key found, so nothing fails over") ||
		!strings.Contains(c.Detail, "if you want overflow") {
		t.Errorf("a configured but keyless secondary must say why it is not in use: %+v", c)
	}
	if c := got["ready"]; !c.OK || c.Label != "Secondary (optional)" || !strings.Contains(c.Detail, "GLM, key ok") {
		t.Errorf("ready: %+v", c)
	}
}

// Nothing else on the page may call a missing secondary a fault: the rail dot
// is neutral, and the section and its setup test are labelled optional.
func TestSecondaryIsLabelledOptional(t *testing.T) {
	page := string(indexHTML)
	for _, want := range []string{
		`<span class="label">Secondary (optional)</span>`,
		`Secondary provider (optional) <span`,
		`Test secondary (optional)</button>`,
		`setDot("nd-secondary", noSecondary || missingKey ? "" : "ok");`,
		`setHeadState("secondaryHead", none || missingKey ? "" : "ok");`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("admin.html is missing %q", want)
		}
	}
}

// /api/state tells the page there is no secondary to force traffic onto,
// and /api/force refuses, rather than arming a window that sends every
// request nowhere.
func TestSinglePlanStateAndForce(t *testing.T) {
	t.Setenv("AWS_BEARER_TOKEN_BEDROCK", "")
	s := newTestServer(t) // config.Default(): no secondary block, no key
	h := s.Handler()

	req := localRequest(http.MethodGet, "http://x/api/state", nil)
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

	req = localRequest(http.MethodPost, "http://x/api/force", strings.NewReader(`{"minutes":15}`))
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
