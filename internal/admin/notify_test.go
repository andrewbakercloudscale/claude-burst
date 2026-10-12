package admin

import (
	"bytes"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/andrewbakercloudscale/claude-burst/internal/config"
	"github.com/andrewbakercloudscale/claude-burst/internal/router"
)

// isolateGuardLogs points both guard logs at temp files, so the real
// /var/log/claude-burst-pf.log never feeds a test.
func isolateGuardLogs(t *testing.T) (pf string) {
	t.Helper()
	pf = filepath.Join(t.TempDir(), "pf.log")
	old := pfHealLog
	pfHealLog = pf
	t.Cleanup(func() { pfHealLog = old })
	return pf
}

func appendLine(t *testing.T, path, line string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(line + "\n"); err != nil {
		t.Fatal(err)
	}
}

// Starting the gateway must not announce what is already true, and a round
// with nothing new announces nothing.
func TestNotifyFirstRoundIsBaselineOnly(t *testing.T) {
	s := newTestServer(t)
	alerts := captureAlerts(t)
	pf := isolateGuardLogs(t)
	appendLine(t, pf, "2026-09-30 10:00:00 HEALED: rule reloaded")

	n := &notifier{}
	s.notifyRound(n, time.Now())
	s.notifyRound(n, time.Now())
	if got := alerts(); len(got) != 0 {
		t.Fatalf("announced: %v", got)
	}
}

// The watchdog's lines become alerts too.
func TestWatchdogLinesBecomeOnScreenAlerts(t *testing.T) {
	s := newTestServer(t)
	alerts := captureAlerts(t)
	isolateGuardLogs(t)
	_, _, selfLog := selfHealPaths()
	if err := os.MkdirAll(filepath.Dir(selfLog), 0o700); err != nil {
		t.Fatal(err)
	}
	n := &notifier{}
	s.notifyRound(n, time.Now())
	appendLine(t, selfLog, "2026-09-30 10:02:00 gateway is not running (LaunchAgent unloaded or its process gone) -- attempting reload")
	s.notifyRound(n, time.Now())
	if got := alerts(); len(got) != 1 {
		t.Fatalf("alerts = %q", got)
	}
}

// Helpers outside the gateway (the handover writer) put up alerts through
// the dashboard; a bad severity or a missing title is refused.
func TestAlertPublishEndpoint(t *testing.T) {
	s := newTestServer(t)
	alerts := captureAlerts(t)
	if rr := mutate(t, s, "/api/alert", `{"kind":"handover","severity":"ok","title":"Claude handover","detail":"Handover updated in x"}`); rr.Code != 200 {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	for _, bad := range []string{`{"severity":"loud","title":"x"}`, `{"severity":"ok"}`, `not json`} {
		if rr := mutate(t, s, "/api/alert", bad); rr.Code != 400 {
			t.Errorf("%s: status=%d", bad, rr.Code)
		}
	}
	if got := alerts(); len(got) != 1 || got[0] != "ok: Claude handover" {
		t.Fatalf("alerts = %q", got)
	}
}

// A config.json that stops loading does not end the alert rounds or the
// learner: each goes on, reads it again next round, and says once that it
// is paused, the alert rounds on screen as well. Before, both skipped
// every round in silence.
func TestAConfigThatWillNotLoadIsSaidOnceAndTriedAgain(t *testing.T) {
	s := newTestServer(t)
	var logged bytes.Buffer
	dir := t.TempDir()
	gw, err := router.New(config.Default(), filepath.Join(dir, "state.json"), filepath.Join(dir, "metrics.jsonl"), log.New(&logged, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	s.gateway = gw
	alerts := captureAlerts(t)
	isolateGuardLogs(t)
	writeConfig(t, os.Getenv("HOME"))

	n := &notifier{}
	learner := &configWatch{what: "the compaction learner"}
	s.notifyRound(n, time.Now())
	if !n.started || len(alerts()) != 0 {
		t.Fatalf("a config that loads: started %v, alerts %q", n.started, alerts())
	}

	breakConfig(t)
	n.started = false // so a round that got past the load shows
	for i := 0; i < 3; i++ {
		s.notifyRound(n, time.Now())
		if _, ok := s.loadConfig(learner, false); ok {
			t.Fatal("the learner was given a config that does not load")
		}
	}
	if n.started {
		t.Fatal("a round ran on a config that did not load")
	}
	if got := alerts(); len(got) != 1 || got[0] != "error: config.json cannot be read" {
		t.Fatalf("on screen: %q, want it said once", got)
	}
	if c := strings.Count(logged.String(), "alerts paused"); c != 1 {
		t.Errorf("the alert rounds logged it %d times, want once:\n%s", c, logged.String())
	}
	if c := strings.Count(logged.String(), "the compaction learner paused"); c != 1 {
		t.Errorf("the learner logged it %d times, want once:\n%s", c, logged.String())
	}

	writeConfig(t, os.Getenv("HOME"))
	s.notifyRound(n, time.Now())
	if _, ok := s.loadConfig(learner, false); !ok {
		t.Fatal("the learner did not read the file again")
	}
	if !n.started {
		t.Fatal("the alert rounds did not start again once the file loaded")
	}
	if got := alerts(); len(got) != 2 || got[1] != "ok: config.json loads again" {
		t.Fatalf("on screen after the fix: %q", got)
	}
	if !strings.Contains(logged.String(), "alerts resumed") || !strings.Contains(logged.String(), "the compaction learner resumed") {
		t.Errorf("coming back is logged too:\n%s", logged.String())
	}
}
