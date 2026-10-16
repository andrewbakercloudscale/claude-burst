package router

import (
	"bytes"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/andrewbakercloudscale/claude-burst/internal/config"
)

// guardServer returns a gateway whose primary counts the requests that reach
// it, so a test can prove a refused request never got as far as a provider.
func guardServer(t *testing.T, mutate func(*config.Config)) (*Server, *atomic.Int64, *bytes.Buffer) {
	t.Helper()
	var hits atomic.Int64
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`{"type":"message","usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	t.Cleanup(up.Close)
	cfg := config.Default()
	cfg.AnthropicBaseURL = up.URL
	if mutate != nil {
		mutate(&cfg)
	}
	dir := t.TempDir()
	var logBuf bytes.Buffer
	s, err := New(cfg, filepath.Join(dir, "state.json"), filepath.Join(dir, "metrics.jsonl"), log.New(&logBuf, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	return s, &hits, &logBuf
}

func claudeCodeShaped(host string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "http://"+host+"/v1/messages?beta=true",
		strings.NewReader(`{"model":"claude-sonnet-5","messages":[]}`))
	req.Header.Set("content-type", "application/json")
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("user-agent", "claude-cli/2.1.0 (external, cli)")
	return req
}

func TestGuardAllowsClaudeCodeShapedRequests(t *testing.T) {
	s, hits, _ := guardServer(t, func(c *config.Config) {
		c.Intercept.Host = "api.anthropic.com"
	})
	for _, host := range []string{"127.0.0.1:7777", "localhost:7777", "[::1]:7777", "api.anthropic.com", "API.Anthropic.com:443"} {
		rr := httptest.NewRecorder()
		s.ServeHTTP(rr, claudeCodeShaped(host))
		if rr.Code != http.StatusOK {
			t.Errorf("Host %s: status %d, want 200 (%s)", host, rr.Code, rr.Body)
		}
	}
	if hits.Load() != 5 {
		t.Fatalf("primary saw %d requests, want 5", hits.Load())
	}
}

// Sec-Fetch-* alone must not refuse: only Origin is the browser marker this
// guard relies on, so a Node client that adds Sec-Fetch-Mode is unaffected.
func TestGuardIgnoresSecFetchHeaders(t *testing.T) {
	s, _, _ := guardServer(t, nil)
	req := claudeCodeShaped("127.0.0.1:7777")
	req.Header.Set("Sec-Fetch-Mode", "cors")
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	rr := httptest.NewRecorder()
	s.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d, want 200", rr.Code)
	}
}

func TestGuardRefusesOriginHeader(t *testing.T) {
	s, hits, logBuf := guardServer(t, nil)
	for i := 0; i < 3; i++ {
		req := claudeCodeShaped("127.0.0.1:7777")
		req.Header.Set("Origin", "https://evil.example")
		req.Header.Set("content-type", "text/plain")
		rr := httptest.NewRecorder()
		s.ServeHTTP(rr, req)
		if rr.Code != http.StatusForbidden {
			t.Fatalf("status %d, want 403", rr.Code)
		}
	}
	if hits.Load() != 0 {
		t.Fatalf("a refused request reached the primary %d times", hits.Load())
	}
	if n := strings.Count(logBuf.String(), " refused reason="); n != 1 {
		t.Fatalf("logged %d refusal lines for 3 refusals within a minute, want 1:\n%s", n, logBuf)
	}
}

func TestGuardRefusesForeignHost(t *testing.T) {
	s, hits, _ := guardServer(t, nil)
	// A DNS-rebinding page reaches 127.0.0.1 under its own name.
	for _, host := range []string{"rebind.evil.example", "rebind.evil.example:7777", "192.168.1.9:7777"} {
		rr := httptest.NewRecorder()
		s.ServeHTTP(rr, claudeCodeShaped(host))
		if rr.Code != http.StatusForbidden {
			t.Errorf("Host %s: status %d, want 403", host, rr.Code)
		}
	}
	if hits.Load() != 0 {
		t.Fatalf("a refused request reached the primary %d times", hits.Load())
	}
}

func TestGuardAcceptsConfiguredListenHost(t *testing.T) {
	s, _, _ := guardServer(t, func(c *config.Config) { c.Listen = "192.168.1.9:7777" })
	rr := httptest.NewRecorder()
	s.ServeHTTP(rr, claudeCodeShaped("192.168.1.9:7777"))
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d, want 200", rr.Code)
	}
}

func TestGuardWildcardListenAcceptsIPLiteralsOnly(t *testing.T) {
	s, _, _ := guardServer(t, func(c *config.Config) { c.Listen = "0.0.0.0:7777" })
	rr := httptest.NewRecorder()
	s.ServeHTTP(rr, claudeCodeShaped("10.0.0.5:7777"))
	if rr.Code != http.StatusOK {
		t.Fatalf("IP literal: status %d, want 200", rr.Code)
	}
	rr = httptest.NewRecorder()
	s.ServeHTTP(rr, claudeCodeShaped("rebind.evil.example:7777"))
	if rr.Code != http.StatusForbidden {
		t.Fatalf("hostname: status %d, want 403", rr.Code)
	}
}

func TestGuardKeepsHealthzReachable(t *testing.T) {
	s, _, _ := guardServer(t, nil)
	// Even under a Host and Origin the guard would refuse: pf-heal and the
	// admin panel probe this under several names, and it only reads.
	req := httptest.NewRequest(http.MethodGet, "http://api.anthropic.com/healthz", nil)
	req.Header.Set("Origin", "https://evil.example")
	rr := httptest.NewRecorder()
	s.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"ok":true`) {
		t.Fatalf("healthz: status %d body %s", rr.Code, rr.Body)
	}
}

