package admin

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/andrewbakercloudscale/claude-burst/internal/config"
	"github.com/andrewbakercloudscale/claude-burst/internal/coord"
)

// Session coordination: the dashboard switch, its two timings, and who is
// master of which file right now. The work itself is done by hooks
// (internal/coord); this only installs them and reads their state.

const maxCoordMinutes = 240

// Coordinator reads the coordination state the hooks keep.
func Coordinator(cfg config.Config) (*coord.Coordinator, error) {
	dir := os.Getenv("CLAUDE_BURST_COORD_DIR")
	if dir == "" {
		cd, err := config.ConfigDir()
		if err != nil {
			return nil, err
		}
		dir = filepath.Join(cd, "coord")
	}
	exe, _ := os.Executable()
	c := cfg.SessionCoordination.Resolved()
	return &coord.Coordinator{Dir: dir, Exe: exe, Settings: coord.Settings{
		MasterIdle: time.Duration(c.MasterIdleMinutes) * time.Minute,
		Nudge:      time.Duration(c.NudgeMinutes) * time.Minute,
	}.Resolved()}, nil
}

// SyncCoordinationHooks installs the hooks while coordination is on and
// removes them when off. They run this binary.
func SyncCoordinationHooks(cfg config.Config) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if r, err := filepath.EvalSymlinks(exe); err == nil {
		exe = r
	}
	return coord.SyncHooks(cfg.SessionCoordination.Enabled, exe)
}

func (s *Server) handleCoordination(w http.ResponseWriter, r *http.Request) {
	cfg, err := config.Load()
	if err != nil {
		http.Error(w, "config.json does not parse, fix it before changing this: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if r.Method == http.MethodPost {
		var req config.CoordinationConfig
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad request body", http.StatusBadRequest)
			return
		}
		if req.MasterIdleMinutes < 0 || req.MasterIdleMinutes > maxCoordMinutes || req.NudgeMinutes < 0 || req.NudgeMinutes > maxCoordMinutes {
			http.Error(w, fmt.Sprintf("minutes must be between 1 and %d", maxCoordMinutes), http.StatusBadRequest)
			return
		}
		cfg.SessionCoordination = req
		if err := config.Save(cfg); err != nil {
			http.Error(w, "saving config.json: "+err.Error(), http.StatusInternalServerError)
			return
		}
		msg := "session coordination off; its hooks are removed from ~/.claude/settings.json"
		if req.Enabled {
			res := req.Resolved()
			msg = fmt.Sprintf("session coordination on, from every session's next tool call: an idle master hands its files on after %d minutes, and is nudged to commit others' changes after %d", res.MasterIdleMinutes, res.NudgeMinutes)
		}
		if err := SyncCoordinationHooks(cfg); err != nil {
			http.Error(w, "saved, but the hooks could not be updated: "+err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, map[string]string{"ok": msg})
		return
	}
	c, err := Coordinator(cfg)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	resp := struct {
		Config    config.CoordinationConfig `json:"config"`
		Resolved  config.CoordinationConfig `json:"resolved"`
		Installed bool                      `json:"installed"`
		Status    coord.Status              `json:"status"`
		Error     string                    `json:"error,omitempty"`
	}{Config: cfg.SessionCoordination, Resolved: cfg.SessionCoordination.Resolved(), Installed: coord.Installed()}
	if st, err := c.Status(); err != nil {
		resp.Error = err.Error()
	} else {
		resp.Status = st
	}
	writeJSON(w, resp)
}
