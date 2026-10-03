package admin

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/andrewbakercloudscale/claude-burst/internal/config"
	"github.com/andrewbakercloudscale/claude-burst/internal/keepawake"
	"github.com/andrewbakercloudscale/claude-burst/internal/metrics"
	"github.com/andrewbakercloudscale/claude-burst/internal/notice"
)

func captureAlerts(t *testing.T) func() []string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "notices.json")
	notice.SetDefault(notice.New(path, nil))
	t.Cleanup(func() { notice.SetDefault(nil) })
	return func() []string {
		notice.Flush(2 * time.Second)
		evs, err := notice.Read(path)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, e := range evs {
			out = append(out, e.Severity+": "+e.Title)
		}
		return out
	}
}

// Guard alerts go on screen whatever the macOS notification switches say:
// the usage panel has its own.
func TestGuardLinesBecomeOnScreenAlerts(t *testing.T) {
	s := newTestServer(t)
	recordNotifications(t)
	alerts := captureAlerts(t)
	pf := isolateGuardLogs(t)
	saveNotify(t, config.NotifyConfig{})

	n := &notifier{}
	s.notifyRound(n, time.Now())
	appendLine(t, pf, "2026-10-03 10:01:00 BROKEN: rdr rule missing")
	s.notifyRound(n, time.Now())
	appendLine(t, pf, "2026-10-03 10:01:10 HEALED: anchor reloaded")
	s.notifyRound(n, time.Now())

	got := alerts()
	if len(got) != 2 || got[0] != "error: pf guard hit a problem" || got[1] != "ok: pf guard repaired the redirect" {
		t.Fatalf("alerts = %q", got)
	}
}

func TestInterceptAlertsOnlyOnChange(t *testing.T) {
	alerts := captureAlerts(t)
	on := interceptCheck{on: true, hosts: true, ca: true}
	noCA := interceptCheck{on: true, hosts: true}
	noHosts := interceptCheck{on: true, ca: true}

	alertIntercept(interceptCheck{}, noCA) // first look, or mode just switched on: quiet
	alertIntercept(noCA, noCA)             // still broken: quiet
	alertIntercept(noCA, on)
	alertIntercept(on, noCA)
	alertIntercept(noCA, on)
	alertIntercept(on, noHosts)
	alertIntercept(on, interceptCheck{}) // mode switched off: quiet

	got := alerts()
	want := []string{"ok: Transparent mode restored", "error: Burst CA no longer trusted",
		"ok: Transparent mode restored", "error: Transparent redirect missing"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("alerts = %q, want %q", got, want)
	}
}

