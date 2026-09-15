package metrics

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestWriteRotatesOversizedFile is a regression test: metrics.jsonl had no
// rotation at all before this and grew forever for as long as the gateway
// ran. Pre-seeds a file already past the rotation threshold (a single fast
// write) rather than writing thousands of small Events to reach it
// organically -- the rotation mechanics themselves are already covered by
// internal/rotate's own tests; this only needs to prove Writer actually
// wires into them, using the real unexported threshold rather than a
// convenient placeholder.
func TestWriteRotatesOversizedFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "metrics.jsonl")
	if err := os.WriteFile(p, []byte(strings.Repeat("x", maxBytes+1)), 0600); err != nil {
		t.Fatal(err)
	}

	w := New(p)
	if err := w.Write(Event{Route: "anthropic", HTTPStatus: 200}); err != nil {
		t.Fatalf("Write: %v", err)
	}

	if _, err := os.Stat(p + ".1"); err != nil {
		t.Fatalf("expected the oversized file to be rotated to .1: %v", err)
	}
	cur, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(cur), "xxxx") {
		t.Fatal("current file still contains the old oversized content -- rotation did not actually swap files")
	}
	if !strings.Contains(string(cur), `"route":"anthropic"`) {
		t.Fatalf("current file should contain the new event, got: %s", cur)
	}
}

func TestSummarizeCountsBySlot(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "metrics.jsonl")
	w := New(p)
	if err := w.Write(Event{Time: time.Now(), Slot: "primary", Route: "anthropic-api-key", InputTokens: 10}); err != nil {
		t.Fatal(err)
	}
	if err := w.Write(Event{Time: time.Now(), Slot: "secondary", Route: "bedrock", InputTokens: 5}); err != nil {
		t.Fatal(err)
	}
	s, err := Summarize(p, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if s.Requests != 2 || s.PrimaryRequests != 1 || s.SecondaryRequests != 1 {
		t.Fatalf("got %+v", s)
	}
}

// TestSummarizeFallsBackToRouteForLegacyEvents verifies events written
// before the Slot field existed are still counted, keyed off the
// vendor-identifying Route field instead.
func TestSummarizeFallsBackToRouteForLegacyEvents(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "metrics.jsonl")
	w := New(p)
	if err := w.Write(Event{Time: time.Now(), Route: "anthropic"}); err != nil {
		t.Fatal(err)
	}
	if err := w.Write(Event{Time: time.Now(), Route: "bedrock"}); err != nil {
		t.Fatal(err)
	}
	s, err := Summarize(p, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if s.PrimaryRequests != 1 || s.SecondaryRequests != 1 {
		t.Fatalf("legacy Route-based fallback broken: got %+v", s)
	}
}

// TestSummarizeCountsAnthropicAPIKeyRouteUnderSlot guards the specific
// regression this refactor is meant to prevent: once "anthropic-api-key" is
// a valid route value, those requests must not silently vanish from
// `claude-burst stats`.
// TestRecentPreservesDestination guards the "which backend did this actually
// hit" field round-tripping through the JSON encoding Recent() reads back --
// a typo'd struct tag here would silently blank the field the admin UI
// relies on to show a real destination URL instead of just the slot label.
func TestRecentPreservesDestination(t *testing.T) {
	dir := t.TempDir()
	w := New(filepath.Join(dir, "metrics.jsonl"))
	if err := w.Write(Event{Time: time.Now(), Slot: "secondary", Route: "together", Destination: "https://api.together.xyz/v1/chat/completions"}); err != nil {
		t.Fatal(err)
	}
	events, err := Recent(filepath.Join(dir, "metrics.jsonl"), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}
	if got := events[0].Destination; got != "https://api.together.xyz/v1/chat/completions" {
		t.Fatalf("destination = %q", got)
	}
}

