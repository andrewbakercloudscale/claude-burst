package alerting

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/andrewbakercloudscale/claude-burst/internal/config"
	"github.com/andrewbakercloudscale/claude-burst/internal/notice"
)

// No test in this package may run the real helper: it would put a banner
// on the Mac running the tests, and its first run would ask that Mac for
// permission. TestMain makes reaching it a failure; tests that want a
// helper install fakeHelper.
func TestMain(m *testing.M) {
	helperPath = func() string { return "" }
	runHelper = func(context.Context, string, ...string) ([]byte, error) {
		panic("a test ran the notifier helper without stubbing it")
	}
	startHelper = func(string, ...string) error {
		panic("a test started the notifier helper without stubbing it")
	}
	openSettings = func() error { panic("a test opened System Settings without stubbing it") }
	os.Exit(m.Run())
}

// helperCalls records each run of the stand-in helper.
type helperCalls struct {
	mu    sync.Mutex
	calls [][]string
	reply string
	err   error
}

func (h *helperCalls) list() [][]string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([][]string(nil), h.calls...)
}

// fakeHelper installs a stand-in helper that answers reply.
func fakeHelper(t *testing.T, reply string) *helperCalls {
	t.Helper()
	h := &helperCalls{reply: reply}
	oldPath, oldRun := helperPath, runHelper
	helperPath = func() string { return "/fake/claude-burst-notifier" }
	runHelper = func(_ context.Context, path string, args ...string) ([]byte, error) {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.calls = append(h.calls, args)
		return []byte(h.reply + "\n"), h.err
	}
	t.Cleanup(func() { helperPath, runHelper = oldPath, oldRun })
	return h
}

func ev(kind, sev, title string) notice.Event {
	return notice.Event{Kind: kind, Severity: sev, Title: title, At: time.Now()}
}

func TestCriticalIsErrorsAndTheListedAlerts(t *testing.T) {
	cases := []struct {
		e    notice.Event
		want bool
	}{
		{ev("config", notice.Error, "config.json cannot be read"), true},
		{ev("bypass", notice.Error, "Burst bypassed: redirect removed"), true},
		{ev("ext-pf-heal", notice.Warn, "Burst bypassed: redirect removed after 4 failed repairs"), true},
		{ev("upstream", notice.Error, "Requests are failing"), true},
		{ev("upstream", notice.OK, "Requests are succeeding again"), true},
		{ev("compaction", notice.Warn, "Compaction failed"), true},
		{ev("compaction", notice.Warn, "Summary leaves out the oldest messages"), false},
		{ev("failover", notice.Warn, "Failed over to GLM"), false},
		{ev("update", notice.Info, "Claude Burst 1.0 available"), false},
		{ev("handover", notice.Info, "Handover written: x"), false},
		{ev("test", notice.Info, "Test alert 10:00:00"), true},
	}
	for _, c := range cases {
		if got := Critical(c.e); got != c.want {
			t.Errorf("Critical(%s %s %q) = %v, want %v", c.e.Kind, c.e.Severity, c.e.Title, got, c.want)
		}
	}
}

func TestLevels(t *testing.T) {
	routine := ev("failover", notice.Warn, "Failed over to GLM")
	critical := ev("upstream", notice.Error, "Requests are failing")
	for _, c := range []struct {
		level              string
		routine, critical_ bool
	}{
		{config.AlertLevelAll, true, true},
		{config.AlertLevelCritical, false, true},
		{config.AlertLevelOff, false, false},
	} {
		var g gate
		if got := g.pass(c.level, routine); got != c.routine {
			t.Errorf("%s: routine alert passed = %v", c.level, got)
		}
		if got := g.pass(c.level, critical); got != c.critical_ {
			t.Errorf("%s: critical alert passed = %v", c.level, got)
		}
	}
}

