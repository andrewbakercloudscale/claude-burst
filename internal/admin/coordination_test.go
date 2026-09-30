package admin

import (
	"encoding/json"
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
