package metrics

import (
	"math"
	"path/filepath"
	"testing"
	"time"
)

// The secondary's requests are priced at what the model Claude Code asked
// for would have cost, less what the secondary charged, per local day. A
// request with an unknown price, a failed one and the primary's are left out.
func TestOverflowSavingIsListPriceLessWhatTheSecondaryCharged(t *testing.T) {
	// Opus at $5 in, $25 out, $0.50 cache read per million; anything else unpriced.
	SetPricer(func(model string, in, out, read, write int64) (float64, bool) {
		if model != "claude-opus-5-5" {
			return 0, false
		}
		return (float64(in)*5 + float64(out)*25 + float64(read)*0.5) / 1e6, true
	})
	t.Cleanup(func() { SetPricer(nil) })
	now := time.Now()
	yesterday := now.AddDate(0, 0, -1)
	p := filepath.Join(t.TempDir(), "m.jsonl")
	sec := func(at time.Time, asked string, in, out, read int64, paid float64, status int) Event {
		return Event{Time: at, Slot: "secondary", Route: "together", Model: "glm", RequestedModel: asked, HTTPStatus: status,
			InputTokens: in, OutputTokens: out, CacheReadTokens: read, APIEquivalentUSD: paid}
	}
	writeEvents(t, p,
		sec(yesterday, "claude-opus-5-5", 1_000_000, 100_000, 0, 1.50, 200),     // list 5 + 2.5 = 7.50, saved 6.00
		sec(now, "claude-opus-5-5", 0, 0, 2_000_000, 0.40, 200),                 // list 1.00, saved 0.60
		sec(now, "claude-opus-5-5", 100_000, 0, 0, 0.90, 200),                   // list 0.50: the secondary was dearer, -0.40
		sec(now, "some-model-with-no-price", 1_000_000, 0, 0, 0.10, 200),        // counted, not priced
		sec(now, "claude-opus-5-5", 1_000_000, 0, 0, 0, 502),                    // failed: nothing was served
		sec(now.AddDate(0, 0, -30), "claude-opus-5-5", 1_000_000, 0, 0, 0, 200), // before the window
		Event{Time: now, Slot: "primary", Route: "anthropic", Model: "claude-opus-5-5", RequestedModel: "claude-opus-5-5", HTTPStatus: 200, InputTokens: 1_000_000, APIEquivalentUSD: 5},
	)
	st, err := OverflowSince(p, now.AddDate(0, 0, -6))
	if err != nil {
		t.Fatal(err)
	}
	near := func(a, b float64) bool { return math.Abs(a-b) < 1e-9 }
	if st.Requests != 4 || st.Priced != 3 || !near(st.ListUSD, 9.0) || !near(st.PaidUSD, 2.8) || !near(st.SavedUSD, 6.2) {
		t.Fatalf("totals: %+v", st)
	}
	if len(st.Daily) != 7 {
		t.Fatalf("want every day of the window, got %d", len(st.Daily))
	}
	today, yday := st.Daily[6], st.Daily[5]
	if today.Date != now.Format("2006-01-02") || today.Requests != 3 || !near(today.SavedUSD, 0.2) {
		t.Fatalf("today: %+v", today)
	}
	if yday.Requests != 1 || !near(yday.SavedUSD, 6.0) || !near(yday.ListUSD, 7.5) {
		t.Fatalf("yesterday: %+v", yday)
	}
	var sum float64
	for _, d := range st.Daily {
		sum += d.SavedUSD
	}
	if !near(sum, st.SavedUSD) {
		t.Fatalf("the days sum to %v, the total is %v", sum, st.SavedUSD)
	}
}