func TestSummarizeCountsAnthropicAPIKeyRouteUnderSlot(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "metrics.jsonl")
	w := New(p)
	if err := w.Write(Event{Time: time.Now(), Slot: "primary", Route: "anthropic-api-key"}); err != nil {
		t.Fatal(err)
	}
	s, err := Summarize(p, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if s.Requests != 1 || s.PrimaryRequests != 1 {
		t.Fatalf("anthropic-api-key route requests must still be counted via Slot: got %+v", s)
	}
}

// TestSummarizeFlagsUnpricedAndKeepsTotalHonest is a regression test for a
// silent-$0 bug: because the router indexed the pricing map with a bare
// lookup, a served model with no pricing entry priced at $0/Mtok, so an
// entire overflow window reported api_equivalent_usd=0 and `stats` showed
// no secondary spend at all. The number looked fine, which is what stopped
// anyone looking. Summarize must therefore separate "unpriced" from
// "free", and String() must refuse to present a partial figure as a total.
func TestSummarizeFlagsUnpricedAndKeepsTotalHonest(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "metrics.jsonl")
	w := New(p)
	// One priced request, and two unpriced ones on the same model.
	if err := w.Write(Event{Slot: "primary", Route: "anthropic", Model: "claude-opus-5",
		InputTokens: 1_000_000, OutputTokens: 0, APIEquivalentUSD: 5}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := w.Write(Event{Slot: "secondary", Route: "together", Model: "vendor/some-model",
			InputTokens: 500_000, PricingUnknown: true}); err != nil {
			t.Fatal(err)
		}
	}

	s, err := Summarize(p, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if s.UnpricedRequests != 2 {
		t.Fatalf("UnpricedRequests = %d, want 2", s.UnpricedRequests)
	}
	if got := s.UnpricedModels["vendor/some-model"]; got != 2 {
		t.Fatalf("UnpricedModels[vendor/some-model] = %d, want 2", got)
	}
	// The priced total must still be exactly the priced subset -- the point
	// is to disclose the gap, not to guess a number to fill it with.
	if s.APIEquivalentUSD != 5 {
		t.Fatalf("APIEquivalentUSD = %v, want 5 (priced subset only)", s.APIEquivalentUSD)
	}
	out := s.String()
	if !strings.Contains(out, "INCOMPLETE") {
		t.Fatalf("String() must flag an incomplete total, got: %s", out)
	}
	if !strings.Contains(out, "vendor/some-model") {
		t.Fatalf("String() must name the unpriced model so it can be fixed, got: %s", out)
	}
}

// TestSummarizeCleanRunSaysNothingAboutPricing guards the other direction:
// when every request is priced, the summary must stay exactly as it was --
// a warning that fires on healthy data is a warning people learn to ignore.
func TestSummarizeCleanRunSaysNothingAboutPricing(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "metrics.jsonl")
	w := New(p)
	if err := w.Write(Event{Slot: "primary", Route: "anthropic", Model: "claude-opus-5",
		InputTokens: 1_000_000, APIEquivalentUSD: 5}); err != nil {
		t.Fatal(err)
	}
	s, err := Summarize(p, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if s.UnpricedRequests != 0 {
		t.Fatalf("UnpricedRequests = %d, want 0", s.UnpricedRequests)
	}
	if strings.Contains(s.String(), "INCOMPLETE") {
		t.Fatalf("clean summary must not be flagged, got: %s", s.String())
	}
}

// --- Daily -----------------------------------------------------------------

func writeEvents(t *testing.T, path string, events ...Event) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	for _, e := range events {
		b, err := json.Marshal(e)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.Write(append(b, '\n')); err != nil {
			t.Fatal(err)
		}
	}
}

func dayByDate(t *testing.T, h History, date string) Day {
	t.Helper()
	for _, d := range h.Days {
		if d.Date == date {
			return d
		}
	}
	t.Fatalf("no bucket for %s in %v", date, h.Days)
	return Day{}
}