// At "critical" an ok goes where its problem went: "config.json loads
// again" is routine on its own, and must still end the red error the
// person was shown.
func TestOKFollowsTheProblemItEnds(t *testing.T) {
	var g gate
	if !g.pass(config.AlertLevelCritical, ev("config", notice.Error, "config.json cannot be read")) {
		t.Fatal("the error did not pass")
	}
	if !g.pass(config.AlertLevelCritical, ev("config", notice.OK, "config.json loads again")) {
		t.Fatal("the ok that ends it was held back")
	}
	if g.pass(config.AlertLevelCritical, ev("config", notice.OK, "config.json loads again")) {
		t.Fatal("a second ok, with nothing open, passed")
	}
	// A routine ok whose problem was filtered out stays out.
	if g.pass(config.AlertLevelCritical, ev("failover", notice.Warn, "Failed over to GLM")) ||
		g.pass(config.AlertLevelCritical, ev("failover", notice.OK, "Back on Claude")) {
		t.Fatal("a routine pair passed at critical")
	}
}

func TestSeedReopensStandingProblems(t *testing.T) {
	var g gate
	g.seed([]notice.Event{
		ev("config", notice.Error, "config.json cannot be read"),
		ev("network", notice.Error, "Network offline"),
		ev("network", notice.OK, "Network back"),
	})
	if !g.pass(config.AlertLevelCritical, ev("config", notice.OK, "config.json loads again")) {
		t.Error("the ok for a problem standing before a restart was held back")
	}
	if g.open["network"] {
		t.Error("a resolved problem was left open")
	}
}

// The gateway's router: the publisher's dedupe first, then each channel's
// level, and the helper is run for what passes Notification Center's.
func TestGatewayPostsWhatPassesAndRespectsTheRepeatLimit(t *testing.T) {
	h := fakeHelper(t, `{"status":"authorized","posted":true}`)
	dir := t.TempDir()
	path := filepath.Join(dir, "notices.json")
	p := notice.New(path, nil)
	n := NewNotifier("http://127.0.0.1:7788/", nil)
	cfg := config.AlertsConfig{} // the defaults: toasts all, Notification Center critical
	p.SetChannels(&Router{Direct: n, Config: func() config.AlertsConfig { return cfg }})

	p.Publish("upstream", notice.Error, "Requests are failing", "5 of the last 5 failed")
	p.Publish("upstream", notice.Error, "Requests are failing", "again") // within RepeatAfter: dropped
	p.Publish("failover", notice.Warn, "Failed over to GLM", "")
	p.Flush(2 * time.Second)

	calls := waitCalls(t, h, 1)
	if len(calls) != 1 {
		t.Fatalf("helper ran %d times, want once: %v", len(calls), calls)
	}
	got := strings.Join(calls[0], " ")
	for _, want := range []string{"post", "--title Requests are failing", "--body 5 of the last 5 failed", "--group upstream", "--url http://127.0.0.1:7788/", "--sound"} {
		if !strings.Contains(got, want) {
			t.Errorf("helper args %q lack %q", got, want)
		}
	}
	// Both alerts are toasts at the default level, and the audit says the
	// gateway dealt with Notification Center, so the console does not.
	evs, _ := notice.Read(path)
	if len(evs) != 2 {
		t.Fatalf("notices.json holds %d alerts, want 2", len(evs))
	}
	for _, e := range notice.ReadAudit(notice.AuditPath(path), 10) {
		if !e.NotifyDone || e.Hidden {
			t.Errorf("audit entry %q: notify_done=%v hidden=%v", e.Title, e.NotifyDone, e.Hidden)
		}
	}
}

func waitCalls(t *testing.T, h *helperCalls, n int) [][]string {
	t.Helper()
	deadline := time.Now().Add(700 * time.Millisecond)
	for time.Now().Before(deadline) {
		if c := h.list(); len(c) >= n {
			time.Sleep(50 * time.Millisecond) // and no more than n
			return h.list()
		}
		time.Sleep(10 * time.Millisecond)
	}
	return h.list()
}

