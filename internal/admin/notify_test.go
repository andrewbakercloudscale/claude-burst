package admin

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/andrewbakercloudscale/claude-burst/internal/config"
)

type notified struct {
	mu   sync.Mutex
	msgs []string
}

func (n *notified) take() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	out := n.msgs
	n.msgs = nil
	return out
}

func recordNotifications(t *testing.T) *notified {
	t.Helper()
	rec := &notified{}
	old := notifyFunc
	notifyFunc = func(title, body string) error {
		rec.mu.Lock()
		rec.msgs = append(rec.msgs, title+" | "+body)
		rec.mu.Unlock()
		return nil
	}
	t.Cleanup(func() { notifyFunc = old })
	return rec
}

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

func saveNotify(t *testing.T, n config.NotifyConfig) {
	t.Helper()
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	cfg.Notify = n
	if err := config.Save(cfg); err != nil {
		t.Fatal(err)
	}
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

// Starting the gateway must not announce what is already true.
func TestNotifyFirstRoundIsBaselineOnly(t *testing.T) {
	s := newTestServer(t)
	rec := recordNotifications(t)
	pf := isolateGuardLogs(t)
	saveNotify(t, config.NotifyConfig{Failover: true, Compaction: true, Guards: true})
	s.gateway.ForceModelOverflow("claude-opus-5-5", time.Hour, "test")
	appendLine(t, pf, "2026-09-30 10:00:00 HEALED: rule reloaded")

	n := &notifier{}
	s.notifyRound(n, time.Now())
	if got := rec.take(); len(got) != 0 {
		t.Fatalf("first round notified: %v", got)
	}
	s.notifyRound(n, time.Now())
	if got := rec.take(); len(got) != 0 {
		t.Fatalf("nothing changed, yet notified: %v", got)
	}
}

func TestNotifyFailoverAndBack(t *testing.T) {
	s := newTestServer(t)
	rec := recordNotifications(t)
	isolateGuardLogs(t)
	saveNotify(t, config.NotifyConfig{Failover: true})

	n := &notifier{}
	s.notifyRound(n, time.Now())
	s.gateway.ForceModelOverflow("claude-opus-5-5", time.Hour, "test")
	s.notifyRound(n, time.Now())
	got := rec.take()
	if len(got) != 1 || !strings.Contains(got[0], "on the secondary") || !strings.Contains(got[0], "claude-opus-5-5") {
		t.Fatalf("entering overflow: %v", got)
	}

	s.gateway.ClearOverflow()
	s.notifyRound(n, time.Now())
	got = rec.take()
	if len(got) != 1 || !strings.Contains(got[0], "back on your subscription") {
		t.Fatalf("leaving overflow: %v", got)
	}
}

// A window that simply expires counts as coming back too: the round is
// judged at the time it is given, not at the stored window's creation.
func TestNotifyBackWhenTheWindowExpires(t *testing.T) {
	s := newTestServer(t)
	rec := recordNotifications(t)
	isolateGuardLogs(t)
	saveNotify(t, config.NotifyConfig{Failover: true})

	n := &notifier{}
	s.notifyRound(n, time.Now())
	s.gateway.ForceModelOverflow("claude-opus-5-5", time.Minute, "test")
	s.notifyRound(n, time.Now())
	rec.take()
	s.notifyRound(n, time.Now().Add(2*time.Minute))
	if got := rec.take(); len(got) != 1 || !strings.Contains(got[0], "back on your subscription") {
		t.Fatalf("after expiry: %v", got)
	}
}

func TestNotifyFailoverOffStaysQuiet(t *testing.T) {
	s := newTestServer(t)
	rec := recordNotifications(t)
	isolateGuardLogs(t)
	saveNotify(t, config.NotifyConfig{Compaction: true, Guards: true})

	n := &notifier{}
	s.notifyRound(n, time.Now())
	s.gateway.ForceModelOverflow("claude-opus-5-5", time.Hour, "test")
	s.notifyRound(n, time.Now())
	s.gateway.ClearOverflow()
	s.notifyRound(n, time.Now())
	if got := rec.take(); len(got) != 0 {
		t.Fatalf("failover notifications off, yet: %v", got)
	}

	// Switching it on applies from the next round, without a restart, and
	// does not replay the transition it missed.
	saveNotify(t, config.NotifyConfig{Failover: true})
	s.notifyRound(n, time.Now())
	if got := rec.take(); len(got) != 0 {
		t.Fatalf("replayed a missed transition: %v", got)
	}
	s.gateway.ForceModelOverflow("claude-fable-5-1", time.Hour, "test")
	s.notifyRound(n, time.Now())
	if got := rec.take(); len(got) != 1 {
		t.Fatalf("after switching on: %v", got)
	}
}

func TestNotifyGuards(t *testing.T) {
	s := newTestServer(t)
	rec := recordNotifications(t)
	pf := isolateGuardLogs(t)
	saveNotify(t, config.NotifyConfig{Guards: true})
	_, _, selfLog := selfHealPaths()
	if err := os.MkdirAll(filepath.Dir(selfLog), 0o700); err != nil {
		t.Fatal(err)
	}

	n := &notifier{}
	s.notifyRound(n, time.Now())

	// Routine lines (no event keyword) must not notify.
	appendLine(t, pf, "2026-09-30 10:00:00 check ok")
	s.notifyRound(n, time.Now())
	if got := rec.take(); len(got) != 0 {
		t.Fatalf("routine line notified: %v", got)
	}

	appendLine(t, pf, "2026-09-30 10:01:00 BROKEN: rdr rule missing")
	s.notifyRound(n, time.Now())
	if got := rec.take(); len(got) != 1 || !strings.Contains(got[0], "pf guard") {
		t.Fatalf("pf event: %v", got)
	}

	appendLine(t, selfLog, "2026-09-30 10:02:00 gateway is not running (LaunchAgent unloaded or its process gone) -- attempting reload")
	s.notifyRound(n, time.Now())
	if got := rec.take(); len(got) != 1 || !strings.Contains(got[0], "watchdog") {
		t.Fatalf("watchdog event: %v", got)
	}
}

func TestAppleQuote(t *testing.T) {
	cases := map[string]string{
		`plain`:                  `"plain"`,
		`say "hi"`:               `"say \"hi\""`,
		`back\slash`:             `"back\\slash"`,
		`\"`:                     `"\\\""`,
		`"; do shell script "rm`: `"\"; do shell script \"rm"`,
	}
	for in, want := range cases {
		if got := appleQuote(in); got != want {
			t.Errorf("appleQuote(%q) = %s, want %s", in, got, want)
		}
	}
}

// The test says when macOS will drop it: osascript exits 0 either way.
func TestNotifyTestWarnsWhenScriptEditorWasNeverAllowed(t *testing.T) {
	s := newTestServer(t)
	rec := recordNotifications(t)
	old := notifyRegistered
	t.Cleanup(func() { notifyRegistered = old })
	for _, registered := range []bool{false, true} {
		notifyRegistered = func() bool { return registered }
		rr := mutate(t, s, "/api/notify-test", "{}")
		if rr.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
		}
		if got := strings.Contains(rr.Body.String(), "Allow notifications"); got == registered {
			t.Errorf("registered=%v: body %s", registered, rr.Body.String())
		}
	}
	if n := len(rec.take()); n != 2 {
		t.Errorf("sent %d, want 2", n)
	}
}

func TestNotifySetupOpensScriptEditor(t *testing.T) {
	s := newTestServer(t)
	old := openNotifySetup
	t.Cleanup(func() { openNotifySetup = old })
	opened := 0
	openNotifySetup = func() error { opened++; return nil }
	rr := mutate(t, s, "/api/notify-setup", "{}")
	if rr.Code != http.StatusOK || opened != 1 || !strings.Contains(rr.Body.String(), "press Run") {
		t.Fatalf("status=%d opened=%d body=%s", rr.Code, opened, rr.Body.String())
	}
}
