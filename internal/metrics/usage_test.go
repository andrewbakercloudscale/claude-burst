package metrics

import (
	"path/filepath"
	"testing"
	"time"
)

// usageFixture writes a small, known log: two repositories, both slots, every
// result, two models, spread over three hours ending at base.
func usageFixture(t *testing.T, base time.Time) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "metrics.jsonl")
	w := New(path)
	ev := []Event{
		{Time: base.Add(-150 * time.Minute), SessionID: "s1", Slot: "primary", Route: "anthropic", Model: "claude-a", HTTPStatus: 200, DurationMS: 1000, InputTokens: 10, OutputTokens: 100, CacheReadTokens: 90, CacheWriteTokens: 5, APIEquivalentUSD: 1},
		{Time: base.Add(-90 * time.Minute), SessionID: "s1", Slot: "primary", Route: "anthropic", Model: "claude-a", HTTPStatus: 200, DurationMS: 3000, InputTokens: 10, OutputTokens: 200, CacheReadTokens: 90, APIEquivalentUSD: 2},
		{Time: base.Add(-50 * time.Minute), SessionID: "s2", Slot: "secondary", Route: "openai-compatible", Model: "glm", HTTPStatus: 502, DurationMS: 50, APIEquivalentUSD: 0},
		{Time: base.Add(-40 * time.Minute), SessionID: "s2", Slot: "secondary", Route: "openai-compatible", Model: "glm", HTTPStatus: 200, DurationMS: 2000, InputTokens: 80, OutputTokens: 100, PricingUnknown: true},
		{Time: base.Add(-20 * time.Minute), SessionID: "s1", Slot: "primary", Route: "anthropic", Model: "claude-a", HTTPStatus: StatusClientClosed, DurationMS: 400},
		{Time: base.Add(-10 * time.Minute), Slot: "primary", Route: "anthropic", Destination: "https://api.anthropic.com/v1/messages", HTTPStatus: 200, DurationMS: 300},
		{Time: base.Add(-5 * time.Minute), SessionID: "s1", Slot: "primary", Route: "anthropic", Model: "claude-a", HTTPStatus: 0, DurationMS: 10},
		// Outside the window on both sides.
		{Time: base.Add(-10 * time.Hour), SessionID: "s1", Slot: "primary", Route: "anthropic", Model: "claude-a", HTTPStatus: 200, DurationMS: 1, OutputTokens: 1},
		{Time: base.Add(time.Hour), SessionID: "s1", Slot: "primary", Route: "anthropic", Model: "claude-a", HTTPStatus: 200, DurationMS: 1, OutputTokens: 1},
	}
	for _, e := range ev {
		if err := w.Write(e); err != nil {
			t.Fatal(err)
		}
	}
	return path
}

func repoOfFixture(sid string) string {
	return map[string]string{"s1": "alpha", "s2": "beta"}[sid]
}

func TestUsageTotals(t *testing.T) {
	base := time.Date(2026, 10, 3, 12, 0, 0, 0, time.Local)
	path := usageFixture(t, base)
	r, err := Usage(path, UsageFilter{From: base.Add(-3 * time.Hour), To: base, Bucket: time.Hour}, repoOfFixture)
	if err != nil {
		t.Fatal(err)
	}
	tt := r.Totals
	// ok: two claude-a 200s, the glm 200 and the model-less 200.
	if tt.Requests != 7 || tt.OK != 4 || tt.Errors != 1 || tt.Cancelled != 1 || tt.Unknown != 1 {
		t.Fatalf("totals = %+v", tt)
	}
	// 499 and status 0 are in neither side: 4 ok of 4+1.
	if tt.SuccessRate == nil || *tt.SuccessRate != 0.8 {
		t.Fatalf("success rate = %v, want 0.8", tt.SuccessRate)
	}
	// cache read 180 / (input 100 + cache read 180).
	if tt.CacheHitRate == nil || *tt.CacheHitRate != 180.0/280.0 {
		t.Fatalf("cache hit rate = %v", tt.CacheHitRate)
	}
	if tt.InputTokens != 100 || tt.OutputTokens != 400 || tt.CacheReadTokens != 180 || tt.CacheWriteTokens != 5 {
		t.Fatalf("tokens = %+v", tt)
	}
	if tt.USD != 3 || tt.Unpriced != 1 {
		t.Fatalf("usd = %v unpriced = %d", tt.USD, tt.Unpriced)
	}
	// 400 output tokens over 6 s of successful requests with output.
	if tt.OutputTokensPerSec < 66.6 || tt.OutputTokensPerSec > 66.7 {
		t.Fatalf("tokens/s = %v", tt.OutputTokensPerSec)
	}
	// Latency over successful model requests only: 1000, 2000, 3000 (the
	// model-less 300 ms post is left out).
	if tt.LatencyP50MS != 2000 || tt.LatencyP95MS != 3000 {
		t.Fatalf("latency p50=%d p95=%d", tt.LatencyP50MS, tt.LatencyP95MS)
	}
	if !r.Covered {
		t.Fatal("window should be covered: the log reaches back further")
	}
}

