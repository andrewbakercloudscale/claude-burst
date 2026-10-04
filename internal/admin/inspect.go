package admin

import (
	"net/http"
	"strconv"

	"github.com/andrewbakercloudscale/claude-burst/internal/router"
)

// handleInspect: with no session, the sessions the inspector can show; with
// one, that session's context item by item. The content lives in the
// gateway's memory only and is served only here, on this Mac.
func (s *Server) handleInspect(w http.ResponseWriter, r *http.Request) {
	sid := r.URL.Query().Get("session")
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

// inspectItemMax caps one item's full text: a huge tool result is shown
// head first, and the dashboard says it was cut.
const inspectItemMax = 512 << 10

func (s *Server) handleInspectItem(w http.ResponseWriter, r *http.Request) {
	i, err := strconv.Atoi(r.URL.Query().Get("i"))
	if err != nil {
		http.Error(w, "bad item", http.StatusBadRequest)
		return
	}
	text, ok := s.gateway.InspectItem(r.URL.Query().Get("session"), i)
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
