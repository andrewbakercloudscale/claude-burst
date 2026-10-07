package router

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/andrewbakercloudscale/claude-burst/internal/atomicfile"
	"github.com/andrewbakercloudscale/claude-burst/internal/config"
)

type State struct {
	// OverflowUntil is the ACCOUNT-WIDE window: only ForceOverflow sets it,
	// because "route everything to the secondary" is the one situation we
	// know covers every model. A limit Anthropic reported never lands here
	// -- see ModelOverflow.
	OverflowUntil int64  `json:"overflow_until"`
	LimitClaim    string `json:"limit_claim,omitempty"`
	LastReason    string `json:"last_reason,omitempty"`

	// ModelOverflow maps a requested Claude model to the unix time its own
	// rejection window ends.
	//
	// Anthropic's claim headers name the bucket that was exhausted
	// (five_hour, seven_day_opus, seven_day_overage_included, ...) but
	// nothing states which MODELS that bucket covers, and we do not guess:
	// only the model that was actually refused gets a window. If a limit
	// really is account-wide, the next model discovers that for itself on
	// its first request, at the cost of one rejection that bills nothing.
	// Guessing the other way is what cost real money -- one Fable rejection
	// on 2026-09-20 sent every model to a paid secondary for two days while
	// Opus was answering normally.
	ModelOverflow map[string]int64 `json:"model_overflow,omitempty"`

	// ModelClaim is what armed each model's window: an Anthropic limit claim
	// (five_hour, seven_day_overage_included, ...) or metered_* for an outage.
	// The two need different handling -- see releaseOutageWindow.
	ModelClaim map[string]string `json:"model_claim,omitempty"`

	// DowngradeDisabled turns the fallback chain off without editing
	// config.json, from the dashboard, while the gateway runs. Negative so
	// the zero value keeps the chain on: a user who has configured a chain
	// has already opted in, and an empty state file must not silently mean
	// "off".
	DowngradeDisabled bool `json:"downgrade_disabled,omitempty"`
}

func (s *Server) loadState() {
	b, err := os.ReadFile(s.statePath)
	if err != nil {
		if !os.IsNotExist(err) {
			s.logger.Printf("error stage=load_state path=%s err=%v (starting with no overflow state)", s.statePath, err)
		}
		return
	}
	var st State
	if err := json.Unmarshal(b, &st); err != nil {
		s.logger.Printf("error stage=load_state action=parse path=%s err=%v (starting with no overflow state)", s.statePath, err)
		return
	}
	// One-time migration. Before model scoping, any limit Anthropic reported
	// armed the account-wide window, so a legacy state file can carry a
	// days-long window that was only ever evidence about ONE model -- and
	// keeping it would send every model to the paid secondary for the rest
	// of it. A forced window is different: it was a deliberate instruction
	// and is honoured as written.
	if st.OverflowUntil > time.Now().Unix() && st.LimitClaim != "forced" && len(st.ModelOverflow) == 0 {
		s.logger.Printf("dropping pre-model-scoping overflow window (until=%s claim=%s): it recorded one model's rejection as account-wide. The next request per model re-establishes the truth.",
			time.Unix(st.OverflowUntil, 0).Format(time.RFC3339), st.LimitClaim)
		st.OverflowUntil, st.LimitClaim, st.LastReason = 0, "", ""
	}
	s.state = st
}

func (s *Server) saveStateLocked() {
	b, err := json.MarshalIndent(s.state, "", "  ")
	if err != nil {
		s.logger.Printf("error stage=save_state action=marshal err=%v", err)
		return
	}
	if err := atomicfile.Write(s.statePath, append(b, '\n'), 0600); err != nil {
		s.logger.Printf("error stage=save_state action=write path=%s err=%v", s.statePath, err)
	}
}

func (s *Server) Status() State {
	// Deep copy under the lock. A plain return copies only the map headers
	// for ModelOverflow and ModelClaim; callers JSON-encode the result after
	// the RLock is released, and a concurrent write then triggers Go's fatal
	// "concurrent map iteration and map write", which no recover() catches.
	// pf-heal probes /healthz on every network change, exactly when windows
	// are armed, so the overlap is correlated rather than rare.
	s.mu.RLock()
	defer s.mu.RUnlock()
	st := s.state
	st.ModelOverflow = make(map[string]int64, len(s.state.ModelOverflow))
	for k, v := range s.state.ModelOverflow {
		st.ModelOverflow[k] = v
	}
	st.ModelClaim = make(map[string]string, len(s.state.ModelClaim))
	for k, v := range s.state.ModelClaim {
		st.ModelClaim[k] = v
	}
	return st
}

