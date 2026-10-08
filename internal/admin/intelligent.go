package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/andrewbakercloudscale/claude-burst/internal/autocompact"
	"github.com/andrewbakercloudscale/claude-burst/internal/config"
	"github.com/andrewbakercloudscale/claude-burst/internal/metrics"
)

// Intelligent Compaction Mode on the dashboard: the learner
// (internal/autocompact) run once a day and on demand, its table, and
// GetAutoCompactionThreshold, which the usage panel asks for the Compact at
// in force in a folder.

// learnEvery is how often the learner looks. It steps a repository's
// threshold once a local day whatever this is; the rest of the day it only
// refreshes the figures the table shows.
const learnEvery = 30 * time.Minute

// learnedPath is the learner's file, beside the gateway's compaction state
// so a test server's stays in its own folder. "" keeps it in memory.
func (s *Server) learnedPath() string {
	p := s.gateway.CompactionOutcomesPath()
	if p == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(p), "intelligent-compaction.json")
}

// learnCompaction reads the window, works out every repository's target,
// steps the thresholds when the intelligent mode is on, and gives the
// running gateway the result.
func (s *Server) learnCompaction(cfg config.Config, now time.Time) autocompact.State {
	s.learnMu.Lock()
	defer s.learnMu.Unlock()
	path := s.learnedPath()
	if s.learned.Repos == nil && path != "" {
		s.learned = autocompact.Load(path)
	}
	c := cfg.PrimaryCompaction.Resolved()
	since := now.Add(-autocompact.Window)
	runs, failed, err := metrics.CompactionRunsSince(s.metricsPath, since)
	if err != nil {
		return s.learned
	}
	// Every size replayed over each repository's own requests, with the
	// delay a real session has between its compactions: what the learner
	// takes a size from where there are requests enough.
	measured := map[string]metrics.StrategyRepo{}
	if sweep, err := metrics.CompactionStrategiesSince(s.metricsPath, since, config.DefaultCompactionCompactAt, time.Duration(c.WindowMinutes)*time.Minute, s.repos.resolve); err == nil {
		for _, r := range sweep.Repos {
			if r.Path != "" {
				measured[r.Path] = r
			}
		}
	}
	st := autocompact.Learn(s.learned, autocompact.Inputs{
		Runs: runs, Failed: failed, Measured: measured,
		Outcomes:  autocompact.ReadOutcomes(s.gateway.CompactionOutcomesPath(), since),
		Resolve:   s.repos.resolve,
		ReadPrice: metrics.CacheReadPrice,
		Now:       now,
	}, autocompact.Bounds{Floor: c.FloorTokens, Ceiling: c.CompactAtTokens, Start: c.StartTokens(), BufferPercent: *c.BufferPercent}, c.Enabled && c.Intelligent())
	s.learned = st
	if path != "" {
		_ = autocompact.Save(path, st)
	}
	s.recordSizes(c, st, now)
	s.gateway.SetLearnedCompaction(st.Thresholds())
	return st
}

// sizesPath is the record of the Compact at in force, beside the learner's
// file. "" keeps it in memory.
func (s *Server) sizesPath() string {
	p := s.learnedPath()
	if p == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(p), "compaction-sizes.jsonl")
}

// recordSizes puts on record the Compact at each repository has from now:
// what the replay is later checked against (metrics.StrategyTrack). c is
// the resolved settings.
func (s *Server) recordSizes(c config.CompactionConfig, st autocompact.State, now time.Time) {
	s.sizesMu.Lock()
	defer s.sizesMu.Unlock()
	if !s.sizesRead {
		if p := s.sizesPath(); p != "" {
			s.sizes = autocompact.ReadSizes(p)
		}
		s.sizesRead = true
	}
	c.Learned = st.Thresholds()
	roots := append([]string{""}, s.sizes.Roots()...)
	for root := range st.Repos {
		roots = append(roots, root)
	}
	for _, o := range c.RepoOverrides {
		if o.Repo != "" {
			roots = append(roots, filepath.Clean(o.Repo))
		}
	}
	inForceNow := map[string]autocompact.SizeChange{}
	for _, root := range roots {
		at, source := inForce(c, root)
		if !c.Enabled {
			at, source = 0, "off"
		}
		inForceNow[root] = autocompact.SizeChange{At: at, Source: source}
	}
	_, _ = s.sizes.Record(s.sizesPath(), now, inForceNow)
}

