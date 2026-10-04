package router

import (
	"bytes"
	"log"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/andrewbakercloudscale/claude-burst/internal/config"
)

// Single plan: someone with Claude Enterprise only, or any one subscription,
// and no second provider. Everything that does not need a second provider
// must work for them, and nothing Burst does may make their experience
// worse than Claude Code talking to Anthropic directly. In particular,
// Anthropic's own limit responses must reach Claude Code unchanged, since
// Claude Code shows the reset time and handles the retry from them.

// singlePlanUpstream is Anthropic as a single-plan user sees it: it records
// what arrived (auth headers included) and answers with whatever the test
// sets for each model.
type singlePlanUpstream struct {
	srv     *httptest.Server
	mu      sync.Mutex
	models  []string
	auth    []string
	apiKeys []string
	refuse  map[string]http.HandlerFunc
}

func newSinglePlanUpstream(t *testing.T) *singlePlanUpstream {
	t.Helper()
	u := &singlePlanUpstream{refuse: map[string]http.HandlerFunc{}}
	u.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := new(bytes.Buffer)
		_, _ = body.ReadFrom(r.Body)
		m := requestModel(body.Bytes())
		u.mu.Lock()
		u.models = append(u.models, m)
		u.auth = append(u.auth, r.Header.Get("Authorization"))
		u.apiKeys = append(u.apiKeys, r.Header.Get("x-api-key"))
		h := u.refuse[m]
		u.mu.Unlock()
		if h != nil {
			h(w, r)
			return
		}
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`{"type":"message","model":"` + m + `","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	t.Cleanup(u.srv.Close)
	return u
}

func (u *singlePlanUpstream) setRefusal(model string, h http.HandlerFunc) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.refuse[model] = h
}

func (u *singlePlanUpstream) seen() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]string(nil), u.models...)
}

// subscriptionLimit429 is a real subscription limit, with the headers
// Claude Code reads the reset time from.
func subscriptionLimit429(reset time.Time) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("anthropic-ratelimit-unified-status", "rejected")
		w.Header().Set("anthropic-ratelimit-unified-representative-claim", "five_hour")
		w.Header().Set("anthropic-ratelimit-unified-reset", strconv.FormatInt(reset.Unix(), 10))
		w.Header().Set("retry-after", "3600")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"rate_limit_error","message":"You've hit your usage limit"}}`))
	}
}

