package admin

import (
	"context"
	"encoding/json"
	"github.com/andrewbakercloudscale/claude-burst/internal/claudesettings"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/andrewbakercloudscale/claude-burst/internal/config"
	"github.com/andrewbakercloudscale/claude-burst/internal/keepawake"
)

func TestPanelOptionsRoundTripKeepsComments(t *testing.T) {
	p := filepath.Join(t.TempDir(), "options")
	orig := "# header\n# CLAUDE_PANEL_REMOTE_CONTROL: start with --remote-control\nCLAUDE_PANEL_REMOTE_CONTROL=true\nCLAUDE_PANEL_RESTART_TOKENS=400000\n"
	if err := os.WriteFile(p, []byte(orig), 0o644); err != nil {
		t.Fatal(err)
	}
	got := readPanelOptions(p)
	if !got["remote_control"] || got["caffeinate"] || !got["session_title"] {
		t.Fatalf("defaults and file values: %v", got)
	}
	if err := setPanelOption(p, "CLAUDE_PANEL_REMOTE_CONTROL", false); err != nil {
		t.Fatal(err)
	}
	if err := setPanelOption(p, "CLAUDE_PANEL_SESSION_TITLE", false); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p)
	want := "# header\n# CLAUDE_PANEL_REMOTE_CONTROL: start with --remote-control\nCLAUDE_PANEL_REMOTE_CONTROL=false\nCLAUDE_PANEL_RESTART_TOKENS=400000\nCLAUDE_PANEL_SESSION_TITLE=false\n"
	if string(b) != want {
		t.Fatalf("file:\n%s\nwant:\n%s", b, want)
	}
	if got := readPanelOptions(p); got["remote_control"] || got["session_title"] {
		t.Fatalf("after writing: %v", got)
	}
}

func TestPanelOptionsRejectsUnknownKeys(t *testing.T) {
	s := newTestServer(t)
	for _, body := range []string{`{"restart_tokens":true}`, `{}`, `not json`} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:7788/api/panel-options", strings.NewReader(body))
		req.Header.Set("X-Claude-Burst-Admin", "1")
		s.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400", body, rec.Code)
		}
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:7788/api/panel-options", strings.NewReader(`{"caffeinate":true}`))
	req.Header.Set("X-Claude-Burst-Admin", "1")
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !readPanelOptions(panelOptionsPath())["caffeinate"] {
		t.Fatalf("status %d, options %v", rec.Code, readPanelOptions(panelOptionsPath()))
	}
}

func TestParseBlockersOnlyCountsPreventSystemSleep(t *testing.T) {
	out := `Listed by owning process:
   pid 34884(caffeinate): [0x00034570000190d2] 38:44:03 PreventUserIdleSystemSleep named: "caffeinate command-line tool"
	Details: caffeinate asserting on behalf of 'claude' (pid 34411)
   pid 58281(caffeinate): [0x00048f2d00079ce4] 07:11:56 PreventSystemSleep named: "caffeinate command-line tool"
	Details: caffeinate asserting on behalf of '/Users/x/cycle.sh' (pid 58280)
   pid 335(powerd): [0x00044fc100018b2d] 11:42:33 PreventUserIdleSystemSleep named: "Powerd - Prevent sleep while display is on"
`
	bs := keepawake.ParseBlockers(out)
	if len(bs) != 1 || bs[0].PID != "58281" || !strings.Contains(bs[0].For, "cycle.sh") {
		t.Fatalf("got %+v", bs)
	}
}

