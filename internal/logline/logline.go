// Package logline is what every line of the gateway's text log has in
// common: when (local time with its zone, to the millisecond), how bad
// (level=info, warn or error), and no URL query strings.
//
// It is a writer under the logger and not a new logging call, on purpose:
// the 127 places that log keep saying what happened in their own words, and
// one place decides the three things a reader filters on. Until 7 Oct 2026
// a line began "2026/10/07 10:01:51" with no zone, in one of four formats
// across the log files, and severity was spelled five ways or not at all,
// so "show me what went wrong" had no query.
package logline

import (
	"io"
	"regexp"
	"strings"
	"time"
)

// Stamp is the layout every line starts with. Local time, with the zone:
// a pasted line says what clock it is on, and it sorts and joins with the
// RFC 3339 times in metrics.jsonl and audit.jsonl.
const Stamp = "2006-01-02T15:04:05.000-07:00"

// OldStamp is the layout lines had before, still in the rotated files.
const OldStamp = "2006/01/02 15:04:05"

// Levels.
const (
	Info  = "info"
	Warn  = "warn"
	Error = "error"
)

// errorMarks and warnMarks are how the call sites already say how bad a
// line is, and the events worth finding among the requests: a failover, a
// window opening or closing, the network going, a start after a crash.
var (
	errorMarks = []string{"PANIC", "FATAL", "error stage=", " crash", "deadlock", "http: panic", "codex: refused a connection"}
	warnMarks  = []string{"warn stage=", "WARNING", " failover route=", " rejected until ", "no_failover", "released outage window",
		"network-snapshot", " REJECTED ", "compaction dropped", "could not ", " failed", "upstream_error"}
)

// Level is how bad a log line is, from its own words.
func Level(line string) string {
	for _, m := range errorMarks {
		if strings.Contains(line, m) {
			return Error
		}
	}
	for _, m := range warnMarks {
		if strings.Contains(line, m) {
			return Warn
		}
	}
	return Info
}

// query is the query string of an http(s) URL inside a line.
var query = regexp.MustCompile(`(https?://[^\s"'?\\]+)\?[^\s"'\\]*`)

// StripQueries removes the query string of every URL in s. Go's network
// errors quote the whole URL, so a DNS-over-HTTPS lookup put the name it
// resolved in the log, and an update check its device id: 19,920 such
// lines on 7 Oct 2026. The host and path say where the request went.
func StripQueries(s string) string {
	if !strings.Contains(s, "?") {
		return s
	}
	return query.ReplaceAllString(s, "$1")
}

// Writer puts the stamp and the level before each line written to it and
// strips URL queries. log.Logger writes one whole line a call, which is
// what this relies on; the logger above it has no flags of its own.
type Writer struct {
	W   io.Writer
	Now func() time.Time // time.Now when nil
}

func (w *Writer) Write(p []byte) (int, error) {
	now := time.Now
	if w.Now != nil {
		now = w.Now
	}
	line := StripQueries(string(p))
	out := now().Format(Stamp) + " level=" + Level(line) + " " + line
	if _, err := io.WriteString(w.W, out); err != nil {
		return 0, err
	}
	return len(p), nil
}

// ParseStamp reads the time a log line starts with, in either layout, and
// reports whether it had one.
func ParseStamp(line string) (time.Time, bool) {
	if len(line) >= len(Stamp) {
		if t, err := time.Parse(Stamp, line[:len(Stamp)]); err == nil {
			return t, true
		}
	}
	if len(line) >= len(OldStamp) {
		if t, err := time.ParseInLocation(OldStamp, line[:len(OldStamp)], time.Local); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}
