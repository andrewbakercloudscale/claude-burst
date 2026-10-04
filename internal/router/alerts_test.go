package router

import (
	"errors"
	"net/http"
	"path/filepath"
	"strings"
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

// One outage, one pair of alerts: the 5xx replies a dead network causes
// are not a second problem, before or after "Network back".
func TestNetworkOutageDoesNotAlsoAlertUpstream(t *testing.T) {
	events := captureNotices(t)
	s, _ := newChainServer(t, "http://127.0.0.1:1", nil)
	for i := 0; i < upstreamFailures-1; i++ {
		s.alertOutcome("primary", http.StatusBadGateway) // the first failures, before DNS is checked
	}
	s.alertNetworkDown()
	for i := 0; i < 2*upstreamFailures; i++ {
		s.alertOutcome("primary", http.StatusBadGateway)
	}
	s.notePrimaryAnswered("primary")
	s.alertOutcome("primary", http.StatusBadGateway) // one more after: under the threshold again
	got := titles(events())
	if len(got) != 2 || got[0] != "error: Network offline" || got[1] != "ok: Network back" {
		t.Fatalf("events = %q", got)
	}
}

// A failover the previous process announced and never ended is ended by
// the next primary success after a restart, not left standing for a day.
func TestFailoverEndedAfterRestart(t *testing.T) {
	got := captureNotices(t)
	notice.Publish(alertFailover, notice.Warn, "Failed over to together", "until 10:20")
	notice.Flush(2 * time.Second)

	s, _ := newTestServer(t, "", "") // the restarted gateway
	s.ResumeAlerts()
	s.alertOutcome("primary", http.StatusOK)

	evs := got()
	if last := evs[len(evs)-1]; last.Kind != alertFailover || last.Severity != notice.OK {
		t.Fatalf("want Back on Claude last, got %q", titles(evs))
	}
}

// Claude Code's side calls (auto mode's classifier, a recap) carry a whole
// conversation in one prompt, with the session's id, and are never
// continued: no alert, no summary. On 2026-10-04 one at 410k said
// "compaction soon" four times and never compacted.
func TestNoContextAlertForAOneShotRequest(t *testing.T) {
	events := captureNotices(t)
	f := &fakeAnthropic{context: 450_000}
	s := compactServer(t, f, config.CompactionConfig{Enabled: true, CompactAtTokens: 300_000, WarnAtPercent: 80, WindowMinutes: 60})
	one := msgs(t, `[{"role":"user","content":"`+strings.Repeat("transcript ", 2000)+`"}]`)
	send(t, s, "S", one)
	send(t, s, "S", one)
	s.compaction.running.Wait()
	notice.Flush(2 * time.Second)
	if got := titles(events()); len(got) != 0 || f.summaryCount() != 0 {
		t.Fatalf("one-shot: alerts %q, summaries %d", got, f.summaryCount())
	}
}

// Two prompts, but the current turn is nearly all of it: no boundary
// leaves 30% to summarise, so the alert says it cannot summarise yet
// instead of promising a compaction.
func TestContextAlertSaysWhenItCannotSummarise(t *testing.T) {
	events := captureNotices(t)
	f := &fakeAnthropic{context: 450_000}
	s := compactServer(t, f, config.CompactionConfig{Enabled: true, CompactAtTokens: 300_000, WarnAtPercent: 80, WindowMinutes: 60})
	h := msgs(t, `[
 {"role":"user","content":"hi"},
 {"role":"assistant","content":[{"type":"text","text":"hello"}]},
 {"role":"user","content":"big task"},
 {"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"Read","input":{}}]},
 {"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"`+strings.Repeat("x", 20000)+`"}]}]`)
	send(t, s, "S", h[:3])
	send(t, s, "S", h)
	s.compaction.running.Wait()
	notice.Flush(2 * time.Second)
	got := titles(events())
	if len(got) != 1 || got[0] != "warn: Context at 450k of 300k, cannot summarise yet" || f.summaryCount() != 0 {
		t.Fatalf("alerts %q, summaries %d", got, f.summaryCount())
	}
}
