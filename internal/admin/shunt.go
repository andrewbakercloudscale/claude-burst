package admin

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/andrewbakercloudscale/claude-burst/internal/claudesettings"
	"github.com/andrewbakercloudscale/claude-burst/internal/config"
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

	msg := "shunt off"
	switch {
	case cfg.Shunt.Read && cfg.Shunt.Write:
		msg = "shunt on: bulk read and code write"
	case cfg.Shunt.Read:
		msg = "shunt on: bulk read only"
	case cfg.Shunt.Write:
		msg = "shunt on: code write only"
	}
	writeJSON(w, map[string]string{"ok": msg + " — restart Claude Code sessions for the hook and skill to load"})
}