// The mode reaches a root script, so anything but the three values is
// refused before config or sudo is touched.
func TestKeepAwakeModeIsValidatedAndSaved(t *testing.T) {
	s := newTestServer(t)
	var ran [][]string
	old := sudoNonInteractive
	sudoNonInteractive = func(args ...string) ([]byte, error) { ran = append(ran, args); return []byte("applied"), nil }
	t.Cleanup(func() { sudoNonInteractive = old })
	var nap []bool
	oldNap := setGhosttyAppNap
	setGhosttyAppNap = func(on bool) error { nap = append(nap, on); return nil }
	t.Cleanup(func() { setGhosttyAppNap = oldNap })
	defer func() {
		if len(nap) != 3 || !nap[0] || nap[1] || !nap[2] {
			t.Errorf("Ghostty App Nap calls %v, want [true false true]", nap)
		}
	}()
	scripts := filepath.Join(t.TempDir(), "scripts")
	os.MkdirAll(scripts, 0o755)
	s.rootHelper = filepath.Join(scripts, "transparent-root.sh")

	post := func(body string) int {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:7788/api/keep-awake", strings.NewReader(body))
		req.Header.Set("X-Claude-Burst-Admin", "1")
		s.Handler().ServeHTTP(rec, req)
		return rec.Code
	}
	if c := post(`{"mode":"ac; rm -rf /"}`); c != http.StatusBadRequest || len(ran) != 0 {
		t.Fatalf("bad mode: status %d, ran %v", c, ran)
	}
	if c := post(`{"mode":"always"}`); c != http.StatusOK {
		t.Fatalf("status %d", c)
	}
	cfg, _ := config.Load()
	if !cfg.KeepAwakeLidClosed || cfg.KeepAwakeLidClosedPower != "always" {
		t.Fatalf("config not saved: %v %s", cfg.KeepAwakeLidClosed, cfg.KeepAwakeLidClosedPower)
	}
	if len(ran) != 1 || ran[0][1] != "apply" || ran[0][2] != "always" || filepath.Base(ran[0][0]) != "lid-awake-root.sh" {
		t.Fatalf("ran %v", ran)
	}
	post(`{"mode":"off"}`)
	cfg, _ = config.Load()
	if cfg.KeepAwakeLidClosed || ran[1][1] != "remove" {
		t.Fatalf("off: config %v, ran %v", cfg.KeepAwakeLidClosed, ran)
	}

	// The idle window reaches the root script with the activity file, and
	// the file exists at once so the window starts now.
	if c := post(`{"mode":"ac","idle_minutes":1441}`); c != http.StatusBadRequest {
		t.Fatalf("idle over a day: status %d", c)
	}
	if c := post(`{"mode":"ac","idle_minutes":60}`); c != http.StatusOK {
		t.Fatalf("idle 60: status %d", c)
	}
	cfg, _ = config.Load()
	last := ran[len(ran)-1]
	if cfg.KeepAwakeIdleMinutes != 60 || len(last) != 5 || last[3] != "60" || filepath.Base(last[4]) != "last-activity" {
		t.Fatalf("idle: config %d, ran %v", cfg.KeepAwakeIdleMinutes, last)
	}
	if _, err := os.Stat(last[4]); err != nil {
		t.Fatalf("activity file not created: %v", err)
	}
}

func TestPanelInstallRunsOneAtATime(t *testing.T) {
	s := newTestServer(t)
	release := make(chan struct{})
	var scripts []string
	old := runPanelCommand
	runPanelCommand = func(ctx context.Context, dir, script string, args ...string) ([]byte, error) {
		scripts = append(scripts, script)
		<-release
		return []byte("done"), nil
	}
	t.Cleanup(func() { runPanelCommand = old; panelLast = nil })
	panelLast = nil
	repo := filepath.Join(os.Getenv("HOME"), ".local", "share", "claude-burst", "claudecode-cost-usage-panel")
	os.MkdirAll(repo, 0o755)
	os.WriteFile(filepath.Join(repo, "claude-panel-setup.sh"), nil, 0o755)

	post := func(body string) int {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:7788/api/panel-install", strings.NewReader(body))
		req.Header.Set("X-Claude-Burst-Admin", "1")
		s.Handler().ServeHTTP(rec, req)
		return rec.Code
	}
	if c := post(`{"action":"reinstall"}`); c != http.StatusBadRequest {
		t.Fatalf("unknown action: %d", c)
	}
	if c := post(`{"action":"remove"}`); c != http.StatusOK {
		t.Fatalf("remove: %d", c)
	}
	if c := post(`{"action":"install"}`); c != http.StatusConflict {
		t.Fatalf("a second run while one is going: %d", c)
	}
	close(release)
	deadline := time.Now().Add(3 * time.Second)
	for s.readPanel().Running && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	v := s.readPanel()
	if v.Running || !v.LastRun.OK || len(scripts) != 1 || scripts[0] != "claude-panel-uninstall.sh" {
		t.Fatalf("view %+v, scripts %v", v, scripts)
	}
	b, _ := json.Marshal(v)
	if !strings.Contains(string(b), `"action":"remove"`) {
		t.Fatalf("json: %s", b)
	}
}