// Toasts set below an alert keep it out of notices.json (what the panel and
// the mod read) but in the audit, marked hidden.
func TestToastLevelKeepsAlertsOutOfNotices(t *testing.T) {
	path := filepath.Join(t.TempDir(), "notices.json")
	p := notice.New(path, nil)
	cfg := config.AlertsConfig{Toasts: config.AlertChannel{Level: config.AlertLevelCritical},
		NotificationCenter: config.AlertChannel{Level: config.AlertLevelOff}}
	p.SetChannels(&Router{Config: func() config.AlertsConfig { return cfg }})
	p.Publish("failover", notice.Warn, "Failed over to GLM", "")
	p.Publish("bypass", notice.Error, "Burst bypassed: redirect removed", "")
	p.Flush(2 * time.Second)
	evs, _ := notice.Read(path)
	if len(evs) != 1 || evs[0].Kind != "bypass" {
		t.Fatalf("notices.json = %+v, want the bypass alone", evs)
	}
	audit := notice.ReadAudit(notice.AuditPath(path), 10)
	if len(audit) != 2 || !audit[1].Hidden || audit[1].Kind != "failover" {
		t.Fatalf("audit = %+v, want the failover there and hidden", audit)
	}
	// Without a direct notifier the console's watcher decides.
	if audit[0].NotifyDone {
		t.Error("an alert with no direct notifier was marked notify_done")
	}
}

// PublishOnce must not publish again every round an alert the toasts' level
// kept out of notices.json: not in this process, and not after a restart.
func TestPublishOnceHeldBackStaysOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "notices.json")
	cfg := config.AlertsConfig{Toasts: config.AlertChannel{Level: config.AlertLevelOff}}
	route := func(p *notice.Publisher) {
		p.SetChannels(&Router{Config: func() config.AlertsConfig { return cfg }})
	}
	p := notice.New(path, nil)
	route(p)
	if !p.PublishOnce("update", notice.Info, "Claude Burst 9.9 available", "") {
		t.Fatal("first not published")
	}
	if p.PublishOnce("update", notice.Info, "Claude Burst 9.9 available", "") {
		t.Fatal("published twice")
	}
	q := notice.New(path, nil)
	route(q)
	if q.PublishOnce("update", notice.Info, "Claude Burst 9.9 available", "") {
		t.Fatal("published again after a restart")
	}
}

func TestNotificationCenterOffRunsNothing(t *testing.T) {
	h := fakeHelper(t, `{"status":"authorized","posted":true}`)
	path := filepath.Join(t.TempDir(), "notices.json")
	p := notice.New(path, nil)
	cfg := config.AlertsConfig{NotificationCenter: config.AlertChannel{Level: config.AlertLevelOff}}
	p.SetChannels(&Router{Direct: NewNotifier("", nil), Config: func() config.AlertsConfig { return cfg }})
	p.Publish("bypass", notice.Error, "Burst bypassed: redirect removed", "")
	p.Flush(2 * time.Second)
	if c := waitCalls(t, h, 1); len(c) != 0 {
		t.Fatalf("helper ran with Notification Center off: %v", c)
	}
}

// The console's watcher posts the alerts in the audit that no gateway
// posted (`claude-burst notice`, written with the gateway down), and only
// those written after it started.
func TestWatcherPostsAlertsTheGatewayDidNot(t *testing.T) {
	h := fakeHelper(t, `{"status":"authorized","posted":true}`)
	dir := t.TempDir()
	path := filepath.Join(dir, "notices.json")
	audit := notice.AuditPath(path)
	cfg := config.AlertsConfig{}
	before := notice.New(path, nil)
	before.Publish("ext-pf-heal", notice.Error, "Old news", "")
	before.Flush(2 * time.Second)

	w := &Watcher{AuditPath: audit, Notifier: NewNotifier("", nil), Config: func() config.AlertsConfig { return cfg }, Now: time.Now}
	w.Tick() // baseline

	script := notice.New(path, nil)
	script.SetChannels(&Router{Config: func() config.AlertsConfig { return cfg }})
	script.Publish("ext-pf-heal", notice.Error, "Burst redirect broken: repair 1 of 4 failed", "")
	script.Publish("ext-handover", notice.Info, "Handover written", "") // not critical
	script.Flush(2 * time.Second)

	gw := notice.New(path, nil)
	gw.SetChannels(&Router{Direct: NewNotifier("", nil), Config: func() config.AlertsConfig {
		return config.AlertsConfig{NotificationCenter: config.AlertChannel{Level: config.AlertLevelOff}}
	}})
	gw.Publish("upstream", notice.Error, "Requests are failing", "") // the gateway's, decided there
	gw.Flush(2 * time.Second)
	if err := notice.AppendAudit(audit, "console", notice.Info, "Restarted", ""); err != nil { // an action
		t.Fatal(err)
	}

	w.Tick()
	calls := waitCalls(t, h, 1)
	if len(calls) != 1 || !strings.Contains(strings.Join(calls[0], " "), "--title Burst redirect broken: repair 1 of 4 failed") {
		t.Fatalf("helper calls = %v, want the pf guard's alert alone", calls)
	}
	w.Tick()
	if c := waitCalls(t, h, 2); len(c) != 1 {
		t.Fatalf("a second tick posted again: %v", c)
	}
}

