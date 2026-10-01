package admin

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/andrewbakercloudscale/claude-burst/internal/config"
)

// The switch saves config.json and installs the hooks; off removes them.
// The GET reports it, with who masters what.
func TestCoordinationSwitchInstallsAndRemovesTheHooks(t *testing.T) {
	s := newTestServer(t)
	home := os.Getenv("HOME")
	writeConfig(t, home)
	t.Setenv("CLAUDE_BURST_COORD_DIR", t.TempDir())

	rr := mutate(t, s, "/api/coordination-save", `{"enabled":true,"master_idle_minutes":20}`)
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "after 20 minutes") {
		t.Fatalf("save: %d %s", rr.Code, rr.Body)
	}
	cfg, err := config.Load()
	if err != nil || !cfg.SessionCoordination.Enabled || cfg.SessionCoordination.MasterIdleMinutes != 20 {
		t.Fatalf("config.json: %+v %v", cfg.SessionCoordination, err)
	}
	b, _ := os.ReadFile(filepath.Join(home, ".claude", "settings.json"))
	if strings.Count(string(b), " coord ") != 6 {
		t.Fatalf("want the six coordination hooks:\n%s", b)
	}

	req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1/api/coordination", nil)
	get := httptest.NewRecorder()
	s.Handler().ServeHTTP(get, req)
	var d struct {
		Installed bool `json:"installed"`
		Resolved  struct {
			NudgeMinutes int `json:"nudge_minutes"`
		} `json:"resolved"`
	}
	if err := json.Unmarshal(get.Body.Bytes(), &d); err != nil || !d.Installed || d.Resolved.NudgeMinutes != config.DefaultCoordNudge {
		t.Fatalf("GET: %v %s", err, get.Body)
	}

	if rr := mutate(t, s, "/api/coordination-save", `{"nudge_minutes":999}`); rr.Code != http.StatusBadRequest {
		t.Fatalf("999 minutes must be refused, got %d", rr.Code)
	}
	if rr := mutate(t, s, "/api/coordination-save", `{"enabled":false}`); rr.Code != http.StatusOK {
		t.Fatalf("off: %d %s", rr.Code, rr.Body)
	}
	if b, _ := os.ReadFile(filepath.Join(home, ".claude", "settings.json")); strings.Contains(string(b), " coord ") {
		t.Fatalf("off must remove the hooks:\n%s", b)
	}
}

// The dashboard's actions: message a session, hand a file on, and the
// activity log that records both.
func TestCoordinationActions(t *testing.T) {
	s := newTestServer(t)
	writeConfig(t, os.Getenv("HOME"))
	dir := t.TempDir()
	t.Setenv("CLAUDE_BURST_COORD_DIR", dir)
	t.Setenv("CLAUDE_BURST_COORD_TRACK_TMP", "1") // the file below is in a temp dir
	cfg, _ := config.Load()
	c, _ := Coordinator(cfg)
	file := filepath.Join(t.TempDir(), "notes.txt")
	os.WriteFile(file, []byte("x"), 0o644)
	for _, sid := range []string{"aaaa1111-x", "bbbb2222-y"} {
		c.Hook("session-start", strings.NewReader(`{"session_id":"`+sid+`","cwd":"/tmp"}`), io.Discard)
	}
	c.Hook("pre-tool", strings.NewReader(`{"session_id":"aaaa1111-x","cwd":"/tmp","tool_name":"Edit","tool_input":{"file_path":"`+file+`"}}`), io.Discard)

	if rr := mutate(t, s, "/api/coordination-act", `{"session":"bbbb","message":"leave notes.txt to aaaa"}`); rr.Code != http.StatusOK {
		t.Fatalf("message: %d %s", rr.Code, rr.Body)
	}
	var out strings.Builder
	c.Hook("prompt", strings.NewReader(`{"session_id":"bbbb2222-y","cwd":"/tmp"}`), &out)
	if !strings.Contains(out.String(), "from the user") || !strings.Contains(out.String(), "leave notes.txt to aaaa") {
		t.Fatalf("the session must get the dashboard's message:\n%s", out.String())
	}
	if rr := mutate(t, s, "/api/coordination-act", `{"session":"zzzz","message":"x"}`); rr.Code != http.StatusNotFound {
		t.Fatalf("unknown session: want 404, got %d", rr.Code)
	}

	if rr := mutate(t, s, "/api/coordination-act", `{"release":"`+file+`"}`); rr.Code != http.StatusOK {
		t.Fatalf("hand on: %d %s", rr.Code, rr.Body)
	}
	if st, _ := c.Status(); len(st.Files) != 0 {
		t.Fatalf("handed on with nobody else in it: the file is free, got %+v", st.Files)
	}
	if rr := mutate(t, s, "/api/coordination-act", `{"release":"`+file+`"}`); rr.Code != http.StatusConflict {
		t.Fatalf("nothing to hand on: want 409, got %d", rr.Code)
	}

	req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1/api/coordination", nil)
	get := httptest.NewRecorder()
	s.Handler().ServeHTTP(get, req)
	var d struct {
		Activity []string `json:"activity"`
	}
	json.Unmarshal(get.Body.Bytes(), &d)
	if len(d.Activity) < 2 || !strings.Contains(d.Activity[0], "released") || !strings.Contains(strings.Join(d.Activity, "\n"), "message from the dashboard to bbbb2222") {
		t.Fatalf("activity, newest first: %q", d.Activity)
	}
}
