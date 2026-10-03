package main

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/andrewbakercloudscale/claude-burst/internal/notice"
)

func TestRestartIsAnnouncedAndResolved(t *testing.T) {
	path := filepath.Join(t.TempDir(), "notices.json")
	notice.SetDefault(notice.New(path, nil))
	t.Cleanup(func() { notice.SetDefault(nil) })

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