func TestDailyBucketsByLocalDay(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "metrics.jsonl")
	now := time.Now()
	today := now.Format("2006-01-02")
	yesterday := now.AddDate(0, 0, -1).Format("2006-01-02")

	writeEvents(t, path,
		Event{Time: now, Slot: "primary", Model: "claude-opus-5", HTTPStatus: 200,
			DurationMS: 100, InputTokens: 10, OutputTokens: 20, APIEquivalentUSD: 1, SessionID: "a"},
		Event{Time: now, Slot: "secondary", Model: "glm-4.6", HTTPStatus: 500,
			DurationMS: 5000, InputTokens: 1, OutputTokens: 2, SessionID: "a"},
		Event{Time: now.AddDate(0, 0, -1), Slot: "primary", Model: "claude-opus-5",
			HTTPStatus: 200, DurationMS: 300, APIEquivalentUSD: 2, SessionID: "b"},
	)

	h, err := Daily(path, 7)
	if err != nil {
		t.Fatal(err)
	}
	if len(h.Days) != 7 {
		t.Fatalf("want 7 buckets, got %d", len(h.Days))
	}
	if got := h.Days[len(h.Days)-1].Date; got != today {
		t.Errorf("last bucket is %s, want today %s", got, today)
	}

	d := dayByDate(t, h, today)
	if d.Requests != 2 || d.PrimaryRequests != 1 || d.SecondaryRequests != 1 {
		t.Errorf("today: %+v", d)
	}
	if d.Errors != 1 {
		t.Errorf("today errors = %d, want 1 (the 500)", d.Errors)
	}
	if d.Sessions != 1 {
		t.Errorf("today sessions = %d, want 1 (two events, one session)", d.Sessions)
	}
	if y := dayByDate(t, h, yesterday); y.Requests != 1 || y.APIEquivalentUSD != 2 {
		t.Errorf("yesterday: %+v", y)
	}
	// The per-slot cuts have to agree with the day total, or the chart
	// stacks request counts and then silently stops stacking on tokens.
	if d.PrimaryTokens != 30 || d.SecondaryTokens != 3 {
		t.Errorf("today slot tokens: primary=%d secondary=%d, want 30/3", d.PrimaryTokens, d.SecondaryTokens)
	}
	if d.PrimaryTokens+d.SecondaryTokens != d.InputTokens+d.OutputTokens {
		t.Errorf("slot tokens %d+%d do not sum to the day's %d+%d",
			d.PrimaryTokens, d.SecondaryTokens, d.InputTokens, d.OutputTokens)
	}
	if d.PrimaryUSD != 1 || d.SecondaryUSD != 0 {
		t.Errorf("today slot spend: primary=%v secondary=%v, want 1/0", d.PrimaryUSD, d.SecondaryUSD)
	}

	if h.Sessions != 2 {
		t.Errorf("window sessions = %d, want 2 distinct", h.Sessions)
	}
	if h.Window.Requests != 3 || h.Window.APIEquivalentUSD != 3 {
		t.Errorf("window summary: %+v", h.Window)
	}
	// The 500 is excluded: a failed request's duration is not a measure of
	// how fast the gateway serves.
	if h.LatencyP50MS != 100 || h.LatencyP95MS != 300 {
		t.Errorf("latency p50=%d p95=%d, want the two 2xx durations (100,300) — "+
			"the 5000ms 500 must not be in there", h.LatencyP50MS, h.LatencyP95MS)
	}
	if len(h.Models) != 2 || h.Models[0].Model != "claude-opus-5" {
		t.Errorf("models = %+v, want busiest first", h.Models)
	}
}

