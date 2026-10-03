package router

import (
	"bytes"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/andrewbakercloudscale/claude-burst/internal/config"
	"github.com/andrewbakercloudscale/claude-burst/internal/metrics"
)

// A request that never got an upstream answer is recorded with the status
// the client was sent, not 0, so error counts include it.
func TestUpstreamFailureRecordsItsStatus(t *testing.T) {
	cfg := config.Default()
	cfg.AnthropicBaseURL = "http://127.0.0.1:1" // nothing listens: connection refused
	dir := t.TempDir()
	mpath := filepath.Join(dir, "metrics.jsonl")
	s, err := New(cfg, filepath.Join(dir, "state.json"), mpath, log.New(testLogWriter{t}, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	s.probe = func() netProbe { return netProbe{dnsOK: true} } // the network is fine; the upstream is not
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/v1/messages",
		bytes.NewReader([]byte(`{"model":"claude-opus-5-5","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`)))
	req.Header.Set("authorization", "Bearer oauth")
	rr := httptest.NewRecorder()
	s.ServeHTTP(rr, req)
	b, _ := os.ReadFile(mpath)
	if !strings.Contains(string(b), `"http_status":502`) {
		t.Fatalf("client got %d; metrics:\n%s", rr.Code, b)
	}
}

func TestClientClosedIsNotAnError(t *testing.T) {
	for status, want := range map[int]bool{200: false, 429: true, 502: true, metrics.StatusClientClosed: false, 0: false} {
		if got := metrics.IsError(metrics.Event{HTTPStatus: status}); got != want {
			t.Errorf("%d: IsError=%v, want %v", status, got, want)
		}
	}
}
