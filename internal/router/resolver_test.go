package router

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func dohServer(t *testing.T, body dohResponse, calls *int32) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls != nil {
			atomic.AddInt32(calls, 1)
		}
		if got := r.URL.Query().Get("name"); got == "" {
			t.Errorf("DoH request had no name parameter")
		}
		if got := r.Header.Get("Accept"); got != "application/dns-json" {
			t.Errorf("Accept = %q, want application/dns-json", got)
		}
		w.Header().Set("Content-Type", "application/dns-json")
		json.NewEncoder(w).Encode(body)
	}))
}

func TestQueryDoHParsesARecords(t *testing.T) {
	srv := dohServer(t, dohResponse{
		Status: 0,
		Answer: []dohAnswer{
			{Name: "api.anthropic.com", Type: 5, TTL: 60, Data: "cname.target."}, // CNAME, ignored
			{Name: "api.anthropic.com", Type: 1, TTL: 120, Data: "160.79.104.10"},
			{Name: "api.anthropic.com", Type: 1, TTL: 90, Data: "160.79.104.11"},
		},
	}, nil)
	defer srv.Close()

	r := newInterceptResolver("api.anthropic.com", srv.URL, "")
	addrs, ttl, err := r.queryDoH(context.Background(), "api.anthropic.com")
	if err != nil {
		t.Fatalf("queryDoH: %v", err)
	}
	if len(addrs) != 2 || addrs[0] != "160.79.104.10" || addrs[1] != "160.79.104.11" {
		t.Fatalf("addrs = %v", addrs)
	}
	if ttl != 90*time.Second { // smallest TTL among the A records
		t.Errorf("ttl = %v, want 90s", ttl)
	}
}

// The loop this whole type exists to prevent: if DNS ever answers with
// loopback, dialing it would send the gateway's upstream request back into the
// gateway. It must be an error, not a hang.
func TestQueryDoHRejectsLoopbackAnswer(t *testing.T) {
	srv := dohServer(t, dohResponse{
		Status: 0,
		Answer: []dohAnswer{{Type: 1, TTL: 60, Data: "127.0.0.1"}},
	}, nil)
	defer srv.Close()

	r := newInterceptResolver("api.anthropic.com", srv.URL, "")
	_, _, err := r.queryDoH(context.Background(), "api.anthropic.com")
	if err == nil {
		t.Fatal("want error for a loopback answer, got nil")
	}
	if !strings.Contains(err.Error(), "loopback") {
		t.Errorf("error should name the problem, got: %v", err)
	}
}

func TestQueryDoHNonZeroStatusIsAnError(t *testing.T) {
	srv := dohServer(t, dohResponse{Status: 3}, nil) // NXDOMAIN
	defer srv.Close()

	r := newInterceptResolver("api.anthropic.com", srv.URL, "")
	if _, _, err := r.queryDoH(context.Background(), "api.anthropic.com"); err == nil {
		t.Fatal("want error for DNS status 3, got nil")
	}
}

func TestLookupCachesWithinTTL(t *testing.T) {
	var calls int32
	srv := dohServer(t, dohResponse{
		Status: 0,
		Answer: []dohAnswer{{Type: 1, TTL: 120, Data: "160.79.104.10"}},
	}, &calls)
	defer srv.Close()

	r := newInterceptResolver("api.anthropic.com", srv.URL, "")
	for i := 0; i < 5; i++ {
		if _, err := r.lookup(context.Background(), "api.anthropic.com"); err != nil {
			t.Fatalf("lookup %d: %v", i, err)
		}
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("made %d DoH queries for 5 lookups, want 1", got)
	}
}

func TestLookupRefreshesAfterTTL(t *testing.T) {
	var calls int32
	srv := dohServer(t, dohResponse{
		Status: 0,
		Answer: []dohAnswer{{Type: 1, TTL: 60, Data: "160.79.104.10"}},
	}, &calls)
	defer srv.Close()

	now := time.Now()
	r := newInterceptResolver("api.anthropic.com", srv.URL, "")
	r.now = func() time.Time { return now }

	if _, err := r.lookup(context.Background(), "api.anthropic.com"); err != nil {
		t.Fatal(err)
	}
	now = now.Add(61 * time.Second)
	if _, err := r.lookup(context.Background(), "api.anthropic.com"); err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Errorf("made %d DoH queries across a TTL boundary, want 2", got)
	}
}