func TestUsageFilters(t *testing.T) {
	base := time.Date(2026, 10, 3, 12, 0, 0, 0, time.Local)
	path := usageFixture(t, base)
	win := UsageFilter{From: base.Add(-3 * time.Hour), To: base, Bucket: time.Hour}
	cases := []struct {
		name string
		set  func(*UsageFilter)
		want int
	}{
		{"model", func(f *UsageFilter) { f.Model = "glm" }, 2},
		{"slot", func(f *UsageFilter) { f.Provider = "secondary" }, 2},
		{"route", func(f *UsageFilter) { f.Provider = "anthropic" }, 5},
		{"repo", func(f *UsageFilter) { f.Repo = "alpha" }, 4},
		{"session", func(f *UsageFilter) { f.Session = "s2" }, 2},
		{"error", func(f *UsageFilter) { f.Result = ResultError }, 1},
		{"cancelled", func(f *UsageFilter) { f.Result = ResultCancelled }, 1},
		{"combined", func(f *UsageFilter) { f.Repo = "beta"; f.Result = ResultOK }, 1},
	}
	for _, c := range cases {
		f := win
		c.set(&f)
		r, err := Usage(path, f, repoOfFixture)
		if err != nil {
			t.Fatal(err)
		}
		if r.Totals.Requests != c.want {
			t.Errorf("%s: %d requests, want %d", c.name, r.Totals.Requests, c.want)
		}
		// Dropdown options describe the window, not the filtered rows.
		if len(r.Options.Models) != 2 || len(r.Options.Repos) != 2 {
			t.Errorf("%s: options %+v", c.name, r.Options)
		}
	}
}

func TestUsageBuckets(t *testing.T) {
	base := time.Date(2026, 10, 3, 12, 0, 0, 0, time.Local)
	path := usageFixture(t, base)
	r, err := Usage(path, UsageFilter{From: base.Add(-3 * time.Hour), To: base, Bucket: time.Hour}, repoOfFixture)
	if err != nil {
		t.Fatal(err)
	}
	// 09:00, 10:00, 11:00 and 12:00, empty buckets included.
	if len(r.Trend) != 4 {
		t.Fatalf("%d buckets, want 4", len(r.Trend))
	}
	want := []int{1, 1, 5, 0}
	for i, b := range r.Trend {
		if b.Start.Hour() != 9+i || b.Start.Minute() != 0 {
			t.Errorf("bucket %d starts %s", i, b.Start)
		}
		if b.Requests != want[i] {
			t.Errorf("bucket %d: %d requests, want %d", i, b.Requests, want[i])
		}
	}
	if r.Trend[2].Errors != 1 {
		t.Errorf("11:00 bucket errors = %d", r.Trend[2].Errors)
	}

	// Five-minute buckets over the last hour.
	r, err = Usage(path, UsageFilter{From: base.Add(-time.Hour), To: base, Bucket: 5 * time.Minute}, repoOfFixture)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Trend) != 13 || r.Trend[0].Start.Minute() != 0 || r.Trend[1].Start.Minute() != 5 {
		t.Fatalf("5 min buckets: %d, first %s", len(r.Trend), r.Trend[0].Start)
	}

	// Daily buckets start at local midnight.
	r, err = Usage(path, UsageFilter{From: base.AddDate(0, 0, -6), To: base, Daily: true}, repoOfFixture)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Trend) != 7 || r.Trend[6].Start.Hour() != 0 || r.Trend[6].Requests != 8 {
		t.Fatalf("daily: %d buckets, last %+v", len(r.Trend), r.Trend[len(r.Trend)-1])
	}
}