// sizesOnRecord is the record as it stands, for a replay to read while the
// learner goes on adding to it.
func (s *Server) sizesOnRecord() autocompact.Sizes {
	s.sizesMu.Lock()
	defer s.sizesMu.Unlock()
	return s.sizes.Snapshot()
}

// reseatLearned makes the next learn take every repository straight to its
// target, not a tenth of the way: the settings the targets come from changed.
func (s *Server) reseatLearned() {
	s.learnMu.Lock()
	defer s.learnMu.Unlock()
	if s.learned.Repos == nil {
		if p := s.learnedPath(); p != "" {
			s.learned = autocompact.Load(p)
		}
	}
	s.learned.Reseat()
}

// StartLearner runs the learner until ctx ends.
func (s *Server) StartLearner(ctx context.Context) {
	t := time.NewTicker(learnEvery)
	defer t.Stop()
	for {
		if cfg, err := config.Load(); err == nil {
			s.learnCompaction(cfg, time.Now())
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// learnedRepo is one row of the dashboard's table.
type learnedRepo struct {
	autocompact.Repo
	// InForce is the Compact at this repository's sessions get now, 0 when
	// never, and Source why: learned, override, off, fixed or (in the
	// intelligent mode, with nothing learned yet) start.
	InForce int64  `json:"in_force"`
	Source  string `json:"source"`
}

type learnedView struct {
	Mode    string        `json:"mode"` // "" fixed, "intelligent"
	Enabled bool          `json:"enabled"`
	Fixed   int64         `json:"fixed"`
	Start   int64         `json:"start"` // what a repository with nothing learned compacts at
	Floor   int64         `json:"floor"`
	Buffer  int           `json:"buffer_percent"`
	Delay   int           `json:"delay_minutes"`
	Days    int           `json:"window_days"`
	Repos   []learnedRepo `json:"repos"`
}

// inForce is the Compact at for the repository at root, and why.
func inForce(c config.CompactionConfig, root string) (int64, string) {
	res, o := c.ForRepo(root)
	switch {
	case o == nil && c.Intelligent():
		return res.CompactAtTokens, "start"
	case o == nil:
		return res.CompactAtTokens, "fixed"
	case o.Off:
		return 0, "off"
	case o.Learned:
		return res.CompactAtTokens, "learned"
	}
	return res.CompactAtTokens, "override"
}

func (s *Server) learnedView(cfg config.Config, st autocompact.State) learnedView {
	c := cfg.PrimaryCompaction.Resolved()
	c.Learned = st.Thresholds()
	v := learnedView{Mode: c.Mode, Enabled: c.Enabled, Fixed: c.CompactAtTokens, Start: c.StartTokens(), Floor: c.FloorTokens, Buffer: *c.BufferPercent, Delay: c.WindowMinutes,
		Days: int(autocompact.Window.Hours() / 24), Repos: []learnedRepo{}}
	for _, r := range st.Sorted() {
		row := learnedRepo{Repo: r}
		row.InForce, row.Source = inForce(c, r.Root)
		v.Repos = append(v.Repos, row)
	}
	return v
}

// handleIntelligent is the table (GET) and "learn now" (POST).
func (s *Server) handleIntelligent(w http.ResponseWriter, r *http.Request) {
	cfg, err := config.Load()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if r.Method == http.MethodPost {
		writeJSON(w, s.learnedView(cfg, s.learnCompaction(cfg, time.Now())))
		return
	}
	s.learnMu.Lock()
	st := s.learned
	s.learnMu.Unlock()
	if st.Repos == nil {
		st = s.learnCompaction(cfg, time.Now())
	}
	writeJSON(w, s.learnedView(cfg, st))
}

// thresholdAnswer is GetAutoCompactionThreshold's answer.
type thresholdAnswer struct {
	Folder string `json:"folder"`
	Repo   string `json:"repo,omitempty"`
	Root   string `json:"root,omitempty"`
	// Threshold is the Compact at in force for the folder, in tokens: 0
	// when its sessions are never compacted on their own. Source says where
	// it comes from: learned (intelligent mode), override (the user's own
	// for this repository), fixed (the one Compact at) or off.
	Threshold   int64  `json:"threshold"`
	Source      string `json:"source"`
	Intelligent bool   `json:"intelligent"`
	Enabled     bool   `json:"enabled"`
	Fixed       int64  `json:"fixed"`
	Floor       int64  `json:"floor"`
	// BufferPercent is what a learned threshold has on top of the cheapest
	// size, and DelayMinutes the least time between a session's compactions.
	BufferPercent int `json:"buffer_percent"`
	// Target is where the learner says the threshold should be, 0 when it
	// has too little to go on, and Previous what it was before the last
	// daily step.
	Target       int64                `json:"target,omitempty"`
	Previous     int64                `json:"previous,omitempty"`
	DelayMinutes int                  `json:"delay_minutes"`
	Failures     autocompact.Failures `json:"failures"`
	Reason       string               `json:"reason,omitempty"`
}

// handleThreshold is GetAutoCompactionThreshold: the Compact at in force
// for a folder, named by ?folder= (a full path, anywhere inside the
// repository, or the repository folder's name) or by ?session=.
func (s *Server) handleThreshold(w http.ResponseWriter, r *http.Request) {
	cfg, err := config.Load()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	folder := strings.TrimSpace(r.URL.Query().Get("folder"))
	session := strings.TrimSpace(r.URL.Query().Get("session"))
	if folder == "" && session == "" {
		http.Error(w, "GetAutoCompactionThreshold needs ?folder=<name or full path> or ?session=<id>", http.StatusBadRequest)
		return
	}
	s.learnMu.Lock()
	st := s.learned
	s.learnMu.Unlock()
	if st.Repos == nil {
		st = s.learnCompaction(cfg, time.Now())
	}
	c := cfg.PrimaryCompaction.Resolved()
	c.Learned = st.Thresholds()

	var name, root string
	switch {
	case session != "":
		name, root = s.repos.resolve(session)
	case filepath.IsAbs(folder) || strings.HasPrefix(folder, "~/"):
		if strings.HasPrefix(folder, "~/") {
			if home, err := os.UserHomeDir(); err == nil {
				folder = filepath.Join(home, folder[2:])
			}
		}
		name, root = s.repos.RepoOf(folder)
	default:
		// A bare name: a repository the learner or an override knows.
		name = folder
		for _, known := range st.Sorted() {
			if strings.EqualFold(known.Name, folder) {
				name, root = known.Name, known.Root
				break
			}
		}
		for _, o := range c.RepoOverrides {
			if root == "" && strings.EqualFold(filepath.Base(o.Repo), folder) {
				root = filepath.Clean(o.Repo)
			}
		}
	}
	a := thresholdAnswer{Folder: folder, Repo: name, Root: root, Intelligent: c.Intelligent(), Enabled: c.Enabled,
		Fixed: c.CompactAtTokens, Floor: c.FloorTokens, BufferPercent: *c.BufferPercent, DelayMinutes: c.WindowMinutes}
	if folder == "" {
		a.Folder = name
	}
	a.Threshold, a.Source = inForce(c, root)
	if known := st.Repos[root]; known != nil {
		a.Target, a.Previous, a.Failures, a.Reason = known.Target, known.Previous, known.Failures, known.Reason
	} else {
		a.Reason = fmt.Sprintf("nothing learned for this folder yet: the fixed Compact at of %dk", c.CompactAtTokens/1000)
		if c.Intelligent() {
			a.Reason = fmt.Sprintf("nothing learned for this folder yet: the starting size of %dk, the middle of %dk to %dk", c.StartTokens()/1000, c.FloorTokens/1000, c.CompactAtTokens/1000)
		}
	}
	if !c.Enabled {
		a.Threshold, a.Source = 0, "off"
		a.Reason = "Pauseless Compaction is off"
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(a)
}
