package admin

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/andrewbakercloudscale/claude-burst/internal/claudesettings"
	"github.com/andrewbakercloudscale/claude-burst/internal/config"
	"github.com/andrewbakercloudscale/claude-burst/internal/metrics"
	"github.com/andrewbakercloudscale/claude-burst/internal/shunt"
)

// shuntSummary is one window of shunt.jsonl.
type shuntSummary struct {
	Reads              int     `json:"reads"`
	Writes             int     `json:"writes"`
	Denials            int     `json:"denials"`
	Failures           int     `json:"failures"`
	KeptOutTokens      int64   `json:"kept_out_tokens"` // an estimate, ~4 bytes per token
	WorkerInputTokens  int64   `json:"worker_input_tokens"`
	WorkerOutputTokens int64   `json:"worker_output_tokens"`
	WorkerUSD          float64 `json:"worker_usd"`
	Unpriced           int     `json:"unpriced"`
}

type shuntInfo struct {
	Read     bool   `json:"read"`
	Write    bool   `json:"write"`
	MinLines int    `json:"min_lines"`
	Model    string `json:"model,omitempty"`

	// HookInstalled and SkillInstalled are what actually enforce and teach the
	// feature. Read being "on" in config.json while the hook is missing means
	// nothing is refusing anything, and the page must say so rather than show
	// a green switch over a feature that is not running.
	HookInstalled  bool `json:"hook_installed"`
	SkillInstalled bool `json:"skill_installed"`

	WorkerReady bool   `json:"worker_ready"`
	WorkerError string `json:"worker_error,omitempty"`

	Today  shuntSummary `json:"today"`
	Last30 shuntSummary `json:"last30"`

	// RepeatRefusals1h counts refusals in the last hour that were the third or
	// later of the same file by the same session with no answer in between: a
	// session retrying a blocked read instead of running shunt read. Problems24h
	// counts everything else that needs a look (failed delegations, a guard that
	// could not decide). Both turn the card red: a switch that is on over a
	// session that keeps hitting a wall is not "working".
	RepeatRefusals1h int `json:"repeat_refusals_1h"`
	Problems24h      int `json:"problems_24h"`
}

func summarizeShunt(since time.Time) shuntSummary {
	var out shuntSummary
	lp, err := config.ShuntLogPath()
	if err != nil {
		return out
	}
	sum, err := shunt.SummarizeLog(lp, since)
	if err != nil {
		return out
	}
	return shuntSummary{
		Reads: sum.Reads, Writes: sum.Writes, Denials: sum.Denials, Failures: sum.Failures,
		KeptOutTokens: sum.KeptOutTokens(), WorkerInputTokens: sum.WorkerInputTokens,
		WorkerOutputTokens: sum.WorkerOutputTokens, WorkerUSD: sum.WorkerUSD, Unpriced: sum.Unpriced,
	}
}

func (s *Server) shuntInfo(cfg config.Config) shuntInfo {
	si := shuntInfo{
		Read: cfg.Shunt.Read, Write: cfg.Shunt.Write,
		MinLines: cfg.Shunt.MinLinesOrDefault(), Model: shunt.ModelOf(cfg),
		SkillInstalled: shunt.SkillInstalled(),
	}
	if p, err := claudesettings.Path(); err == nil {
		if root, err := claudesettings.Read(p); err == nil {
			si.HookInstalled = shunt.HookInstalled(root)
		}
	}
	// Readiness never reads the secret: this runs on every dashboard poll.
	if err := shunt.Readiness(cfg, s.keyInfo); err != nil {
		si.WorkerError = err.Error()
	} else {
		si.WorkerReady = true
	}
	now := time.Now()
	si.Today = summarizeShunt(now.Add(-24 * time.Hour))
	si.Last30 = summarizeShunt(now.Add(-30 * 24 * time.Hour))
	if lp, err := config.ShuntLogPath(); err == nil {
		if evs, err := shunt.Recent(lp, 500, nil); err == nil {
			for _, e := range evs { // newest first
				age := now.Sub(e.Time)
				if age > 24*time.Hour {
					break
				}
				if e.Kind == shunt.KindDeny && e.Repeat >= 2 && age <= time.Hour {
					si.RepeatRefusals1h++
				} else if e.IsProblem() && e.Kind != shunt.KindDeny {
					si.Problems24h++
				}
			}
		}
	}
	return si
}

