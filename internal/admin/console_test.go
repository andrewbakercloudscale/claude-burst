package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/andrewbakercloudscale/claude-burst/internal/config"
	"github.com/andrewbakercloudscale/claude-burst/internal/notice"
)

// newTestConsole runs nothing real: launchctl and Terminal are recorders,
// and HOME is a temp dir.
func newTestConsole(t *testing.T) (*Console, *[]string, *string) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	dir, _ := config.ConfigDir()
	os.MkdirAll(dir, 0o700)
	notice.SetDefault(notice.New(notice.Path(dir), nil))
	t.Cleanup(func() { notice.SetDefault(nil) })
	var ran []string
	var opened string
	c := &Console{
		Version: "test", RootHelper: "/repo/scripts/transparent-root.sh",
		Run: func(ctx context.Context, name string, args ...string) ([]byte, error) {
			ran = append(ran, name+" "+strings.Join(args, " "))
			if len(args) > 0 && args[0] == "print" {
				return []byte("\tstate = running\n"), nil
			}
			return nil, nil
		},
		Terminal: func(p string) error { opened = p; return nil },
	}
	return c, &ran, &opened
}

func consoleDo(t *testing.T, h http.Handler, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, "http://127.0.0.1:7789"+path, nil)
	if method == http.MethodPost {
		req.Header.Set(mutationHeader, "1")
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// The console answers with the gateway down and a config that does not
// load: that is when it is needed.
func TestConsoleStatusWithBrokenConfig(t *testing.T) {
	c, _, _ := newTestConsole(t)
	p, _ := config.ConfigPath()
	os.WriteFile(p, []byte("{broken"), 0o600)
	rec := consoleDo(t, c.Handler(), http.MethodGet, "/api/console")
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	var st consoleStatus
	json.Unmarshal(rec.Body.Bytes(), &st)
	if st.ConfigError == "" || len(st.Checks) == 0 || st.Checks[0].Name != "Gateway service" || !st.Checks[0].OK {
		t.Fatalf("%+v", st)
	}
}

// Restart kickstarts a loaded gateway, clears the turned-off marker, and is
// in the audit.
func TestConsoleRestartIsAudited(t *testing.T) {
	c, ran, _ := newTestConsole(t)
	dir, _ := config.ConfigDir()
	os.WriteFile(filepath.Join(dir, "rolled-back"), []byte("x"), 0o600)
	rec := consoleDo(t, c.Handler(), http.MethodPost, "/api/console/restart")
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	if !strings.Contains(strings.Join(*ran, "\n"), "kickstart -k gui/") {
		t.Fatalf("ran %q", *ran)
	}
	if _, err := os.Stat(filepath.Join(dir, "rolled-back")); err == nil {
		t.Error("the turned-off marker survived Restart")
	}
	notice.Flush(2 * time.Second)
	evs := notice.ReadAudit(notice.AuditPath(notice.Path(dir)), 5)
	if len(evs) != 1 || evs[0].Title != "Console: /api/console/restart" || !evs[0].AuditOnly {
		t.Fatalf("audit = %+v", evs)
	}
}

// Repair runs in a Terminal, from the checkout's scripts.
func TestConsoleRepairOpensTerminal(t *testing.T) {
	c, _, opened := newTestConsole(t)
	rec := consoleDo(t, c.Handler(), http.MethodPost, "/api/console/repair")
	if rec.Code != 200 || *opened == "" {
		t.Fatalf("status %d opened %q: %s", rec.Code, *opened, rec.Body)
	}
	b, _ := os.ReadFile(*opened)
	if !strings.Contains(string(b), "'/repo/scripts/repair.sh'") {
		t.Errorf("script:\n%s", b)
	}
}

// The console refuses a foreign Host and a POST without the header, as the
// dashboard does.
func TestConsoleGuards(t *testing.T) {
	c, ran, _ := newTestConsole(t)
	req := httptest.NewRequest(http.MethodGet, "http://evil.example/api/console", nil)
	rec := httptest.NewRecorder()
	c.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("foreign host: %d", rec.Code)
	}
	req = httptest.NewRequest(http.MethodPost, "http://127.0.0.1/api/console/restart", nil)
	rec = httptest.NewRecorder()
	c.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden || len(*ran) != 0 {
		t.Errorf("no header: %d, ran %q", rec.Code, *ran)
	}
}

// The log around an entry leaves out the per-request noise and redacts.
func TestLogAround(t *testing.T) {
	p := filepath.Join(t.TempDir(), "claude-burst.log")
	at := time.Date(2026, 10, 4, 17, 16, 18, 0, time.Local)
	f := func(d time.Duration, s string) string {
		return at.Add(d).Format("2006/01/02 15:04:05") + " " + s + "\n"
	}
	os.WriteFile(p, []byte(
		f(-10*time.Minute, "too early")+
			f(-90*time.Second, `req=a start method="POST" path="/v1/messages"`)+
			f(-89*time.Second, "req=a retry route=anthropic attempt=2 Authorization: Bearer sk-secret")+
			f(-5*time.Second, `req=b client_gone route=anthropic err=Post "https://api.anthropic.com/api/event_logging/v2/batch"`)+
			f(0, "req=a failover route=anthropic")+
			f(10*time.Minute, "too late")), 0o600)
	lines, err := logAround(p, at, contextBefore, contextAfter)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(lines, "\n")
	if len(lines) != 2 || !strings.Contains(got, "failover") || strings.Contains(got, "sk-secret") || !strings.Contains(got, "REDACTED") {
		t.Fatalf("lines:\n%s", got)
	}
}

// Hook traffic is not an action: a prompt-notice POST on every prompt would
// bury the clicks.
func TestHookPostsAreNotAudited(t *testing.T) {
	newTestConsole(t)
	dir, _ := config.ConfigDir()
	h := audited("dashboard", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) })
	h(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/api/prompt-notice", nil))
	h(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/api/force", nil))
	notice.Flush(2 * time.Second)
	evs := notice.ReadAudit(notice.AuditPath(notice.Path(dir)), 5)
	if len(evs) != 1 || evs[0].Title != "Dashboard: /api/force" {
		t.Fatalf("audit = %+v", evs)
	}
}
