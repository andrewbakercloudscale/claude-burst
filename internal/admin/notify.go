package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/andrewbakercloudscale/claude-burst/internal/config"
	"github.com/andrewbakercloudscale/claude-burst/internal/notice"
	"github.com/andrewbakercloudscale/claude-burst/internal/tlsca"
)

// macOS notifications for events that matter while looking at something
// else: requests moving to the paid secondary (and back), Burst compacting a
// session, and a guard repairing or removing the redirect. Polls the same
// state the dashboard reads, so nothing in the request path changes, and
// re-reads config.json each round so switching a kind on or off needs no
// restart.

// notifyFunc delivers one notification; a variable so tests record instead.
var notifyFunc = func(title, body string) error {
	script := fmt.Sprintf("display notification %s with title %s", appleQuote(body), appleQuote(title))
	return exec.Command("osascript", "-e", script).Run()
}

// notify delivers one notification and logs it. osascript exits 0 even
// when macOS drops the notification (Script Editor not allowed to notify),
// so a logged "sent" proves Burst tried, not that it appeared; the
// dashboard's test button is how to find out.
func (s *Server) notify(title, body string) {
	if err := notifyFunc(title, body); err != nil {
		s.gateway.Logf("notify FAILED title=%q err=%v", title, err)
		return
	}
	s.gateway.Logf("notify sent title=%q", title)
}

// handleNotifyTest sends one notification on demand, so someone can tell
// whether macOS shows them at all before relying on them.
func (s *Server) handleNotifyTest(w http.ResponseWriter, r *http.Request) {
	if err := notifyFunc("Claude Burst: test notification", "If you can see this, notifications from Burst reach you."); err != nil {
		http.Error(w, "osascript could not send it: "+err.Error(), http.StatusInternalServerError)
		return
	}
	s.gateway.Logf("notify sent title=%q (test from the dashboard)", "Claude Burst: test notification")
	writeJSON(w, map[string]string{"ok": "sent"})
}

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

// appleQuote makes an AppleScript string literal.
func appleQuote(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}

type notifier struct {
	started    bool
	overflow   map[string]bool // models on the secondary at the last look
	compacted  map[string]time.Time
	pfEvents   int
	selfEvents int
	intercept  interceptCheck
	alerts     alertRounds
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
	nc := cfg.Notify

	over := map[string]bool{}
	st := s.gateway.Status()
	if st.OverflowUntil > now.Unix() {
		over["all models"] = true
	}
	for m, until := range st.ModelOverflow {
		if until > now.Unix() {
			over[m] = true
		}
	}
	comp := map[string]time.Time{}
	for _, c := range s.gateway.CompactionSessions() {
		if !c.CompactedAt.IsZero() {
			comp[c.Session] = c.CompactedAt
		}
	}
	pf := len(pfHealEvents(pfHealLog, 1000))
	_, _, selfLog := selfHealPaths()
	self := len(pfHealEvents(selfLog, 1000))
	ic := readInterceptCheck(cfg)

	if n.started {
		if nc.Failover {
			var moved, back []string
			for m := range over {
				if !n.overflow[m] {
					moved = append(moved, m)
				}
			}
			for m := range n.overflow {
				if !over[m] {
					back = append(back, m)
				}
			}
			if len(moved) > 0 {
				s.notify("Claude Burst: on the secondary", strings.Join(moved, ", ")+" now go to "+cfg.Secondary.Provider+" (paid). "+st.LastReason)
			}
			if len(back) > 0 && len(over) == 0 {
				s.notify("Claude Burst: back on your subscription", "Requests go to Anthropic again.")
			}
		}
		if nc.Compaction {
			for sid, at := range comp {
				if at.After(n.compacted[sid]) {
					short := sid
					if len(short) > 8 {
						short = short[:8]
					}
					s.notify("Claude Burst: compacted a session", "Session "+short+" now runs on a summary of its older history.")
				}
			}
		}
		if nc.Guards {
			if pf > n.pfEvents {
				s.notify("Claude Burst: pf guard acted", "The redirect was repaired or removed. Details under Guards.")
			}
			if self > n.selfEvents {
				s.notify("Claude Burst: gateway watchdog acted", "The gateway was restarted or the redirect removed. Details under Guards.")
			}
		}
		// On screen, whatever the macOS notification settings: the usage
		// panel has its own switch for these.
		if pf > n.pfEvents {
			alertGuardLine("pf", lastLine(pfHealEvents(pfHealLog, 1000)))
		}
		if self > n.selfEvents {
			alertGuardLine("watchdog", lastLine(pfHealEvents(selfLog, 1000)))
		}
		alertIntercept(n.intercept, ic)
	}
	s.alertRound(&n.alerts, cfg, now)
	n.started, n.overflow, n.compacted, n.pfEvents, n.selfEvents, n.intercept = true, over, comp, pf, self, ic
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

// alertGuardLine turns a new guard log line into an alert: a repair is
// good news, anything else the guard logs is a problem it met.
func alertGuardLine(guard, line string) {
	if line == "" {
		return
	}
	name := map[string]string{"pf": "pf guard", "watchdog": "Gateway watchdog"}[guard]
	for _, good := range []string{"HEALED", "recovered", "reloaded successfully"} {
		if strings.Contains(line, good) {
			notice.Publish("guard-"+guard, notice.OK, name+" repaired the redirect", line)
			return
		}
	}
	notice.Publish("guard-"+guard, notice.Error, name+" hit a problem", line+" (details under Guards on the dashboard)")
}

func lastLine(lines []string) string {
	if len(lines) == 0 {
		return ""
	}
	return lines[len(lines)-1]
}
