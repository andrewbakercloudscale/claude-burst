package router

import (
	"bytes"
	"log"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/andrewbakercloudscale/claude-burst/internal/config"
)

// failingUpstream answers the first `fail` model requests with status, then 200.
func failingUpstream(t *testing.T, fail, status int) (*httptest.Server, func() int) {
	t.Helper()
	var mu sync.Mutex
	n := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		n++
		k := n
		mu.Unlock()
		if k <= fail {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"type":"error","error":{"type":"api_error","message":"Internal server error"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","content":[],"usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	t.Cleanup(srv.Close)
	return srv, func() int { mu.Lock(); defer mu.Unlock(); return n }
}

// A 500 that clears within Burst's waits never reaches Claude Code: on
// 2026-11-03 at 18:05 three bursts of two 500s each put "API Error: 500" on
// a phone, Claude Code's own retries being over within two seconds.
func TestServerErrorIsResentAfterAWait(t *testing.T) {
	srv, calls := failingUpstream(t, 2, http.StatusInternalServerError)
	s, logBuf := newChainServer(t, srv.URL, nil)
	rr := httptest.NewRecorder()
	s.ServeHTTP(rr, messagesRequest("claude-opus-5-5"))
	if rr.Code != http.StatusOK {
		t.Fatalf("two 500s then a 200 should reach the client as the 200, got %d: %s", rr.Code, rr.Body.String())
	}
	if calls() != 3 {
		t.Fatalf("expected the first attempt and two resends, got %d", calls())
	}
	if !strings.Contains(logBuf.String(), "Burst's resend 2 of 2") {
		t.Fatalf("the resends were not logged:\n%s", logBuf.String())
	}
}

// The waits are bounded: a 500 that persists goes back to the client after
// two resends, so a request Anthropic will never take is not held forever.
func TestServerErrorResendsAreBounded(t *testing.T) {
	srv, calls := failingUpstream(t, 100, http.StatusInternalServerError)
	s, _ := newChainServer(t, srv.URL, nil)
	rr := httptest.NewRecorder()
	s.ServeHTTP(rr, messagesRequest("claude-opus-5-5"))
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("a lasting 500 must reach the client, got %d", rr.Code)
	}
	if calls() != 1+len(outageRetryDelays) {
		t.Fatalf("expected %d attempts, got %d", 1+len(outageRetryDelays), calls())
	}
}

// A 400 says the request is wrong, and sending it again gets the same answer.
func TestClientErrorIsNotResent(t *testing.T) {
	srv, calls := failingUpstream(t, 100, http.StatusBadRequest)
	s, _ := newChainServer(t, srv.URL, nil)
	s.ServeHTTP(httptest.NewRecorder(), messagesRequest("claude-opus-5-5"))
	if calls() != 1 {
		t.Fatalf("a 400 must not be resent, got %d attempts", calls())
	}
}

// A request too large, or a picture over the API's limit, is the request's
// own fault: it goes back to Claude Code as Anthropic wrote it, once, and
// is never replayed to the secondary, which would refuse it too and be paid
// for the trying. Checked with the strictest failover there is, one failure
// and a plan already on overage (17 Nov 2026).
func TestATooLargeRequestIsPassedBackAndNeverFailsOver(t *testing.T) {
	for _, c := range []struct {
		status int
		body   string
	}{
		{http.StatusRequestEntityTooLarge, `{"type":"error","error":{"type":"request_too_large","message":"Request exceeds the maximum allowed number of bytes."}}`},
		{http.StatusBadRequest, `{"type":"error","error":{"type":"invalid_request_error","message":"messages.3.content.0.image.source.base64: image exceeds 5 MB maximum: 7340032 bytes > 5242880 bytes"}}`},
	} {
		var mu sync.Mutex
		calls := 0
		up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			calls++
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("anthropic-ratelimit-unified-status", "rejected")
			w.WriteHeader(c.status)
			_, _ = w.Write([]byte(c.body))
		}))
		t.Setenv("AWS_BEARER_TOKEN_BEDROCK", "test-key")
		cfg := config.Default()
		cfg.AnthropicBaseURL = up.URL
		cfg.BedrockBaseURL = "http://127.0.0.1:1"
		cfg.Primary = config.RouteConfig{Provider: "oauth-passthrough", BaseURL: up.URL, FailoverStrategy: "subscription-limit+metered-failures"}
		cfg.Secondary = config.RouteConfig{}
		cfg.MeteredFailover = config.MeteredFailoverConfig{WindowSeconds: 60, MinFailures: 1, TransportErrorMinFailures: 1}
		dir := t.TempDir()
		var logBuf bytes.Buffer
		s, err := New(cfg, filepath.Join(dir, "state.json"), filepath.Join(dir, "metrics.jsonl"), log.New(&logBuf, "", 0))
		if err != nil {
			t.Fatal(err)
		}
		if !s.HasSecondary() {
			t.Fatal("setup: the secondary should be ready")
		}
		for i := 0; i < 2; i++ {
			rr := httptest.NewRecorder()
			s.ServeHTTP(rr, messagesRequest("claude-opus-5-5"))
			if rr.Code != c.status || rr.Body.String() != c.body {
				t.Fatalf("status %d: the client got %d %s", c.status, rr.Code, rr.Body.String())
			}
		}
		mu.Lock()
		n := calls
		mu.Unlock()
		if n != 2 {
			t.Errorf("status %d: %d upstream calls for 2 requests: resent", c.status, n)
		}
		if s.inOverflow(time.Now()) || s.modelInOverflow("claude-opus-5-5", time.Now()) {
			t.Errorf("status %d: counted as a provider failure and failed over:\n%s", c.status, logBuf.String())
		}
		up.Close()
	}
}

// Only model requests wait: a heartbeat or event upload that fails is not
// worth holding, and Claude Code sends the next one anyway.
func TestNonModelRequestIsNotResent(t *testing.T) {
	srv, calls := failingUpstream(t, 100, http.StatusInternalServerError)
	s, _ := newChainServer(t, srv.URL, nil)
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/api/event_logging/v2/batch", strings.NewReader(`{}`))
	s.ServeHTTP(httptest.NewRecorder(), req)
	if calls() != 1 {
		t.Fatalf("a non-model request must not be resent, got %d attempts", calls())
	}
}

// A dead network: nothing reached Anthropic, so Burst waits and sends again
// rather than answering 502 at once; the network coming back inside the
// waits means Claude Code never sees the outage.
func TestNetworkDownIsWaitedOut(t *testing.T) {
	srv, calls := failingUpstream(t, 0, 0)
	s, logBuf := newChainServer(t, srv.URL, nil)
	ft := &flakyTransport{fail: 1, err: dialTimeout(), next: s.client.Transport}
	s.client.Transport = ft
	probes := 0
	s.probe = func() netProbe { probes++; return netProbe{dnsOK: probes > 1} }
	rr := httptest.NewRecorder()
	s.ServeHTTP(rr, messagesRequest("claude-opus-5-5"))
	if rr.Code != http.StatusOK {
		t.Fatalf("the network came back within the wait; want 200, got %d: %s", rr.Code, rr.Body.String())
	}
	if calls() != 1 {
		t.Fatalf("expected one request to reach the upstream, got %d", calls())
	}
	if !strings.Contains(logBuf.String(), `reason="network down" -> waiting`) {
		t.Fatalf("the request should have been waited out, not retried at once:\n%s", logBuf.String())
	}
}
