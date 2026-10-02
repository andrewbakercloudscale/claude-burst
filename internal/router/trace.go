package router

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"strings"
	"sync"
	"time"
)

// The dashboard's "Send test message" sends one real request down the same
// path Claude Code's traffic takes (/etc/hosts, pf, this listener's TLS) and
// then needs to know what the gateway did with THAT request: where it routed
// it and why, and every upstream hop it made. Log lines and the recent ring
// are shared with live traffic, so instead the admin registers a one-off
// token, sends it in TraceHeader, and collects the record afterwards.
//
// Only tokens the admin registered are honoured, they expire, and the header
// is removed before anything is forwarded, so it never reaches a provider.

// TraceHeader carries a token from BeginTrace.
const TraceHeader = "X-Claude-Burst-Trace"

// traceTTL bounds how long an uncollected trace is kept.
const traceTTL = 2 * time.Minute

// TraceHop is one upstream attempt the gateway made for a traced request,
// recorded where the attempt's outcome is written to metrics, so every exit
// path (success, upstream error, transport error, failover) produces one.
type TraceHop struct {
	Slot           string `json:"slot"`
	Route          string `json:"route"`
	Model          string `json:"model,omitempty"`
	RequestedModel string `json:"requested_model,omitempty"`
	Status         int    `json:"status"`
	DurationMS     int64  `json:"duration_ms"`
	Note           string `json:"note,omitempty"`
	Destination    string `json:"destination,omitempty"`
}

// Trace is what the gateway did with one traced request.
type Trace struct {
	RequestID string `json:"request_id"`
	// Slot and Reason are the routing decision made before the first hop.
	Slot   string     `json:"slot"`
	Reason string     `json:"reason"`
	Hops   []TraceHop `json:"hops"`
}

type traceRecord struct {
	mu      sync.Mutex
	t       Trace
	created time.Time
	seen    bool
}

type traceRegistry struct {
	traceMu sync.Mutex
	traces  map[string]*traceRecord
}

type traceCtxKey struct{}

// BeginTrace registers a token for one traced request.
func (s *Server) BeginTrace() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	tok := hex.EncodeToString(b[:])
	s.traceMu.Lock()
	defer s.traceMu.Unlock()
	if s.traces == nil {
		s.traces = map[string]*traceRecord{}
	}
	now := time.Now()
	for k, r := range s.traces {
		if now.Sub(r.created) > traceTTL {
			delete(s.traces, k)
		}
	}
	s.traces[tok] = &traceRecord{created: now}
	return tok
}

// EndTrace returns and forgets the trace for tok. ok is false when no
// request carrying the token reached the gateway: the request died before it
// got here (DNS, pf, TLS), which is itself the answer.
func (s *Server) EndTrace(tok string) (Trace, bool) {
	s.traceMu.Lock()
	r := s.traces[tok]
	delete(s.traces, tok)
	s.traceMu.Unlock()
	if r == nil {
		return Trace{}, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	t := r.t
	t.Hops = append([]TraceHop(nil), r.t.Hops...)
	return t, r.seen
}

// withTrace strips TraceHeader from r and, when it names a registered
// token, returns r with the record attached to its context.
func (s *Server) withTrace(r *http.Request, rid string) *http.Request {
	tok := r.Header.Get(TraceHeader)
	if tok == "" {
		return r
	}
	r.Header.Del(TraceHeader)
	s.traceMu.Lock()
	rec := s.traces[tok]
	s.traceMu.Unlock()
	if rec == nil {
		return r
	}
	rec.mu.Lock()
	rec.seen = true
	rec.t.RequestID = rid
	rec.mu.Unlock()
	return r.WithContext(context.WithValue(r.Context(), traceCtxKey{}, rec))
}

func traceFrom(ctx context.Context) *traceRecord {
	rec, _ := ctx.Value(traceCtxKey{}).(*traceRecord)
	return rec
}

// traceRoute records the routing decision. The first call wins: a downgrade
// or failover later is a hop, not a new decision.
func traceRoute(ctx context.Context, slot, reason string) {
	rec := traceFrom(ctx)
	if rec == nil {
		return
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if rec.t.Slot == "" {
		rec.t.Slot, rec.t.Reason = slot, reason
	}
}

func traceHop(ctx context.Context, h TraceHop) {
	rec := traceFrom(ctx)
	if rec == nil {
		return
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	rec.t.Hops = append(rec.t.Hops, h)
}

// clientAuth is the credential Claude Code sent on its most recent inference
// request. IN MEMORY ONLY, never logged or served: it exists so the
// dashboard's test message can authenticate exactly as Claude Code does,
// through the same passthrough, without a credential of its own. The
// gateway already holds these headers for the life of every request it
// forwards; this keeps the newest set a little longer.
type clientAuth struct {
	authMu  sync.Mutex
	authHdr http.Header
	authAt  time.Time
}

// credentialHeaders are the request headers that make up Claude Code's
// authentication to Anthropic.
var credentialHeaders = []string{"Authorization", "X-Api-Key", "Anthropic-Beta", "Anthropic-Version"}

// authBetas keeps the betas that belong to the credential (oauth-*).
func authBetas(values []string) []string {
	var out []string
	for _, v := range values {
		for _, b := range strings.Split(v, ",") {
			if b = strings.TrimSpace(b); strings.HasPrefix(b, "oauth-") {
				out = append(out, b)
			}
		}
	}
	return out
}

func (s *Server) noteClientAuth(h http.Header) {
	if h.Get("Authorization") == "" && h.Get("X-Api-Key") == "" {
		return
	}
	c := http.Header{}
	for _, k := range credentialHeaders {
		if v := h.Values(k); len(v) > 0 {
			c[k] = append([]string(nil), v...)
		}
	}
	// Anthropic-Beta carries the oauth beta the token needs, beside betas
	// for the request in hand. Those belong to Claude Code's model, not the
	// test message's: context-1m from an Opus [1m] session sent with Haiku
	// came back 400 "long context beta is not yet available".
	if betas := authBetas(c.Values("Anthropic-Beta")); len(betas) > 0 {
		c.Set("Anthropic-Beta", strings.Join(betas, ","))
	} else {
		c.Del("Anthropic-Beta")
	}
	s.authMu.Lock()
	s.authHdr, s.authAt = c, time.Now()
	s.authMu.Unlock()
}

// ClientCredential returns a copy of the credential headers Claude Code sent
// most recently, and when; nil when none has been seen since startup.
func (s *Server) ClientCredential() (http.Header, time.Time) {
	s.authMu.Lock()
	defer s.authMu.Unlock()
	if s.authHdr == nil {
		return nil, time.Time{}
	}
	return s.authHdr.Clone(), s.authAt
}

// ResolveUpstream answers what the intercepted host really resolves to,
// bypassing /etc/hosts, the same way the gateway's own upstream dials do.
// ok is false outside transparent mode, where nothing is intercepted.
func (s *Server) ResolveUpstream(ctx context.Context) (host string, addrs []string, ok bool, err error) {
	if s.resolver == nil {
		return "", nil, false, nil
	}
	if s.resolver.pinned != "" {
		return s.resolver.host, []string{s.resolver.pinned}, true, nil
	}
	addrs, err = s.resolver.lookup(ctx, strings.ToLower(s.resolver.host))
	return s.resolver.host, addrs, true, err
}
