package admin

import (
	"os"

	"github.com/andrewbakercloudscale/claude-burst/internal/coord"
	"regexp"
	"strings"
	"time"
)

// Session coordination metrics, counted from coord.log. The log is the
// only record coordination keeps, so the counts reach back to the day it
// was first switched on, and nothing extra is written by the hooks.

// coordKinds are the counted events, in the order the dashboard shows
// them. Coordinating is shared+refused+taken+passed+asked: the events that
// only happen when two sessions meet over a file or a repository.
var coordKinds = []struct {
	key string
	re  *regexp.Regexp
}{
	{"shared", regexp.MustCompile(`^share `)},
	{"refused", regexp.MustCompile(`^refuse `)},
	{"taken", regexp.MustCompile(`^master of .* taken over by `)},
	{"passed", regexp.MustCompile(`^master of .* passes from `)},
	{"asked", regexp.MustCompile(`^\S+ asks \S+ for `)},
	{"inherited", regexp.MustCompile(`^\S+ masters .*, which already had uncommitted changes$`)},
	{"held", regexp.MustCompile(`^hold \S+ to commit `)},
	{"stopped", regexp.MustCompile(`^\S+ stopped with uncommitted `)},
	{"errors", regexp.MustCompile(`^(ERROR|PANIC) in `)},
	{"released", regexp.MustCompile(`^free `)},
}

var (
	coordLineRe    = regexp.MustCompile(`^(\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2}) (.*)$`)
	coordStoppedRe = regexp.MustCompile(`^(\S+) stopped with uncommitted (.*)$`)
	coordFreeRe    = regexp.MustCompile(`^free (.*?) (?:from \S+ )?\(`)
	coordPassRe    = regexp.MustCompile(`^master of (.*) (?:passes from|taken over by) `)
)

// coordClearedLine is logged by the dashboard's Clear button: every hook
// error before it counts as dealt with.
const coordClearedLine = "hook errors cleared from the dashboard"

type coordDay struct {
	Day    string         `json:"day"` // YYYY-MM-DD, local
	Counts map[string]int `json:"counts"`
}

// coordIssue is something that went wrong: a hook error or panic (the
// edit went ahead uncoordinated), or a session that stopped with files it
// masters still uncommitted. Resolved is set once every one of those
// files was later committed or handed to another session.
type coordIssue struct {
	At       string   `json:"at"`
	Kind     string   `json:"kind"` // "error" or "stopped"
	Text     string   `json:"text"`
	Session  string   `json:"session,omitempty"`
	Files    []string `json:"files,omitempty"`
	Pending  []string `json:"pending,omitempty"`
	Resolved bool     `json:"resolved"`
}

type coordMetrics struct {
	Days       int            `json:"days"`
	Since      string         `json:"since,omitempty"` // first line in the log
	Totals     map[string]int `json:"totals"`
	AllTime    map[string]int `json:"all_time"`
	PerDay     []coordDay     `json:"per_day"`
	Issues     []coordIssue   `json:"issues"`
	Unresolved int            `json:"unresolved"` // stopped issues still pending, and errors in the last 24 hours
}

// coordStats counts coord.log over the last days days (today included),
// in local time.
func coordStats(path string, days int, now time.Time) coordMetrics {
	if days < 1 {
		days = 1
	}
	m := coordMetrics{Days: days, Totals: map[string]int{}, AllTime: map[string]int{}}
	for _, k := range coordKinds {
		m.Totals[k.key], m.AllTime[k.key] = 0, 0
	}
	start := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location()).AddDate(0, 0, -(days - 1))
	index := map[string]int{}
	for i := 0; i < days; i++ {
		d := start.AddDate(0, 0, i).Format("2006-01-02")
		index[d] = i
		m.PerDay = append(m.PerDay, coordDay{Day: d, Counts: map[string]int{}})
	}

	b, err := os.ReadFile(path)
	if err != nil {
		return m
	}
	// Open "stopped" issues by file, so a later commit or hand-on of the
	// file resolves them. Issues before the window still resolve: only
	// those inside it are shown.
	var all []*coordIssue
	open := map[string][]*coordIssue{}
	resolve := func(p string) {
		for _, is := range open[p] {
			is.Pending = without(is.Pending, p)
			is.Resolved = len(is.Pending) == 0
		}
		delete(open, p)
	}
	for _, line := range strings.Split(string(b), "\n") {
		lm := coordLineRe.FindStringSubmatch(line)
		if lm == nil {
			continue
		}
		at, msg := lm[1], lm[2]
		if m.Since == "" {
			m.Since = at
		}
		if msg == coordClearedLine {
			for _, is := range all {
				if is.Kind == "error" {
					is.Resolved = true
				}
			}
			continue
		}
		t, err := time.ParseInLocation("2006-01-02 15:04:05", at, now.Location())
		if err != nil {
			continue
		}
		inWindow := !t.Before(start)
		kind := ""
		for _, k := range coordKinds {
			if k.re.MatchString(msg) {
				kind = k.key
				break
			}
		}
		if kind == "" {
			continue
		}
		m.AllTime[kind]++
		if inWindow {
			m.Totals[kind]++
			if i, ok := index[at[:10]]; ok {
				m.PerDay[i].Counts[kind]++
			}
		}
		switch kind {
		case "errors":
			all = append(all, &coordIssue{At: at, Kind: "error", Text: msg})
		case "stopped":
			sm := coordStoppedRe.FindStringSubmatch(msg)
			files := strings.Split(sm[2], ", ")
			is := &coordIssue{At: at, Kind: "stopped", Text: msg, Session: sm[1], Files: files, Pending: append([]string(nil), files...)}
			all = append(all, is)
			for _, p := range files {
				open[p] = append(open[p], is)
			}
		case "released":
			if fm := coordFreeRe.FindStringSubmatch(msg); fm != nil {
				resolve(fm[1])
			}
		case "passed", "taken":
			if pm := coordPassRe.FindStringSubmatch(msg); pm != nil {
				resolve(pm[1])
			}
		}
	}
	// A file can leave tracking without a log line (committed while no hook
	// ran, or its worktree merged and removed), so an issue still pending
	// asks git: whatever has nothing uncommitted any more is settled.
	for _, is := range all {
		if is.Kind != "stopped" || is.Resolved {
			continue
		}
		var still []string
		for _, p := range is.Pending {
			if uncommitted(p) {
				still = append(still, p)
			}
		}
		is.Pending, is.Resolved = still, len(still) == 0
	}
	// Newest first, only those inside the window.
	for i := len(all) - 1; i >= 0; i-- {
		is := all[i]
		t, err := time.ParseInLocation("2006-01-02 15:04:05", is.At, now.Location())
		if err != nil || t.Before(start) {
			continue
		}
		if is.Kind == "error" && is.Resolved {
			continue // cleared: off the list, still in the counts
		}
		// An error is resolved only by the Clear button, so it also stops
		// counting as open after a day: long enough to be seen, short
		// enough not to stick.
		if !is.Resolved && (is.Kind != "error" || now.Sub(t) < 24*time.Hour) {
			m.Unresolved++
		}
		m.Issues = append(m.Issues, *is)
	}
	return m
}

// uncommitted asks git; a variable so tests can answer for it.
var uncommitted = coord.Uncommitted

func without(list []string, s string) []string {
	var out []string
	for _, x := range list {
		if x != s {
			out = append(out, x)
		}
	}
	return out
}