// One model served by BOTH slots is two rows, not one. Merged, the row took
// whichever slot it saw first and carried the other slot's spend inside it:
// metered Bedrock spend filed under the subscription, in the one view whose
// job is to say where the money went.
func TestDailySplitsAModelServedByBothSlots(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "metrics.jsonl")
	now := time.Now()
	writeEvents(t, path,
		Event{Time: now, Slot: "primary", Model: "claude-opus-5", HTTPStatus: 200, APIEquivalentUSD: 1},
		Event{Time: now, Slot: "primary", Model: "claude-opus-5", HTTPStatus: 200, APIEquivalentUSD: 1},
		Event{Time: now, Slot: "secondary", Model: "claude-opus-5", HTTPStatus: 200, APIEquivalentUSD: 32},
	)
	h, err := Daily(path, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(h.Models) != 2 {
		t.Fatalf("got %d model rows, want one per slot: %+v", len(h.Models), h.Models)
	}
	bySlot := map[string]ModelUse{}
	for _, m := range h.Models {
		bySlot[m.Slot] = m
	}
	if bySlot["primary"].USD != 2 || bySlot["primary"].Requests != 2 {
		t.Errorf("primary row = %+v, want 2 requests / $2", bySlot["primary"])
	}
	if bySlot["secondary"].USD != 32 || bySlot["secondary"].Requests != 1 {
		t.Errorf("secondary row = %+v, want 1 request / $32", bySlot["secondary"])
	}
}

func TestDailyReportsCoverage(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "metrics.jsonl")
	writeEvents(t, path, Event{Time: time.Now().AddDate(0, 0, -2), Slot: "primary", HTTPStatus: 200})

	// The window reaches back further than any event: the empty early bars
	// are "we have no data", not "nothing happened", and Covered says so.
	h, err := Daily(path, 30)
	if err != nil {
		t.Fatal(err)
	}
	if h.Covered {
		t.Error("Covered = true, but the oldest event is 2 days old and the window is 30")
	}
	if h.Earliest == "" {
		t.Error("Earliest is empty, so the page cannot say where its data begins")
	}

	// A window entirely inside the data is covered.
	h2, err := Daily(path, 3)
	if err != nil {
		t.Fatal(err)
	}
	if !h2.Covered {
		t.Errorf("Covered = false for a 3-day window over 2-day-old data (earliest %s)", h2.Earliest)
	}
}

// Summarize reads only the live file, which is why All-time under-reports
// after the first rotation. Daily reads the backups too; this is that
// difference, pinned.
func TestDailyReadsRotatedBackups(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "metrics.jsonl")
	writeEvents(t, path+".1", Event{Time: time.Now().AddDate(0, 0, -1), Slot: "primary", HTTPStatus: 200})
	writeEvents(t, path, Event{Time: time.Now(), Slot: "primary", HTTPStatus: 200})

	h, err := Daily(path, 7)
	if err != nil {
		t.Fatal(err)
	}
	if h.Files != 2 {
		t.Errorf("read %d files, want 2 (the live one and its backup)", h.Files)
	}
	if h.Window.Requests != 2 {
		t.Errorf("window requests = %d, want 2 — the backup's event was dropped", h.Window.Requests)
	}
}

// A backup last written before the window starts cannot hold an event
// inside it, and opening it anyway is what a 14-day chart pays for in
// parsing 40MB of older JSON.
func TestDailySkipsBackupsOlderThanWindow(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "metrics.jsonl")
	old := path + ".1"
	writeEvents(t, old, Event{Time: time.Now().AddDate(0, 0, -40), Slot: "primary", HTTPStatus: 200})
	stale := time.Now().AddDate(0, 0, -40)
	if err := os.Chtimes(old, stale, stale); err != nil {
		t.Fatal(err)
	}
	writeEvents(t, path, Event{Time: time.Now(), Slot: "primary", HTTPStatus: 200})

	h, err := Daily(path, 7)
	if err != nil {
		t.Fatal(err)
	}
	if h.Files != 1 {
		t.Errorf("read %d files, want 1 — the 40-day-old backup should not be opened", h.Files)
	}
}

func TestDailyOnMissingFile(t *testing.T) {
	h, err := Daily(filepath.Join(t.TempDir(), "nope.jsonl"), 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(h.Days) != 5 || h.Window.Requests != 0 {
		t.Errorf("want 5 empty buckets, got %+v", h)
	}
	if h.Earliest != "" || h.Covered {
		t.Errorf("no data must not claim coverage: %+v", h)
	}
}
