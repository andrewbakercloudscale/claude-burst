package admin

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/andrewbakercloudscale/claude-burst/internal/config"
	"github.com/andrewbakercloudscale/claude-burst/internal/notice"
	"github.com/andrewbakercloudscale/claude-burst/internal/tlsca"
)

// The on-screen alerts the usage panel shows (internal/notice), for what
// this gateway runs into. Polls the same state the dashboard reads, so
// nothing in the request path changes. Burst used to send macOS
// notifications through osascript as well; macOS filed them under Script
// Editor and dropped them silently until Script Editor had been allowed to
// notify, so they were removed on 3 Oct 2026 in favour of the alerts.

// handleAlertTest publishes one on-screen alert, so someone can see what
// they look like and that the usage panel shows them. Each press shows:
// the title carries the time, so the repeat limit never holds one back.
func (s *Server) handleAlertTest(w http.ResponseWriter, r *http.Request) {
	if notice.Default() == nil {
		http.Error(w, "on-screen alerts are not running in this gateway", http.StatusServiceUnavailable)
		return
	}
	notice.Publish("test", notice.Info, "Test alert "+time.Now().Format("15:04:05"),
		"If you can see this over Claude Code, gateway alerts reach you.")
	writeJSON(w, map[string]string{"ok": "sent"})
}

// handleAlertPublish puts up an alert for a helper that runs outside the
// gateway, such as the handover writer: {"kind", "severity", "title",
// "detail"}. Kind is prefixed "ext-" so it can never stand in for one of
// the gateway's own kinds, which resolve each other.
func (s *Server) handleAlertPublish(w http.ResponseWriter, r *http.Request) {
	if notice.Default() == nil {
		http.Error(w, "on-screen alerts are not running in this gateway", http.StatusServiceUnavailable)
		return
	}
	var req struct{ Kind, Severity, Title, Detail string }
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad JSON: "+err.Error(), http.StatusBadRequest)
		return
	}
	switch req.Severity {
	case notice.Info, notice.OK, notice.Warn, notice.Error:
	default:
		http.Error(w, "severity must be info, ok, warn or error", http.StatusBadRequest)
		return
	}
	if req.Title == "" || len(req.Title) > 200 || len(req.Detail) > 1000 || len(req.Kind) > 40 {
		http.Error(w, "title is required; title at most 200 characters, detail 1000, kind 40", http.StatusBadRequest)
		return
	}
	notice.Publish("ext-"+req.Kind, req.Severity, req.Title, req.Detail)
	writeJSON(w, map[string]string{"ok": "sent"})
}