func TestReadInterceptCheck(t *testing.T) {
	dir := t.TempDir()
	hosts := filepath.Join(dir, "hosts")
	old := hostsFile
	hostsFile = hosts
	t.Cleanup(func() { hostsFile = old })

	cfg := config.Default()
	if got := readInterceptCheck(cfg); got.on {
		t.Fatalf("base-url mode read as transparent: %+v", got)
	}
	cfg.Intercept.Mode = config.InterceptTransparent
	cfg.Intercept.CABundle = filepath.Join(dir, "bundle.pem")
	if got := readInterceptCheck(cfg); !got.on || got.ca || got.hosts {
		t.Fatalf("nothing installed read as %+v", got)
	}
	if err := os.WriteFile(hosts, []byte("127.0.0.1 localhost\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := readInterceptCheck(cfg); got.hosts {
		t.Fatalf("hosts without the redirect read as installed")
	}
}

func TestAlertSpendOncePerDayPerLevel(t *testing.T) {
	s := newTestServer(t)
	alerts := captureAlerts(t)
	spent := 12.0
	old := summarizeSpend
	summarizeSpend = func(string, time.Time) (metrics.Summary, error) {
		return metrics.Summary{APIEquivalentUSD: spent}, nil
	}
	t.Cleanup(func() { summarizeSpend = old })

	cfg := config.Default()
	now := time.Date(2026, 10, 3, 15, 0, 0, 0, time.Local)
	a := &alertRounds{}
	s.alertSpend(a, cfg, now) // off: nothing
	cfg.AlertDailySpendUSD = 20
	s.alertSpend(a, cfg, now.Add(time.Minute)) // under the level
	spent = 25
	s.alertSpend(a, cfg, now.Add(2*time.Minute))
	s.alertSpend(a, cfg, now.Add(3*time.Minute)) // same day, same level
	// A restarted gateway, same day: PublishOnce finds it in notices.json.
	s.alertSpend(&alertRounds{}, cfg, now.Add(4*time.Minute))
	cfg.AlertDailySpendUSD = 7.5
	s.alertSpend(a, cfg, now.Add(5*time.Minute)) // a new level shows
	s.alertSpend(a, cfg, now.Add(25*time.Hour))  // a new day shows

	got := alerts()
	want := []string{"warn: Spend today passed $20 (3 Oct)", "warn: Spend today passed $7.5 (3 Oct)", "warn: Spend today passed $7.5 (4 Oct)"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("alerts = %q, want %q", got, want)
	}
}

func TestAlertKeepAwakeTurnsOffAndBack(t *testing.T) {
	s := newTestServer(t)
	alerts := captureAlerts(t)
	live := keepawake.Status{SleepDisabled: true, SleepDisabledKnown: true, OnAC: true,
		AppliedMode: config.KeepAwakeOnAC, DaemonInstalled: true, GhosttyNapOff: true}
	old := readKeepAwake
	readKeepAwake = func() keepawake.Status { return live }
	t.Cleanup(func() { readKeepAwake = old })

	cfg := config.Default()
	cfg.KeepAwakeLidClosed = true
	cfg.KeepAwakeLidClosedPower = config.KeepAwakeOnAC
	now := time.Now()
	a := &alertRounds{}
	s.alertKeepAwake(a, cfg, now) // baseline: on, nothing said
	live.OnAC, live.SleepDisabled = false, false
	s.alertKeepAwake(a, cfg, now.Add(10*time.Second)) // too soon to look
	s.alertKeepAwake(a, cfg, now.Add(30*time.Second))
	live.OnAC, live.SleepDisabled = true, true
	s.alertKeepAwake(a, cfg, now.Add(60*time.Second))
	live.GhosttyNapOff = false
	s.alertKeepAwake(a, cfg, now.Add(90*time.Second))

	got := alerts()
	want := []string{"warn: Keep-awake turned off", "ok: Keep-awake back on", "warn: Keep-awake problem"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("alerts = %q, want %q", got, want)
	}
	if r := keepAwakeOffReason(keepawake.Status{OnAC: false, SleepDisabledKnown: true}, cfg, now, ""); !strings.Contains(r, "On battery") {
		t.Errorf("battery reason = %q", r)
	}
	cfg.KeepAwakeIdleMinutes = 30
	if r := keepAwakeOffReason(keepawake.Status{OnAC: true, LastActivity: now.Add(-31 * time.Minute)}, cfg, now, ""); !strings.Contains(r, "30 minutes") {
		t.Errorf("idle reason = %q", r)
	}
}

func TestAlertHandoverFromTheLog(t *testing.T) {
	alerts := captureAlerts(t)
	logPath := filepath.Join(t.TempDir(), "handover.log")
	old := handoverLogPath
	handoverLogPath = func() (string, error) { return logPath, nil }
	t.Cleanup(func() { handoverLogPath = old })

	appendLine(t, logPath, "2026-10-02 13:00:00 wrote  /x/old-repo (local only) [aaaa-1111]")
	a := &alertRounds{}
	alertHandovers(a) // baseline: what is already in the log is old news
	appendLine(t, logPath, "2026-10-02 13:32:14 queue  /x/claude-burst: 13 typed prompts, session ended (other) [c3fd-3765]")
	appendLine(t, logPath, "2026-10-02 13:32:15 write  /x/claude-burst with opus [c3fd-3765]")
	appendLine(t, logPath, "2026-10-02 13:33:09 wrote  /x/claude-burst (local only: HANDOFF.md is gitignored) [c3fd-3765]")
	alertHandovers(a)

	got := alerts()
	want := []string{"info: Session finished: claude-burst", "info: Handover written: claude-burst"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("alerts = %q, want %q", got, want)
	}
	evs, _ := notice.Read(notice.Default().Path())
	if evs[0].Session != "c3fd-3765" {
		t.Errorf("session = %q; the event must name it so its own panel can skip it", evs[0].Session)
	}
}

func TestAlertUpgradeOncePerVersion(t *testing.T) {
	alerts := captureAlerts(t)
	alertUpgrade(upgradeStatus{RunningVersion: "0.5.0", LatestVersion: "0.5.0"})
	alertUpgrade(upgradeStatus{RunningVersion: "0.5.0", LatestVersion: "0.6.0", Error: "offline"})
	alertUpgrade(upgradeStatus{RunningVersion: "0.5.0", LatestVersion: "0.6.0"})
	alertUpgrade(upgradeStatus{RunningVersion: "0.5.0", LatestVersion: "0.6.0"})
	alertUpgrade(upgradeStatus{RunningVersion: "0.6.1", LatestVersion: "0.6.0"})
	if got := alerts(); len(got) != 1 || got[0] != "info: Claude Burst 0.6.0 available" {
		t.Fatalf("alerts = %q", got)
	}
	for _, c := range []struct {
		a, b string
		want bool
	}{{"0.10.0", "0.9.9", true}, {"1.0.0", "0.99.0", true}, {"0.5.0", "0.5.0", false}, {"v0.5.1", "0.5.0", true}, {"", "0.5.0", false}, {"0.5", "0.4.0", false}} {
		if got := versionNewer(c.a, c.b); got != c.want {
			t.Errorf("versionNewer(%q, %q) = %v", c.a, c.b, got)
		}
	}
}

func TestAlertSpendEndpoint(t *testing.T) {
	s := newTestServer(t)
	for _, body := range []string{`{}`, `not json`, `{"usd":-1}`, `{"usd":100001}`} {
		if rr := mutate(t, s, "/api/alert-spend", body); rr.Code != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400", body, rr.Code)
		}
	}
	if rr := mutate(t, s, "/api/alert-spend", `{"usd":42.5}`); rr.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rr.Code, rr.Body.String())
	}
	cfg, err := config.Load()
	if err != nil || cfg.AlertDailySpendUSD != 42.5 {
		t.Fatalf("saved %v (err %v), want 42.5", cfg.AlertDailySpendUSD, err)
	}
}

func TestAlertTestEndpoint(t *testing.T) {
	s := newTestServer(t)
	if rr := mutate(t, s, "/api/alert-test", ""); rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("with no publisher: %d %s", rr.Code, rr.Body.String())
	}
	alerts := captureAlerts(t)
	if rr := mutate(t, s, "/api/alert-test", ""); rr.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rr.Code, rr.Body.String())
	}
	if got := alerts(); len(got) != 1 || !strings.HasPrefix(got[0], "info: Test alert ") {
		t.Fatalf("alerts = %q", got)
	}
}
