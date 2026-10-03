package router

import (
	"errors"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/andrewbakercloudscale/claude-burst/internal/config"
	"github.com/andrewbakercloudscale/claude-burst/internal/metrics"
	"github.com/andrewbakercloudscale/claude-burst/internal/notice"
)

// captureNotices installs a publisher writing a temp notices.json and
// returns a function reading what it holds.
func captureNotices(t *testing.T) func() []notice.Event {
	t.Helper()
	path := filepath.Join(t.TempDir(), "notices.json")
	notice.SetDefault(notice.New(path, nil))
	t.Cleanup(func() { notice.SetDefault(nil) })
	return func() []notice.Event {
		notice.Flush(2 * time.Second)
		evs, err := notice.Read(path)
		if err != nil {
			t.Fatal(err)
		}
		return evs
	}
}

func titles(evs []notice.Event) []string {
	var out []string
	for _, e := range evs {
		out = append(out, e.Severity+": "+e.Title)
	}
	return out
}

func TestAlertFailoverAndBack(t *testing.T) {
	events := captureNotices(t)
	s, _ := newChainServerWithSecondary(t, "http://127.0.0.1:1", nil)

	s.activateOverflow("claude-opus-5-5", time.Now().Add(time.Hour).Unix(), "five_hour", "limit hit")
	// A primary answer while the window is still open is not "back".
	s.alertOutcome("primary", http.StatusOK)
	s.ClearOverflow()
	s.alertOutcome("primary", http.StatusOK)

	got := titles(events())
	want := []string{"warn: Failed over to " + s.secondary.Name(), "ok: Back on Claude"}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("events = %q, want %q", got, want)
	}
}

func TestAlertLimitWithNoSecondary(t *testing.T) {
	events := captureNotices(t)
	s, _ := newChainServer(t, "http://127.0.0.1:1", nil)
	s.activateOverflow("claude-opus-5-5", time.Now().Add(time.Hour).Unix(), "five_hour", "limit hit")
	got := titles(events())
	if len(got) != 1 || got[0] != "warn: Claude limit reached" {
		t.Fatalf("events = %q", got)
	}
}

func TestAlertUpstreamFailuresTripAndClear(t *testing.T) {
	events := captureNotices(t)
	s, _ := newChainServer(t, "http://127.0.0.1:1", nil)

	for i := 0; i < upstreamFailures-1; i++ {
		s.alertOutcome("primary", http.StatusBadGateway)
	}
	// Client cancellations are not upstream failures.
	s.alertOutcome("primary", metrics.StatusClientClosed)
	if n := len(events()); n != 0 {
		t.Fatalf("%d events before the threshold", n)
	}
	s.alertOutcome("secondary", http.StatusInternalServerError)
	// A success straight after is not yet a recovery.
	s.alertOutcome("primary", http.StatusOK)
	s.alerts.mu.Lock()
	s.alerts.lastFailure = time.Now().Add(-upstreamWindow)
	s.alerts.mu.Unlock()
	s.alertOutcome("primary", http.StatusOK)

	got := titles(events())
	if len(got) != 2 || got[0] != "error: Requests are failing" || got[1] != "ok: Requests are succeeding again" {
		t.Fatalf("events = %q", got)
	}
}

func TestAlertSecondaryKeyOnlyWhenAWorkingKeyStops(t *testing.T) {
	events := captureNotices(t)
	s, _ := newChainServerWithSecondary(t, "http://127.0.0.1:1", nil)
	s.alerts = alertState{}                      // forget the setup's own check
	s.alertSecondaryKey(errors.New("never set")) // a keyless secondary: not news
	s.alertSecondaryKey(nil)
	s.alertSecondaryKey(errors.New("timed out: Keychain locked"))
	s.alertSecondaryKey(nil)
	got := titles(events())
	if len(got) != 2 || got[0] != "warn: Secondary key unavailable" || got[1] != "ok: Secondary key available" {
		t.Fatalf("events = %q", got)
	}
}

func TestAlertContextNearCompaction(t *testing.T) {
	events := captureNotices(t)
	f := &fakeAnthropic{context: 340_000}
	s := compactServer(t, f, config.CompactionConfig{Enabled: true, CompactAtTokens: 400_000, WarnAtPercent: 80, WindowMinutes: 60})
	all := msgs(t, session)
	send(t, s, "S", all[:5]) // learns the context: 340k
	send(t, s, "S", all[:7]) // over 320k: warned
	send(t, s, "S", all[:9]) // same window: not again
	s.compaction.running.Wait()

	notice.Flush(2 * time.Second)
	evs, _ := notice.Read(notice.Default().Path())
	var got []notice.Event
	for _, e := range evs {
		if e.Kind == alertContext {
			got = append(got, e)
		}
	}
	if len(got) != 1 || got[0].Title != "Context at 340k of 400k, compaction soon" || got[0].Session != "S" {
		t.Fatalf("context alerts = %+v (all: %q)", got, titles(events()))
	}
}

func TestAlertNetworkDownAndUp(t *testing.T) {
	events := captureNotices(t)
	s, _ := newChainServer(t, "http://127.0.0.1:1", nil)
	s.notePrimaryAnswered("primary") // up while never down says nothing
	s.alertNetworkDown()
	s.alertNetworkDown()
	s.notePrimaryAnswered("primary")
	got := titles(events())
	if len(got) != 2 || got[0] != "error: Network offline" || got[1] != "ok: Network back" {
		t.Fatalf("events = %q", got)
	}
}
