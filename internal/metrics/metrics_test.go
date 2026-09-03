package metrics

import (
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