// TestStaleLidDaemonIsAProblem: the root daemon runs its own copy of
// lid-awake-root.sh and never rereads the repo, so a newer script in the
// checkout must show as a problem the dashboard tells you to Apply.
func TestStaleLidDaemonIsAProblem(t *testing.T) {
	s := newTestServer(t)
	scripts := filepath.Join(t.TempDir(), "scripts")
	os.MkdirAll(scripts, 0o755)
	s.rootHelper = filepath.Join(scripts, "transparent-root.sh")
	os.WriteFile(filepath.Join(scripts, "lid-awake-root.sh"), []byte("new"), 0o755)
	installed := filepath.Join(t.TempDir(), "lid-awake-root.sh")
	old := installedLidScript
	installedLidScript = installed
	t.Cleanup(func() { installedLidScript = old })

	if p := s.staleLidDaemon(); p != "" {
		t.Fatalf("nothing installed: %q, want no problem", p)
	}
	os.WriteFile(installed, []byte("old"), 0o755)
	if p := s.staleLidDaemon(); !strings.Contains(p, "older copy") {
		t.Fatalf("stale copy: %q", p)
	}
	os.WriteFile(installed, []byte("new"), 0o755)
	if p := s.staleLidDaemon(); p != "" {
		t.Fatalf("current copy: %q", p)
	}
}

// The machine-wide half reads as one of off, missing, stale, stopped or ok,
// so the dashboard can say exactly what the password is for.
func TestLidDaemonState(t *testing.T) {
	s := newTestServer(t)
	scripts := filepath.Join(t.TempDir(), "scripts")
	os.MkdirAll(scripts, 0o755)
	s.rootHelper = filepath.Join(scripts, "transparent-root.sh")
	os.WriteFile(filepath.Join(scripts, "lid-awake-root.sh"), []byte("new"), 0o755)
	installedLidScript = filepath.Join(t.TempDir(), "lid-awake-root.sh")
	running := true
	lidDaemonRunning = func() bool { return running }

	if got := s.lidDaemonState(false); got != "off" {
		t.Errorf("keep-awake off: %q", got)
	}
	if got := s.lidDaemonState(true); got != "missing" {
		t.Errorf("never installed: %q", got)
	}
	os.WriteFile(installedLidScript, []byte("old"), 0o755)
	if got := s.lidDaemonState(true); got != "stale" {
		t.Errorf("older copy: %q", got)
	}
	os.WriteFile(installedLidScript, []byte("new"), 0o755)
	running = false
	if got := s.lidDaemonState(true); got != "stopped" {
		t.Errorf("not running: %q", got)
	}
	running = true
	if got := s.lidDaemonState(true); got != "ok" {
		t.Errorf("current and running: %q", got)
	}
}

// Bypass is settings.json's permissions.defaultMode, so it reaches every way
// Claude Code starts. Saving it edits that one key, keeps the rest of the
// file, writes the launcher's panel key beside it, and turning it off leaves
// a mode the user chose themselves alone.
func TestBypassPermissionsOptionEditsOnlyDefaultMode(t *testing.T) {
	s := newTestServer(t)
	sp, _ := claudesettings.Path()
	if err := os.MkdirAll(filepath.Dir(sp), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sp, []byte(`{"permissions":{"allow":["Bash(ls)"]},"statusLine":{"type":"command","command":"x"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	post := func(body string) {
		t.Helper()
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:7788/api/panel-options", strings.NewReader(body))
		req.Header.Set("X-Claude-Burst-Admin", "1")
		s.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status %d %s", body, rec.Code, rec.Body.String())
		}
	}
	read := func() map[string]any {
		root, err := claudesettings.Read(sp)
		if err != nil {
			t.Fatal(err)
		}
		return root
	}

	post(`{"bypass_permissions":true}`)
	root := read()
	perms := root["permissions"].(map[string]any)
	if perms["defaultMode"] != "bypassPermissions" || perms["allow"] == nil || root["statusLine"] == nil {
		t.Fatalf("on: %v", root)
	}
	if !s.readPanel().Options["bypass_permissions"] {
		t.Fatal("the dashboard must read it back from settings.json")
	}
	if b, _ := os.ReadFile(panelOptionsPath()); !strings.Contains(string(b), "CLAUDE_PANEL_BYPASS_PERMISSIONS=true") {
		t.Fatalf("launcher key: %s", b)
	}

	post(`{"bypass_permissions":false}`)
	perms = read()["permissions"].(map[string]any)
	if _, ok := perms["defaultMode"]; ok || perms["allow"] == nil {
		t.Fatalf("off: %v", perms)
	}
	if b, _ := os.ReadFile(panelOptionsPath()); !strings.Contains(string(b), "CLAUDE_PANEL_BYPASS_PERMISSIONS=false") {
		t.Fatalf("launcher key: %s", b)
	}

	root = read()
	root["permissions"].(map[string]any)["defaultMode"] = "plan"
	if err := claudesettings.Write(sp, root); err != nil {
		t.Fatal(err)
	}
	post(`{"bypass_permissions":false}`)
	if read()["permissions"].(map[string]any)["defaultMode"] != "plan" {
		t.Fatal("off must not remove a mode the user chose")
	}
}