func TestUsageGroupsAndPaging(t *testing.T) {
	base := time.Date(2026, 10, 3, 12, 0, 0, 0, time.Local)
	path := usageFixture(t, base)
	f := UsageFilter{From: base.Add(-3 * time.Hour), To: base, Bucket: time.Hour, Limit: 2}
	r, err := Usage(path, f, repoOfFixture)
	if err != nil {
		t.Fatal(err)
	}
	if r.ByRepo[0].Key != "alpha" || r.ByRepo[0].Requests != 4 {
		t.Fatalf("by repo = %+v", r.ByRepo)
	}
	found := false
	for _, g := range r.ByModel {
		if g.Key == "glm" {
			found = true
			if !g.Unpriced || g.Errors != 1 {
				t.Errorf("glm row = %+v", g)
			}
		}
	}
	if !found {
		t.Fatal("no glm row")
	}
	// Newest first: the status-0 request, then the model-less post.
	if len(r.Recent) != 2 || r.Recent[0].Result != ResultUnknown || r.Recent[1].Model != "" {
		t.Fatalf("page 1 = %+v", r.Recent)
	}
	if r.Recent[0].Repo != "alpha" || r.Recent[0].Provider != "anthropic" {
		t.Fatalf("row detail = %+v", r.Recent[0])
	}
	f.Offset = 2
	r, err = Usage(path, f, repoOfFixture)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Recent) != 2 || r.Recent[0].Result != ResultCancelled {
		t.Fatalf("page 2 = %+v", r.Recent)
	}
	f.Offset = 6
	r, _ = Usage(path, f, repoOfFixture)
	if len(r.Recent) != 1 {
		t.Fatalf("last page = %d rows, want 1", len(r.Recent))
	}
}

// Calls that are not model requests (Remote Control heartbeats, telemetry,
// token counts) stay out of a report unless all traffic is asked for: on
// 3 Oct 2026 they were 46 of the newest 50 rows, every one blank.
func TestUsageModelRequestsOnly(t *testing.T) {
	base := time.Date(2026, 10, 3, 12, 0, 0, 0, time.Local)
	path := filepath.Join(t.TempDir(), "metrics.jsonl")
	w := New(path)
	for _, e := range []Event{
		{Time: base.Add(-5 * time.Minute), Route: "anthropic", Model: "claude-a", Destination: "https://api.anthropic.com/v1/messages", HTTPStatus: 200, OutputTokens: 10},
		{Time: base.Add(-4 * time.Minute), Route: "anthropic", Destination: "https://api.anthropic.com/v1/messages", HTTPStatus: 502},
		{Time: base.Add(-3 * time.Minute), Route: "openai-compatible", Destination: "https://api.together.xyz/v1/chat/completions", HTTPStatus: 200},
		{Time: base.Add(-2 * time.Minute), Route: "anthropic", Destination: "https://api.anthropic.com/v1/code/sessions/cse_1/worker/heartbeat", HTTPStatus: 200},
		{Time: base.Add(-2 * time.Minute), Route: "anthropic", Model: "claude-a", Destination: "https://api.anthropic.com/v1/messages/count_tokens", HTTPStatus: 200},
		{Time: base.Add(-1 * time.Minute), Route: "anthropic", Destination: "https://api.anthropic.com/api/event_logging/batch", HTTPStatus: 200},
	} {
		if err := w.Write(e); err != nil {
			t.Fatal(err)
		}
	}
	f := UsageFilter{From: base.Add(-time.Hour), To: base, Bucket: time.Hour}
	r, err := Usage(path, f, nil)
	if err != nil {
		t.Fatal(err)
	}
	if r.Totals.Requests != 3 || len(r.Recent) != 3 {
		t.Fatalf("model requests: %d totals, %d rows, want 3", r.Totals.Requests, len(r.Recent))
	}
	f.AllTraffic = true
	if r, err = Usage(path, f, nil); err != nil || r.Totals.Requests != 6 {
		t.Fatalf("all traffic: %d, %v, want 6", r.Totals.Requests, err)
	}
}
