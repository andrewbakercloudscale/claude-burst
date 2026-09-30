package admin

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/andrewbakercloudscale/claude-burst/internal/config"
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
				notifyFunc("Claude Burst: on the secondary", strings.Join(moved, ", ")+" now go to "+cfg.Secondary.Provider+" (paid). "+st.LastReason)
			}
			if len(back) > 0 && len(over) == 0 {
				notifyFunc("Claude Burst: back on your subscription", "Requests go to Anthropic again.")
			}
		}
		if nc.Compaction {
			for sid, at := range comp {
				if at.After(n.compacted[sid]) {
					short := sid
					if len(short) > 8 {
						short = short[:8]
					}
					notifyFunc("Claude Burst: compacted a session", "Session "+short+" now runs on a summary of its older history.")
				}
			}
		}
		if nc.Guards {
			if pf > n.pfEvents {
				notifyFunc("Claude Burst: pf guard acted", "The redirect was repaired or removed. Details under Guards.")
			}
			if self > n.selfEvents {
				notifyFunc("Claude Burst: gateway watchdog acted", "The gateway was restarted or the redirect removed. Details under Guards.")
			}
		}
	}
	n.started, n.overflow, n.compacted, n.pfEvents, n.selfEvents = true, over, comp, pf, self
}
