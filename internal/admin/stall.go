package admin

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
)

// Stall is the gateway's last freeze, as its memory watcher saw it: the
// session mod reads it in /api/mod to say why it heard nothing for a while.
type Stall struct {
	EndedMS    int64   `json:"ended_ms"`
	Seconds    float64 `json:"seconds"`
	SwapUsedMB uint64  `json:"swap_used_mb,omitempty"`
	SwapOfMB   uint64  `json:"swap_of_mb,omitempty"`
	Pressure   string  `json:"pressure,omitempty"`
}

// stallKept is how long after a freeze /api/mod still reports it.
const stallKept = 10 * time.Minute

var lastStall atomic.Pointer[Stall]

// NoteStall records a freeze the memory watcher saw.
func NoteStall(st Stall) { lastStall.Store(&st) }

// recentStall is the last freeze when it ended within stallKept of now.
func recentStall(now time.Time) *Stall {
	st := lastStall.Load()
	if st == nil || now.UnixMilli()-st.EndedMS > stallKept.Milliseconds() {
		return nil
	}
	return st
}

// modDown is what a session's mod reports once the dashboard answers again
// after it toasted that it did not: the toast itself could not be logged,
// since the gateway it would have told was the thing not answering.
type modDown struct {
	Session  string `json:"session"`
	Shown    string `json:"shown"`    // the toast's text
	FiredMS  int64  `json:"fired_ms"` // when it was shown
	FirstMS  int64  `json:"first_ms"` // when the first check went unanswered
	Misses   int    `json:"misses"`   // checks in a row without an answer
	Error    string `json:"error"`    // what the last of them said
	BackMS   int64  `json:"back_ms"`  // when the dashboard answered again
	Freezing bool   `json:"freezing"` // its error was a timeout, not a refusal
}

// handleModDown writes the mod's report to the log, beside the gateway's own
// freeze lines, so a toast in a session can be matched to what caused it.
func (s *Server) handleModDown(w http.ResponseWriter, r *http.Request) {
	var d modDown
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&d); err != nil || d.FiredMS <= 0 {
		http.Error(w, "need fired_ms", http.StatusBadRequest)
		return
	}
	log.Print(modDownLine(d, recentStall(time.Now())))
	writeJSON(w, map[string]bool{"ok": true})
}

// modDownLine is the log line for d, with the gateway's own last freeze.
func modDownLine(d modDown, st *Stall) string {
	at := func(ms int64) string { return time.UnixMilli(ms).Format("15:04:05") }
	clip := func(v string, n int) string {
		v = strings.Join(strings.Fields(v), " ")
		if len(v) > n {
			v = v[:n]
		}
		return v
	}
	sid := clip(d.Session, 64)
	if sid == "" {
		sid = "?"
	}
	line := fmt.Sprintf("WARNING mod: session %s showed %q at %s after %d checks in a row went unanswered from %s (%s, last error: %s)",
		sid, clip(d.Shown, 80), at(d.FiredMS), d.Misses, at(d.FirstMS),
		map[bool]string{true: "timed out: the gateway was there but did not answer", false: "refused or failed"}[d.Freezing],
		clip(d.Error, 200))
	if d.BackMS > 0 && d.FirstMS > 0 {
		line += fmt.Sprintf("; answered again at %s, %s after the first miss", at(d.BackMS),
			(time.Duration(d.BackMS-d.FirstMS) * time.Millisecond).Round(time.Second))
	}
	if st != nil {
		line += fmt.Sprintf("; the gateway's last freeze: %s ending %s", time.Duration(st.Seconds*float64(time.Second)).Round(time.Second), at(st.EndedMS))
		if st.SwapOfMB > 0 {
			line += fmt.Sprintf(", swap %dMB of %dMB", st.SwapUsedMB, st.SwapOfMB)
		}
		if st.Pressure != "" {
			line += ", memory pressure " + st.Pressure
		}
	} else {
		line += "; the gateway noticed no freeze of its own in the last 10 minutes"
	}
	return line
}
