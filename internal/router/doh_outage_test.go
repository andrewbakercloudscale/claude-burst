package router

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The 2026-09-30 DoH outage, end to end: the network reset every TLS
// connection to the named DoH servers, the gateway had no other way to find
// Anthropic, and every request got a 502. These tests pin each piece of the
// fix (0c08eba) so a later change cannot quietly undo one.

// udpDNSServer answers every A query with ip, echoing the query id, and
// counts the queries it saw.
func udpDNSServer(t *testing.T, ip [4]byte) (addr string, queries func() int) {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pc.Close() })
	n := make(chan struct{}, 64)
	go func() {
		buf := make([]byte, 512)
		for {
			m, from, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			if m < 12 {
				continue
			}
			n <- struct{}{}
			_, _ = pc.WriteTo(dnsReply(binary.BigEndian.Uint16(buf[0:]), ip), from)
		}
	}()
	return pc.LocalAddr().String(), func() int { return len(n) }
}

// resettingServer closes every connection without answering, the nearest a
// test server gets to the SNI reset that took the named DoH servers out.
func resettingServer(t *testing.T) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hj, _ := w.(http.Hijacker)
		c, _, _ := hj.Hijack()
		c.Close()
	}))
	t.Cleanup(s.Close)
	return s
}

// With every DoH endpoint failing, plain DNS on port 53 is the last resort,
// and it must actually produce an address.
func TestPlainDNSAnswersWhenEveryDoHEndpointFails(t *testing.T) {
	udp, queries := udpDNSServer(t, [4]byte{160, 79, 104, 10})
	r := newInterceptResolver("api.anthropic.com", resettingServer(t).URL, "")
	r.fallbacks = []string{resettingServer(t).URL}
	r.udpServers = []string{udp}

	addrs, err := r.lookup(context.Background(), "api.anthropic.com")
	if err != nil || len(addrs) != 1 || addrs[0] != "160.79.104.10" {
		t.Fatalf("lookup = %v, %v; want the plain DNS answer", addrs, err)
	}
	if queries() != 1 {
		t.Fatalf("plain DNS server saw %d queries, want 1", queries())
	}
}

// Plain DNS is asked only after every DoH endpoint: it is unencrypted, so it
// must stay the last resort and not become the first thing asked.
func TestPlainDNSIsNotAskedWhileDoHAnswers(t *testing.T) {
	udp, queries := udpDNSServer(t, [4]byte{160, 79, 104, 99})
	good := dohServer(t, dohResponse{Answer: []dohAnswer{{Name: "api.anthropic.com", Type: 1, TTL: 60, Data: "160.79.104.10"}}}, nil)
	defer good.Close()
	r := newInterceptResolver("api.anthropic.com", good.URL, "")
	r.udpServers = []string{udp}

	addrs, err := r.lookup(context.Background(), "api.anthropic.com")
	if err != nil || len(addrs) != 1 || addrs[0] != "160.79.104.10" {
		t.Fatalf("lookup = %v, %v; want the DoH answer", addrs, err)
	}
	if queries() != 0 {
		t.Fatalf("plain DNS was asked %d times while DoH answered", queries())
	}
}

// Every resolver failing, with nothing cached, is a LookupError, and its
// message names every attempt. On 2026-09-30 the error named one server,
// which hid that it was the only one ever asked.
func TestEveryResolverFailingIsALookupErrorNamingEachAttempt(t *testing.T) {
	// A UDP port nobody listens on: the read times out or is refused.
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	deadUDP := pc.LocalAddr().String()
	pc.Close()

	first, second := resettingServer(t), resettingServer(t)
	r := newInterceptResolver("api.anthropic.com", first.URL, "")
	r.fallbacks = []string{second.URL}
	r.udpServers = []string{deadUDP}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err = r.lookup(ctx, "api.anthropic.com")
	var le *LookupError
	if !errors.As(err, &le) {
		t.Fatalf("err = %v (%T), want a *LookupError", err, err)
	}
	for _, want := range []string{first.URL, second.URL, "dns://" + deadUDP} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not name %s: %v", want, err)
		}
	}
}

// The fallbacks exist because the named servers were blocked by SNI. A
// fallback addressed by name would be blocked the same way, so every one
// must be an IP address.
func TestDefaultFallbacksAreAddressedByIP(t *testing.T) {
	for _, e := range defaultDoHFallbacks {
		u, err := url.Parse(e)
		if err != nil || u.Scheme != "https" || net.ParseIP(u.Hostname()) == nil {
			t.Errorf("DoH fallback %q must be https to a bare IP address", e)
		}
	}
	for _, s := range defaultUDPServers {
		host, port, err := net.SplitHostPort(s)
		if err != nil || port != "53" || net.ParseIP(host) == nil {
			t.Errorf("plain DNS server %q must be IP:53", s)
		}
	}
	if len(defaultDoHFallbacks) < 2 || len(defaultUDPServers) < 2 {
		t.Error("one fallback of each kind is another single point of failure")
	}
}

