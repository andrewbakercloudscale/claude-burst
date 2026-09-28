package metrics

import (
	"path/filepath"
	"testing"
	"time"
)

func TestEfficiencySplitsRoutesCacheAndPruning(t *testing.T) {
	path := filepath.Join(t.TempDir(), "m.jsonl")
	w := New(path)
	now := time.Now()
	for _, e := range []Event{
		{Time: now, Slot: "primary", Model: "claude-opus-5", HTTPStatus: 200, InputTokens: 10, CacheReadTokens: 900, CacheWriteTokens: 90},
		{Time: now, Slot: "primary", Route: "anthropic", HTTPStatus: 200}, // heartbeat: no model, not inference
		{Time: now, Slot: "secondary", Model: "glm", HTTPStatus: 200, InputTokens: 1000, PrunedBytes: 4000, PrunedToolResults: 3, APIEquivalentUSD: 0.5},
		{Time: now, Slot: "secondary", Model: "glm", HTTPStatus: 500, PrunedBytes: 100, TruncatedToolResults: 1},
		{Time: now, Slot: "secondary", Model: "glm", HTTPStatus: 200, InputTokens: 500},
		{Time: now, Slot: "secondary", Model: "glm", HTTPStatus: 0},
		{Time: now, Slot: "secondary", Model: "glm", Note: "client cancelled: context canceled"},
		{Time: now.Add(-48 * time.Hour), Slot: "secondary", Model: "glm", HTTPStatus: 200, PrunedBytes: 1},
	} {
		if err := w.Write(e); err != nil {
			t.Fatal(err)
		}
	}
	eff, err := EfficiencySince(path, now.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if eff.Primary.Requests != 1 || eff.Primary.CacheHitRate() != 0.9 {
		t.Fatalf("primary: %+v hit=%v", eff.Primary, eff.Primary.CacheHitRate())
	}
	if eff.Secondary.Requests != 4 || eff.Secondary.USD != 0.5 {
		t.Fatalf("secondary: %+v (cancellations and old rows must be excluded)", eff.Secondary)
	}
	if eff.PrunedRequests != 2 || eff.PrunedOK != 1 || eff.PrunedFailed != 1 || eff.UnprunedOK != 1 || eff.UnprunedFailed != 1 {
		t.Fatalf("outcome split: %+v", eff)
	}
	if eff.PrunedBytes != 4100 || eff.StubbedResults != 3 || eff.TruncatedResults != 1 || eff.LastPrunedAt.IsZero() {
		t.Fatalf("pruning totals: %+v", eff)
	}
}

func TestCacheHitRateWithNoTraffic(t *testing.T) {
	if (RouteEfficiency{}).CacheHitRate() != 0 {
		t.Fatal("no traffic must be a 0 hit rate, not NaN")
	}
}