// singlePlanServer builds the gateway the way a single-plan user has it.
// explicitNone picks between the two ways that happens: a secondary set to
// "none", or no secondary block at all, which resolves to a Bedrock
// secondary with no key.
func singlePlanServer(t *testing.T, primaryURL string, explicitNone bool, strategy string, chain map[string][]string) (*Server, *bytes.Buffer) {
	t.Helper()
	t.Setenv("AWS_BEARER_TOKEN_BEDROCK", "")
	t.Setenv("HOME", t.TempDir()) // no Keychain entry can be found under a fresh HOME's account either
	cfg := config.Default()
	cfg.AnthropicBaseURL = primaryURL
	cfg.KeychainService = "claude-burst-single-plan-test-no-such-item"
	cfg.Primary = config.RouteConfig{Provider: "oauth-passthrough", BaseURL: primaryURL, FailoverStrategy: strategy}
	if explicitNone {
		cfg.Secondary = config.RouteConfig{Provider: config.ProviderNone}
	} else {
		cfg.Secondary = config.RouteConfig{}
	}
	cfg.FallbackChain = chain
	dir := t.TempDir()
	var logBuf bytes.Buffer
	s, err := New(cfg, filepath.Join(dir, "state.json"), filepath.Join(dir, "metrics.jsonl"), log.New(&logBuf, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	s.probe = func() netProbe { return netProbe{dnsOK: true} }
	return s, &logBuf
}

// Both shapes of "no secondary" run the same tests.
func eachSinglePlan(t *testing.T, fn func(t *testing.T, explicitNone bool)) {
	t.Run("secondary none", func(t *testing.T) { fn(t, true) })
	t.Run("no secondary block", func(t *testing.T) { fn(t, false) })
}

func TestSinglePlanHasNoSecondary(t *testing.T) {
	eachSinglePlan(t, func(t *testing.T, none bool) {
		up := newSinglePlanUpstream(t)
		s, _ := singlePlanServer(t, up.srv.URL, none, "subscription-limit", nil)
		if s.HasSecondary() {
			t.Fatal("a single plan must not look as if it has somewhere to fail over to")
		}
	})
}

// Ordinary requests go through untouched, whichever way Claude Code signs
// in: an OAuth login (Enterprise through claude.ai, Pro, Max) or an API key
// from the Console.
func TestSinglePlanServesRequestsWithTheUsersOwnAuth(t *testing.T) {
	eachSinglePlan(t, func(t *testing.T, none bool) {
		up := newSinglePlanUpstream(t)
		s, _ := singlePlanServer(t, up.srv.URL, none, "subscription-limit", nil)

		oauth := messagesRequest("claude-opus-5-5")
		oauth.Header.Set("Authorization", "Bearer sk-ant-oat-enterprise-sso")
		rr := httptest.NewRecorder()
		s.ServeHTTP(rr, oauth)
		if rr.Code != http.StatusOK {
			t.Fatalf("OAuth request: %d %s", rr.Code, rr.Body.String())
		}

		key := messagesRequest("claude-sonnet-5-5")
		key.Header.Set("x-api-key", "sk-ant-api-enterprise-console")
		rr = httptest.NewRecorder()
		s.ServeHTTP(rr, key)
		if rr.Code != http.StatusOK {
			t.Fatalf("API key request: %d %s", rr.Code, rr.Body.String())
		}

		up.mu.Lock()
		defer up.mu.Unlock()
		if up.auth[0] != "Bearer sk-ant-oat-enterprise-sso" || up.apiKeys[1] != "sk-ant-api-enterprise-console" {
			t.Fatalf("the user's own credential must reach Anthropic unchanged: auth=%q keys=%q", up.auth, up.apiKeys)
		}
	})
}

// The one that matters most: a real limit. Claude Code must get Anthropic's
// 429, headers and body, exactly as it would without Burst. It used to get
// a gateway 503 from a keyless Bedrock "failover".
func TestSinglePlanLimitReachesClaudeCodeUnchanged(t *testing.T) {
	eachSinglePlan(t, func(t *testing.T, none bool) {
		up := newSinglePlanUpstream(t)
		reset := time.Now().Add(time.Hour)
		up.setRefusal("claude-opus-5-5", subscriptionLimit429(reset))
		s, _ := singlePlanServer(t, up.srv.URL, none, "subscription-limit", nil)

		rr := httptest.NewRecorder()
		s.ServeHTTP(rr, messagesRequest("claude-opus-5-5"))

		if rr.Code != http.StatusTooManyRequests {
			t.Fatalf("status %d, want Anthropic's 429: %s", rr.Code, rr.Body.String())
		}
		if !strings.Contains(rr.Body.String(), "You've hit your usage limit") {
			t.Errorf("body changed: %s", rr.Body.String())
		}
		if got := rr.Header().Get("anthropic-ratelimit-unified-reset"); got != strconv.FormatInt(reset.Unix(), 10) {
			t.Errorf("reset header = %q, want it passed through", got)
		}
		if rr.Header().Get("retry-after") != "3600" {
			t.Error("retry-after must pass through")
		}
		if s.inOverflow(time.Now()) || s.modelInOverflow("claude-opus-5-5", time.Now()) {
			t.Fatal("no window may be armed with nowhere to send the traffic")
		}
	})
}

// After a limit, the next request still goes to Anthropic, and is served as
// soon as Anthropic allows it. A window armed on a single plan would have
// kept answering 502/503 until the reset, even after the limit lifted.
func TestSinglePlanKeepsAskingAnthropicAfterALimit(t *testing.T) {
	eachSinglePlan(t, func(t *testing.T, none bool) {
		up := newSinglePlanUpstream(t)
		up.setRefusal("claude-opus-5-5", subscriptionLimit429(time.Now().Add(time.Hour)))
		s, _ := singlePlanServer(t, up.srv.URL, none, "subscription-limit", nil)

		s.ServeHTTP(httptest.NewRecorder(), messagesRequest("claude-opus-5-5"))
		up.setRefusal("claude-opus-5-5", nil) // the limit lifts early

		rr := httptest.NewRecorder()
		s.ServeHTTP(rr, messagesRequest("claude-opus-5-5"))
		if rr.Code != http.StatusOK {
			t.Fatalf("second request: %d %s", rr.Code, rr.Body.String())
		}
		if n := len(up.seen()); n != 2 {
			t.Fatalf("Anthropic saw %d requests, want both", n)
		}
	})
}

// A rate limit that is not a subscription limit (a per-minute limit, which
// is what an Enterprise org's API limits send, with no unified headers)
// passes through as well.
func TestSinglePlanPlainRateLimitPassesThrough(t *testing.T) {
	eachSinglePlan(t, func(t *testing.T, none bool) {
		up := newSinglePlanUpstream(t)
		up.setRefusal("claude-opus-5-5", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("retry-after", "12")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"type":"error","error":{"type":"rate_limit_error","message":"Number of request tokens has exceeded your per-minute rate limit"}}`))
		})
		s, _ := singlePlanServer(t, up.srv.URL, none, "subscription-limit+metered-failures", nil)
		for i := 0; i < 5; i++ {
			rr := httptest.NewRecorder()
			s.ServeHTTP(rr, messagesRequest("claude-opus-5-5"))
			if rr.Code != http.StatusTooManyRequests || rr.Header().Get("retry-after") != "12" {
				t.Fatalf("request %d: %d %q", i, rr.Code, rr.Header().Get("retry-after"))
			}
		}
		if n := len(up.seen()); n != 5 {
			t.Fatalf("every request must reach Anthropic, got %d of 5", n)
		}
	})
}

// Fallback models need no second provider: Fable refused goes to Opus on
// the same plan, and the user keeps working.
func TestSinglePlanFallbackModelsStillWork(t *testing.T) {
	eachSinglePlan(t, func(t *testing.T, none bool) {
		up := newSinglePlanUpstream(t)
		up.setRefusal("claude-fable-5-1", subscriptionLimit429(time.Now().Add(time.Hour)))
		s, _ := singlePlanServer(t, up.srv.URL, none, "subscription-limit",
			map[string][]string{"claude-fable-5-1": {"claude-opus-5-5"}})

		rr := httptest.NewRecorder()
		s.ServeHTTP(rr, messagesRequest("claude-fable-5-1"))
		if rr.Code != http.StatusOK {
			t.Fatalf("want Opus to answer for the refused Fable, got %d %s", rr.Code, rr.Body.String())
		}
		if got := up.seen(); len(got) != 2 || got[1] != "claude-opus-5-5" {
			t.Fatalf("calls = %v, want fable then opus", got)
		}

		// While Fable's window is open, the next Fable request goes to Opus
		// directly, without asking for Fable again.
		rr = httptest.NewRecorder()
		s.ServeHTTP(rr, messagesRequest("claude-fable-5-1"))
		if got := up.seen(); rr.Code != http.StatusOK || got[len(got)-1] != "claude-opus-5-5" || len(got) != 3 {
			t.Fatalf("second fable request: %d, calls %v", rr.Code, got)
		}
	})
}

// With every fallback refused too, Claude Code gets the last refusal as
// Anthropic sent it, and the last model is not put in a window.
func TestSinglePlanExhaustedChainReturnsAnthropicsAnswer(t *testing.T) {
	eachSinglePlan(t, func(t *testing.T, none bool) {
		up := newSinglePlanUpstream(t)
		up.setRefusal("claude-fable-5-1", subscriptionLimit429(time.Now().Add(time.Hour)))
		up.setRefusal("claude-opus-5-5", subscriptionLimit429(time.Now().Add(2*time.Hour)))
		s, _ := singlePlanServer(t, up.srv.URL, none, "subscription-limit",
			map[string][]string{"claude-fable-5-1": {"claude-opus-5-5"}})

		rr := httptest.NewRecorder()
		s.ServeHTTP(rr, messagesRequest("claude-fable-5-1"))
		if rr.Code != http.StatusTooManyRequests || !strings.Contains(rr.Body.String(), "usage limit") {
			t.Fatalf("want Anthropic's 429 for the last rung, got %d %s", rr.Code, rr.Body.String())
		}
		if s.modelInOverflow("claude-opus-5-5", time.Now()) {
			t.Fatal("Opus had nowhere after it; a window on it would block the user's own Opus requests")
		}
		// A direct Opus request still reaches Anthropic.
		before := len(up.seen())
		s.ServeHTTP(httptest.NewRecorder(), messagesRequest("claude-opus-5-5"))
		if len(up.seen()) != before+1 {
			t.Fatal("a direct Opus request must still reach Anthropic")
		}
	})
}

// A window left in state.json from when a secondary existed, or forced from
// the dashboard, must not block a single-plan user.
func TestSinglePlanIgnoresAStaleWindow(t *testing.T) {
	eachSinglePlan(t, func(t *testing.T, none bool) {
		up := newSinglePlanUpstream(t)
		s, logBuf := singlePlanServer(t, up.srv.URL, none, "subscription-limit", nil)
		s.ForceOverflow(0, "left over from a removed secondary")
		s.activateOverflow("claude-opus-5-5", time.Now().Add(time.Hour).Unix(), "five_hour", "stale")

		rr := httptest.NewRecorder()
		s.ServeHTTP(rr, messagesRequest("claude-opus-5-5"))
		if rr.Code != http.StatusOK {
			t.Fatalf("want the primary to serve it, got %d %s", rr.Code, rr.Body.String())
		}
		if !strings.Contains(logBuf.String(), "no usable secondary") {
			t.Errorf("the log should say why the window was ignored:\n%s", logBuf.String())
		}
	})
}

// A network failure with nowhere to fail over to gets one immediate resend
// on a fresh connection (a dead pooled connection is the usual cause), then
// returns at once, so Claude Code's own retries (which show on screen) take
// over, and the dashboard's "Anthropic answering" check sees it.
func TestSinglePlanTransportErrorFailsFastAndIsVisible(t *testing.T) {
	eachSinglePlan(t, func(t *testing.T, none bool) {
		up := newSinglePlanUpstream(t)
		s, _ := singlePlanServer(t, up.srv.URL, none, "subscription-limit+metered-failures", nil)
		ft := &flakyTransport{neverRecover: true, err: timeoutErr{}, next: s.client.Transport}
		s.client.Transport = ft

		start := time.Now()
		rr := httptest.NewRecorder()
		s.ServeHTTP(rr, messagesRequest("claude-opus-5-5"))
		if rr.Code != http.StatusBadGateway {
			t.Fatalf("status %d", rr.Code)
		}
		if d := time.Since(start); d > time.Second {
			t.Fatalf("took %v: the 30s retry ladder is for deciding a failover there is none of", d)
		}
		if len(ft.bodies) != 2 {
			t.Fatalf("attempts = %d, want 2 (the original and one fresh-connection resend)", len(ft.bodies))
		}
		if h := s.Health(); h.Failures != 1 {
			t.Fatalf("health = %+v, want the failure recorded", h)
		}
		if s.inOverflow(time.Now()) {
			t.Fatal("no window may be armed")
		}
	})
}

// A metered primary (a Console API key, which some Enterprise orgs use)
// with no secondary: a run of 5xx never arms anything and every response
// passes through.
func TestSinglePlanMeteredPrimaryPassesErrorsThrough(t *testing.T) {
	up := newSinglePlanUpstream(t)
	up.setRefusal("claude-opus-5-5", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"api_error","message":"Internal server error"}}`))
	})
	s, _ := singlePlanServer(t, up.srv.URL, true, "metered-failures", nil)
	for i := 0; i < 6; i++ {
		rr := httptest.NewRecorder()
		s.ServeHTTP(rr, messagesRequest("claude-opus-5-5"))
		if rr.Code != http.StatusInternalServerError {
			t.Fatalf("request %d: %d, want Anthropic's 500 passed through", i, rr.Code)
		}
	}
	if s.inOverflow(time.Now()) || s.modelInOverflow("claude-opus-5-5", time.Now()) {
		t.Fatal("no window may be armed")
	}
}

