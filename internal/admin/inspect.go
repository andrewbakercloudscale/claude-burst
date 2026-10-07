package admin

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/andrewbakercloudscale/claude-burst/internal/codex"
	"github.com/andrewbakercloudscale/claude-burst/internal/config"
	"github.com/andrewbakercloudscale/claude-burst/internal/ctxview"
	"github.com/andrewbakercloudscale/claude-burst/internal/metrics"
	"github.com/andrewbakercloudscale/claude-burst/internal/router"
)

// The context inspector, for Claude Code (engine "claude", the default) and
// Codex (engine "codex"). The content lives in the gateway's memory only and
// is served only here, on this Mac.

// handleInspect: with no session, the sessions the inspector can show; with
// one, that session's context item by item.
func (s *Server) handleInspect(w http.ResponseWriter, r *http.Request) {
	sid := r.URL.Query().Get("session")
	if r.URL.Query().Get("engine") == "codex" {
		if s.codex == nil {
			writeJSON(w, []codex.InspectSession{})
			return
		}
		if sid == "" {
			writeJSON(w, s.codex.InspectSessions())
			return
		}
		rep := s.codex.InspectContext(sid, codexContextOf(sid))
		if rep == nil {
			http.Error(w, "no Codex turn from that session since the gateway started", http.StatusNotFound)
			return
		}
		writeJSON(w, rep)
		return
	}
	if sid == "" {
		list := s.gateway.InspectSessions()
		if list == nil {
			list = []router.InspectSession{}
		}
		writeJSON(w, list)
		return
	}
	rep := s.gateway.InspectContext(sid)
	if rep == nil {
		http.Error(w, "no request from that session since the gateway started", http.StatusNotFound)
		return
	}
	writeJSON(w, rep)
}

// codexContextOf is a Codex session's size as ChatGPT last reported it.
func codexContextOf(sid string) int64 {
	mp, err := config.CodexMetricsPath()
	if err != nil {
		return 0
	}
	evs, err := metrics.Recent(mp, 2000)
	if err != nil {
		return 0
	}
	for _, cs := range codexSessions(evs, nil) {
		if cs.ID == sid {
			return cs.Context
		}
	}
	return 0
}

// inspectItemMax caps one item's full text: a huge tool result is shown
// head first, and the dashboard says it was cut.
const inspectItemMax = 512 << 10

func (s *Server) handleInspectItem(w http.ResponseWriter, r *http.Request) {
	i, err := strconv.Atoi(r.URL.Query().Get("i"))
	if err != nil {
		http.Error(w, "bad item", http.StatusBadRequest)
		return
	}
	sid := r.URL.Query().Get("session")
	var text string
	var ok bool
	if r.URL.Query().Get("engine") == "codex" {
		if s.codex != nil {
			text, ok = s.codex.InspectItem(sid, i)
		}
	} else {
		text, ok = s.gateway.InspectItem(sid, i)
	}
	if !ok {
		http.Error(w, "no such item: the session may have moved on; reload", http.StatusNotFound)
		return
	}
	if len(text) > inspectItemMax {
		text = text[:inspectItemMax] + "\n\n[cut here: " + strconv.Itoa(len(text)) + " bytes in all]"
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(text))
}

// handleInspectRemove removes an item from a session's context, or restores
// one: {"engine", "session", "id", "restore"}. The item is looked up in the
// session's latest request, so only what the inspector shows as removable
// can be removed.
func (s *Server) handleInspectRemove(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Engine  string `json:"engine"`
		Session string `json:"session"`
		ID      string `json:"id"`
		Restore bool   `json:"restore"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Session == "" || req.ID == "" {
		http.Error(w, "need engine, session and id", http.StatusBadRequest)
		return
	}
	var store *ctxview.Store
	var key string
	var items []ctxview.Item
	if req.Engine == "codex" {
		if s.codex == nil {
			http.Error(w, "the Codex gateway is not running", http.StatusConflict)
			return
		}
		store, key = s.codex.Removals(), codex.RemovalKey(req.Session)
		if rep := s.codex.InspectContext(req.Session, 0); rep != nil {
			items = rep.Items
		}
	} else {
		store, key = s.gateway.Removals(), router.RemovalKey(req.Session)
		if rep := s.gateway.InspectContext(req.Session); rep != nil {
			items = rep.Items
		}
	}
	if req.Restore {
		if err := store.Restore(key, req.ID); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, map[string]string{"detail": "Restored: it goes back into this session's next request."})
		return
	}
	for _, it := range items {
		if it.ID != req.ID || !it.Removable || it.Removed {
			continue
		}
		if err := store.Add(key, ctxview.Removal{ID: it.ID, Group: it.Group, Name: it.Name, Bytes: it.Bytes}); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, map[string]string{"detail": "Removed: this session's next request leaves it out, with a one-line note in its place."})
		return
	}
	http.Error(w, "that item is not in the session's latest request, or cannot be removed; reload", http.StatusNotFound)
}

// handleInspectPrune removes many items from a Claude Code session's context
// at once, chosen by a word: {"session", "what"}, or {"session", "restore":
// true} to put back everything removed. /burst-prune in a session asks.
func (s *Server) handleInspectPrune(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Session string `json:"session"`
		What    string `json:"what"`
		Restore bool   `json:"restore"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Session == "" || (strings.TrimSpace(req.What) == "" && !req.Restore) {
		http.Error(w, "need session and what", http.StatusBadRequest)
		return
	}
	if req.Restore {
		n, err := s.gateway.RestoreContext(req.Session)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		detail := "Nothing was removed from this session's context."
		if n > 0 {
			detail = "Put back " + strconv.Itoa(n) + " items: they go into this session's next request, which writes them to the cache again."
		}
		writeJSON(w, map[string]any{"restored": n, "detail": detail})
		return
	}
	res, err := s.gateway.PruneContext(req.Session, req.What)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if res == nil {
		http.Error(w, "no request from that session since the gateway started", http.StatusNotFound)
		return
	}
	writeJSON(w, res)
}

// handleInspectRefresh has a session's whole conversation asked for with its
// next request, so the inspector shows a session on a message thread as it
// is now: {"session"}. /burst-dump asks when what it shows is behind.
func (s *Server) handleInspectRefresh(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Session string `json:"session"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Session == "" {
		http.Error(w, "need session", http.StatusBadRequest)
		return
	}
	s.gateway.WantHistory(req.Session, "its context was asked for (/burst-dump), and the last request seen whole is behind")
	writeJSON(w, map[string]string{"detail": "The session's next request brings its whole conversation."})
}
