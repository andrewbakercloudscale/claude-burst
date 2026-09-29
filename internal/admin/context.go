package admin

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/andrewbakercloudscale/claude-burst/internal/config"
	"github.com/andrewbakercloudscale/claude-burst/internal/metrics"
	"github.com/andrewbakercloudscale/claude-burst/internal/router"
)

// The "Context & cache" section: switches for pruning overflow requests,
// and the numbers that say whether it and prompt caching are working.
// See router/prune.go for what pruning does and why only the secondary.

const contextWindow = 7 * 24 * time.Hour

type contextInfo struct {
	// Applicable is false when the secondary is not openai-compatible, so
	// there is nothing for pruning to act on.
	Applicable bool               `json:"applicable"`
	Pruning    config.PruneConfig `json:"pruning"`
	Efficiency metrics.Efficiency `json:"efficiency"`
	WindowDays int                `json:"window_days"`

	PrimaryCacheHit   float64 `json:"primary_cache_hit"`
	SecondaryCacheHit float64 `json:"secondary_cache_hit"`
	TokensNotSent     int64   `json:"tokens_not_sent"`
	// SavedPct is the share of the secondary's input that pruning removed,
	// 0-100: the one number for "how successful".
	SavedPct    int     `json:"saved_pct"`
	USDNotSpent float64 `json:"usd_not_spent"`

	// Verdict is the one line that answers "is it working": ok, info
	// (nothing to judge yet, or off) or bad.
	Verdict      verdict `json:"verdict"`
	CacheVerdict verdict `json:"cache_verdict"`

	// Compaction is proxy-side compaction of long primary sessions, and
	// Sessions the sessions it is tracking, largest context first.
	Compaction config.CompactionConfig    `json:"compaction"`
	Sessions   []router.CompactionSession `json:"sessions"`
	// CompactionStats is what compaction did over the same window.
	CompactionStats metrics.CompactionStats `json:"compaction_stats"`
}

type verdict struct {
	Level string `json:"level"`
	Text  string `json:"text"`
}

func (s *Server) contextInfo(cfg config.Config) contextInfo {
	eff, _ := metrics.EfficiencySince(s.metricsPath, time.Now().Add(-contextWindow))
	ci := contextInfo{
		Applicable: cfg.Secondary.Provider == "openai-compatible",
		Pruning:    cfg.SecondaryPruning,
		Efficiency: eff,
		WindowDays: int(contextWindow / (24 * time.Hour)),

		PrimaryCacheHit:   eff.Primary.CacheHitRate(),
		SecondaryCacheHit: eff.Secondary.CacheHitRate(),
		TokensNotSent:     eff.PrunedBytes / metrics.BytesPerToken,
		SavedPct:          int(eff.SavedShare()*100 + 0.5),
	}
	ci.USDNotSpent = float64(ci.TokensNotSent) / 1_000_000 * cfg.Pricing[cfg.Secondary.Model].InputPerMTok
	ci.Compaction = cfg.PrimaryCompaction
	ci.Sessions = s.gateway.CompactionSessions()
	ci.CompactionStats, _ = metrics.CompactionStatsSince(s.metricsPath, time.Now().Add(-contextWindow))
	ci.Verdict = pruneVerdict(ci)
	ci.CacheVerdict = cacheVerdict(eff)
	return ci
}

// failRate is failures as a share of requests, 0 with no requests.
func failRate(ok, failed int) float64 {
	if ok+failed == 0 {
		return 0
	}
	return float64(failed) / float64(ok+failed)
}

// minComparable is how many pruned requests it takes before their failure
// rate is compared with the unpruned one; below it, one bad request would
// swing the verdict.
const minComparable = 5

// maxRerunShare is the share of stubbed calls the model may fetch again
// before the verdict says the cut-off is too aggressive.
const maxRerunShare = 0.25

func pruneVerdict(ci contextInfo) verdict {
	e, p := ci.Efficiency, ci.Pruning
	switch {
	case !ci.Applicable:
		return verdict{"info", "Not applicable: the secondary is not an openai-compatible provider, so there is nothing to prune."}
	case p.Disabled || (p.NoStub && p.NoCap):
		return verdict{"info", "Off: overflow requests are sent with their full history."}
	case e.Secondary.Requests == 0:
		return verdict{"info", fmt.Sprintf("On, waiting: no overflow requests in the last %d days. Pruning only acts when traffic goes to the secondary.", ci.WindowDays)}
	case e.PrunedRequests == 0:
		return verdict{"info", fmt.Sprintf("On, but none of the %d overflow requests had old or oversized tool output to remove.", e.Secondary.Requests)}
	}
	pf, uf := failRate(e.PrunedOK, e.PrunedFailed), failRate(e.UnprunedOK, e.UnprunedFailed)
	if e.PrunedRequests >= minComparable && pf > uf+0.10 {
		return verdict{"bad", fmt.Sprintf("Pruned requests fail %.0f%% of the time vs %.0f%% unpruned: pruning may be removing context the model needs. Raise \"keep recent\" or turn stubbing off.", pf*100, uf*100)}
	}
	// A stub the model fetches again costs a round trip instead of saving
	// one. Occasional re-runs are the price of stubbing; a quarter or more
	// means the cut-off is removing output that is still in use.
	if e.StubbedResults >= minComparable && float64(e.RerunsAfterStub) > maxRerunShare*float64(e.StubbedResults) {
		return verdict{"bad", fmt.Sprintf("The model re-ran %d of %d stubbed tool calls to get their output back: the cut-off is removing output still in use. Raise \"keep recent\".",
			e.RerunsAfterStub, e.StubbedResults)}
	}
	return verdict{"ok", fmt.Sprintf("Working: removed %d%% of overflow input (≈%s tokens, ≈$%.2f) across %d of %d requests. Failure rate %.0f%% pruned vs %.0f%% unpruned; %d stubbed call(s) re-run.",
		ci.SavedPct, humanCount(ci.TokensNotSent), ci.USDNotSpent, e.PrunedRequests, e.Secondary.Requests, pf*100, uf*100, e.RerunsAfterStub)}
}

