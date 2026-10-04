package admin

import (
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/andrewbakercloudscale/claude-burst/internal/config"
)

// handleCodexAlertTest leaves a test request for the support console, which
// draws alerts over Codex (CodexAlerts).
func (s *Server) handleCodexAlertTest(w http.ResponseWriter, r *http.Request) {
	dir, err := config.ConfigDir()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if home, err := os.UserHomeDir(); err != nil || !fileExists(filepath.Join(home, ".local", "bin", "claude-panel-overlay")) {
		http.Error(w, "the usage panel is not installed: its overlay draws the alerts, over Codex as over Claude Code", http.StatusConflict)
		return
	}
	if err := writeCodexAlertTest(CodexAlertTestPath(dir), CodexAlertTest{Requested: time.Now()}); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]string{"detail": "Switch to Codex (the ChatGPT app) within 2 minutes: a test alert appears over its window."})
}

// handleCodexAlertTestStatus: {"state": "waiting"|"shown"|"failed"|"none", "detail"}.
func (s *Server) handleCodexAlertTestStatus(w http.ResponseWriter, r *http.Request) {
	dir, err := config.ConfigDir()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	t, ok := readCodexAlertTest(CodexAlertTestPath(dir))
	switch {
	case !ok:
		writeJSON(w, map[string]string{"state": "none"})
	case !t.Shown.IsZero():
		writeJSON(w, map[string]string{"state": "shown",
			"detail": "Shown over Codex at " + t.Shown.Format("15:04:05") + ". If you did not see it, update the usage panel (its overlay draws it) and try again."})
	case t.Error != "":
		writeJSON(w, map[string]string{"state": "failed", "detail": t.Error})
	case time.Since(t.Requested) > codexAlertTestWait+10*time.Second:
		writeJSON(w, map[string]string{"state": "failed",
			"detail": "Nothing answered the request. The support console draws these alerts: check it is running at http://" + config.DefaultConsoleListen + "."})
	default:
		writeJSON(w, map[string]string{"state": "waiting", "detail": "Waiting for Codex to come to the front..."})
	}
}
