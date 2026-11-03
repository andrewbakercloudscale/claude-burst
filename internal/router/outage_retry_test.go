package router

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
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
