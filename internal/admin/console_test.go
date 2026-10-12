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
	"github.com/andrewbakercloudscale/claude-burst/internal/logline"
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
	// The publisher writes on its own goroutine: an audit entry still queued
	// would be written into HOME while the test removes it (directory not
	// empty, which failed the build of 0.20.25 once).
	t.Cleanup(func() { notice.Default().Flush(2 * time.Second); notice.SetDefault(nil) })
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
	req := localRequest(method, "http://127.0.0.1:7789"+path, nil)
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
	req := localRequest(http.MethodGet, "http://evil.example/api/console", nil)
	rec := httptest.NewRecorder()
	c.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("foreign host: %d", rec.Code)
	}
	req = localRequest(http.MethodPost, "http://127.0.0.1/api/console/restart", nil)
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
			f(-6*time.Second, `req=c ok route=anthropic model="" status=200 dur_ms=371 in_tok=0 out_tok=0 note=""`)+
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
	h(httptest.NewRecorder(), localRequest(http.MethodPost, "/api/prompt-notice", nil))
	h(httptest.NewRecorder(), localRequest(http.MethodPost, "/api/force", nil))
	notice.Flush(2 * time.Second)
	evs := notice.ReadAudit(notice.AuditPath(notice.Path(dir)), 5)
	if len(evs) != 1 || evs[0].Title != "Dashboard: /api/force" {
		t.Fatalf("audit = %+v", evs)
	}
}

// A config.json that cannot be read stops the gateway starting, and Restart
// and Repair with it. The console says so, and its one button puts the
// newest backup that loads back.
func TestConsoleRestoresAConfigThatWillNotLoad(t *testing.T) {
	c, _, _ := newTestConsole(t)
	home, _ := os.UserHomeDir()
	t.Setenv("CLAUDE_BURST_BACKUP_DIR", filepath.Join(home, "backups"))
	good := config.Default()
	good.AdminListen = "127.0.0.1:7999"
	if err := config.Save(good); err != nil {
		t.Fatal(err)
	}
	p, _ := config.ConfigPath()
	if err := os.WriteFile(p, []byte(`{"listen": `), 0o600); err != nil {
		t.Fatal(err)
	}
	get := httptest.NewRecorder()
	c.Handler().ServeHTTP(get, localRequest("GET", "http://127.0.0.1:7789/api/console", nil))
	var st consoleStatus
	if err := json.Unmarshal(get.Body.Bytes(), &st); err != nil || st.ConfigError == "" {
		t.Fatalf("the console must name the config error: %v %s", err, get.Body.String())
	}
	req := localRequest("POST", "http://127.0.0.1:7789/api/console/restore-config", nil)
	req.Header.Set(mutationHeader, "1")
	w := httptest.NewRecorder()
	c.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "Restored config.json") {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	if cfg, err := config.Load(); err != nil || cfg.AdminListen != "127.0.0.1:7999" {
		t.Fatalf("after the restore: %v, admin_listen %q", err, cfg.AdminListen)
	}
	// Pressed again with a config that loads: nothing to do, and said.
	w = httptest.NewRecorder()
	req = localRequest("POST", "http://127.0.0.1:7789/api/console/restore-config", nil)
	req.Header.Set(mutationHeader, "1")
	c.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "nothing was restored") {
		t.Fatalf("a second press: status %d: %s", w.Code, w.Body.String())
	}
}

// An audit entry older than the log file in use is found in the rotated
// files, in order, and a file wholly after the window is not read for it.
func TestLogAroundReadsRotatedFiles(t *testing.T) {
	p := filepath.Join(t.TempDir(), "claude-burst.log")
	at := time.Date(2026, 10, 4, 17, 16, 18, 0, time.Local)
	f := func(d time.Duration, s string) string {
		return at.Add(d).Format(logline.Stamp) + " level=info " + s + "\n"
	}
	os.WriteFile(p+".2", []byte(f(-3*time.Hour, "long before")+f(-2*time.Minute, "the cause")), 0o600)
	os.WriteFile(p+".1", []byte(f(-time.Minute, "the retry")+f(0, "the failover")+f(20*time.Second, "the recovery")+f(time.Hour, "an hour on")), 0o600)
	os.WriteFile(p, []byte(f(2*time.Hour, "today")+f(3*time.Hour, "later today")), 0o600)
	lines, err := logAround(p, at, contextBefore, contextAfter)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, l := range lines {
		got = append(got, l[strings.Index(l, "level=info ")+len("level=info "):])
	}
	if want := "the cause|the retry|the failover|the recovery"; strings.Join(got, "|") != want {
		t.Fatalf("lines %q, want %q", got, want)
	}
	// Nothing rotated yet and an entry from before the file began: no
	// lines, and no error for the missing .1.
	os.Remove(p + ".1")
	os.Remove(p + ".2")
	if lines, err := logAround(p, at, contextBefore, contextAfter); err != nil || len(lines) != 0 {
		t.Fatalf("with no rotated files: %q, %v", lines, err)
	}
}

// /api/log goes through the same backstop as the audit view and the
// console: a credential that reached the log is not shown by any of them.
func TestTheLogEndpointRedactsLikeTheAuditView(t *testing.T) {
	s := newTestServer(t)
	p, err := config.LogPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(p, []byte("2026-10-04T17:16:18.000+02:00 level=warn req=a retry Authorization: Bearer sk-secret-1\n"+
		"2026-10-04T17:16:19.000+02:00 level=info req=b ok route=anthropic status=200 note=\"kept as it is\"\n"), 0o600)
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, localRequest(http.MethodGet, "http://127.0.0.1/api/log", nil))
	body := rr.Body.String()
	if rr.Code != http.StatusOK || strings.Contains(body, "sk-secret-1") || !strings.Contains(body, "Bearer REDACTED") {
		t.Fatalf("status %d:\n%s", rr.Code, body)
	}
	if !strings.Contains(body, "2026-10-04T17:16:19.000+02:00 level=info req=b ok route=anthropic status=200 note=\"kept as it is\"\n") {
		t.Fatalf("a line with nothing to hide was changed:\n%s", body)
	}
}
