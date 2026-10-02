package admin

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/andrewbakercloudscale/claude-burst/internal/tlswatch"
)

// The 2026-10-02 outage: a deploy rotated the CA, every running Claude Code
// session refused the new certificate, and the page said 6/6. These pin the
// check that sees it.

type pageCheck struct {
	Key      string `json:"key"`
	OK       bool   `json:"ok"`
	Critical bool   `json:"critical"`
	Label    string `json:"label"`
	Detail   string `json:"detail"`
}

func TestClientTLSCheck(t *testing.T) {
	var got map[string]*pageCheck
	runPageJS(t, []string{"clientTLSCheck"}, `
out({
  baseurl: clientTLSCheck(undefined),
  healthy: clientTLSCheck({window_seconds: 300, successes: 12, failures: 3, threshold: 30, rejecting: false,
    by_class: {distrust: 3}}),
  outage: clientTLSCheck({window_seconds: 300, successes: 0, failures: 85, threshold: 30, rejecting: true,
    by_class: {hangup: 85}, last_error: "EOF"}),
});`, &got)

	if got["baseurl"] != nil {
		t.Errorf("base-url mode has no TLS to refuse, want no check, got %+v", got["baseurl"])
	}
	if c := got["healthy"]; c == nil || !c.OK || !c.Critical || c.Label != "Claude Code accepts the gateway's certificate" ||
		!strings.Contains(c.Detail, "12 handshakes completed") || !strings.Contains(c.Detail, "3 failed") {
		t.Errorf("background failures beside working handshakes must pass and say so: %+v", c)
	}
	c := got["outage"]
	if c == nil || c.OK || !c.Critical {
		t.Fatalf("the outage must be a failing critical check: %+v", c)
	}
	for _, want := range []string{"85 TLS handshakes failed", "against 0 that completed", "85 hung up mid-handshake", "local CA changed", "Last error: EOF"} {
		if !strings.Contains(c.Detail, want) {
			t.Errorf("outage detail missing %q: %s", want, c.Detail)
		}
	}
}

// The check sits in the list the meter is built from, so a rejection turns
// the ring red even while the gateway's own probe passes.
func TestReadinessChecksIncludeClientTLS(t *testing.T) {
	state := map[string]any{
		"intercept":      map[string]any{"mode": "transparent", "self_heal": map[string]any{"running": true}},
		"secondary":      map[string]any{"provider": "none"},
		"primary_health": nil,
		"client_tls":     map[string]any{"window_seconds": 300, "successes": 0, "failures": 75, "threshold": 30, "rejecting": true, "by_class": map[string]int{"hangup": 75}},
	}
	js, _ := json.Marshal(state)
	var got struct {
		Checks []pageCheck `json:"checks"`
		State  string      `json:"state"`
	}
	runPageJS(t, []string{"primaryHealthCheck", "secondaryCheck", "clientTLSCheck", "readinessChecks", "checksState"}, fmt.Sprintf(`
const lastHistory = null;
const checks = readinessChecks(%s, {ok: true});
out({checks, state: checksState(checks)});`, js), &got)

	var found *pageCheck
	for i := range got.Checks {
		if got.Checks[i].Key == "clienttls" {
			found = &got.Checks[i]
		}
	}
	if found == nil || found.OK {
		t.Fatalf("want a failing clienttls check beside a passing path probe, got %+v", got.Checks)
	}
	if got.State != "bad" {
		t.Fatalf("a refused certificate must turn the meter red, got %q", got.State)
	}
	if !strings.Contains(string(indexHTML), "  clienttls: \"restart the Claude Code sessions") ||
		!strings.Contains(string(indexHTML), "NODE_EXTRA_CA_CERTS points at the bundle") {
		t.Fatal("the check needs its fix text: restart the sessions, or check NODE_EXTRA_CA_CERTS")
	}
}

// /api/state carries the counts when the listener speaks TLS, and nothing in
// base-url mode.
func TestStateReportsClientTLS(t *testing.T) {
	s := newTestServer(t)
	get := func() map[string]json.RawMessage {
		req := httptest.NewRequest(http.MethodGet, "http://x/api/state", nil)
		req.Host = "127.0.0.1:7788"
		rr := httptest.NewRecorder()
		s.Handler().ServeHTTP(rr, req)
		var m map[string]json.RawMessage
		if err := json.Unmarshal(rr.Body.Bytes(), &m); err != nil {
			t.Fatal(err)
		}
		return m
	}
	if _, ok := get()["client_tls"]; ok {
		t.Fatal("no TLS listener, no client_tls")
	}

	w := tlswatch.New(io.Discard)
	for i := 0; i < 40; i++ {
		fmt.Fprintf(w, "2026/10/02 12:50:07 http: TLS handshake error from 127.0.0.1:%d: EOF\n", 64000+i)
	}
	s.SetHandshakes(w)
	var ct tlswatch.Snapshot
	if err := json.Unmarshal(get()["client_tls"], &ct); err != nil {
		t.Fatal(err)
	}
	if !ct.Rejecting || ct.Failures != 40 || ct.ByClass["hangup"] != 40 || ct.Threshold != tlswatch.RejectThreshold {
		t.Fatalf("client_tls: %+v", ct)
	}
}

// The meter counts only checks that apply to this setup: base-url mode has
// no pf redirect to guard and no TLS for Claude Code to refuse, and a single
// plan has no secondary. Each of those is absent, not a free pass.
func TestReadinessChecksFollowTheMode(t *testing.T) {
	keys := func(state map[string]any) []string {
		js, _ := json.Marshal(state)
		var got struct {
			Checks []pageCheck `json:"checks"`
		}
		runPageJS(t, []string{"primaryHealthCheck", "secondaryCheck", "clientTLSCheck", "readinessChecks", "checksState"}, fmt.Sprintf(`
const lastHistory = null;
out({checks: readinessChecks(%s, {ok: true})});`, js), &got)
		var k []string
		for _, c := range got.Checks {
			k = append(k, c.Key)
		}
		return k
	}
	baseURL := keys(map[string]any{
		"intercept":      map[string]any{"mode": "base-url", "self_heal": map[string]any{"running": true}},
		"secondary":      map[string]any{"provider": "none"},
		"primary_health": map[string]any{"failures": 0},
	})
	if got, want := strings.Join(baseURL, ","), "path,watchdog,primary"; got != want {
		t.Errorf("base-url, single plan: checks %s, want %s", got, want)
	}
	transparent := keys(map[string]any{
		"intercept":      map[string]any{"mode": "transparent", "self_heal": map[string]any{"running": true}, "pf_heal": map[string]any{"running": true}},
		"secondary":      map[string]any{"provider": "openai-compatible", "model": "GLM", "key_env_var": "TOGETHER_API_KEY", "key_present": true},
		"primary_health": map[string]any{"failures": 0},
		"client_tls":     map[string]any{"window_seconds": 300, "successes": 5, "threshold": 30},
	})
	if got, want := strings.Join(transparent, ","), "path,clienttls,watchdog,pfguard,secondary,primary"; got != want {
		t.Errorf("transparent with a secondary: checks %s, want %s", got, want)
	}
}
