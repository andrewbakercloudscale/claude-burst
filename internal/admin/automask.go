package admin

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/andrewbakercloudscale/claude-burst/internal/automask"
	"github.com/andrewbakercloudscale/claude-burst/internal/config"
	"github.com/andrewbakercloudscale/claude-burst/internal/router"
)

// automaskRule is one rule as the dashboard shows it.
type automaskRule struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Note    string `json:"note"`
	Default bool   `json:"default"`
	On      bool   `json:"on"`
	Masked  int    `json:"masked"`
}

type automaskStatus struct {
	Enabled bool                 `json:"enabled"`
	Rules   []automaskRule       `json:"rules"`
	Words   []string             `json:"words"`
	Recent  []router.AutomaskHit `json:"recent"`
	// Since is when the rules' Masked counts began.
	Since time.Time `json:"since"`
}

func (s *Server) automaskStatus(c config.AutomaskConfig) automaskStatus {
	totals := s.gateway.AutomaskTotals()
	out := automaskStatus{Enabled: c.Enabled, Words: append([]string{}, c.Words...), Recent: s.gateway.AutomaskRecent(), Since: s.gateway.AutomaskSince()}
	for _, r := range automask.Rules {
		out.Rules = append(out.Rules, automaskRule{ID: r.ID, Name: r.Name, Note: r.Note, Default: r.Default,
			On: router.RuleOn(c, r), Masked: totals[r.ID]})
	}
	return out
}

// handleAutomask is the switches and how many values each rule masked.
func (s *Server) handleAutomask(w http.ResponseWriter, r *http.Request) {
	cfg, err := config.Load()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, s.automaskStatus(cfg.Automask))
}

// handleAutomaskSave takes {enabled, rules:{id:bool}, words:[...]}. Only
// rules switched away from their default are stored. Without words the
// saved list is kept.
func (s *Server) handleAutomaskSave(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Enabled bool            `json:"enabled"`
		Rules   map[string]bool `json:"rules"`
		Words   *[]string       `json:"words"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request body", http.StatusBadRequest)
		return
	}
	next := config.AutomaskConfig{Enabled: req.Enabled}
	for id, on := range req.Rules {
		var rule *automask.Rule
		for _, x := range automask.Rules {
			if x.ID == id {
				rule = x
			}
		}
		if rule == nil {
			http.Error(w, fmt.Sprintf("unknown rule %q", id), http.StatusBadRequest)
			return
		}
		if on != rule.Default {
			if next.Rules == nil {
				next.Rules = map[string]bool{}
			}
			next.Rules[id] = on
		}
	}
	if req.Words != nil {
		next.Words = automask.CleanWords(*req.Words)
	}
	if _, ok := updateConfig(w, func(c *config.Config) error {
		if req.Words == nil {
			next.Words = c.Automask.Words
		}
		c.Automask = next
		return nil
	}); !ok {
		return
	}
	s.gateway.SetAutomask(next)
	msg := "automask off"
	if next.Enabled {
		n := 0
		for _, x := range automask.Rules {
			if router.RuleOn(next, x) {
				n++
			}
		}
		msg = fmt.Sprintf("automask on: %d rules, from the next request", n)
		if len(next.Words) > 0 {
			msg = fmt.Sprintf("automask on: %d rules and your own word list (%d), from the next request", n, len(next.Words))
		}
	}
	writeJSON(w, map[string]any{"ok": msg, "status": s.automaskStatus(next)})
}
