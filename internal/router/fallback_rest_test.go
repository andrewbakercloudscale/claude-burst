package router

import (
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/andrewbakercloudscale/claude-burst/internal/config"
)

func TestOutOfFunds(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   bool
	}{
		{402, `{}`, true},
		{402, ``, true},
		{400, `{"error":{"message":"Your credit balance is too low to access the Anthropic API"}}`, true},
		{429, `{"error":{"code":"insufficient_quota","message":"You exceeded your current quota"}}`, true},
		{403, `{"error":"Insufficient credits. Add more using https://openrouter.ai/credits"}`, true},
		{429, `{"error":{"message":"rate limit reached"}}`, false},
		{400, `{"error":{"message":"messages: field required"}}`, false},
		{500, `billing service down`, false},
	}
	for _, c := range cases {
		if got := outOfFunds(c.status, []byte(c.body)); got != c.want {
			t.Errorf("outOfFunds(%d, %q) = %v, want %v", c.status, c.body, got, c.want)
		}
	}
}

func TestRestFor(t *testing.T) {
	if got := restFor(402, true, ""); got != fundsRest {
		t.Errorf("out of credit rests %v", got)
	}
	if got := restFor(429, false, "30"); got != 30*time.Second {
		t.Errorf("Retry-After 30 rests %v", got)
	}
	if got := restFor(429, false, "86400"); got != maxRest {
		t.Errorf("a day's Retry-After rests %v, want the cap", got)
	}
	if got := restFor(503, false, ""); got != busyRest {
		t.Errorf("a 503 rests %v", got)
	}
}

// A route out of credit is tried once, then skipped: the next request goes
// straight to the route after it.
func TestFallbackChainSkipsRouteOutOfCredit(t *testing.T) {
	t.Setenv("OPENROUTER_FUNDS_TEST_API_KEY", "k1")
	t.Setenv("GROQ_FUNDS_TEST_API_KEY", "k2")
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("anthropic-ratelimit-unified-status", "rejected")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"rate_limit_error","message":"plan limit"}}`))
	}))
	defer primary.Close()
	var broke, good atomic.Int32
	openrouter := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		broke.Add(1)
		w.WriteHeader(http.StatusPaymentRequired)
		_, _ = w.Write([]byte(`{"error":{"message":"Insufficient credits"}}`))
	}))
	defer openrouter.Close()
	groq := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		good.Add(1)
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":3}}`))
	}))
	defer groq.Close()

	cfg := config.Default()
	cfg.Primary = config.RouteConfig{Provider: "oauth-passthrough", BaseURL: primary.URL, FailoverStrategy: "subscription-limit"}
	cfg.Secondary = config.RouteConfig{Provider: "openai-compatible", BaseURL: openrouter.URL, Model: "openrouter/free", KeychainService: "claude-burst-openrouter-funds-test"}
	cfg.FallbackRoutes = []config.RouteConfig{{Provider: "openai-compatible", BaseURL: groq.URL, Model: "openai/gpt-oss-20b", KeychainService: "claude-burst-groq-funds-test"}}
	dir := t.TempDir()
	s, err := New(cfg, filepath.Join(dir, "state.json"), filepath.Join(dir, "metrics.jsonl"), log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		rr := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/v1/messages", strings.NewReader(`{"model":"claude-sonnet-5","messages":[{"role":"user","content":"hi"}]}`))
		req.Header.Set("Authorization", "Bearer oauth-token")
		s.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("request %d: status=%d body=%s", i, rr.Code, rr.Body.String())
		}
	}
	if broke.Load() != 1 || good.Load() != 3 {
		t.Fatalf("calls: out-of-credit route=%d, next route=%d; want 1 and 3", broke.Load(), good.Load())
	}
}