// Pauseless compaction runs on the primary and needs no second provider.
func TestSinglePlanCompactionWorks(t *testing.T) {
	f := &fakeAnthropic{context: 450_000}
	up := httptest.NewServer(http.HandlerFunc(f.handler))
	t.Cleanup(up.Close)
	t.Setenv("AWS_BEARER_TOKEN_BEDROCK", "")
	cfg := config.Default()
	cfg.AnthropicBaseURL = up.URL
	cfg.Secondary = config.RouteConfig{Provider: config.ProviderNone}
	cfg.PrimaryCompaction = config.CompactionConfig{Enabled: true, CompactAtTokens: 400_000, WarnAtPercent: 75, WindowMinutes: 60}
	dir := t.TempDir()
	s, err := New(cfg, filepath.Join(dir, "state.json"), filepath.Join(dir, "metrics.jsonl"), log.New(testLogWriter{t}, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.compaction.running.Wait)
	if s.HasSecondary() {
		t.Fatal("setup: no secondary")
	}

	all := msgs(t, session)
	send(t, s, "S", all[:5])
	send(t, s, "S", all[:7])
	waitFor(t, func() bool { return f.summaryCount() == 1 })
	waitFor(t, func() bool { return s.compactionReady("S") })
	send(t, s, "S", all[:9])
	if !strings.Contains(f.last(), "THE GIST OF THE FIRST TASK") {
		t.Fatalf("the compacted turn must carry the summary on a single plan:\n%s", f.last())
	}
}

// A key added later (claude-burst keychain-set) is picked up without a
// restart, within the readiness TTL.
func TestSecondaryKeyAddedLaterIsPickedUp(t *testing.T) {
	up := newSinglePlanUpstream(t)
	s, _ := singlePlanServer(t, up.srv.URL, false, "subscription-limit", nil)
	if s.HasSecondary() {
		t.Fatal("setup: no key yet")
	}
	t.Setenv("AWS_BEARER_TOKEN_BEDROCK", "added-later")
	if s.HasSecondary() {
		t.Fatal("inside the TTL the cached answer stands; checking the Keychain per request would be slow")
	}
	s.readyMu.Lock()
	s.readyAt = time.Now().Add(-secondaryReadyTTL - time.Second)
	s.readyMu.Unlock()
	if !s.HasSecondary() {
		t.Fatal("after the TTL the new key must be seen")
	}
}