// isOutageClaim reports whether a failover was armed by failures rather than by
// Anthropic saying a limit was reached.
func isOutageClaim(claim string) bool { return strings.HasPrefix(claim, "metered_") }

// releaseOutageWindow ends a model's window if -- and only if -- an outage armed
// it. A rate-limit window is left alone: the primary would just refuse again.
func (s *Server) releaseOutageWindow(model string) {
	if model == "" {
		return
	}
	s.mu.Lock()
	if !isOutageClaim(s.state.ModelClaim[model]) || s.state.ModelOverflow[model] == 0 {
		s.mu.Unlock()
		return
	}
	delete(s.state.ModelOverflow, model)
	delete(s.state.ModelClaim, model)
	s.saveStateLocked()
	// Unlocked before the alert, as activateOverflow does: alertOutcome
	// holds the alerts lock and then reads the state, so taking the alerts
	// lock with the state still held is a deadlock that stops every request.
	s.mu.Unlock()
	s.logger.Printf("released outage window for model=%q: the secondary also failed at the transport level, so the next request tries the primary", model)
	s.addFailoverNotice(fmt.Sprintf("\u26a1 Claude Burst: back on Anthropic for %s. The secondary could not answer either, so the outage is being treated as this machine's network.", model))
	s.alertBackOnPrimary("The secondary could not answer either, so " + model + " goes to Anthropic again.")
}

// ClearOverflow reopens every route: the forced account-wide window and each
// model's own. DowngradeDisabled deliberately survives -- it is a policy the
// user set, not a window that expires, and "Back to primary" silently
// re-enabling a chain they turned off would be a setting that undoes itself.
func (s *Server) ClearOverflow() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state = State{DowngradeDisabled: s.state.DowngradeDisabled}
	s.saveStateLocked()
}

// inOverflow reports whether ANY window is open -- the forced account-wide
// one, or any single model's. Routing never asks this question (it always has
// a model in hand, and asks modelInOverflow); it is for status output, where
// "is anything diverted right now" is the useful summary.
func (s *Server) inOverflow(now time.Time) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.state.OverflowUntil > now.Unix() {
		return true
	}
	for _, until := range s.state.ModelOverflow {
		if until > now.Unix() {
			return true
		}
	}
	return false
}

// forcedOverflow is the account-wide window. It deliberately bypasses the
// fallback chain: someone who pressed "Force -> secondary" is exercising the
// secondary, and quietly serving them a different Claude model instead would
// defeat the only test the secondary path ever gets.
func (s *Server) forcedOverflow(now time.Time) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.state.OverflowUntil > now.Unix()
}

func (s *Server) modelInOverflow(model string, now time.Time) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.state.ModelOverflow[model] > now.Unix()
}

// DowngradeEnabled reports whether the fallback chain is live.
func (s *Server) DowngradeEnabled() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return !s.state.DowngradeDisabled
}

// SetDowngradeEnabled turns the fallback chain on or off for the running
// gateway and persists the choice, so the dashboard toggle survives a
// restart without a config edit.
func (s *Server) SetDowngradeEnabled(on bool) {
	s.mu.Lock()
	s.state.DowngradeDisabled = !on
	s.saveStateLocked()
	s.mu.Unlock()
	s.logger.Printf("model downgrade before secondary: %v", on)
}

// FallbackChain returns the configured rungs for a model, for display.
func (s *Server) FallbackChain() map[string][]string { return s.cfg.FallbackChain }

// StartupConfig is the config this gateway was started with. Never written
// after New, so reading it needs no lock; the dashboard compares it with
// config.json to say when a saved change is waiting for a restart.
func (s *Server) StartupConfig() config.Config { return s.cfg }

// ModelOverflow returns a copy of the per-model windows, for display.
func (s *Server) ModelOverflow() map[string]int64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string]int64, len(s.state.ModelOverflow))
	for k, v := range s.state.ModelOverflow {
		out[k] = v
	}
	return out
}

// ladderFor returns the rungs still worth trying for a requested model: the
// configured chain minus any model already inside its own rejection window,
// and minus the requested model itself (a chain that loops back would retry
// the model that was just refused).
func (s *Server) ladderFor(model string, now time.Time) []string {
	if model == "" || !s.DowngradeEnabled() {
		return nil
	}
	var out []string
	for _, rung := range s.cfg.FallbackChain[model] {
		if rung == "" || rung == model || s.modelInOverflow(rung, now) {
			continue
		}
		out = append(out, rung)
	}
	return out
}

