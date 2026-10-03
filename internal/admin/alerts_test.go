package admin

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/andrewbakercloudscale/claude-burst/internal/config"
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
