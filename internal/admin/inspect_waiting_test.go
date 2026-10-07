package admin

import (
	"testing"
	"time"

	"github.com/andrewbakercloudscale/claude-burst/internal/metrics"
	"github.com/andrewbakercloudscale/claude-burst/internal/router"
)

// After a gateway restart the inspector has nothing of a session until its
// next request, so the sessions at work in the last hours are listed as
// waiting: every repository is there, not only the first to send something.
func TestSessionsWithNothingSinceTheRestartAreListedAsWaiting(t *testing.T) {
	s := newTestServer(t)
	now := time.Now()
	w := metrics.New(s.metricsPath)
	for _, e := range []metrics.Event{
		{Time: now.Add(-40 * time.Minute), SessionID: "quiet", Model: "claude-opus-5-5", CacheReadTokens: 90000},
		{Time: now.Add(-30 * time.Minute), SessionID: "quiet", Model: "claude-haiku-4-5", InputTokens: 300},
		{Time: now.Add(-10 * time.Minute), SessionID: "listed", Model: "claude-opus-5-5", CacheReadTokens: 50000},
		{Time: now.Add(-20 * time.Minute), SessionID: "other", Model: "claude-sonnet-5-5", InputTokens: 4000},
		{Time: now.Add(-9 * time.Hour), SessionID: "gone", Model: "claude-opus-5-5", InputTokens: 4000},
		{Time: now.Add(-5 * time.Minute), Model: "claude-opus-5-5", InputTokens: 10},
	} {
		if err := w.Write(e); err != nil {
			t.Fatal(err)
		}
	}
	got := s.waitingSessions([]router.InspectSession{{Session: "listed"}}, now)
	if len(got) != 2 || got[0].Session != "other" || got[1].Session != "quiet" {
		t.Fatalf("waiting sessions = %+v, want other then quiet", got)
	}
	if !got[1].Waiting || got[1].Model != "claude-opus-5-5" {
		t.Errorf("quiet = %+v, want waiting on the model of its conversation, not its helper's", got[1])
	}
	if got[1].At.Sub(now.Add(-30*time.Minute)).Abs() > time.Second {
		t.Errorf("quiet last seen %v, want its latest request", got[1].At)
	}
}
