package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/andrewbakercloudscale/claude-burst/internal/metrics"
)

func TestCodexInsightsBucketTheLogByDayAndModel(t *testing.T) {
	path := filepath.Join(t.TempDir(), "codex-metrics.jsonl")
	now := time.Date(2026, 10, 6, 15, 0, 0, 0, time.Local)
	at := func(daysAgo, hour int) time.Time {
		return time.Date(2026, 10, 6-daysAgo, hour, 30, 0, 0, time.Local)
	}
	w := metrics.New(path)
	for _, e := range []metrics.Event{
		// Older than a 3-day window: read, never counted.
		{Time: at(5, 9), SessionID: "old", Model: "gpt-6.1-sol", HTTPStatus: 200, DurationMS: 99000, InputTokens: 1, OutputTokens: 1},
		{Time: at(2, 9), SessionID: "a", Model: "gpt-6.1-sol", HTTPStatus: 200, DurationMS: 4000, InputTokens: 1000, CacheReadTokens: 9000, OutputTokens: 100},
		{Time: at(2, 9), SessionID: "a", Model: "gpt-6.1-sol", HTTPStatus: 200, DurationMS: 6000, InputTokens: 2000, CacheReadTokens: 18000, OutputTokens: 200},
		{Time: at(0, 9), SessionID: "b", Model: "gpt-6.1-mini", HTTPStatus: 200, DurationMS: 1000, InputTokens: 500, OutputTokens: 50},
		// A refused turn is an error and no latency; Codex hanging up is neither.
		{Time: at(0, 11), SessionID: "b", Model: "gpt-6.1-mini", HTTPStatus: 429, DurationMS: 5},
		{Time: at(0, 11), SessionID: "b", Model: "gpt-6.1-mini", HTTPStatus: metrics.StatusClientClosed, DurationMS: 7},
	} {
		if err := w.Write(e); err != nil {
			t.Fatal(err)
		}
	}

	in, err := readCodexInsights(path, 3, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(in.Daily) != 3 || in.Daily[0].Date != "2026-10-04" || in.Daily[2].Date != "2026-10-06" {
		t.Fatalf("days: %+v", in.Daily)
	}
	if d := in.Daily[0]; d.Turns != 2 || d.Sessions != 1 || d.Input != 3000 || d.Cached != 27000 || d.Output != 300 || d.Errors != 0 {
		t.Errorf("4 Oct: %+v", d)
	}
	if d := in.Daily[1]; d.Turns != 0 {
		t.Errorf("5 Oct had no turns: %+v", d)
	}
	if d := in.Daily[2]; d.Turns != 3 || d.Errors != 1 || d.Sessions != 1 {
		t.Errorf("6 Oct: %+v", d)
	}
	if in.Turns != 5 || in.Errors != 1 || in.Sessions != 2 || in.Input != 3500 || in.Cached != 27000 || in.Output != 350 {
		t.Errorf("window: %+v", in)
	}
	// Answered turns only: 1000, 4000, 6000.
	if in.LatencyP50MS != 4000 || in.LatencyP95MS != 6000 {
		t.Errorf("latency p50 %d p95 %d, want 4000 and 6000", in.LatencyP50MS, in.LatencyP95MS)
	}
	if in.LargestContext != 20200 || in.LargestContextModel != "gpt-6.1-sol" {
		t.Errorf("largest context %d on %s", in.LargestContext, in.LargestContextModel)
	}
	if in.BusiestHour != 9 || in.BusiestHourTurns != 3 {
		t.Errorf("busiest hour %d with %d turns", in.BusiestHour, in.BusiestHourTurns)
	}
	if len(in.Models) != 2 || in.Models[0].Model != "gpt-6.1-sol" || in.Models[0].Turns != 2 || in.Models[0].LatencyP50MS != 4000 ||
		in.Models[1].Model != "gpt-6.1-mini" || in.Models[1].Turns != 3 || in.Models[1].Errors != 1 || in.Models[1].LatencyP50MS != 1000 {
		t.Errorf("models, most tokens first: %+v", in.Models)
	}
	// The log goes back before the window, so no day in it is "no data".
	if !in.Covered || in.Files != 1 || !strings.HasPrefix(in.Earliest, "2026-10-01") {
		t.Errorf("coverage: covered %v, files %d, earliest %s", in.Covered, in.Files, in.Earliest)
	}

	// A window longer than the log says so.
	long, err := readCodexInsights(path, 14, now)
	if err != nil {
		t.Fatal(err)
	}
	if long.Covered || long.Turns != 6 {
		t.Errorf("14 days: covered %v, turns %d", long.Covered, long.Turns)
	}
}

func TestCodexInsightsWithNoLog(t *testing.T) {
	in, err := readCodexInsights(filepath.Join(t.TempDir(), "none.jsonl"), 7, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(in.Daily) != 7 || in.Turns != 0 || in.BusiestHour != -1 || in.Earliest != "" || in.Covered {
		t.Errorf("empty: %+v", in)
	}
	b, _ := json.Marshal(in)
	if !strings.Contains(string(b), `"models":[]`) {
		t.Errorf("the page maps over models: %s", b)
	}
}

func TestCodexInsightsEndpoint(t *testing.T) {
	s := newTestServer(t)
	for _, q := range []string{"", "?days=30", "?days=0", "?days=abc", "?days=9999"} {
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, localRequest(http.MethodGet, "http://127.0.0.1/api/codex/insights"+q, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", q, rec.Code, rec.Body)
		}
		var in codexInsights
		if err := json.Unmarshal(rec.Body.Bytes(), &in); err != nil {
			t.Fatal(err)
		}
		want := 14
		if q == "?days=30" {
			want = 30
		}
		if in.Days != want || len(in.Daily) != want {
			t.Errorf("%s: %d days, %d bars, want %d", q, in.Days, len(in.Daily), want)
		}
	}
}

// The tiles, run under node.
func TestCodexInsightTiles(t *testing.T) {
	type tile struct {
		K, V, H, Cls string
	}
	var got map[string][]tile
	runPageJS(t, []string{"codexInsightTiles", "num"}, `
out({
  some: codexInsightTiles({turns: 200, errors: 6, sessions: 8, input_tokens: 3e6, cached_tokens: 27e6, output_tokens: 4e5,
    latency_p50_ms: 6890, latency_p95_ms: 23815, largest_context: 137000, largest_context_model: "gpt-6.1-sol",
    busiest_hour: 9, busiest_hour_turns: 41, daily: [{date: "2026-10-05", turns: 150}, {date: "2026-10-06", turns: 50}]}),
  uncached: codexInsightTiles({turns: 40, errors: 0, sessions: 1, input_tokens: 9e6, cached_tokens: 1e6, output_tokens: 0, busiest_hour: 3, daily: []}),
  none: codexInsightTiles({turns: 0, errors: 0, sessions: 0, busiest_hour: -1, daily: [{date: "2026-10-06", turns: 0}]}),
});`, &got)
	by := func(name string) map[string]tile {
		m := map[string]tile{}
		for _, x := range got[name] {
			m[x.K] = x
		}
		return m
	}
	s := by("some")
	for k, want := range map[string]string{
		"Median latency": "6.9 s", "p95 latency": "24 s", "Error rate": "3.0%", "Cached input": "90%",
		"Tokens per turn": "152k", "Turns per session": "25", "Largest context": "137k",
		"Busiest day": "2026-10-05", "Busiest hour": "09:00",
	} {
		if s[k].V != want {
			t.Errorf("%s = %q (%s), want %q", k, s[k].V, s[k].H, want)
		}
	}
	if s["Error rate"].Cls != "warn" || !strings.Contains(s["Error rate"].H, "6 of 200 turns") {
		t.Errorf("error rate: %+v", s["Error rate"])
	}
	if s["Cached input"].Cls != "" || !strings.Contains(s["Cached input"].H, "27.0M of 30.0M") {
		t.Errorf("cached input: %+v", s["Cached input"])
	}
	if u := by("uncached")["Cached input"]; u.V != "10%" || u.Cls != "warn" {
		t.Errorf("a session sending nine tenths uncached is a warning: %+v", u)
	}
	for k, x := range by("none") {
		if x.V != "-" || strings.Contains(x.H, "NaN") || x.Cls != "" {
			t.Errorf("no turns, %s: %+v", k, x)
		}
	}
}

func TestCodexInsightsAreOnTheCodexTab(t *testing.T) {
	page := string(indexHTML)
	for _, want := range []string{
		`data-target="sec-codex-insights"`, `<section id="sec-codex-insights"`,
		`"sec-codex", "sec-codex-insights",`, `id="cxInsChart"`, `id="cxInsTiles"`, `id="cxInsModels"`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("the page lacks %s", want)
		}
	}
}
