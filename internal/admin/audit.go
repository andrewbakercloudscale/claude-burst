package admin

import (
	"bufio"
	"bytes"
	_ "embed"
	"io"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/andrewbakercloudscale/claude-burst/internal/config"
	"github.com/andrewbakercloudscale/claude-burst/internal/notice"
)

// The audit view: every alert that was shown, every action taken from the
// dashboard or the console, and for any one of them the log lines around
// it. Served by the dashboard and by the console from the same handlers and
// the same script, so a support question gets the same answer from either.

//go:embed audit.js
var auditJS []byte

func handleAuditJS(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(auditJS)
}

func auditPath() string {
	dir, err := config.ConfigDir()
	if err != nil {
		return ""
	}
	return notice.AuditPath(notice.Path(dir))
}

// handleAudit returns the newest audit entries, newest first.
func handleAudit(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	evs := notice.ReadAudit(auditPath(), limit)
	if evs == nil {
		evs = []notice.Event{}
	}
	writeJSON(w, map[string]any{"path": auditPath(), "events": evs})
}

// contextBefore and contextAfter bound the log lines shown for an entry:
// the cause comes before the alert, and a minute or two covers a retry
// ladder.
const (
	contextBefore = 3 * time.Minute
	contextAfter  = 30 * time.Second
	contextMax    = 300
	// contextScan is how much of the log end is read: hours of traffic.
	contextScan = 24 << 20
)

// handleAuditContext returns the log lines around one audit entry, without
// the per-request start and done lines and the cancelled background polls,
// which are most of the log and none of the story.
func handleAuditContext(w http.ResponseWriter, r *http.Request) {
	at, err := strconv.ParseInt(r.URL.Query().Get("at"), 10, 64)
	if err != nil || at <= 0 {
		http.Error(w, "at must be a Unix time", http.StatusBadRequest)
		return
	}
	path, err := config.LogPath()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	lines, err := logAround(path, time.Unix(at, 0), contextBefore, contextAfter)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"path": path, "lines": lines})
}

// logAround reads the log lines stamped within [t-before, t+after], local
// time, skipping noise. Lines from before the log switched to local time
// are UTC and simply fall outside the window.
func logAround(path string, t time.Time, before, after time.Duration) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if st, err := f.Stat(); err == nil && st.Size() > contextScan {
		if _, err := f.Seek(st.Size()-contextScan, io.SeekStart); err != nil {
			return nil, err
		}
	}
	from, to := t.Add(-before), t.Add(after)
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 4<<20)
	out := []string{}
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) < 19 {
			continue
		}
		ts, err := time.ParseInLocation("2006/01/02 15:04:05", string(line[:19]), time.Local)
		if err != nil || ts.Before(from) {
			continue
		}
		if ts.After(to) {
			break
		}
		if logNoise(line) {
			continue
		}
		out = append(out, redactLine(string(line)))
		if len(out) >= contextMax {
			out = append(out, "... more lines in the log file")
			break
		}
	}
	return out, sc.Err()
}

func logNoise(line []byte) bool {
	if bytes.Contains(line, []byte(" start method=")) || bytes.Contains(line, []byte(" done method=")) {
		return true
	}
	// A successful control-plane call (heartbeats, events: no model).
	if bytes.Contains(line, []byte(` ok route=`)) && bytes.Contains(line, []byte(` model="" `)) {
		return true
	}
	// A background poll cancelled by its own client is routine; a cancelled
	// turn is not.
	return bytes.Contains(line, []byte(" client_gone ")) && !bytes.Contains(line, []byte("/v1/messages"))
}

// redactLine keeps credentials out of what the page shows, as diagnose.sh
// does for its report. The log should hold none; this is the backstop.
func redactLine(s string) string { return secretInLog.ReplaceAllString(s, "${1}REDACTED") }

var secretInLog = regexp.MustCompile(`(?i)(bearer\s+|basic\s+|x-api-key:\s*|api[_-]?key[=:]\s*)[^\s",;']+`)

// statusRecorder captures the status a handler wrote, for the audit.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	if r.status == 0 {
		r.status = code
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	return r.ResponseWriter.Write(b)
}

func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// notAnAction are POSTs that hooks and scripts make on every prompt or
// event, not a person choosing something: auditing them would bury the
// actions. /api/alert publishes a notice, which the audit has already.
var notAnAction = map[string]bool{
	"/api/prompt-notice": true,
	"/api/alert":         true,
}

// audited records every change made from a page: who did what is half of
// supporting it. where is "dashboard" or "console".
func audited(where string, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if notAnAction[r.URL.Path] {
			h(w, r)
			return
		}
		rec := &statusRecorder{ResponseWriter: w}
		h(rec, r)
		if rec.status == 0 {
			rec.status = http.StatusOK
		}
		sev, outcome := notice.Info, "done"
		if rec.status >= 400 {
			sev, outcome = notice.Warn, "refused or failed"
		}
		notice.Record("action", sev, strings.ToUpper(where[:1])+where[1:]+": "+r.URL.Path,
			outcome+" (HTTP "+strconv.Itoa(rec.status)+")")
	}
}
