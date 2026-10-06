package admin

import (
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/andrewbakercloudscale/claude-burst/internal/config"
	"github.com/andrewbakercloudscale/claude-burst/internal/metrics"
)

// Codex insights: the Codex tab's Daily activity, Analytics and Tokens by
// model, as the Claude tab has them for Claude Code. In tokens, not money:
// Burst has no API-equivalent price for Codex's models. Read from
// codex-metrics.jsonl alone, so no Claude Code figure is in any of it.

// codexDay is one local day. The three token kinds are apart because they
// are not the same thing to the plan: cached input is the conversation sent
// again, uncached input and output are what the turn added.
type codexDay struct {
	Date     string `json:"date"` // YYYY-MM-DD, local
	Turns    int    `json:"turns"`
	Errors   int    `json:"errors"`
	Sessions int    `json:"sessions"`
	Input    int64  `json:"input_tokens"`
	Cached   int64  `json:"cached_tokens"`
	Output   int64  `json:"output_tokens"`
}

type codexModelUse struct {
	Model  string `json:"model"`
	Turns  int    `json:"turns"`
	Errors int    `json:"errors"`
	Input  int64  `json:"input_tokens"`
	Cached int64  `json:"cached_tokens"`
	Output int64  `json:"output_tokens"`
	// LatencyP50MS is over the model's answered (2xx) turns.
	LatencyP50MS int64 `json:"latency_p50_ms"`
}

type codexInsights struct {
	Days     int             `json:"days"`
	Daily    []codexDay      `json:"daily"`
	Models   []codexModelUse `json:"models"`
	Turns    int             `json:"turns"`
	Errors   int             `json:"errors"`
	Sessions int             `json:"sessions"`
	Input    int64           `json:"input_tokens"`
	Cached   int64           `json:"cached_tokens"`
	Output   int64           `json:"output_tokens"`
	// Latencies are over answered (2xx) turns only, as the Claude tab's are:
	// a refused turn is instant and would make a broken day look fast.
	LatencyP50MS int64 `json:"latency_p50_ms"`
	LatencyP95MS int64 `json:"latency_p95_ms"`
	// LargestContext is the fullest any session's context got: one turn's
	// whole input plus its output.
	LargestContext      int64  `json:"largest_context"`
	LargestContextModel string `json:"largest_context_model,omitempty"`
	// BusiestHour is the local hour of day (0 to 23) with the most turns
	// over the window, -1 with no turns.
	BusiestHour      int `json:"busiest_hour"`
	BusiestHourTurns int `json:"busiest_hour_turns"`
	// Earliest, Covered and Files say how far back the log really goes, as
	// metrics.History does: a day before the first record is "no data".
	Earliest string `json:"earliest,omitempty"`
	Covered  bool   `json:"covered"`
	Files    int    `json:"files"`
}

// readCodexInsights buckets the last `days` local days of the Codex metrics
// log at path, today included.
func readCodexInsights(path string, days int, now time.Time) (codexInsights, error) {
	start := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location()).AddDate(0, 0, -(days - 1))
	in := codexInsights{Days: days, Daily: make([]codexDay, 0, days), Models: []codexModelUse{}, BusiestHour: -1}
	index := map[string]int{}
	daySessions := map[string]map[string]struct{}{}
	for d := start; !d.After(now); d = d.AddDate(0, 0, 1) {
		key := d.Format("2006-01-02")
		index[key] = len(in.Daily)
		in.Daily = append(in.Daily, codexDay{Date: key})
		daySessions[key] = map[string]struct{}{}
	}
	sessions := map[string]struct{}{}
	models := map[string]*codexModelUse{}
	modelLat := map[string][]int64{}
	var lat []int64
	var hours [24]int

	earliest, files, err := metrics.Scan(path, start, func(e metrics.Event) {
		i, ok := index[e.Time.Local().Format("2006-01-02")]
		if !ok {
			return
		}
		d := &in.Daily[i]
		name := e.Model
		if name == "" {
			name = "(no model)"
		}
		m := models[name]
		if m == nil {
			m = &codexModelUse{Model: name}
			models[name] = m
		}
		d.Turns++
		m.Turns++
		in.Turns++
		hours[e.Time.Local().Hour()]++
		if metrics.IsError(e) {
			d.Errors++
			m.Errors++
			in.Errors++
		}
		if e.SessionID != "" {
			sessions[e.SessionID] = struct{}{}
			daySessions[d.Date][e.SessionID] = struct{}{}
		}
		d.Input, d.Cached, d.Output = d.Input+e.InputTokens, d.Cached+e.CacheReadTokens, d.Output+e.OutputTokens
		m.Input, m.Cached, m.Output = m.Input+e.InputTokens, m.Cached+e.CacheReadTokens, m.Output+e.OutputTokens
		in.Input, in.Cached, in.Output = in.Input+e.InputTokens, in.Cached+e.CacheReadTokens, in.Output+e.OutputTokens
		if e.HTTPStatus >= 200 && e.HTTPStatus < 300 {
			lat = append(lat, e.DurationMS)
			modelLat[name] = append(modelLat[name], e.DurationMS)
			if c := e.InputTokens + e.CacheReadTokens + e.OutputTokens; c > in.LargestContext {
				in.LargestContext, in.LargestContextModel = c, e.Model
			}
		}
	})
	if err != nil {
		return in, err
	}
	for i := range in.Daily {
		in.Daily[i].Sessions = len(daySessions[in.Daily[i].Date])
	}
	in.Sessions = len(sessions)
	for name, m := range models {
		m.LatencyP50MS = metrics.Percentile(modelLat[name], 0.50)
		in.Models = append(in.Models, *m)
	}
	sort.Slice(in.Models, func(i, j int) bool {
		a, b := in.Models[i], in.Models[j]
		if at, bt := a.Input+a.Cached+a.Output, b.Input+b.Cached+b.Output; at != bt {
			return at > bt
		}
		return a.Model < b.Model
	})
	in.LatencyP50MS = metrics.Percentile(lat, 0.50)
	in.LatencyP95MS = metrics.Percentile(lat, 0.95)
	for h, n := range hours {
		if n > in.BusiestHourTurns {
			in.BusiestHour, in.BusiestHourTurns = h, n
		}
	}
	in.Files = files
	if !earliest.IsZero() {
		in.Earliest = earliest.Format(time.RFC3339)
		// By day, as metrics.Daily compares: see History.Covered.
		in.Covered = earliest.Local().Format("2006-01-02") <= start.Format("2006-01-02")
	}
	return in, nil
}

func (s *Server) handleCodexInsights(w http.ResponseWriter, r *http.Request) {
	days, _ := strconv.Atoi(r.URL.Query().Get("days"))
	if days < 1 || days > 90 {
		days = 14
	}
	path, err := config.CodexMetricsPath()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	in, err := readCodexInsights(path, days, time.Now())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, in)
}
