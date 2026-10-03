package admin

import (
	"os"
	"path/filepath"
	"testing"
	"time"
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