func TestWatcherSkipsAlertsOlderThanItsWindow(t *testing.T) {
	h := fakeHelper(t, `{"status":"authorized","posted":true}`)
	dir := t.TempDir()
	audit := filepath.Join(dir, "audit.jsonl")
	if err := os.WriteFile(audit, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	w := &Watcher{AuditPath: audit, Notifier: NewNotifier("", nil), Config: func() config.AlertsConfig { return config.AlertsConfig{} }, Now: time.Now}
	w.Tick()
	old := notice.Event{ID: "1", Kind: "bypass", Severity: notice.Error, Title: "Burst bypassed", At: time.Now().Add(-time.Hour)}
	b, _ := json.Marshal(old)
	if err := os.WriteFile(audit, append(b, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	w.Tick()
	if c := waitCalls(t, h, 1); len(c) != 0 {
		t.Fatalf("an hour-old alert was posted: %v", c)
	}
}

func TestPostReadsTheHelpersAnswer(t *testing.T) {
	h := fakeHelper(t, `{"status":"denied","posted":false,"error":"turned off"}`)
	h.err = &exec.ExitError{} // exit 2: the line still says why
	r, err := NewNotifier("", nil).Post(context.Background(), ev("bypass", notice.Error, "x"))
	if err != nil || r.Status != "denied" || r.Posted || r.Error != "turned off" {
		t.Fatalf("Post = %+v, %v", r, err)
	}
	helperPath = func() string { return "" }
	if _, err := Status(context.Background()); !errors.Is(err, ErrNoHelper) {
		t.Fatalf("Status with no helper: %v", err)
	}
}

func TestAskIfUnaskedStartsThePromptOnlyOnce(t *testing.T) {
	var started [][]string
	old := startHelper
	startHelper = func(_ string, args ...string) error { started = append(started, args); return nil }
	t.Cleanup(func() { startHelper = old })

	h := fakeHelper(t, `{"status":"not-determined","posted":false}`)
	if st, err := AskIfUnasked(context.Background()); err != nil || st != "not-determined" {
		t.Fatalf("AskIfUnasked = %q, %v", st, err)
	}
	if len(started) != 1 || started[0][0] != "request" {
		t.Fatalf("started %v, want one request", started)
	}
	h.reply = `{"status":"denied","posted":false}`
	if _, err := AskIfUnasked(context.Background()); err != nil || len(started) != 1 {
		t.Fatalf("asked again after an answer: %v, %v", started, err)
	}
}

func TestLiveKeepsTheLastGoodSettings(t *testing.T) {
	calls := 0
	good := config.AlertsConfig{Toasts: config.AlertChannel{Level: config.AlertLevelOff}}
	l := &Live{Interval: 0, Load: func() (config.Config, error) {
		calls++
		if calls == 1 {
			return config.Config{Alerts: good}, nil
		}
		return config.Config{}, errors.New("broken")
	}}
	if l.Get() != good || l.Get() != good {
		t.Fatal("a config.json that stopped loading lost the settings")
	}
	l.Set(config.AlertsConfig{})
	if l.Get().Toasts.Level != "" {
		t.Fatal("Set did not apply")
	}
}
