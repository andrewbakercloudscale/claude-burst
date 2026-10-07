package router

import (
	"strings"
	"time"
)

// plainReason says, in one line of plain English, why Burst started sending
// requests to the paid secondary.
func plainReason(claim, reason string) string {
	switch claim {
	case "metered_sustained_failures", "metered_single_failure":
		return "the connection to Anthropic kept failing"
	case "five_hour":
		return "the subscription's five-hour limit was reached"
	case "weekly":
		return "the subscription's weekly limit was reached"
	}
	if r := strings.TrimSpace(reason); r != "" {
		return r
	}
	return "Anthropic refused the model"
}

// maxFailoverNotices is how many undelivered lines are kept.
const maxFailoverNotices = 20

// addFailoverNotice queues a line for Claude Code's window, shown at each
// session's next prompt: a switch to the secondary spends money, so it is
// never silent.
func (s *Server) addFailoverNotice(line string) {
	s.failoverMu.Lock()
	defer s.failoverMu.Unlock()
	s.failoverNotices = append(s.failoverNotices, line)
	// They wait for the next request to show them. With no session open
	// nothing takes them, so only the newest are kept.
	if n := len(s.failoverNotices); n > maxFailoverNotices {
		s.failoverNotices = append([]string(nil), s.failoverNotices[n-maxFailoverNotices:]...)
	}
}

// PrimaryHealth is what the dashboard needs to say "Anthropic is not
// answering" instead of a green 5/5: on 2026-09-30 every request got a 502
// for minutes while every check passed, because the only error check was a
// 14-day average.
type PrimaryHealth struct {
	LastAnswer   time.Time `json:"last_answer"`          // any HTTP response from the primary
	LastFailure  time.Time `json:"last_failure"`         // a transport error, after retries
	LastError    string    `json:"last_error,omitempty"` // that error, verbatim
	FailingSince time.Time `json:"failing_since"`        // first failure since the last answer
	Failures     int       `json:"failures"`             // failures since the last answer
	// NetworkDown: the failures are this Mac having no network, not
	// Anthropic refusing. The dashboard then says only that.
	NetworkDown bool `json:"network_down"`
}

func (s *Server) notePrimaryAnswered(slot string) {
	if slot != "primary" {
		return
	}
	s.healthMu.Lock()
	defer s.healthMu.Unlock()
	s.health.LastAnswer = time.Now()
	s.health.Failures = 0
	s.health.FailingSince = time.Time{}
	s.health.NetworkDown = false
	s.alertNetworkUp()
}

// webDownGrace is how recent a reply from the primary must be to overrule a
// control request that failed: a reply is traffic passing.
const webDownGrace = 15 * time.Second

func (s *Server) primaryAnsweredWithin(d time.Duration) bool {
	s.healthMu.Lock()
	defer s.healthMu.Unlock()
	return !s.health.LastAnswer.IsZero() && time.Since(s.health.LastAnswer) < d
}

func (s *Server) notePrimaryFailure(slot string, err error) {
	if slot != "primary" || err == nil {
		return
	}
	s.healthMu.Lock()
	defer s.healthMu.Unlock()
	now := time.Now()
	if s.health.Failures == 0 {
		s.health.FailingSince = now
	}
	s.health.Failures++
	s.health.LastFailure = now
	s.health.LastError = err.Error()
	s.health.NetworkDown = isLocalConnectivityFailure(err)
}

// Health returns a copy of the primary's current health.
func (s *Server) Health() PrimaryHealth {
	s.healthMu.Lock()
	defer s.healthMu.Unlock()
	h := s.health
	s.alerts.mu.Lock()
	h.NetworkDown = h.Failures > 0 && (h.NetworkDown || s.alerts.networkDown)
	s.alerts.mu.Unlock()
	return h
}

// takeFailoverNotices returns and clears the pending lines.
func (s *Server) takeFailoverNotices() []string {
	s.failoverMu.Lock()
	defer s.failoverMu.Unlock()
	out := s.failoverNotices
	s.failoverNotices = nil
	return out
}