// ForceOverflow routes inference to the secondary for d, regardless of what
// the upstream is actually saying.
//
// This exists because a subscription primary only fails over on genuine
// exhaustion signals, which cannot be provoked on demand -- so without it the
// secondary path is untestable until the day it is needed, which is the worst
// possible moment to discover it is misconfigured. The claim is recorded as
// "forced" so the metrics and status output never imply Anthropic reported a
// limit that it did not.
func (s *Server) ForceOverflow(d time.Duration, reason string) time.Time {
	if d <= 0 {
		d = 15 * time.Minute
	}
	until := time.Now().Add(d)
	s.mu.Lock()
	// Same care as ClearOverflow: replacing the whole struct here silently
	// reset the downgrade toggle, so forcing the secondary for 15 minutes
	// also turned a chain the user had switched off back on, permanently.
	s.state = State{OverflowUntil: until.Unix(), LimitClaim: "forced", LastReason: reason,
		DowngradeDisabled: s.state.DowngradeDisabled}
	s.saveStateLocked()
	s.mu.Unlock()
	s.logger.Printf("FORCED to secondary until %s reason=%s", until.Format(time.RFC3339), reason)
	return until
}

// ForceModelOverflow routes one requested model to the secondary for d and
// leaves every other model where it is. It uses the same per-model window a
// real rejection opens, so routing, the dashboard and Back to primary treat
// it exactly like one. It is how overflow pruning is tested on purpose: a
// test session on this model spends secondary tokens while the user's own
// sessions on other models stay on the subscription.
func (s *Server) ForceModelOverflow(model string, d time.Duration, reason string) time.Time {
	if d <= 0 {
		d = 15 * time.Minute
	}
	until := time.Now().Add(d)
	s.mu.Lock()
	if s.state.ModelOverflow == nil {
		s.state.ModelOverflow = map[string]int64{}
	}
	s.state.ModelOverflow[model] = until.Unix()
	// A forced window is the user's, never an outage's: a stale metered_*
	// claim left from an expired outage window would let one secondary
	// transport failure release it mid-test.
	if s.state.ModelClaim == nil {
		s.state.ModelClaim = map[string]string{}
	}
	s.state.ModelClaim[model] = "forced"
	s.saveStateLocked()
	s.mu.Unlock()
	s.logger.Printf("FORCED model=%q to secondary until %s reason=%s", model, until.Format(time.RFC3339), reason)
	return until
}

// activateOverflow records that ONE model was refused, until resetAt. It no
// longer touches the account-wide window: see State.ModelOverflow for why a
// claim header is not evidence about models it does not name.
func (s *Server) activateOverflow(model string, resetAt int64, claim, reason string) {
	if resetAt <= time.Now().Unix() {
		ttl := time.Duration(s.cfg.UnknownResetSeconds) * time.Second
		if isOutageClaim(claim) {
			// An outage has no reset time to read, and the unknown-reset default
			// (5 minutes) is sized for a rate limit that genuinely lasts that
			// long. An outage that has passed should not keep steering traffic
			// to a secondary: hold it only as long as the failures that armed it
			// were counted over.
			ttl = time.Duration(s.cfg.MeteredFailover.WindowSeconds) * time.Second
		}
		resetAt = time.Now().Add(ttl).Unix()
	}
	resetAt += int64(s.cfg.ResetGraceSeconds)
	s.mu.Lock()
	if s.state.ModelOverflow == nil {
		s.state.ModelOverflow = map[string]int64{}
	}
	if s.state.ModelClaim == nil {
		s.state.ModelClaim = map[string]string{}
	}
	if model != "" {
		s.state.ModelClaim[model] = claim
	}
	// An empty model means the body carried none to read. Scoping that to ""
	// would arm a window nothing ever matches, so it falls back to the
	// account-wide behaviour it had before -- diverting too much is bad, but
	// silently diverting nothing while believing otherwise is worse.
	if model == "" {
		s.state.OverflowUntil = resetAt
	} else {
		s.state.ModelOverflow[model] = resetAt
	}
	s.state.LimitClaim, s.state.LastReason = claim, reason
	s.saveStateLocked()
	s.mu.Unlock()
	// Shown in Claude Code's window at each session's next prompt, so a
	// switch to the paid secondary is never silent.
	what := "requests"
	if model != "" {
		what = model + " requests"
	}
	s.addFailoverNotice(fmt.Sprintf("\u26a1 Claude Burst: %s now go to the secondary until %s, because %s", what, time.Unix(resetAt, 0).Format("15:04"), plainReason(claim, reason)))
	s.alertFailedOver(what, time.Unix(resetAt, 0), plainReason(claim, reason))
	s.logger.Printf("model=%q rejected until %s claim=%s reason=%s", model, time.Unix(resetAt, 0).Format(time.RFC3339), claim, reason)
}