func TestTransparentPassesBrowserRequestsToTheInterceptedHostThrough(t *testing.T) {
	var gotOrigin, gotCookie string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotOrigin, gotCookie = r.Header.Get("Origin"), r.Header.Get("Cookie")
		w.Header().Set("Access-Control-Allow-Origin", r.Header.Get("Origin"))
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(up.Close)
	s, hits, _ := guardServer(t, func(c *config.Config) {
		c.Intercept.Mode = config.InterceptTransparent
		c.Intercept.Host = "api.anthropic.com"
	})
	u, err := url.Parse(up.URL)
	if err != nil {
		t.Fatal(err)
	}
	s.browserUpstream = u
	req := httptest.NewRequest(http.MethodPost, "https://api.anthropic.com/api/web/url_hash_check/browser_extension",
		strings.NewReader(`{"url_hash":"x"}`))
	req.Header.Set("Origin", "chrome-extension://fcoeoabgfenejglbffodgkkbkcdhcgfn")
	req.Header.Set("Cookie", "sessionKey=browser")
	rr := httptest.NewRecorder()
	s.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK || rr.Body.String() != `{"ok":true}` {
		t.Fatalf("status %d body %q, want the upstream's 200", rr.Code, rr.Body)
	}
	if gotOrigin != "chrome-extension://fcoeoabgfenejglbffodgkkbkcdhcgfn" || gotCookie != "sessionKey=browser" {
		t.Fatalf("upstream saw Origin %q Cookie %q, want the browser's own", gotOrigin, gotCookie)
	}
	if rr.Header().Get("Access-Control-Allow-Origin") == "" {
		t.Fatal("the upstream's CORS headers must reach the browser")
	}
	if hits.Load() != 0 {
		t.Fatalf("a browser request reached the primary %d times", hits.Load())
	}
}

func TestBrowserRequestsAreStillRefusedOffTheInterceptedHost(t *testing.T) {
	for name, mutate := range map[string]func(*config.Config){
		"transparent, loopback Host": func(c *config.Config) {
			c.Intercept.Mode = config.InterceptTransparent
			c.Intercept.Host = "api.anthropic.com"
		},
		"base-url mode": func(c *config.Config) { c.Intercept.Host = "api.anthropic.com" },
	} {
		s, hits, _ := guardServer(t, mutate)
		host := "api.anthropic.com"
		if s.cfg.Intercept.Transparent() {
			host = "127.0.0.1:7777"
		}
		req := claudeCodeShaped(host)
		req.Header.Set("Origin", "https://evil.example")
		rr := httptest.NewRecorder()
		s.ServeHTTP(rr, req)
		if rr.Code != http.StatusForbidden || hits.Load() != 0 {
			t.Errorf("%s: status %d, primary hits %d; want 403 and none", name, rr.Code, hits.Load())
		}
	}
}

func TestInterceptUpstreamIsTheRealHost(t *testing.T) {
	cfg := config.Default()
	cfg.Intercept.Host = "api.anthropic.com"
	if u := interceptUpstream(cfg); u != nil {
		t.Fatalf("base-url mode: %v, want nil", u)
	}
	cfg.Intercept.Mode = config.InterceptTransparent
	for base, want := range map[string]string{
		"https://api.anthropic.com":        "https://api.anthropic.com",
		"https://portkey.internal.example": "https://api.anthropic.com",
	} {
		cfg.AnthropicBaseURL = base
		if got := interceptUpstream(cfg).String(); got != want {
			t.Errorf("anthropic_base_url %s: upstream %s, want %s", base, got, want)
		}
	}
}
