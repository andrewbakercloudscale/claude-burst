package admin

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/andrewbakercloudscale/claude-burst/internal/config"
	"github.com/andrewbakercloudscale/claude-burst/internal/router"
)

func mutate(t *testing.T, s *Server, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1"+path, strings.NewReader(body))
	req.Header.Set(mutationHeader, "1")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	return rr
}

func stateOf(t *testing.T, s *Server) stateResponse {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1/api/state", nil)
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("/api/state status=%d body=%s", rr.Code, rr.Body.String())
	}
	var got stateResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	return got
}

func TestDowngradeToggleRoundTrips(t *testing.T) {
	s := newTestServer(t)
	writeConfig(t, os.Getenv("HOME"))

	if !stateOf(t, s).Downgrade.Enabled {
		t.Fatal("the chain should be on by default: a configured fallback_chain is already an opt-in")
	}

	if rr := mutate(t, s, "/api/downgrade", `{"enabled":false}`); rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if stateOf(t, s).Downgrade.Enabled {
		t.Fatal("the toggle did not reach the running gateway")
	}
	if s.gateway.DowngradeEnabled() {
		t.Fatal("gateway and dashboard disagree about the toggle")
	}

	if rr := mutate(t, s, "/api/downgrade", `{"enabled":true}`); rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if !stateOf(t, s).Downgrade.Enabled {
		t.Fatal("the toggle did not come back on")
	}
}

// The toggle on its own is a claim. The page has to show which models are
// actually inside a window and where their traffic is going instead, or
// there is no way to tell the feature is doing anything.
func TestStateReportsRejectedModelsAndWhereTheyGo(t *testing.T) {
	// Seeded through the state file the gateway actually loads, rather than
	// a test-only setter on router.Server: the window this asserts on is
	// then the same shape a real rejection writes.
	s := newTestServerWithState(t, router.State{
		ModelOverflow: map[string]int64{"claude-fable-5-1": time.Now().Add(2 * time.Hour).Unix()},
		LimitClaim:    "seven_day_overage_included",
	})
	writeConfig(t, os.Getenv("HOME"))

	got := stateOf(t, s).Downgrade
	if len(got.Rejected) != 1 {
		t.Fatalf("expected one rejected model, got %+v", got.Rejected)
	}
	r := got.Rejected[0]
	if r.Model != "claude-fable-5-1" {
		t.Fatalf("wrong model reported: %+v", r)
	}
	if r.FallsBackTo != "claude-opus-5" {
		t.Fatalf("the page must name the rung traffic is actually taking, got %q", r.FallsBackTo)
	}
	if got.Chain["claude-fable-5-1"][0] != "claude-opus-5" {
		t.Fatalf("the configured chain is not being surfaced: %+v", got.Chain)
	}

	// With the chain off, the same window sends traffic somewhere else, and
	// the page must say so rather than keep naming a rung nothing takes.
	if rr := mutate(t, s, "/api/downgrade", `{"enabled":false}`); rr.Code != http.StatusOK {
		t.Fatalf("status=%d", rr.Code)
	}
	if fb := stateOf(t, s).Downgrade.Rejected[0].FallsBackTo; fb != "" {
		t.Fatalf("with the chain off no rung is taken, but the page still names %q", fb)
	}
}

func TestDowngradeRejectsGarbageBody(t *testing.T) {
	s := newTestServer(t)
	if rr := mutate(t, s, "/api/downgrade", `not json`); rr.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
}

// newTestServerWithState is newTestServer with a state.json already on disk,
// so a test can start the gateway from a state a real run would have
// produced.
func newTestServerWithState(t *testing.T, st router.State) *Server {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)

	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")
	b, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(statePath, b, 0600); err != nil {
		t.Fatal(err)
	}
	gw, err := router.New(config.Default(), statePath, filepath.Join(dir, "metrics.jsonl"), log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	return New(gw, filepath.Join(dir, "metrics.jsonl"), "test-version", "", "/path/to/transparent-root.sh")
}
