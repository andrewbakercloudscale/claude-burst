package router

import (
	"net/http"
	"strings"
	"sync"
	"time"
)

// recentCapacity is how many recent responses are kept for the admin view.
const recentCapacity = 20

// RecentResponse is one upstream response's metadata, kept for inspection in
// the admin UI.
//
// IN MEMORY ONLY, and deliberately so. These headers are the ones that decide
// failover (anthropic-ratelimit-unified-status and friends), which makes them
// worth showing; they are not worth writing to disk, where they would
// accumulate indefinitely and could capture a Set-Cookie or similar. The ring
// is lost on restart, which is the right trade for a debugging view.
type RecentResponse struct {
	Time       time.Time         `json:"time"`
	RequestID  string            `json:"request_id"`
	Method     string            `json:"method"`
	Path       string            `json:"path"`
	Slot       string            `json:"slot"`
	Route      string            `json:"route"`
	Model      string            `json:"model,omitempty"`
	Status     int               `json:"status"`
	DurationMS int64             `json:"duration_ms"`
	Headers    map[string]string `json:"headers"`
	// Destination is the actual outbound URL (scheme+host+path, no query)
	// this hop was sent to -- what actually answers "did this go to
	// primary or secondary", independent of the Slot label.
	Destination string `json:"destination,omitempty"`
}

// headerAllowlist is matched case-insensitively as a prefix. Anything not
// matching is dropped rather than redacted, so a header added upstream in
// future cannot leak into the UI by default.
var headerAllowlist = []string{
	"anthropic-",     // ratelimit-unified-status, reset timestamps: the failover signals
	"ratelimit-",     // generic rate-limit headers
	"x-ratelimit-",   //
	"retry-after",    //
	"x-request-id",   // correlate with Anthropic support
	"request-id",     //
	"content-type",   //
	"x-should-retry", //
}

// headerDenylist wins over the allowlist. Belt and braces: nothing here should
// match a prefix above, but a credential leaking into a debugging UI is a bad
// enough outcome to check twice.
var headerDenylist = []string{"authorization", "cookie", "set-cookie", "x-api-key", "proxy-authorization"}

func filterHeaders(h http.Header) map[string]string {
	out := map[string]string{}
	for k, v := range h {
		lk := strings.ToLower(k)
		denied := false
		for _, d := range headerDenylist {
			if strings.Contains(lk, d) {
				denied = true
				break
			}
		}
		if denied {
			continue
		}
		for _, a := range headerAllowlist {
			if strings.HasPrefix(lk, a) {
				out[lk] = strings.Join(v, ", ")
				break
			}
		}
	}
	return out
}

type recentRing struct {
	mu    sync.Mutex
	items []RecentResponse
}

func (r *recentRing) add(item RecentResponse) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.items) < recentCapacity {
		r.items = append(r.items, item)
		return
	}
	copy(r.items, r.items[1:])
	r.items[recentCapacity-1] = item
}

// snapshot returns the ring newest-first, copied so callers cannot mutate it.
func (r *recentRing) snapshot() []RecentResponse {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]RecentResponse, 0, len(r.items))
	for i := len(r.items) - 1; i >= 0; i-- {
		out = append(out, r.items[i])
	}
	return out
}

// RecentResponses returns metadata for the most recent upstream responses,
// newest first.
func (s *Server) RecentResponses() []RecentResponse { return s.recent.snapshot() }
