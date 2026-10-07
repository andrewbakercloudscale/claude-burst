package router

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// A primary failing because this Mac has no route is reported as the
// network being down, until the primary answers again.
func TestHealthSaysNetworkDown(t *testing.T) {
	s := &Server{}
	s.notePrimaryFailure("primary", fmt.Errorf("dial: %w", syscall.ENETUNREACH))
	if h := s.Health(); !h.NetworkDown || h.Failures != 1 {
		t.Fatalf("unreachable: %+v", h)
	}
	s.notePrimaryFailure("primary", errors.New("tls: handshake failure"))
	if s.Health().NetworkDown {
		t.Fatal("an error from the far side is not the network being down")
	}
	s.alertNetworkDown()
	if !s.Health().NetworkDown {
		t.Fatal("the network alert being up counts")
	}
	s.notePrimaryAnswered("primary")
	if s.Health().NetworkDown {
		t.Fatal("an answer ends it")
	}
}

// Releasing an outage window and a primary answer ending a failover take
// the state and the alerts locks. Taken in opposite orders they deadlocked,
// and every request after that waited for ever.
func TestReleasingAnOutageWindowDoesNotDeadlockWithAnAnswer(t *testing.T) {
	up := newRecordingUpstream(t)
	s, _ := newChainServerWithSecondary(t, up.srv.URL, nil)
	const model = "claude-sonnet-5"
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 300; i++ {
			s.mu.Lock()
			s.state.ModelClaim = map[string]string{model: "metered_transport"}
			s.state.ModelOverflow = map[string]int64{model: time.Now().Add(time.Hour).Unix()}
			s.mu.Unlock()
			s.alerts.mu.Lock()
			s.alerts.failedOver = true
			s.alerts.mu.Unlock()
			var wg sync.WaitGroup
			wg.Add(2)
			go func() { defer wg.Done(); s.releaseOutageWindow(model) }()
			go func() { defer wg.Done(); s.alertOutcome("primary", 200) }()
			wg.Wait()
		}
	}()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("deadlock: releaseOutageWindow and alertOutcome never finished")
	}
}

// An upstream error body can echo the request. Only the provider's own
// type and message reach the log and the metrics.
func TestErrorExcerptKeepsTheErrorAndNotTheEcho(t *testing.T) {
	for body, want := range map[string]string{
		`{"type":"error","error":{"type":"invalid_request_error","message":"prompt is too long"},"request":{"messages":[{"content":"SECRET PROMPT"}]}}`: "invalid_request_error: prompt is too long",
		`{"error":{"message":"model not found","type":"","code":404},"input":"SECRET PROMPT"}`:                                                          "model not found",
		`{"error":"quota exceeded","echo":"SECRET PROMPT"}`:                                                                                             "quota exceeded",
		`{"message":"Forbidden"}`:                    "Forbidden",
		`{"messages":[{"content":"SECRET PROMPT"}]}`: "(42 bytes of JSON with no error message)",
		"<html>\n<h1>502 Bad Gateway</h1></html>":    "<html> <h1>502 Bad Gateway</h1></html>",
	} {
		if got := errorExcerpt([]byte(body)); got != want {
			t.Errorf("%s\n got %q\nwant %q", body, got, want)
		}
	}
	if got := errorExcerpt([]byte(`{"error":{"type":"x","message":"` + strings.Repeat("m", 1000) + `"}}`)); len(got) > maxErrorExcerpt+3 {
		t.Errorf("not clipped: %d characters", len(got))
	}
}
