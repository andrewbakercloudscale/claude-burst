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
