package admin

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
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
	// Providers is where it applies, empty for everywhere; Choices is
	// what can be named.
	Providers []string           `json:"providers"`
	Choices   []automaskProvider `json:"choices"`
}

// automaskProvider is one place a request can go, as the dashboard names it.
type automaskProvider struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Note string `json:"note"`
}

func automaskChoices(cfg config.Config) []automaskProvider {
	sec := "none is set up"
	cfg.ResolveRoutes()
	if p := cfg.Secondary.Provider; p != "" && p != config.ProviderNone {
		sec = p
	}
	return []automaskProvider{
		{router.MaskAnthropic, "Anthropic", "What Claude Code sends. Its history is masked once, before anything else reads it, so a secondary that takes over gets the masked history too."},
		{router.MaskSecondary, "Secondary (" + sec + ")", "What Claude Code sends when it is routed to the secondary. With Anthropic not ticked, Anthropic gets the request as written and only the secondary's copy is masked."},
		{router.MaskChatGPT, "ChatGPT", "What Codex sends through Burst: its instructions, messages and tool output."},
	}
}

func (s *Server) automaskStatus(cfg config.Config) automaskStatus {
	c := cfg.Automask
	totals := s.gateway.AutomaskTotals()
	out := automaskStatus{Enabled: c.Enabled, Words: append([]string{}, c.Words...), Recent: s.gateway.AutomaskRecent(), Since: s.gateway.AutomaskSince(),
		Providers: append([]string{}, c.Providers...), Choices: automaskChoices(cfg)}
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
	writeJSON(w, s.automaskStatus(cfg))
}

// handleAutomaskSave takes {enabled, rules:{id:bool}, words:[...],
// providers:[...]}. Only rules switched away from their default are
// stored. Without words the saved list is kept, and without providers the
// saved choice; an empty providers list, or one naming every provider,
// means everywhere.
func (s *Server) handleAutomaskSave(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Enabled bool            `json:"enabled"`
		Rules   map[string]bool `json:"rules"`
		Words   *[]string       `json:"words"`
		// Providers is a pointer so that a save from before the choice
		// existed keeps it.
		Providers *[]string `json:"providers"`
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
	if req.Providers != nil {
		picked := map[string]bool{}
		for _, p := range *req.Providers {
			known := false
			for _, k := range router.MaskProviders {
				known = known || k == p
			}
			if !known {
				http.Error(w, fmt.Sprintf("unknown provider %q", p), http.StatusBadRequest)
				return
			}
			picked[p] = true
		}
		if len(picked) < len(router.MaskProviders) {
			for _, k := range router.MaskProviders {
				if picked[k] {
					next.Providers = append(next.Providers, k)
				}
			}
		}
	}
	saved, ok := updateConfig(w, func(c *config.Config) error {
		if req.Words == nil {
			next.Words = c.Automask.Words
		}
		if req.Providers == nil {
			next.Providers = c.Automask.Providers
		}
		c.Automask = next
		return nil
	})
	if !ok {
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
		msg = fmt.Sprintf("automask on: %d rules", n)
		if len(next.Words) > 0 {
			msg += fmt.Sprintf(" and your own word list (%d)", len(next.Words))
		}
		if len(next.Providers) > 0 {
			var names []string
			for _, ch := range automaskChoices(saved) {
				for _, p := range next.Providers {
					if p == ch.ID {
						names = append(names, ch.Name)
					}
				}
			}
			msg += ", only for " + strings.Join(names, " and ")
		} else {
			msg += ", everywhere"
		}
		msg += ", from the next request"
	}
	writeJSON(w, map[string]any{"ok": msg, "status": s.automaskStatus(saved)})
}
