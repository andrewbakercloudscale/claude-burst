package router

import (
	"bytes"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/andrewbakercloudscale/claude-burst/internal/config"
)

// A laptop moving between networks leaves a dead keep-alive connection in the
// pool. The first write onto it fails with EPIPE -- observed live at 17:48 on
// 2026-09-21 as `write tcp ...: write: broken pipe` -- and that used to count
// as an Anthropic failure.
func writeEPIPE() error {
	return &net.OpError{Op: "write", Net: "tcp", Err: &os.SyscallError{Syscall: "write", Err: syscall.EPIPE}}
}

// flakyTransport fails the first n calls with err, then delegates. It also
// records every request body, which is the point: a retry must resend the
// whole request intact.
type flakyTransport struct {
	mu     sync.Mutex
	fail   int
	err    error
	bodies []string
	next   http.RoundTripper
}

func (f *flakyTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	b, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	f.bodies = append(f.bodies, string(b))
	fail := f.fail > 0
	if fail {
		f.fail--
	}
	f.mu.Unlock()
	if fail {
		return nil, f.err
	}
	r.Body = io.NopCloser(bytes.NewReader(b))
	return f.next.RoundTrip(r)
}

func TestStaleConnectionWriteIsRetriedOnceAndDoesNotCountAsAFailure(t *testing.T) {
	up := newRecordingUpstream(t)
	s, logBuf := newChainServer(t, up.srv.URL, nil)
	ft := &flakyTransport{fail: 1, err: writeEPIPE(), next: s.client.Transport}
	s.client.Transport = ft

	rr := httptest.NewRecorder()
	s.ServeHTTP(rr, messagesRequest("claude-sonnet-5"))

	if rr.Code != http.StatusOK {
		t.Fatalf("the retry on a fresh connection should have succeeded, got %d: %s", rr.Code, rr.Body.String())
	}
	if len(ft.bodies) != 2 || ft.bodies[0] != ft.bodies[1] || !strings.Contains(ft.bodies[1], "claude-sonnet-5") {
		t.Fatalf("the request must be resent whole and unchanged: %q", ft.bodies)
	}
	if s.inOverflow(time.Now()) {
		t.Fatal("a dead pooled connection is not evidence about the model; nothing may be armed")
	}
	if !strings.Contains(logBuf.String(), "one more attempt on a fresh connection") {
		t.Fatalf("the retry was silent:\n%s", logBuf.String())
	}
}

// One retry, not a loop: a second failure is a real one.
func TestStaleWriteRetryIsBoundedToOne(t *testing.T) {
	up := newRecordingUpstream(t)
	s, _ := newChainServer(t, up.srv.URL, nil)
	ft := &flakyTransport{fail: 5, err: writeEPIPE(), next: s.client.Transport}
	s.client.Transport = ft
	s.probe = func() netProbe { return netProbe{dnsOK: true} }

	s.ServeHTTP(httptest.NewRecorder(), messagesRequest("claude-sonnet-5"))
	if len(ft.bodies) != 2 {
		t.Fatalf("expected the original attempt plus exactly one retry, got %d attempts", len(ft.bodies))
	}
}

// A read-side reset gives no guarantee the server did not process the request,
// so resending could run one generation twice. Only writes are retried.
func TestReadSideResetIsNotRetried(t *testing.T) {
	up := newRecordingUpstream(t)
	s, _ := newChainServer(t, up.srv.URL, nil)
	readReset := &net.OpError{Op: "read", Net: "tcp", Err: &os.SyscallError{Syscall: "read", Err: syscall.ECONNRESET}}
	ft := &flakyTransport{fail: 1, err: readReset, next: s.client.Transport}
	s.client.Transport = ft
	s.probe = func() netProbe { return netProbe{dnsOK: true} }

	s.ServeHTTP(httptest.NewRecorder(), messagesRequest("claude-sonnet-5"))
	if len(ft.bodies) != 1 {
		t.Fatalf("a read-side reset must not be replayed on the same primary, got %d attempts", len(ft.bodies))
	}
}

