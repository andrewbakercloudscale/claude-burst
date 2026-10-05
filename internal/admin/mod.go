package admin

import (
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/andrewbakercloudscale/claude-burst/internal/metrics"
	"github.com/andrewbakercloudscale/claude-burst/internal/notice"
	"github.com/andrewbakercloudscale/claude-burst/internal/router"
)

// modResponse is what the Claude Code mod (mods/burst) reads every few
// seconds for the band above the prompt: one small answer, so the mod never
// parses /api/state, which reads transcripts and grows with every feature.
type modResponse struct {
	Version  string `json:"version"`
	Route    string `json:"route"` // PRIMARY or SECONDARY
	Overflow bool   `json:"overflow"`
	Reason   string `json:"reason,omitempty"`
	// PrimaryFailing is how many transport failures since Anthropic last
	// answered; 0 is healthy.
	PrimaryFailing int     `json:"primary_failing"`
	TodayUSD       float64 `json:"today_usd"`
	TodayRequests  int     `json:"today_requests"`
	// Session is this session's compaction row, absent until the gateway has
	// seen a request from it.
	Session *router.CompactionSession `json:"session,omitempty"`
	// Alerts are events newer than ?since= for this session or for nobody;
	// another session's own events stay with that session.
	Alerts []notice.Event `json:"alerts"`
	// Problems are the warnings and errors still standing, newest first:
	// the band shows the first as a line until its "back" event clears it.
	Problems []notice.Event `json:"problems"`
	// Toasts is the dashboard's "alerts and compaction lines as toasts in
	// the session" option, on unless it was turned off.
	Toasts bool `json:"toasts"`
}

func (s *Server) handleMod(w http.ResponseWriter, r *http.Request) {
	sid := r.URL.Query().Get("session")
	since, _ := strconv.ParseInt(r.URL.Query().Get("since"), 10, 64)

	st := s.gateway.Status()
	resp := modResponse{Version: s.version, Route: "PRIMARY", Alerts: []notice.Event{}, Problems: []notice.Event{},
		Toasts: readModSettings().Toasts}
	if st.OverflowUntil > time.Now().Unix() {
		resp.Route, resp.Overflow = "SECONDARY", true
		resp.Reason = st.LastReason
	}
	resp.PrimaryFailing = s.gateway.Health().Failures
	if today, err := metrics.Summarize(s.metricsPath, time.Now().Add(-24*time.Hour)); err == nil {
		resp.TodayUSD, resp.TodayRequests = today.APIEquivalentUSD, today.Requests
	}
	if sid != "" {
		// Largest context first, so the first row is the main conversation
		// and not one of its subagents.
		for _, cs := range s.gateway.CompactionSessions() {
			if cs.Session == sid {
				cs := cs
				resp.Session = &cs
				break
			}
		}
	}
	if p := notice.Default(); p != nil {
		evs, _ := notice.Read(p.Path())
		for _, e := range evs {
			if e.TS > since && (e.Session == "" || e.Session == sid) {
				resp.Alerts = append(resp.Alerts, e)
			}
		}
		resp.Problems = standingProblems(evs, sid, time.Now())

	}
	writeJSON(w, resp)
}

// standingProblems are the warnings and errors no later event of the same
// kind has cleared, newest first. A problem older than a day is dropped: a
// gateway that never saw the matching "back" event must not leave a line
// in the band forever.
func standingProblems(evs []notice.Event, sid string, now time.Time) []notice.Event {
	last := map[string]notice.Event{}
	order := []string{}
	for _, e := range evs {
		if e.Session != "" && e.Session != sid {
			continue
		}
		// What Claude Code holds beyond what Burst sends was an alert until
		// 5 Oct 2026. It is the tool working: one left in notices.json by an
		// older gateway is not a problem.
		if e.Kind == "exposure" {
			continue
		}
		if _, seen := last[e.Kind]; !seen {
			order = append(order, e.Kind)
		}
		last[e.Kind] = e
	}
	out := []notice.Event{}
	for _, k := range order {
		e := last[k]
		if (e.Severity == notice.Warn || e.Severity == notice.Error) && now.Sub(e.At) < 24*time.Hour {
			out = append(out, e)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].TS > out[j].TS })
	return out
}