// withFallbacks must not ask the configured endpoint twice, and must load
// the on-disk cache so a restart on a blocking network still has an address.
func TestWithFallbacksSkipsTheConfiguredEndpointAndLoadsTheCache(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")
	seed := newInterceptResolver("api.anthropic.com", "https://unused.invalid/dns-query", "")
	seed.cachePath = resolverCachePath(statePath)
	seed.saveCache("api.anthropic.com", []string{"160.79.104.10"})

	r := newInterceptResolver("api.anthropic.com", defaultDoHFallbacks[0], "").withFallbacks(resolverCachePath(statePath))
	for _, e := range r.fallbacks {
		if e == r.endpoint {
			t.Fatalf("configured endpoint %s is also a fallback: %v", e, r.fallbacks)
		}
	}
	if len(r.fallbacks) != len(defaultDoHFallbacks)-1 {
		t.Fatalf("fallbacks = %v", r.fallbacks)
	}
	r.mu.Lock()
	e, ok := r.cache["api.anthropic.com"]
	r.mu.Unlock()
	if !ok || len(e.addrs) != 1 || e.addrs[0] != "160.79.104.10" {
		t.Fatalf("cache after withFallbacks = %+v, %v; want the saved address", e, ok)
	}
}

// A failed lookup never opened a connection, so nothing was sent: it is safe
// to resend, and it is NOT "this machine has no network". Classifying it as
// the latter is what made the DoH block a 502 on every request (ab5005d).
func TestLookupErrorIsResendableAndNotALocalNetworkFailure(t *testing.T) {
	le := &LookupError{Host: "api.anthropic.com", Err: errors.New("every resolver failed")}
	// The shape it reaches the router in: DialContext wraps it, and
	// http.Client wraps that in a *url.Error.
	wrapped := &url.Error{Op: "Post", URL: "https://api.anthropic.com/v1/messages",
		Err: fmt.Errorf("resolve api.anthropic.com over DoH (x): %w", le)}
	if !safeToResend(wrapped) {
		t.Error("a failed lookup sent nothing and must be safe to resend")
	}
	if isLocalConnectivityFailure(wrapped) {
		t.Error("a failed lookup must count towards failover, not be written off as the local network")
	}
}

// End to end in the router: lookups failing is retried through the whole
// ladder, then counts towards failover. Before 0c08eba it was never counted,
// so every request 502'd with Anthropic and the secondary both reachable.
func TestLookupFailureRetriesThenFailsOver(t *testing.T) {
	old := primaryRetryDelays
	primaryRetryDelays = []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond}
	t.Cleanup(func() { primaryRetryDelays = old })
	up := newRecordingUpstream(t)
	s, _ := newChainServer(t, up.srv.URL, nil)
	s.probe = func() netProbe { return netProbe{dnsOK: true} }
	s.primaryDetector = newMeteredFailureDetector(60, 3, 1)
	lookupFail := fmt.Errorf("resolve api.anthropic.com over DoH (x): %w",
		&LookupError{Host: "api.anthropic.com", Err: errors.New("every resolver failed")})
	ft := &flakyTransport{neverRecover: true, err: lookupFail, next: s.client.Transport}
	s.client.Transport = ft

	s.ServeHTTP(httptest.NewRecorder(), messagesRequest("claude-sonnet-5"))

	if n := len(ft.bodies); n != len(primaryRetryDelays)+1 {
		t.Fatalf("got %d primary attempts, want the original plus the ladder (%d)", n, len(primaryRetryDelays)+1)
	}
	if !s.inOverflow(time.Now()) {
		t.Fatal("a lookup that kept failing through the ladder must count and fail over")
	}
	if h := s.Health(); h.Failures == 0 || !strings.Contains(h.LastError, "every resolver failed") {
		t.Fatalf("the dashboard's health must show the lookup failure, got %+v", h)
	}
}

// The network-down path returns before the ladder. It must still mark the
// primary as failing, or the "Anthropic answering" check stays green while
// every request gets a 502.
func TestNetworkDownMarksThePrimaryFailing(t *testing.T) {
	up := newRecordingUpstream(t)
	s := chainServerWithSecondary(t, up.srv.URL, func() netProbe {
		return netProbe{dnsOK: false, dnsErr: errors.New("lookup www.apple.com: no such host")}
	})
	s.client.Transport = &flakyTransport{neverRecover: true, err: timeoutErr{}, next: s.client.Transport}

	s.ServeHTTP(httptest.NewRecorder(), messagesRequest("claude-sonnet-5"))
	s.ServeHTTP(httptest.NewRecorder(), messagesRequest("claude-sonnet-5"))

	h := s.Health()
	if h.Failures != 2 || h.FailingSince.IsZero() || h.LastFailure.IsZero() || h.LastError == "" {
		t.Fatalf("health after two network-down 502s = %+v", h)
	}
	if h.FailingSince.After(h.LastFailure) {
		t.Fatalf("failing_since must be the FIRST failure: %+v", h)
	}
}

// Health describes the primary only. A secondary answering or failing must
// not make Anthropic look up, or down.
func TestHealthIgnoresTheSecondary(t *testing.T) {
	s, _ := newChainServer(t, "http://127.0.0.1:1", nil)
	s.notePrimaryFailure("primary", errors.New("boom"))
	s.notePrimaryAnswered("secondary")
	if h := s.Health(); h.Failures != 1 {
		t.Fatalf("a secondary answer cleared the primary's failure: %+v", h)
	}
	s.notePrimaryFailure("secondary", errors.New("other"))
	if h := s.Health(); h.Failures != 1 || h.LastError != "boom" {
		t.Fatalf("a secondary failure was counted against the primary: %+v", h)
	}
	s.notePrimaryAnswered("primary")
	if h := s.Health(); h.Failures != 0 || !h.FailingSince.IsZero() || h.LastAnswer.IsZero() {
		t.Fatalf("a primary answer must clear the failure run: %+v", h)
	}
}
