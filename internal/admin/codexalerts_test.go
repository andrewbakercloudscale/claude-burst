package admin

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/andrewbakercloudscale/claude-burst/internal/notice"
)

type shown struct {
	pid        int
	sev, title string
}

func newTestCodexAlerts(t *testing.T, front int) (*CodexAlerts, *notice.Publisher, *[]shown) {
	t.Helper()
	dir := t.TempDir()
	var got []shown
	c := &CodexAlerts{
		NoticesPath: filepath.Join(dir, "notices.json"),
		ClaimsDir:   filepath.Join(dir, "claims"),
		OptionsPath: filepath.Join(dir, "options"),
		Front:       func() int { return front },
		IsCodex:     func(pid int) bool { return pid == 42 },
		Show:        func(pid, secs int, sev, title, detail string) { got = append(got, shown{pid, sev, title}) },
		Now:         time.Now,
	}
	return c, notice.New(c.NoticesPath, nil), &got
}

// A Burst-wide alert shows over Codex when Codex is in front, once, and is
// claimed so no panel shows it again.
func TestCodexAlertShownOnceAndClaimed(t *testing.T) {
	c, p, got := newTestCodexAlerts(t, 42)
	seen := map[string]bool{}
	p.Publish("failover", notice.Warn, "Failed over to together", "why")
	p.Flush(2 * time.Second)
	c.tick(seen)
	c.tick(seen)
	if len(*got) != 1 || (*got)[0].pid != 42 || (*got)[0].title != "Failed over to together" {
		t.Fatalf("shown %+v", *got)
	}
	evs, _ := notice.Read(c.NoticesPath)
	if !c.claimed(evs[0].ID) {
		t.Error("not claimed: a panel would show it again")
	}
}

// Not over another app, not one a panel already claimed, and never an alert
// about one Claude Code session.
func TestCodexAlertSkips(t *testing.T) {
	c, p, got := newTestCodexAlerts(t, 7) // Ghostty in front
	seen := map[string]bool{}
	p.Publish("network", notice.Error, "Network offline", "")
	p.PublishFor("sess-1", "context", notice.Info, "Context at 277k of 300k", "")
	p.Flush(2 * time.Second)
	c.tick(seen)
	if len(*got) != 0 {
		t.Fatalf("shown over another app: %+v", *got)
	}
	// Codex comes to the front: the held Burst-wide alert shows, the
	// session one never does.
	c.Front = func() int { return 42 }
	c.tick(seen)
	c.tick(seen)
	if len(*got) != 1 || (*got)[0].title != "Network offline" {
		t.Fatalf("shown %+v", *got)
	}
	// Claimed by a panel first: left alone.
	p.Publish("spend", notice.Warn, "Spend passed $50", "")
	p.Flush(2 * time.Second)
	evs, _ := notice.Read(c.NoticesPath)
	os.MkdirAll(filepath.Join(c.ClaimsDir, evs[len(evs)-1].ID), 0o700)
	c.tick(seen)
	if len(*got) != 1 {
		t.Fatalf("showed a claimed alert: %+v", *got)
	}
}

// The panel's "Show gateway alerts on screen" switch covers Codex too.
func TestCodexAlertsHonourTheSwitch(t *testing.T) {
	c, p, got := newTestCodexAlerts(t, 42)
	os.WriteFile(c.OptionsPath, []byte("CLAUDE_PANEL_ALERTS=false\n"), 0o600)
	p.Publish("failover", notice.Warn, "Failed over", "")
	p.Flush(2 * time.Second)
	c.tick(map[string]bool{})
	if len(*got) != 0 {
		t.Fatalf("shown with alerts off: %+v", *got)
	}
}

// The dashboard's test waits for Codex to come to the front, shows once,
// records when, and gives up after two minutes.
func TestCodexAlertTestRequest(t *testing.T) {
	c, _, got := newTestCodexAlerts(t, 7)
	c.TestPath = filepath.Join(t.TempDir(), "codex-alert-test.json")
	os.WriteFile(c.OptionsPath, []byte("CLAUDE_PANEL_ALERTS=off\n"), 0o600) // asked for, so shown anyway
	writeCodexAlertTest(c.TestPath, CodexAlertTest{Requested: time.Now()})
	seen := map[string]bool{}
	c.tick(seen)
	if len(*got) != 0 {
		t.Fatal("shown over another app")
	}
	c.Front = func() int { return 42 }
	c.tick(seen)
	c.tick(seen)
	if len(*got) != 1 || (*got)[0].pid != 42 {
		t.Fatalf("shown %+v", *got)
	}
	if st, _ := readCodexAlertTest(c.TestPath); st.Shown.IsZero() {
		t.Error("shown time not recorded for the dashboard")
	}
	writeCodexAlertTest(c.TestPath, CodexAlertTest{Requested: time.Now().Add(-3 * time.Minute)})
	c.tick(seen)
	if st, _ := readCodexAlertTest(c.TestPath); st.Error == "" || len(*got) != 1 {
		t.Errorf("expired request: %+v, shown %d", st, len(*got))
	}
}