// handleAlertSpend saves the daily spend level for the on-screen alert:
// {"usd": N}, 0 to turn it off. Read live by the notifier.
func (s *Server) handleAlertSpend(w http.ResponseWriter, r *http.Request) {
	var req struct {
		USD *float64 `json:"usd"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.USD == nil {
		http.Error(w, `body must be {"usd": <dollars>}, 0 for off`, http.StatusBadRequest)
		return
	}
	if *req.USD < 0 || *req.USD > 100000 || math.IsNaN(*req.USD) {
		http.Error(w, "the spend level must be between 0 (off) and 100000 dollars", http.StatusBadRequest)
		return
	}
	if err := config.Update(func(c *config.Config) error {
		c.AlertDailySpendUSD = *req.USD
		return nil
	}); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.gateway.Logf("alert: daily spend level set to $%v (0 is off)", *req.USD)
	writeJSON(w, map[string]float64{"usd": *req.USD})
}

type notifier struct {
	started bool
	// guardBroken is which guards have a problem on screen, so their repair
	// is shown and an unseen one is not.
	guardBroken map[string]bool
	pfEvents    int
	selfEvents  int
	intercept   interceptCheck
	alerts      alertRounds
}

// StartNotifier runs until ctx ends.
func (s *Server) StartNotifier(ctx context.Context) {
	n := &notifier{}
	t := time.NewTicker(10 * time.Second)
	defer t.Stop()
	for {
		s.notifyRound(n, time.Now())
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// notifyRound compares now with the last look. The first round only records
// a baseline: starting the gateway must not announce everything that is
// already true.
func (s *Server) notifyRound(n *notifier, now time.Time) {
	cfg, err := config.Load()
	if err != nil {
		return
	}

	pf := len(pfHealEvents(pfHealLog, 1000))
	_, _, selfLog := selfHealPaths()
	self := len(pfHealEvents(selfLog, 1000))
	ic := readInterceptCheck(cfg)

	if n.started {
		// The usage panel has its own switch for these.
		if pf > n.pfEvents {
			n.alertGuardLine("pf", lastLine(pfHealEvents(pfHealLog, 1000)))
		}
		if self > n.selfEvents {
			n.alertGuardLine("watchdog", lastLine(pfHealEvents(selfLog, 1000)))
		}
		alertIntercept(n.intercept, ic)
	}
	s.alertRound(&n.alerts, cfg, now)
	n.started, n.pfEvents, n.selfEvents, n.intercept = true, pf, self, ic
}

// interceptCheck is what transparent mode needs to work, read from disk
// each round: the /etc/hosts redirect, and the local CA in the bundle
// Claude Code trusts.
type interceptCheck struct {
	on        bool // transparent mode configured
	hosts, ca bool
}

func readInterceptCheck(cfg config.Config) interceptCheck {
	if !cfg.Intercept.Transparent() {
		return interceptCheck{}
	}
	ic := interceptCheck{on: true}
	if b, err := os.ReadFile(cfg.Intercept.CABundle); err == nil {
		ic.ca = tlsca.HasBlock(string(b))
	}
	if h, err := os.ReadFile(hostsFile); err == nil {
		ic.hosts = config.HostsRedirectActive(h, cfg.Intercept.Host)
	}
	return ic
}

// hostsFile is /etc/hosts; a variable for tests.
var hostsFile = "/etc/hosts"

// alertIntercept puts a change in transparent mode's two prerequisites on
// screen. Only changes: a Mac set up without them says so on the
// dashboard, not in a popup every ten seconds.
func alertIntercept(was, now interceptCheck) {
	if !was.on || !now.on {
		return
	}
	switch {
	case was.ca && !now.ca:
		notice.Publish("intercept", notice.Error, "Burst CA no longer trusted",
			"Claude Code's certificate bundle lost the Burst CA, so its requests fail TLS. Reinstall transparent mode from the dashboard.")
	case was.hosts && !now.hosts:
		notice.Publish("intercept", notice.Error, "Transparent redirect missing",
			"/etc/hosts no longer sends Claude Code to Burst, so it talks to Anthropic directly. Reinstall transparent mode from the dashboard.")
	case now.ca && now.hosts && !(was.ca && was.hosts):
		notice.Publish("intercept", notice.OK, "Transparent mode restored", "Claude Code is routed through Burst again.")
	}
}

// alertGuardLine turns a new guard log line into an alert: anything other
// than a repair is a problem the guard met. A repair is shown only when its
// problem was: a guard that fixes a dropped pf rule within its own round
// (a network reconnect does that) did its job unseen, and a green "repaired"
// popup for a break nobody saw was one more alert in a burst of five.
func (n *notifier) alertGuardLine(guard, line string) {
	if line == "" {
		return
	}
	name := map[string]string{"pf": "pf guard", "watchdog": "Gateway watchdog"}[guard]
	for _, good := range []string{"HEALED", "recovered", "reloaded successfully"} {
		if strings.Contains(line, good) {
			if n.guardBroken[guard] {
				delete(n.guardBroken, guard)
				notice.Publish("guard-"+guard, notice.OK, name+" repaired the redirect", line)
			}
			return
		}
	}
	if n.guardBroken == nil {
		n.guardBroken = map[string]bool{}
	}
	n.guardBroken[guard] = true
	notice.Publish("guard-"+guard, notice.Error, name+" hit a problem", line+" (details under Guards on the dashboard)")
}

func lastLine(lines []string) string {
	if len(lines) == 0 {
		return ""
	}
	return lines[len(lines)-1]
}