// A short CDN TTL must not turn into a DoH round-trip per request.
func TestTTLFloorIsApplied(t *testing.T) {
	var calls int32
	srv := dohServer(t, dohResponse{
		Status: 0,
		Answer: []dohAnswer{{Type: 1, TTL: 1, Data: "160.79.104.10"}},
	}, &calls)
	defer srv.Close()

	now := time.Now()
	r := newInterceptResolver("api.anthropic.com", srv.URL, "")
	r.now = func() time.Time { return now }

	if _, err := r.lookup(context.Background(), "api.anthropic.com"); err != nil {
		t.Fatal(err)
	}
	now = now.Add(5 * time.Second) // past the advertised TTL, inside the floor
	if _, err := r.lookup(context.Background(), "api.anthropic.com"); err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("made %d DoH queries, want 1 (TTL floor of %v should apply)", got, minDNSTTL)
	}
}

// Only the intercepted hostname goes via DoH; everything else must keep using
// the standard dialer, so redirecting one name does not route unrelated
// traffic through a third-party resolver.
func TestDialContextOnlyDivertsTheInterceptedHost(t *testing.T) {
	var calls int32
	srv := dohServer(t, dohResponse{
		Status: 0,
		Answer: []dohAnswer{{Type: 1, TTL: 60, Data: "203.0.113.1"}},
	}, &calls)
	defer srv.Close()

	// A real local listener to dial for the non-intercepted case.
	ln := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer ln.Close()
	target := strings.TrimPrefix(ln.URL, "http://")

	r := newInterceptResolver("api.anthropic.com", srv.URL, "")
	conn, err := r.DialContext(context.Background(), "tcp", target)
	if err != nil {
		t.Fatalf("dialing a non-intercepted host failed: %v", err)
	}
	conn.Close()

	if got := atomic.LoadInt32(&calls); got != 0 {
		t.Errorf("DoH was queried %d times for an unrelated host, want 0", got)
	}
}

func TestDialContextUsesPinnedAddress(t *testing.T) {
	var calls int32
	srv := dohServer(t, dohResponse{Status: 0, Answer: []dohAnswer{{Type: 1, TTL: 60, Data: "203.0.113.1"}}}, &calls)
	defer srv.Close()

	ln := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer ln.Close()
	host, port, _ := strings.Cut(strings.TrimPrefix(ln.URL, "http://"), ":")

	r := newInterceptResolver("api.anthropic.com", srv.URL, host)
	conn, err := r.DialContext(context.Background(), "tcp", "api.anthropic.com:"+port)
	if err != nil {
		t.Fatalf("pinned dial failed: %v", err)
	}
	conn.Close()

	if got := atomic.LoadInt32(&calls); got != 0 {
		t.Errorf("DoH queried %d times despite a pinned address, want 0", got)
	}
}

// A long-poll that withholds response headers must not be killed by
// ResponseHeaderTimeout. That timeout stays on the inference client, where it
// is load-bearing for metered-failures failover, but control-plane paths
// (Remote Control registers, then polls for work) need it absent.
func TestClientForSeparatesInferenceFromPassthrough(t *testing.T) {
	bounded := &http.Client{}
	unbounded := &http.Client{}
	s := &Server{client: bounded, passthroughClient: unbounded}

	if got := s.clientFor("/v1/messages"); got != bounded {
		t.Error("/v1/messages must use the bounded client")
	}
	if got := s.clientFor("/v1/messages?beta=true"); got != bounded {
		t.Error("/v1/messages with a query must use the bounded client")
	}
	if got := s.clientFor("/api/hello"); got != unbounded {
		t.Error("control-plane path must use the unbounded client")
	}

	// A Server built without passthroughClient (as older tests do) must not panic.
	legacy := &Server{client: bounded}
	if got := legacy.clientFor("/api/hello"); got != bounded {
		t.Error("nil passthroughClient should fall back to the bounded client")
	}
}
