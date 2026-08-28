package metrics

import (
	"path/filepath"
	"testing"
	"time"
)

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
