package admin

import (
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/andrewbakercloudscale/claude-burst/internal/config"
	"github.com/andrewbakercloudscale/claude-burst/internal/metrics"
)

func TestPruningToggleSavesAndShowsInState(t *testing.T) {
	s := newTestServer(t)
	writeConfig(t, os.Getenv("HOME"))

	if p := stateOf(t, s).Context.Pruning; p.Disabled || p.NoStub || p.NoCap {
		t.Fatalf("pruning must default to on: %+v", p)
	}
	rr := mutate(t, s, "/api/pruning", `{"no_stub":true,"keep_recent":20,"step":5,"max_tool_result_bytes":65536}`)
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "pruning on") {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	want := config.PruneConfig{NoStub: true, KeepRecent: 20, Step: 5, MaxToolResultBytes: 65536}
	if cfg.SecondaryPruning != want {
		t.Fatalf("config.json has %+v, want %+v", cfg.SecondaryPruning, want)
	}
	if got := stateOf(t, s).Context.Pruning; got != want {
		t.Fatalf("state reports %+v", got)
	}
	if rr := mutate(t, s, "/api/pruning", `{"disabled":true}`); !strings.Contains(rr.Body.String(), "pruning off") {
		t.Fatalf("body=%s", rr.Body.String())
	}
}

func TestPruningRejectsOutOfRangeSettings(t *testing.T) {
	s := newTestServer(t)
	writeConfig(t, os.Getenv("HOME"))
	for _, body := range []string{`{"keep_recent":-1}`, `{"step":1000}`, `{"max_tool_result_bytes":10}`, `not json`} {
		if rr := mutate(t, s, "/api/pruning", body); rr.Code != http.StatusBadRequest {
			t.Fatalf("%s: status=%d, want 400", body, rr.Code)
		}
	}
}

func TestPruneVerdict(t *testing.T) {
	sec := func(n int) metrics.RouteEfficiency { return metrics.RouteEfficiency{Requests: n} }
	cases := []struct {
		name  string
		ci    contextInfo
		level string
		has   string
	}{
		{"no openai secondary", contextInfo{}, "info", "Not applicable"},
		{"off", contextInfo{Applicable: true, Pruning: config.PruneConfig{Disabled: true}}, "info", "Off"},
		{"both techniques off", contextInfo{Applicable: true, Pruning: config.PruneConfig{NoStub: true, NoCap: true}}, "info", "Off"},
		{"no traffic", contextInfo{Applicable: true, WindowDays: 7}, "info", "waiting"},
		{"nothing to prune", contextInfo{Applicable: true, Efficiency: metrics.Efficiency{Secondary: sec(3), UnprunedOK: 3}}, "info", "none of the 3"},
		{"working", contextInfo{Applicable: true, TokensNotSent: 250_000, USDNotSpent: 0.35,
			Efficiency: metrics.Efficiency{Secondary: sec(10), PrunedRequests: 6, PrunedOK: 6, UnprunedOK: 4}}, "ok", "pruned 6 of 10"},
		{"pruned fail more", contextInfo{Applicable: true,
			Efficiency: metrics.Efficiency{Secondary: sec(20), PrunedRequests: 10, PrunedOK: 6, PrunedFailed: 4, UnprunedOK: 10}}, "bad", "40%"},
		{"too few to compare", contextInfo{Applicable: true,
			Efficiency: metrics.Efficiency{Secondary: sec(5), PrunedRequests: 2, PrunedFailed: 2, UnprunedOK: 3}}, "ok", "Working"},
	}
	for _, tc := range cases {
		v := pruneVerdict(tc.ci)
		if v.Level != tc.level || !strings.Contains(v.Text, tc.has) {
			t.Errorf("%s: got %+v, want level %q containing %q", tc.name, v, tc.level, tc.has)
		}
	}
}

func TestCacheVerdict(t *testing.T) {
	if v := cacheVerdict(metrics.Efficiency{Primary: metrics.RouteEfficiency{Requests: 5, InputTokens: 100}}); v.Level != "bad" {
		t.Fatalf("a primary with no cache reads must be flagged: %+v", v)
	}
	if v := cacheVerdict(metrics.Efficiency{Secondary: metrics.RouteEfficiency{Requests: 5, InputTokens: 100}}); v.Level != "info" || !strings.Contains(v.Text, "no cached tokens") {
		t.Fatalf("got %+v", v)
	}
	if v := cacheVerdict(metrics.Efficiency{Primary: metrics.RouteEfficiency{Requests: 5, CacheReadTokens: 100}}); v.Level != "ok" {
		t.Fatalf("got %+v", v)
	}
}