type shuntRequest struct {
	Read     bool `json:"read"`
	Write    bool `json:"write"`
	MinLines int  `json:"min_lines"` // 0 leaves the threshold alone
}

const (
	minShuntLines = 20
	maxShuntLines = 100000
)

// handleShunt sets both switches and, optionally, the threshold, then makes
// settings.json and the skill agree -- the same shunt.Apply the CLI runs.
//
// Both flags are sent every time, not one at a time: two checkboxes each
// posting only their own change would race when clicked in quick succession,
// each overwriting the other's half from a stale read.
func (s *Server) handleShunt(w http.ResponseWriter, r *http.Request) {
	var req shuntRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request body", http.StatusBadRequest)
		return
	}
	cfg, err := config.Load()
	if err != nil {
		http.Error(w, "config.json does not parse, fix it before changing this: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if req.MinLines != 0 && (req.MinLines < minShuntLines || req.MinLines > maxShuntLines) {
		http.Error(w, fmt.Sprintf("threshold must be between %d and %d lines", minShuntLines, maxShuntLines), http.StatusBadRequest)
		return
	}

	// Turning something ON needs a worker to send it to. Turning it off never
	// does: a broken secondary must not be able to trap the guard on.
	turningOn := (req.Read && !cfg.Shunt.Read) || (req.Write && !cfg.Shunt.Write)
	if turningOn {
		if err := shunt.Readiness(cfg, s.keyInfo); err != nil {
			http.Error(w, "cannot enable: "+err.Error(), http.StatusBadRequest)
			return
		}
	}

	cfg.Shunt.Read, cfg.Shunt.Write = req.Read, req.Write
	if req.MinLines != 0 {
		cfg.Shunt.MinLines = req.MinLines
	}
	if err := config.Save(cfg); err != nil {
		http.Error(w, "saving config.json: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if err := shunt.Apply(cfg, s.shuntBin); err != nil {
		http.Error(w, "config.json saved, but updating Claude Code's hook and skill failed: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// Turning it off is immediate -- the guard consults config.json on every
	// call, so nothing is refused from this moment -- and only unloading the
	// skill from an already-running session needs a restart. Turning it on is
	// the reverse: the hook and skill load at session start.
	msg := "shunt off — the guard stops refusing reads immediately; restart Claude Code sessions to unload the skill"
	restart := false
	switch {
	case cfg.Shunt.Read && cfg.Shunt.Write:
		msg, restart = "shunt on: bulk read and code write", true
	case cfg.Shunt.Read:
		msg, restart = "shunt on: bulk read only", true
	case cfg.Shunt.Write:
		msg, restart = "shunt on: code write only", true
	}
	if restart {
		msg += " — restart Claude Code sessions for the hook and skill to load"
	}
	writeJSON(w, map[string]string{"ok": msg})
}

// SlotShunt marks a row in the requests table as a Token Shunt rather than a
// gateway request. It is a display tag, never written to metrics.jsonl: shunt
// events live in shunt.jsonl, so the activity chart and the totals are not
// inflated by calls that were never Claude Code traffic.
const SlotShunt = "shunt"

// requestRow is one line of /api/requests. For gateway requests it is exactly
// the metrics.Event it always was; the extra fields are only set on shunt rows.
type requestRow struct {
	metrics.Event
	Shunt   string `json:"shunt,omitempty"`    // "read" | "write" on a Token Shunt row
	ShuntOK *bool  `json:"shunt_ok,omitempty"` // whether the worker call succeeded
}

// shuntRequestRow turns a worker call into a requests-table row. The model is
// the WORKER's (what actually answered), since that is what the row is for.
func shuntRequestRow(e shunt.Event) requestRow {
	ok := e.OK
	row := requestRow{
		Event: metrics.Event{
			Time:             e.Time,
			SessionID:        e.Session,
			Slot:             SlotShunt,
			Route:            "token-shunt",
			Model:            e.Model,
			Destination:      e.Destination,
			DurationMS:       e.DurationMS,
			InputTokens:      e.InputTokens,
			OutputTokens:     e.OutputTokens,
			APIEquivalentUSD: e.USD,
			PricingUnknown:   e.PricingUnknown,
		},
		Shunt:   e.Kind,
		ShuntOK: &ok,
	}
	switch {
	case !e.OK:
		row.Note = e.Note
	case e.Kind == shunt.KindRead:
		row.Note = fmt.Sprintf("read %d file(s) · ≈%s tokens kept out of context", e.Files, groupInt(e.KeptOutTokens()))
	case e.Kind == shunt.KindWrite:
		row.Note = fmt.Sprintf("generated %s straight to disk · ≈%s output tokens not spent", byteSize(e.BytesOut), groupInt(e.KeptOutTokens()))
	}
	return row
}

// shuntCalls are the events that are worker calls, as opposed to a refusal
// (which never reached the worker and belongs only in the panel's own list).
func shuntCalls(e shunt.Event) bool { return e.Kind == shunt.KindRead || e.Kind == shunt.KindWrite }

// mergeRequests interleaves gateway events with worker calls, newest first,
// and cuts the result to limit.
func mergeRequests(events []metrics.Event, calls []shunt.Event, limit int) []requestRow {
	rows := make([]requestRow, 0, len(events)+len(calls))
	for _, e := range events {
		rows = append(rows, requestRow{Event: e})
	}
	for _, c := range calls {
		rows = append(rows, shuntRequestRow(c))
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].Time.After(rows[j].Time) })
	if len(rows) > limit {
		rows = rows[:limit]
	}
	return rows
}

func groupInt(n int64) string {
	s := strconv.FormatInt(n, 10)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}

func byteSize(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d bytes", n)
}

// shuntActivityRow is one line of the panel's own recent-activity list.
// Unlike the requests table it includes refused direct reads, which are the
// guard doing its job even though no worker call followed.
type shuntActivityRow struct {
	Time time.Time `json:"time"`
	Kind string    `json:"kind"` // read | write | deny | guard_error
	// Tag and Detail are built by shunt.Event, the same wording `claude-burst
	// shunt log` prints, so the page and the terminal cannot disagree.
	Tag            string  `json:"tag"`
	Detail         string  `json:"detail"`
	Problem        bool    `json:"problem"`
	Project        string  `json:"project,omitempty"`
	Session        string  `json:"session,omitempty"`    // short form, for display
	SessionFull    string  `json:"session_id,omitempty"` // full id, for the tooltip and for matching
	Tool           string  `json:"tool,omitempty"`
	Path           string  `json:"path,omitempty"`
	Stage          string  `json:"stage,omitempty"`
	Repeat         int     `json:"repeat,omitempty"`
	OK             bool    `json:"ok"`
	Files          int     `json:"files,omitempty"`
	Calls          int     `json:"calls,omitempty"`
	BytesIn        int64   `json:"bytes_in,omitempty"`
	KeptOutTokens  int64   `json:"kept_out_tokens"`
	InputTokens    int64   `json:"input_tokens,omitempty"`
	OutputTokens   int64   `json:"output_tokens,omitempty"`
	Model          string  `json:"model,omitempty"`
	Destination    string  `json:"destination,omitempty"`
	USD            float64 `json:"usd,omitempty"`
	PricingUnknown bool    `json:"pricing_unknown,omitempty"`
	DurationMS     int64   `json:"duration_ms,omitempty"`
	Note           string  `json:"note,omitempty"`
	Cwd            string  `json:"cwd,omitempty"`
}

func (s *Server) handleShuntActivity(w http.ResponseWriter, r *http.Request) {
	limit := 20
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 200 {
			limit = n
		}
	}
	rows := []shuntActivityRow{}
	lp, err := config.ShuntLogPath()
	if err == nil {
		events, err := shunt.Recent(lp, limit, nil)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		for _, e := range events {
			rows = append(rows, shuntActivityRow{
				Time: e.Time, Kind: e.Kind, Tag: e.Tag(), Detail: e.Describe(), Problem: e.IsProblem(),
				Project: e.Project(), Session: shunt.ShortSession(e.Session), SessionFull: e.Session,
				Tool: e.Tool, Path: e.Path, Stage: e.Stage, Repeat: e.Repeat, OK: e.OK, Files: e.Files, Calls: e.Calls, BytesIn: e.BytesIn,
				KeptOutTokens: e.KeptOutTokens(), InputTokens: e.InputTokens, OutputTokens: e.OutputTokens,
				Model: e.Model, Destination: e.Destination, USD: e.USD, PricingUnknown: e.PricingUnknown,
				DurationMS: e.DurationMS, Note: e.Note, Cwd: e.Cwd,
			})
		}
	}
	writeJSON(w, rows)
}
