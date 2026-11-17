package alerting

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/andrewbakercloudscale/claude-burst/internal/config"
	"github.com/andrewbakercloudscale/claude-burst/internal/notice"
)

// Each thing that shows Burst's toasts counts, and nothing (a Claude
// Desktop user's Mac) does not; an old claim from an abandoned panel does
// not either.
func TestToastsShownFindsTheSurfaces(t *testing.T) {
	now := time.Now()
	home := t.TempDir()
	if ToastsShown(home, now) {
		t.Fatal("an empty home shows toasts")
	}
	claims := filepath.Join(home, ".config", "claude-panel", "alerts-claimed")
	if err := os.MkdirAll(filepath.Join(claims, "evt-1"), 0o700); err != nil {
		t.Fatal(err)
	}
	old := now.Add(-30 * 24 * time.Hour)
	if err := os.Chtimes(filepath.Join(claims, "evt-1"), old, old); err != nil {
		t.Fatal(err)
	}
	if ToastsShown(home, now) {
		t.Fatal("a month-old claim counted")
	}
	if err := os.MkdirAll(filepath.Join(claims, "evt-2"), 0o700); err != nil {
		t.Fatal(err)
	}
	if !ToastsShown(home, now) {
		t.Fatal("a fresh claim did not count")
	}

	for _, f := range []struct {
		rel  string
		mode os.FileMode
	}{{".config/claude-panel/mod-installed", 0o644}, {".local/bin/ccusage-panel.sh", 0o755}} {
		h := t.TempDir()
		p := filepath.Join(h, f.rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, nil, f.mode); err != nil {
			t.Fatal(err)
		}
		if !ToastsShown(h, now) {
			t.Errorf("%s did not count", f.rel)
		}
	}
}

// Without toasts, Notification Center's default takes routine alerts too;
// with them it stays critical only; a chosen level wins either way.
func TestRouterNotificationCenterDefaultFollowsTheToasts(t *testing.T) {
	for _, c := range []struct {
		name   string
		shown  bool
		level  string
		posted int
	}{
		{"terminal user, default", true, "", 1},
		{"Desktop user, default", false, "", 2},
		{"Desktop user who chose critical", false, config.AlertLevelCritical, 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := fakeHelper(t, `{"status":"authorized","posted":true}`)
			p := notice.New(filepath.Join(t.TempDir(), "notices.json"), nil)
			cfg := config.AlertsConfig{NotificationCenter: config.AlertChannel{Level: c.level}}
			shown := c.shown
			p.SetChannels(&Router{Direct: NewNotifier("", nil), Config: func() config.AlertsConfig { return cfg },
				ToastsShown: func() bool { return shown }})
			p.Publish("upstream", notice.Error, "Requests are failing", "")
			p.Publish("compaction", notice.Info, "Compacted a session", "")
			p.Flush(2 * time.Second)
			if calls := waitCalls(t, h, c.posted); len(calls) != c.posted {
				t.Fatalf("posted %d, want %d: %v", len(calls), c.posted, calls)
			}
		})
	}
}

// Not yet asked, a post does not ask from the background: Post opens the
// prompt as an app through LaunchServices, once for a run of alerts.
func TestPostOpensThePromptThroughLaunchServicesOnce(t *testing.T) {
	var started [][]string
	old := startHelper
	startHelper = func(path string, args ...string) error {
		started = append(started, append([]string{path}, args...))
		return nil
	}
	asked.mu.Lock()
	asked.at = time.Time{}
	asked.mu.Unlock()
	t.Cleanup(func() { startHelper = old })

	fakeHelper(t, `{"status":"not-determined","posted":false}`)
	n := NewNotifier("", nil)
	for i := 0; i < 3; i++ {
		if r, err := n.Post(context.Background(), notice.Event{Title: "x", Kind: "k"}); err != nil || r.Posted {
			t.Fatalf("Post = %+v, %v", r, err)
		}
	}
	if len(started) != 1 || strings.Join(started[0][1:], " ") != "request --wait 120" {
		t.Fatalf("started %v, want one request", started)
	}

	got := strings.Join(openArgs("/Users/x/Applications/Claude Burst.app/Contents/Helpers/Claude Burst Notifier.app/Contents/MacOS/claude-burst-notifier", "request", "--wait", "120"), "|")
	want := "-n|/Users/x/Applications/Claude Burst.app/Contents/Helpers/Claude Burst Notifier.app|--args|request|--wait|120"
	if got != want {
		t.Errorf("open args %q, want %q", got, want)
	}
}

func TestOpenSettingsNamesTheNotifier(t *testing.T) {
	opened := 0
	old := openSettings
	openSettings = func() error { opened++; return nil }
	t.Cleanup(func() { openSettings = old })
	if err := OpenSettings(); err != nil || opened != 1 || !strings.HasSuffix(SettingsURL, "?id=ninja.andrewbaker.claude-burst.notifier") {
		t.Fatalf("%v %d %s", err, opened, SettingsURL)
	}
}