func TestIsStaleWriteFailureClassification(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"write EPIPE", writeEPIPE(), true},
		{"write ECONNRESET", &net.OpError{Op: "write", Err: &os.SyscallError{Err: syscall.ECONNRESET}}, true},
		{"read ECONNRESET", &net.OpError{Op: "read", Err: &os.SyscallError{Err: syscall.ECONNRESET}}, false},
		{"dial refused", &net.OpError{Op: "dial", Err: &os.SyscallError{Err: syscall.ECONNREFUSED}}, false},
		{"plain", errors.New("boom"), false},
	}
	for _, c := range cases {
		if got := isStaleWriteFailure(c.err); got != c.want {
			t.Errorf("%s: got %v want %v", c.name, got, c.want)
		}
	}
}

// Silence from the far side while this machine cannot resolve any name is the
// network being down. Failing over then waits on a second dead host: four
// Together timeouts in a row, five minutes, 2026-09-21.
type timeoutErr struct{}

func (timeoutErr) Error() string   { return "net/http: timeout awaiting response headers" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }

func chainServerWithSecondary(t *testing.T, primaryURL string, probe func() netProbe) *Server {
	t.Helper()
	t.Setenv("AWS_BEARER_TOKEN_BEDROCK", "test-key")
	cfg := config.Default()
	cfg.AnthropicBaseURL = primaryURL
	cfg.BedrockBaseURL = "http://127.0.0.1:1" // nothing listens: the secondary is unreachable
	cfg.FallbackChain = nil
	dir := t.TempDir()
	s, err := New(cfg, filepath.Join(dir, "state.json"), filepath.Join(dir, "metrics.jsonl"), log.New(&bytes.Buffer{}, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	s.probe = probe
	return s
}

func TestNetworkDownDoesNotFailOverAndArmsNothing(t *testing.T) {
	up := newRecordingUpstream(t)
	s := chainServerWithSecondary(t, up.srv.URL, func() netProbe {
		return netProbe{dnsOK: false, dnsErr: errors.New("lookup www.apple.com: no such host")}
	})
	s.client.Transport = &flakyTransport{fail: 99, err: timeoutErr{}, next: s.client.Transport}

	rr := httptest.NewRecorder()
	start := time.Now()
	s.ServeHTTP(rr, messagesRequest("claude-sonnet-5"))

	if rr.Code != http.StatusBadGateway || !strings.Contains(rr.Body.String(), "local network unavailable") {
		t.Fatalf("want a fast, explicit 502, got %d: %s", rr.Code, rr.Body.String())
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("it must not wait on a second dead host, took %v", time.Since(start))
	}
	if s.inOverflow(time.Now()) {
		t.Fatal("a laptop changing WiFi must not put any model in a rejection window")
	}
}

// The other half of the scoping: a refused connection is a peer answering, so
// the machine's network is demonstrably working and failover stays available
// for a genuine Anthropic outage -- even if the control lookup happened to fail.
func TestRefusedConnectionStillFailsOverEvenIfControlDNSFailed(t *testing.T) {
	var mu sync.Mutex
	secondaryHits := 0
	secondary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		secondaryHits++
		mu.Unlock()
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`{"type":"message","content":[]}`))
	}))
	t.Cleanup(secondary.Close)

	t.Setenv("AWS_BEARER_TOKEN_BEDROCK", "test-key")
	cfg := config.Default()
	cfg.AnthropicBaseURL = "http://127.0.0.1:1"
	cfg.BedrockBaseURL = secondary.URL
	// The strategy the live machine runs. Plain "subscription-limit" never fails
	// over on a transport error, which would make this test pass for the wrong
	// reason.
	cfg.Primary = config.RouteConfig{Provider: "oauth-passthrough", BaseURL: "http://127.0.0.1:1", FailoverStrategy: "subscription-limit+metered-failures"}
	dir := t.TempDir()
	s, err := New(cfg, filepath.Join(dir, "state.json"), filepath.Join(dir, "metrics.jsonl"), log.New(&bytes.Buffer{}, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	s.probe = func() netProbe { return netProbe{dnsOK: false} }

	s.ServeHTTP(httptest.NewRecorder(), messagesRequest("claude-sonnet-5"))

	mu.Lock()
	defer mu.Unlock()
	if secondaryHits != 1 {
		t.Fatalf("a refused connection is a peer answering; failover must still reach the secondary, got %d hits", secondaryHits)
	}
}

// An outage has no reset time to read. Holding it for the 5-minute unknown-reset
// default (sized for a rate limit) kept steering a recovered primary's traffic
// to a secondary. It is held only for the metered window.
func TestOutageWindowIsShortAndRateLimitWindowIsNot(t *testing.T) {
	s, _ := newChainServer(t, "http://127.0.0.1:1", nil)
	now := time.Now().Unix()

	s.activateOverflow("claude-sonnet-5", 0, "metered_sustained_failures", "1 failure")
	outage := s.state.ModelOverflow["claude-sonnet-5"] - now
	if outage > int64(s.cfg.MeteredFailover.WindowSeconds+s.cfg.ResetGraceSeconds)+2 {
		t.Fatalf("an outage window should last about %ds, lasted %ds", s.cfg.MeteredFailover.WindowSeconds, outage)
	}

	s.activateOverflow("claude-opus-5", 0, "five_hour", "no reset header")
	limit := s.state.ModelOverflow["claude-opus-5"] - now
	if limit < int64(s.cfg.UnknownResetSeconds) {
		t.Fatalf("a rate-limit window keeps the unknown-reset default (%ds), got %ds", s.cfg.UnknownResetSeconds, limit)
	}
}

// The 17:49-17:53 sequence: an outage window sent every retry to a secondary
// that could not answer. When the secondary fails at the transport level the
// outage window is released, so the next request tries the primary again.
func TestFailingSecondaryReleasesAnOutageWindow(t *testing.T) {
	up := newRecordingUpstream(t)
	s := chainServerWithSecondary(t, up.srv.URL, func() netProbe { return netProbe{dnsOK: true} })
	s.activateOverflow("claude-sonnet-5", 0, "metered_sustained_failures", "1 failure")
	if !s.modelInOverflow("claude-sonnet-5", time.Now()) {
		t.Fatal("setup: window should be armed")
	}

	s.ServeHTTP(httptest.NewRecorder(), messagesRequest("claude-sonnet-5")) // goes to the dead secondary

	if s.modelInOverflow("claude-sonnet-5", time.Now()) {
		t.Fatal("the secondary failed at the transport level; the outage window must be released")
	}
	rr := httptest.NewRecorder()
	s.ServeHTTP(rr, messagesRequest("claude-sonnet-5"))
	if got := up.models(); len(got) == 0 {
		t.Fatalf("the next request should reach the primary again, got %d (%s)", rr.Code, rr.Body.String())
	}
}

// A rate-limit window is different: the primary would only refuse again, so a
// failing secondary must not release it.
func TestFailingSecondaryDoesNotReleaseARateLimitWindow(t *testing.T) {
	up := newRecordingUpstream(t)
	s := chainServerWithSecondary(t, up.srv.URL, func() netProbe { return netProbe{dnsOK: true} })
	s.activateOverflow("claude-sonnet-5", time.Now().Add(time.Hour).Unix(), "seven_day_overage_included", "rejected")

	s.ServeHTTP(httptest.NewRecorder(), messagesRequest("claude-sonnet-5"))

	if !s.modelInOverflow("claude-sonnet-5", time.Now()) {
		t.Fatal("a genuine rate-limit window must survive the secondary failing")
	}
}
