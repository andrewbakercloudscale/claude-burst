package admin

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/andrewbakercloudscale/claude-burst/internal/config"
	"github.com/andrewbakercloudscale/claude-burst/internal/looseends"
	"github.com/andrewbakercloudscale/claude-burst/internal/notice"
)

// Loose ends: uncommitted files, unpushed commits and stashes in every
// repository a Claude Code session has worked in lately, with the session
// coordination knows left each file. It only reads; what to do with
// someone's half-finished work is the user's call.

// looseWithin is how far back a transcript names a repository to look at.
const looseWithin = 30 * 24 * time.Hour

// looseStale is the age at which a loose end is worth a daily reminder.
const looseStale = 24 * time.Hour

// LooseEnds scans the repositories sessions have worked in.
func LooseEnds(cfg config.Config, now time.Time) looseends.Report {
	var owners map[string]looseends.Owner
	if c, err := Coordinator(cfg); err == nil {
		owners = c.Owners()
	}
	var extra []string
	for p := range owners {
		extra = append(extra, filepath.Dir(p))
	}
	return looseends.Scan(looseends.Roots(claudeProjectsDir(), looseWithin, extra, now), owners, now)
}

func claudeProjectsDir() string {
	base := os.Getenv("CLAUDE_CONFIG_DIR")
	if base == "" {
		home, _ := os.UserHomeDir()
		base = filepath.Join(home, ".claude")
	}
	return filepath.Join(base, "projects")
}

// looseSummary is "3 uncommitted files, 2 unpushed commits, 1 stash", or "".
func looseSummary(files, commits, stashes int) string {
	var parts []string
	add := func(n int, one, many string) {
		if n == 1 {
			parts = append(parts, "1 "+one)
		} else if n > 1 {
			parts = append(parts, fmt.Sprintf("%d %s", n, many))
		}
	}
	add(files, "uncommitted file", "uncommitted files")
	add(commits, "unpushed commit", "unpushed commits")
	add(stashes, "stash", "stashes")
	return strings.Join(parts, ", ")
}

// staleCounts counts what in rp is older than looseStale, and names the
// repositories it is in.
func staleCounts(rp looseends.Report, now time.Time) (files, commits, stashes int, repos []string) {
	cut := float64(now.Add(-looseStale).Unix())
	old := func(t float64) bool { return t > 0 && t < cut }
	for _, r := range rp.Repos {
		n := 0
		for _, f := range r.Uncommitted {
			at := f.MTime
			if f.Owner != nil {
				if f.Owner.Live {
					continue // someone is working on it right now
				}
				at = f.Owner.Since
			}
			if old(at) {
				files++
				n++
			}
		}
		for _, c := range r.Unpushed {
			if old(c.When) {
				commits++
				n++
			}
		}
		for _, s := range r.Stashes {
			if old(s.When) {
				stashes++
				n++
			}
		}
		if n > 0 {
			repos = append(repos, filepath.Base(r.Root))
		}
	}
	return
}

func (s *Server) handleLooseEnds(w http.ResponseWriter, r *http.Request) {
	cfg, err := config.Load()
	if err != nil {
		http.Error(w, "config.json: "+err.Error(), http.StatusInternalServerError)
		return
	}
	rp, _ := cachedScan("loose-ends", func() (looseends.Report, error) { return LooseEnds(cfg, time.Now()), nil })
	f, c, st, _ := staleCounts(rp, time.Now())
	writeJSON(w, map[string]any{"report": rp, "stale": map[string]int{"files": f, "commits": c, "stashes": st},
		"stale_hours": int(looseStale.Hours()), "within_days": int(looseWithin.Hours() / 24)})
}

// looseNudgeHour is when, local time, the daily reminder may first go up.
const looseNudgeHour = 9

// looseNudge puts up one reminder a day, from 09:00, when loose ends older
// than a day are lying about. The scan runs off the notifier's round.
func (n *notifier) looseNudge(cfg config.Config, now time.Time) {
	day := now.Format("2 Jan")
	if now.Hour() < looseNudgeHour || n.looseDay == day || notice.Default() == nil {
		return
	}
	n.looseDay = day
	go looseRemind(looseScan, cfg, now)
}

// looseScan is LooseEnds; tests stand in for it.
var looseScan = LooseEnds

func looseRemind(scan func(config.Config, time.Time) looseends.Report, cfg config.Config, now time.Time) {
	f, c, st, repos := staleCounts(scan(cfg, now), now)
	if f+c+st == 0 {
		return
	}
	if len(repos) > 4 {
		repos = append(repos[:4], fmt.Sprintf("%d more", len(repos)-4))
	}
	notice.PublishOnce("loose-ends-daily", notice.Warn, fmt.Sprintf("Loose ends older than a day (%s): %s", now.Format("2 Jan"), looseSummary(f, c, st)),
		"In "+strings.Join(repos, ", ")+". See Loose ends in the dashboard; nothing is committed, pushed or deleted for you.")
}