func cacheVerdict(e metrics.Efficiency) verdict {
	if e.Secondary.Requests > 0 && e.Secondary.CacheReadTokens == 0 {
		return verdict{"info", "The secondary reports no cached tokens: it either does not cache prompts or does not say so. Every overflow request is billed in full, which is what pruning is for."}
	}
	if e.Primary.Requests > 0 && e.Primary.CacheReadTokens == 0 {
		return verdict{"bad", "The primary reports no cache reads at all: prompt caching is not working, so every turn resends the full context."}
	}
	return verdict{"ok", ""}
}

func humanCount(n int64) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	case n >= 1_000:
		return fmt.Sprintf("%.0fk", float64(n)/1_000)
	}
	return fmt.Sprint(n)
}

// Bounds for the numeric settings. Zero means "use the default".
const (
	maxKeepRecent = 200
	maxPruneStep  = 100
	minCapBytes   = 4 * 1024
	maxCapBytes   = 1024 * 1024
)

func (s *Server) handlePruning(w http.ResponseWriter, r *http.Request) {
	var req config.PruneConfig
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request body", http.StatusBadRequest)
		return
	}
	switch {
	case req.KeepRecent < 0 || req.KeepRecent > maxKeepRecent:
		http.Error(w, fmt.Sprintf("keep recent must be between 1 and %d", maxKeepRecent), http.StatusBadRequest)
		return
	case req.Step < 0 || req.Step > maxPruneStep:
		http.Error(w, fmt.Sprintf("step must be between 1 and %d", maxPruneStep), http.StatusBadRequest)
		return
	case req.MaxToolResultBytes != 0 && (req.MaxToolResultBytes < minCapBytes || req.MaxToolResultBytes > maxCapBytes):
		http.Error(w, fmt.Sprintf("cap must be between %d and %d bytes", minCapBytes, maxCapBytes), http.StatusBadRequest)
		return
	}
	cfg, err := config.Load()
	if err != nil {
		http.Error(w, "config.json does not parse, fix it before changing this: "+err.Error(), http.StatusInternalServerError)
		return
	}
	// stub_min_bytes has no control on the page; keep whatever config.json says.
	req.StubMinBytes = cfg.SecondaryPruning.StubMinBytes
	cfg.SecondaryPruning = req
	if err := config.Save(cfg); err != nil {
		http.Error(w, "saving config.json: "+err.Error(), http.StatusInternalServerError)
		return
	}
	live := s.gateway.SetSecondaryPruning(req)
	msg := "saved and applied to the running gateway"
	if !live {
		msg = "saved; there is no openai-compatible secondary running, so it has nothing to act on yet"
	}
	on := !req.Disabled && !(req.NoStub && req.NoCap)
	state := "pruning off"
	if on {
		state = "pruning on"
	}
	writeJSON(w, map[string]string{"ok": state + " - " + msg})
}

// Bounds for compaction thresholds. Below minCompactAt a summary would cost
// more than it saves; the model's window is the ceiling.
const (
	minCompactAt = 50_000
	maxCompactAt = 900_000
	maxWindow    = 24 * 60
)

func (s *Server) handleCompaction(w http.ResponseWriter, r *http.Request) {
	var req config.CompactionConfig
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request body", http.StatusBadRequest)
		return
	}
	res := req.Resolved()
	switch {
	case res.CompactAtTokens < minCompactAt || res.CompactAtTokens > maxCompactAt:
		http.Error(w, fmt.Sprintf("compact threshold must be between %dk and %dk tokens", minCompactAt/1000, maxCompactAt/1000), http.StatusBadRequest)
		return
	case res.WarnAtTokens >= res.CompactAtTokens:
		http.Error(w, "the warning must come before the compaction threshold", http.StatusBadRequest)
		return
	case req.WindowMinutes < 0 || res.WindowMinutes > maxWindow:
		http.Error(w, fmt.Sprintf("window must be between 1 and %d minutes", maxWindow), http.StatusBadRequest)
		return
	}
	cfg, err := config.Load()
	if err != nil {
		http.Error(w, "config.json does not parse, fix it before changing this: "+err.Error(), http.StatusInternalServerError)
		return
	}
	cfg.PrimaryCompaction = req
	if err := config.Save(cfg); err != nil {
		http.Error(w, "saving config.json: "+err.Error(), http.StatusInternalServerError)
		return
	}
	s.gateway.SetCompaction(req)
	state := "compaction off"
	if req.Enabled {
		state = fmt.Sprintf("compaction on: warn at %dk, compact at %dk, at most once per %d minutes per session", res.WarnAtTokens/1000, res.CompactAtTokens/1000, res.WindowMinutes)
	}
	writeJSON(w, map[string]string{"ok": state + "; applied to the running gateway"})
}
