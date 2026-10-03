package router

import (
	"fmt"
	"path/filepath"
	"sync"
	"time"

	"github.com/andrewbakercloudscale/claude-burst/internal/metrics"
	"github.com/andrewbakercloudscale/claude-burst/internal/notice"
)

// On-screen alerts: what the router already detects, published through
// internal/notice so the usage panel can float it over Claude Code. Each
// hook is one call at a place that already logs the same thing; this file
// holds the little state needed to say when a problem has ended.

// Kinds published from here. An ok event of a kind resolves it.
const (
	alertFailover  = "failover"
	alertLimit     = "limit"
	alertUpstream  = "upstream"
	alertNetwork   = "network"
	alertSecondary = "secondary-key"
	alertCompact   = "compaction"
	alertContext   = "context"
)

// Upstream failures: this many 5xx replies within upstreamWindow is an
// error on screen; upstreamWindow with none after that is the all clear.
var (
	upstreamFailures = 5
	upstreamWindow   = 2 * time.Minute
)

type alertState struct {
	mu          sync.Mutex
	failedOver  bool
	networkDown bool
	keyMissing  bool
	keyWorked   bool
	failures    []time.Time
	failing     bool
	lastFailure time.Time
}

// alertFailedOver is called when a model's rejection window opens.
func (s *Server) alertFailedOver(what string, until time.Time, why string) {
	if s.secondaryReady() {
		s.alerts.mu.Lock()
		s.alerts.failedOver = true
		s.alerts.mu.Unlock()
		notice.Publish(alertFailover, notice.Warn, "Failed over to "+s.secondary.Name(),
			fmt.Sprintf("%s go to the secondary until %s, because %s.", what, until.Format("15:04"), why))
		return
	}
	notice.Publish(alertLimit, notice.Warn, "Claude limit reached",
		fmt.Sprintf("%s are refused until %s, because %s. No secondary is set up, so the refusal goes to Claude Code.", what, until.Format("15:04"), why))
}

// alertBackOnPrimary is called when an outage window is released early.
func (s *Server) alertBackOnPrimary(detail string) {
	s.alerts.mu.Lock()
	s.alerts.failedOver = false
	s.alerts.mu.Unlock()
	notice.Publish(alertFailover, notice.OK, "Back on Claude", detail)
}

// alertOutcome sees every request's outcome (from writeMetric): it ends a
// failover once the primary answers with every window closed, and counts
// upstream 5xx replies.
func (s *Server) alertOutcome(slot string, status int) {
	now := time.Now()
	if slot == "primary" && status > 0 && status < 400 {
		s.alerts.mu.Lock()
		back := s.alerts.failedOver && !s.anyOverflow(now)
		if back {
			s.alerts.failedOver = false
		}
		s.alerts.mu.Unlock()
		if back {
			notice.Publish(alertFailover, notice.OK, "Back on Claude", "Every limit has reset; requests go to Anthropic again.")
		}
	}
	if status >= 500 && status != metrics.StatusClientClosed {
		s.alertUpstreamFailure(slot, now)
		return
	}
	if status > 0 && status < 400 {
		s.alertUpstreamClean(now)
	}
}

func (s *Server) alertUpstreamFailure(slot string, now time.Time) {
	s.alerts.mu.Lock()
	a := &s.alerts
	// While the network is down every request fails for that one reason,
	// which "Network offline" already says: counting them here added a
	// "Requests are failing" and a "succeeding again" to the same outage.
	if a.networkDown {
		a.mu.Unlock()
		return
	}
	a.lastFailure = now
	a.failures = append(a.failures, now)
	for len(a.failures) > 0 && now.Sub(a.failures[0]) > upstreamWindow {
		a.failures = a.failures[1:]
	}
	trip := !a.failing && len(a.failures) >= upstreamFailures
	if trip {
		a.failing = true
	}
	n := len(a.failures)
	a.mu.Unlock()
	if trip {
		notice.Publish(alertUpstream, notice.Error, "Requests are failing",
			fmt.Sprintf("%d replies failed in the last %s (latest on the %s). Claude Code retries; the dashboard's log says why.", n, upstreamWindow, slot))
	}
}

func (s *Server) alertUpstreamClean(now time.Time) {
	s.alerts.mu.Lock()
	recovered := s.alerts.failing && now.Sub(s.alerts.lastFailure) >= upstreamWindow
	if recovered {
		s.alerts.failing = false
		s.alerts.failures = nil
	}
	s.alerts.mu.Unlock()
	if recovered {
		notice.Publish(alertUpstream, notice.OK, "Requests are succeeding again", "No failures for "+upstreamWindow.String()+".")
	}
}

func (s *Server) alertNetworkDown() {
	s.alerts.mu.Lock()
	first := !s.alerts.networkDown
	s.alerts.networkDown = true
	s.alerts.mu.Unlock()
	if first {
		notice.Publish(alertNetwork, notice.Error, "Network offline",
			"DNS is failing on this Mac, so requests cannot reach Anthropic. Nothing fails over: the secondary is behind the same network.")
	}
}

func (s *Server) alertNetworkUp() {
	s.alerts.mu.Lock()
	was := s.alerts.networkDown
	s.alerts.networkDown = false
	if was {
		// Failures from before the outage belong to it too.
		s.alerts.failures = nil
	}
	s.alerts.mu.Unlock()
	if was {
		notice.Publish(alertNetwork, notice.OK, "Network back", "Anthropic is answering again.")
	}
}

// alertSecondaryKey reports the secondary's credential going missing or
// coming back (a locked Keychain times out; a deleted key is missing). Only
// a key that worked and then stopped is shown: a secondary with no key at
// all is a supported single plan setup, and saying so at every start would
// be noise.
func (s *Server) alertSecondaryKey(err error) {
	s.alerts.mu.Lock()
	warn := err != nil && s.alerts.keyWorked && !s.alerts.keyMissing
	back := err == nil && s.alerts.keyMissing
	if warn {
		s.alerts.keyMissing = true
	}
	if err == nil {
		s.alerts.keyWorked, s.alerts.keyMissing = true, false
	}
	s.alerts.mu.Unlock()
	switch {
	case warn:
		notice.Publish(alertSecondary, notice.Warn, "Secondary key unavailable",
			"The "+s.secondary.Name()+" key could not be read ("+err.Error()+"), so nothing fails over until it can.")
	case back:
		notice.Publish(alertSecondary, notice.OK, "Secondary key available", "Failover to "+s.secondary.Name()+" works again.")
	}
}

// alertContextNear says a session's context has reached the warn level
// (80% of its Compact at by default, the repository's own when it has
// one), once per session per compaction window: it is called from the
// same place, and on the same schedule, as the warn line in the log. A
// repository with compaction off never gets here, its warn level being
// NeverTokens.
func alertContextNear(sid, root string, context, compactAt int64) {
	where := "this session"
	if root != "" {
		where = filepath.Base(root)
	}
	notice.PublishFor(sid, alertContext, notice.Info,
		fmt.Sprintf("Context at %dk of %dk, compaction soon", context/1000, compactAt/1000),
		"In "+where+". Burst summarises the older history in the background at "+fmt.Sprintf("%dk", compactAt/1000)+"; nothing pauses.")
}

func (s *Server) anyOverflow(now time.Time) bool {
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
