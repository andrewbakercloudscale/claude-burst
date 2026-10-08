package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/andrewbakercloudscale/claude-burst/internal/notice"
)

func TestRestartIsAnnouncedAndResolved(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "planned-restart")
	old := plannedRestartPath
	plannedRestartPath = func() string { return marker }
	t.Cleanup(func() { plannedRestartPath = old })
	path := filepath.Join(t.TempDir(), "notices.json")
	notice.SetDefault(notice.New(path, nil))
	t.Cleanup(func() { notice.Default().Flush(2 * time.Second); notice.SetDefault(nil) })

	announceDrain(2)
	announceReady("9.9.9")
	notice.Flush(2 * time.Second)

	evs, err := notice.Read(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 2 {
		t.Fatalf("got %d events, want 2: %+v", len(evs), evs)
	}
	d, r := evs[0], evs[1]
	if d.Severity != notice.Info || d.Title != "Gateway restarting" || d.Detail != "2 replies in flight finish first." {
		t.Errorf("drain event = %+v", d)
	}
	if r.Severity != notice.OK || r.Title != "Gateway ready" || r.Resolves != "gateway" || d.Kind != "gateway" {
		t.Errorf("ready event = %+v, want an ok that resolves the restart", r)
	}
}

func TestPlannedRestartIsQuietOnce(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "planned-restart")
	old := plannedRestartPath
	plannedRestartPath = func() string { return marker }
	t.Cleanup(func() { plannedRestartPath = old })
	path := filepath.Join(t.TempDir(), "notices.json")
	notice.SetDefault(notice.New(path, nil))
	t.Cleanup(func() { notice.Default().Flush(2 * time.Second); notice.SetDefault(nil) })

	if err := os.WriteFile(marker, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	announceDrain(0)
	announceReady("9.9.9") // quiet, and uses the marker up
	// The watchdog checks every 30 seconds: it finds this instead of the
	// marker, and does not count the restart towards a crash loop.
	if _, err := os.Stat(marker + ".done"); err != nil {
		t.Errorf("a planned restart that finished left no .done for the watchdog: %v", err)
	}
	announceDrain(0) // the next restart is news again
	notice.Flush(2 * time.Second)
	evs, _ := notice.Read(path)
	if len(evs) != 1 || evs[0].Title != "Gateway restarting" {
		t.Fatalf("events = %+v, want only the unplanned restart", evs)
	}

	// A marker older than a deploy can take is ignored and removed.
	os.WriteFile(marker, nil, 0o644)
	past := time.Now().Add(-10 * time.Minute)
	os.Chtimes(marker, past, past)
	if plannedRestart(false) {
		t.Fatal("a stale marker counted")
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("a stale marker was left behind")
	}
}
