package admin

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/andrewbakercloudscale/claude-burst/internal/handover"
)

// The "Session handover" section: the SessionStart/SessionEnd hooks that
// brief Claude from HANDOFF.md and write it back when a session closes.
// See internal/handover.

func (s *Server) handleHandover(w http.ResponseWriter, r *http.Request) {
	var req handover.Config
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request body", http.StatusBadRequest)
		return
	}
	if _, err := req.Normalize(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := handover.Save(req); err != nil {
		http.Error(w, "saving handover settings: "+err.Error(), http.StatusInternalServerError)
		return
	}
	msg := "saved; applies to the next session that starts or ends, nothing to restart"
	if !handover.GetStatus().Installed {
		msg = "saved; the hooks are not installed, so nothing uses these settings until you install them"
	}
	writeJSON(w, map[string]string{"ok": msg})
}

func (s *Server) handleHandoverInstall(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Install bool `json:"install"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request body", http.StatusBadRequest)
		return
	}
	var err error
	msg := "hooks installed in ~/.claude/settings.json; new sessions are briefed and closed sessions write HANDOFF.md"
	if req.Install {
		err = handover.Install()
	} else {
		err = handover.Uninstall()
		msg = "hooks removed from ~/.claude/settings.json; your settings and the log are kept for a reinstall"
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]string{"ok": msg})
}

// handleHandoverAudit lists every HANDOFF.md the writer has changed.
func (s *Server) handleHandoverAudit(w http.ResponseWriter, r *http.Request) {
	entries, err := handover.Audit()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if entries == nil {
		entries = []handover.AuditEntry{}
	}
	writeJSON(w, entries)
}

// handleHandoverFile returns one audited HANDOFF.md. The root must be in the
// audit, so this cannot be used to read any other file.
func (s *Server) handleHandoverFile(w http.ResponseWriter, r *http.Request) {
	body, err := handover.ReadAudited(r.URL.Query().Get("root"))
	switch {
	case errors.Is(err, handover.ErrNotAudited):
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	case err != nil:
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]string{"content": body})
}

// handleHandoverDelete deletes every audited HANDOFF.md, after backing each
// one up. The body must say {"confirm": "delete"}: a stray POST deletes
// nothing.
func (s *Server) handleHandoverDelete(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Confirm string `json:"confirm"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Confirm != "delete" {
		http.Error(w, `body must be {"confirm":"delete"}`, http.StatusBadRequest)
		return
	}
	backup, results, err := handover.DeleteAudited()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"backup": backup, "results": results})
}
