package router

import (
	"net/http"
	"regexp"
	"strconv"
	"time"
)

// A fallback route that answered "no credit" or "slow down" is skipped for a
// while, so later requests go straight to the next route instead of paying
// for the same failed call every time.
const (
	fundsRest = 15 * time.Minute // out of credit: needs a top-up, not a retry
	busyRest  = time.Minute      // rate limit or server error, unless Retry-After says more
	maxRest   = 15 * time.Minute
)

// fundsWords match the errors providers give when an account has no credit
// left: OpenRouter's 402 "Insufficient credits", OpenAI's 429
// "insufficient_quota", Anthropic's 400 "credit balance is too low",
// Together's and Groq's billing messages.
var fundsWords = regexp.MustCompile(`(?i)insufficient[_ ]?(credit|quota|fund|balance)|credit balance|out of credits?|no credits?|exceeded your current quota|billing|payment required|top up|add (more )?credits`)

// outOfFunds reports whether a provider's error says the account has no
// credit left. 402 always does; 400, 403 and 429 do when the body says so.
func outOfFunds(status int, body []byte) bool {
	switch status {
	case http.StatusPaymentRequired:
		return true
	case http.StatusBadRequest, http.StatusForbidden, http.StatusTooManyRequests:
		return fundsWords.Match(body)
	}
	return false
}

// restFor is how long a failed fallback route is skipped.
func restFor(status int, broke bool, retryAfter string) time.Duration {
	if broke {
		return fundsRest
	}
	if secs, err := strconv.Atoi(retryAfter); err == nil && secs > 0 {
		if d := time.Duration(secs) * time.Second; d < maxRest {
			return d
		}
		return maxRest
	}
	return busyRest
}

func (s *Server) restRoute(index int, d time.Duration, reason string) {
	if index < 0 || index >= len(s.fallbacks) || d <= 0 {
		return
	}
	s.restMu.Lock()
	if s.restUntil == nil {
		s.restUntil = map[int]time.Time{}
	}
	s.restUntil[index] = time.Now().Add(d)
	s.restMu.Unlock()
	s.logger.Printf("fallback route=%s index=%d resting %s: %s", s.fallbacks[index].Name(), index+2, d.Round(time.Second), reason)
}

// nextAwake returns the first fallback route at or after from that is not
// resting, or -1.
func (s *Server) nextAwake(from int) int {
	s.restMu.Lock()
	defer s.restMu.Unlock()
	now := time.Now()
	for i := from; i < len(s.fallbacks); i++ {
		if until, ok := s.restUntil[i]; ok && now.Before(until) {
			continue
		}
		return i
	}
	return -1
}
