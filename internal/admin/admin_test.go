package admin

import (
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/andrewbakercloudscale/claude-burst/internal/config"
	"github.com/andrewbakercloudscale/claude-burst/internal/router"
)

// newTestServer builds an admin *Server backed by a real router.Server
// (mirroring internal/router's own test helper pattern) and points HOME at
// a fresh temp dir so config.Load() -- which admin.go calls directly,
// un-injected -- reads from a location this test controls rather than the
// real machine's ~/.config/claude-burst/config.json.
func newTestServer(t *testing.T) *Server {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)

	dir := t.TempDir()
	gw, err := router.New(config.Default(), filepath.Join(dir, "state.json"), filepath.Join(dir, "metrics.jsonl"), log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	return New(gw, filepath.Join(dir, "metrics.jsonl"), "test-version", "", "/path/to/transparent-root.sh")
}

// writeConfig writes a minimal valid config.json to this test's HOME, so
// config.Load() succeeds instead of falling back to Default() (the
// no-file-at-all case, which is a different code path than "a config.json
// exists and parses").
func writeConfig(t *testing.T, home string) {
	t.Helper()
	dir := filepath.Join(home, ".config", "claude-burst")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	b, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.json"), b, 0600); err != nil {
		t.Fatal(err)
	}
}

// TestGuardRejectsNonLoopbackHost is the actual DNS-rebinding defence this
// package's own doc comment describes: an attacker's page resolving to
// 127.0.0.1 still arrives with a Host header naming the attacker's domain,
// not "127.0.0.1" or "localhost". If this ever silently started accepting
// arbitrary Host headers, that defence would be gone with no other signal.
func TestGuardRejectsNonLoopbackHost(t *testing.T) {
	s := newTestServer(t)
	h := s.Handler()

	cases := []struct {
		host string
		want int
	}{
		{"127.0.0.1", http.StatusOK},
		{"127.0.0.1:7788", http.StatusOK},
		{"localhost", http.StatusOK},
		{"localhost:7788", http.StatusOK},
		{"evil.example.com", http.StatusForbidden},
		{"evil.example.com:7788", http.StatusForbidden},
		{"127.0.0.1.evil.example.com", http.StatusForbidden},
	}
	for _, c := range cases {
		t.Run(c.host, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "http://x/api/state", nil)
			req.Host = c.host
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, req)
			if rr.Code != c.want {
				t.Fatalf("Host=%q: status=%d, want %d (body=%s)", c.host, rr.Code, c.want, rr.Body.String())
			}
		})
	}
}

// TestGuardAcceptsConfiguredExtraHost verifies the escape hatch for
// --admin-hostname works, and specifically that it doesn't accidentally
// widen acceptance beyond the one configured name.
func TestGuardAcceptsConfiguredExtraHost(t *testing.T) {
	home := t.TempDir()
	dir := t.TempDir()
	gw, err := router.New(config.Default(), filepath.Join(dir, "state.json"), filepath.Join(dir, "metrics.jsonl"), log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	s := New(gw, filepath.Join(dir, "metrics.jsonl"), "v", "cloudscale-claudeburst.test", "/path/to/transparent-root.sh")
	_ = home

	h := s.Handler()
	for host, want := range map[string]int{
		"cloudscale-claudeburst.test": http.StatusOK,
		"other-host.test":             http.StatusForbidden,
	} {
		req := httptest.NewRequest(http.MethodGet, "http://x/api/state", nil)
		req.Host = host
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		if rr.Code != want {
			t.Errorf("Host=%q: status=%d, want %d", host, rr.Code, want)
		}
	}
}

// TestMutatingRequiresHeaderAndPost is the second DNS-rebinding defence:
// even if an attacker's page could somehow get the Host check to pass, a
// cross-origin request cannot attach the mutation header without a CORS
// preflight this server never answers. Both halves of that must actually
// be enforced, not just documented.
func TestMutatingRequiresHeaderAndPost(t *testing.T) {
	s := newTestServer(t)
	h := s.Handler()

	post := func(withHeader bool) int {
		req := httptest.NewRequest(http.MethodPost, "http://x/api/reset", nil)
		req.Host = "127.0.0.1"
		if withHeader {
			req.Header.Set(mutationHeader, "1")
		}
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		return rr.Code
	}

	if code := post(false); code != http.StatusForbidden {
		t.Errorf("POST without mutation header: status=%d, want 403", code)
	}
	if code := post(true); code != http.StatusOK {
		t.Errorf("POST with mutation header: status=%d, want 200", code)
	}

	req := httptest.NewRequest(http.MethodGet, "http://x/api/reset", nil)
	req.Host = "127.0.0.1"
	req.Header.Set(mutationHeader, "1")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET on a mutating endpoint: status=%d, want 405", rr.Code)
	}
}

// TestReadOnlyRejectsPost mirrors the mutating check for the other
// direction: a read endpoint must not accept a POST, which matters because
// /api/state etc. are exactly what a cross-origin GET (no preflight needed)
// could otherwise reach.
func TestReadOnlyRejectsPost(t *testing.T) {
	s := newTestServer(t)
	h := s.Handler()
	req := httptest.NewRequest(http.MethodPost, "http://x/api/state", nil)
	req.Host = "127.0.0.1"
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST on a read-only endpoint: status=%d, want 405", rr.Code)
	}
}

// TestHandleStateDegradesGracefullyOnBrokenConfig is a regression test for
// the fix in this pass: handleState used to return a raw HTTP 500 when
// config.json failed to parse, which blanks the ENTIRE dashboard on every
// load and every Refresh click -- exactly the moment a working dashboard
// matters most, since a broken config.json is itself what someone would
// come here to diagnose. It must now report the failure as a normal 200
// with config_error set, not an HTTP error status.
func TestHandleStateDegradesGracefullyOnBrokenConfig(t *testing.T) {
	s := newTestServer(t)
	home := os.Getenv("HOME")
	dir := filepath.Join(home, ".config", "claude-burst")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte("{not valid json"), 0600); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "http://x/api/state", nil)
	req.Host = "127.0.0.1"
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200 even though config.json is broken (body=%s)", rr.Code, rr.Body.String())
	}
	var resp stateResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("response was not valid JSON: %v\nbody=%s", err, rr.Body.String())
	}
	if resp.ConfigError == "" {
		t.Fatal("expected config_error to be set")
	}
	if resp.Version != "test-version" {
		t.Errorf("version=%q, want test-version -- even the degraded response should say what it can", resp.Version)
	}
}

// TestHandleStateHealthyPath is the complement: with a valid config.json,
// handleState must NOT set config_error, and must reflect the actual
// configured primary/secondary rather than leaving them zero-valued.
func TestHandleStateHealthyPath(t *testing.T) {
	s := newTestServer(t)
	writeConfig(t, os.Getenv("HOME"))

	req := httptest.NewRequest(http.MethodGet, "http://x/api/state", nil)
	req.Host = "127.0.0.1"
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200 (body=%s)", rr.Code, rr.Body.String())
	}
	var resp stateResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("response was not valid JSON: %v", err)
	}
	if resp.ConfigError != "" {
		t.Fatalf("unexpected config_error with a valid config.json: %q", resp.ConfigError)
	}
	if resp.Primary.Provider == "" {
		t.Error("expected a non-empty primary provider from a valid config")
	}
}

// TestHandleTestConnection_BaseURLModeShortCircuits verifies base-url mode
// reports OK without making any network call: there is no separate "real
// traffic path" to test independent of ANTHROPIC_BASE_URL, which /api/state
// already reports.
// TestHandleLog_ServesTailWhenOversized verifies the size cap actually
// truncates: a log bigger than logTailBytes must return roughly that much
// content (plus the truncation banner), not the whole file, since the
// gateway's log can now grow to 200MB under rotation and this endpoint
// feeds a browser tab.
func TestHandleLog_ServesTailWhenOversized(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".config", "claude-burst")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}

	big := make([]byte, logTailBytes*2)
	for i := range big {
		big[i] = 'x'
	}
	if err := os.WriteFile(filepath.Join(dir, "claude-burst.log"), big, 0600); err != nil {
		t.Fatal(err)
	}

	s := newTestServer(t)
	t.Setenv("HOME", home) // newTestServer reset HOME to its own temp dir; point it back
	req := httptest.NewRequest(http.MethodGet, "http://x/api/log", nil)
	req.Host = "127.0.0.1"
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200 (body=%s)", rr.Code, rr.Body.String())
	}
	if rr.Body.Len() >= len(big) {
		t.Fatalf("expected a truncated response, got %d bytes for a %d-byte log", rr.Body.Len(), len(big))
	}
	if !strings.Contains(rr.Body.String(), "showing the last") {
		t.Fatalf("expected a truncation banner, got: %.200s", rr.Body.String())
	}
}

func TestHandleTestConnection_BaseURLModeShortCircuits(t *testing.T) {
	s := newTestServer(t)
	writeConfig(t, os.Getenv("HOME")) // config.Default() -> base-url mode

	req := httptest.NewRequest(http.MethodGet, "http://x/api/test-connection", nil)
	req.Host = "127.0.0.1"
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)

	var resp testConnectionResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("response was not valid JSON: %v (body=%s)", err, rr.Body.String())
	}
	if !resp.OK || resp.Mode != "base-url" {
		t.Fatalf("got %+v, want ok=true mode=base-url", resp)
	}
}

// TestHandleTestConnection_TransparentModeUnreachableHostReportsFailure is a
// regression test for the actual 2026-09-03 incident this endpoint exists to
// catch: in transparent mode, if the intercepted hostname doesn't actually
// lead back to this gateway, the dashboard must say so rather than silently
// looking idle. Using a hostname that cannot resolve at all exercises the
// "could not reach it" branch of that same failure family (the "reached it,
// but it wasn't us" branch needs a real TLS listener on the exact intercept
// host:port, which isn't worth faking here) -- both branches report ok=false
// with an actionable detail, which is what the dashboard renders.
func TestHandleTestConnectionTransparentModeUnreachableHostReportsFailure(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, ".config", "claude-burst")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Intercept.Mode = config.InterceptTransparent
	cfg.Intercept.Host = "this-host-does-not-resolve.invalid"
	b, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.json"), b, 0600); err != nil {
		t.Fatal(err)
	}
	// Set HOME after writing config.json, and build the gateway/admin Server
	// directly (not via newTestServer, which allocates and switches HOME to
	// its own separate temp dir) -- same pattern as
	// TestHandleForceRejectsWhenNoSecondaryConfigured, for the same reason.
	t.Setenv("HOME", home)

	gdir := t.TempDir()
	gw, err := router.New(cfg, filepath.Join(gdir, "state.json"), filepath.Join(gdir, "metrics.jsonl"), log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	s := New(gw, filepath.Join(gdir, "metrics.jsonl"), "v", "", "/path/to/transparent-root.sh")

	req := httptest.NewRequest(http.MethodGet, "http://x/api/test-connection", nil)
	req.Host = "127.0.0.1"
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)

	var resp testConnectionResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("response was not valid JSON: %v (body=%s)", err, rr.Body.String())
	}
	if resp.OK {
		t.Fatalf("an unresolvable intercept host must report ok=false, got %+v", resp)
	}
	if resp.Mode != "transparent" || resp.Detail == "" {
		t.Fatalf("expected a transparent-mode failure with a non-empty detail, got %+v", resp)
	}
}

// TestHandleForceRejectsWhenNoSecondaryConfigured verifies force-secondary
// fails with a clear error rather than silently no-op'ing (or worse,
// activating an overflow window with nowhere to actually send traffic) when
// --secondary none is configured.
func TestHandleForceRejectsWhenNoSecondaryConfigured(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, ".config", "claude-burst")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Secondary = config.RouteConfig{Provider: config.ProviderNone}
	b, _ := json.Marshal(cfg)
	if err := os.WriteFile(filepath.Join(dir, "config.json"), b, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)

	// The gateway must be built with the SAME no-secondary config as disk,
	// not config.Default() on its own: Default()'s BedrockBaseURL is
	// non-empty, so ResolveRoutes (called inside router.New) auto-fills a
	// bedrock secondary -- which would make the running gateway actually
	// have a secondary, the opposite of what this test means to exercise,
	// now that handleForce validates against the live gateway (see
	// router.Server.HasSecondary) rather than a freshly re-read config file.
	gdir := t.TempDir()
	gw, err := router.New(cfg, filepath.Join(gdir, "state.json"), filepath.Join(gdir, "metrics.jsonl"), log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	s := New(gw, filepath.Join(gdir, "metrics.jsonl"), "v", "", "/path")

	req := httptest.NewRequest(http.MethodPost, "http://x/api/force", nil)
	req.Host = "127.0.0.1"
	req.Header.Set(mutationHeader, "1")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400 when no secondary is configured (body=%s)", rr.Code, rr.Body.String())
	}
}

// TestInterceptActive covers the distinction the dashboard was missing:
// "configured" and "actually in the traffic path" are different questions,
// and reporting the first as if it were the second is what let a green
// PRIMARY badge sit over a gateway nothing was talking to.
func TestInterceptActive(t *testing.T) {
	baseURL := config.Default()
	baseURL.Listen = "127.0.0.1:7777"
	baseURL.Intercept.Mode = config.InterceptBaseURL

	transparent := config.Default()
	transparent.Listen = "127.0.0.1:7777"
	transparent.Intercept.Mode = "transparent"
	transparent.Intercept.Host = "api.anthropic.com"

	cases := []struct {
		name       string
		cfg        config.Config
		ii         interceptInfo
		wantActive bool
		wantReason string // substring
	}{
		{"base-url enabled", baseURL,
			interceptInfo{SettingsURL: "http://127.0.0.1:7777"}, true, ""},
		{"base-url configured but never enabled", baseURL,
			interceptInfo{}, false, "ANTHROPIC_BASE_URL is not set"},
		{"base-url pointing somewhere else", baseURL,
			interceptInfo{SettingsURL: "http://127.0.0.1:9999"}, false, "not this gateway"},
		{"transparent fully installed", transparent,
			interceptInfo{Host: "api.anthropic.com", HostsEntry: true, CATrusted: true}, true, ""},
		{"transparent configured but not installed", transparent,
			interceptInfo{Host: "api.anthropic.com"}, false, "not installed"},
		{"transparent missing hosts entry", transparent,
			interceptInfo{Host: "api.anthropic.com", CATrusted: true}, false, "/etc/hosts redirect is missing"},
		// Worse than inactive: traffic arrives and is rejected. Must not
		// report active just because the redirect is in place.
		{"transparent redirect without CA trust", transparent,
			interceptInfo{Host: "api.anthropic.com", HostsEntry: true}, false, "fail TLS"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			active, reason := interceptActive(c.cfg, c.ii)
			if active != c.wantActive {
				t.Errorf("active = %v, want %v (reason %q)", active, c.wantActive, reason)
			}
			if c.wantReason != "" && !strings.Contains(reason, c.wantReason) {
				t.Errorf("reason %q does not mention %q", reason, c.wantReason)
			}
			if c.wantActive && reason != "" {
				t.Errorf("active but still gave a reason: %q", reason)
			}
		})
	}
}

// newSecondaryTestServer is newTestServer with the two Keychain operations
// replaced by in-memory fakes, and returns HOME so a test can inspect the
// config.json that was actually written. Nothing here may touch the real
// login Keychain: `security add-generic-password -U` overwrites, so a test
// storing under a plausible service name would destroy the key a live
// secondary is running on.
func newSecondaryTestServer(t *testing.T) (*Server, string, map[string]string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)

	dir := t.TempDir()
	gw, err := router.New(config.Default(), filepath.Join(dir, "state.json"), filepath.Join(dir, "metrics.jsonl"), log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	s := New(gw, filepath.Join(dir, "metrics.jsonl"), "test-version", "", "/path/to/transparent-root.sh")
	stored := map[string]string{}
	s.storeKey = func(service, value string) error { stored[service] = value; return nil }
	s.hasKey = func(service, envVar string) bool { _, ok := stored[service]; return ok }
	return s, home, stored
}

func postSecondary(t *testing.T, s *Server, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/api/secondary", strings.NewReader(body))
	req.Host = "127.0.0.1"
	req.Header.Set(mutationHeader, "1")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	return rr
}

func loadSavedConfig(t *testing.T, home string) config.Config {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(home, ".config", "claude-burst", "config.json"))
	if err != nil {
		t.Fatalf("config.json was not written: %v", err)
	}
	var cfg config.Config
	if err := json.Unmarshal(b, &cfg); err != nil {
		t.Fatalf("config.json does not parse: %v", err)
	}
	return cfg
}

// TestSecondarySavesProviderAndKey is the whole point of the form: an
// openai-compatible secondary configured from the browser must land in
// config.json under the same field names the gateway reads, with the secret
// in the Keychain rather than in that file.
func TestSecondarySavesProviderAndKey(t *testing.T) {
	s, home, stored := newSecondaryTestServer(t)
	writeConfig(t, home)

	rr := postSecondary(t, s, `{"provider":"openai-compatible","base_url":"https://api.z.ai/api/coding/paas/v4/",
		"model":"glm-4.6","keychain_service":"claude-burst-zai","api_key":"sk-secret"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}

	cfg := loadSavedConfig(t, home)
	if cfg.Secondary.Provider != "openai-compatible" {
		t.Errorf("provider = %q, want openai-compatible", cfg.Secondary.Provider)
	}
	// The trailing slash must be gone: config.Load trims it on read, but a
	// value that only becomes correct on the way back in is one refactor
	// from being concatenated with a path and producing a double slash.
	if cfg.Secondary.BaseURL != "https://api.z.ai/api/coding/paas/v4" {
		t.Errorf("base_url = %q, want the trailing slash trimmed", cfg.Secondary.BaseURL)
	}
	if cfg.Secondary.Model != "glm-4.6" {
		t.Errorf("model = %q, want glm-4.6", cfg.Secondary.Model)
	}
	if cfg.Secondary.KeychainService != "claude-burst-zai" {
		t.Errorf("keychain_service = %q, want claude-burst-zai", cfg.Secondary.KeychainService)
	}
	if stored["claude-burst-zai"] != "sk-secret" {
		t.Errorf("key stored under %v, want it under claude-burst-zai", stored)
	}
	// The secret must not be anywhere in the file, under any field name.
	raw, _ := os.ReadFile(filepath.Join(home, ".config", "claude-burst", "config.json"))
	if strings.Contains(string(raw), "sk-secret") {
		t.Fatal("the API key was written into config.json in the clear")
	}
	// Nor may it come back out of the API in any response.
	if strings.Contains(rr.Body.String(), "sk-secret") {
		t.Fatal("the API key was echoed back in the response")
	}
}

// TestSecondaryBlankKeyKeepsStoredOne covers the ordinary edit -- changing a
// model or endpoint without re-typing the secret. A blank field meaning
// "wipe it" would silently disarm failover on the most routine action the
// form supports.
func TestSecondaryBlankKeyKeepsStoredOne(t *testing.T) {
	s, home, stored := newSecondaryTestServer(t)
	writeConfig(t, home)
	stored["claude-burst-zai"] = "sk-existing"

	rr := postSecondary(t, s, `{"provider":"openai-compatible","base_url":"https://api.z.ai/v4",
		"model":"glm-4.7","keychain_service":"claude-burst-zai","api_key":""}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if stored["claude-burst-zai"] != "sk-existing" {
		t.Fatalf("stored key = %q, want the existing one untouched", stored["claude-burst-zai"])
	}
	var resp secondaryResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Warning != "" {
		t.Errorf("warned about a missing key when one is stored: %q", resp.Warning)
	}
}

// TestSecondaryWarnsWithNoKey locks in the saved-but-not-working case being
// reported at save time rather than at the first real failover.
func TestSecondaryWarnsWithNoKey(t *testing.T) {
	s, home, _ := newSecondaryTestServer(t)
	writeConfig(t, home)

	rr := postSecondary(t, s, `{"provider":"openai-compatible","base_url":"https://api.z.ai/v4","model":"glm-4.6"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var resp secondaryResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Warning == "" {
		t.Fatal("saved a secondary with no credential anywhere and reported no warning")
	}
	// The warning has to name both places a key could come from, or it
	// sends the reader looking in only one of them.
	if !strings.Contains(resp.Warning, "TOGETHER_API_KEY") {
		t.Errorf("warning does not name the env var the gateway reads: %q", resp.Warning)
	}
}

// TestSecondaryKeyStoreFailureAbortsSave is the ordering the handler exists
// to get right: a config naming a provider whose key was never stored defers
// the failure to the moment the primary runs out.
func TestSecondaryKeyStoreFailureAbortsSave(t *testing.T) {
	s, home, _ := newSecondaryTestServer(t)
	writeConfig(t, home)
	s.storeKey = func(service, value string) error { return errUnavailable }

	rr := postSecondary(t, s, `{"provider":"openai-compatible","base_url":"https://api.z.ai/v4","model":"glm-4.6","api_key":"sk-x"}`)
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d, want 500 (body=%s)", rr.Code, rr.Body.String())
	}
	if cfg := loadSavedConfig(t, home); cfg.Secondary.Provider == "openai-compatible" {
		t.Fatal("config.json was updated even though storing the key failed")
	}
}

var errUnavailable = errors.New("keychain unavailable")

// TestSecondaryNoneClearsLegacyBedrockField guards the resurrection trap
// documented on config.ProviderNone: ResolveRoutes rebuilds a bedrock
// secondary out of the legacy flat field whenever the slot is empty, so
// "none" that leaves bedrock_base_url set silently undoes itself.
func TestSecondaryNoneClearsLegacyBedrockField(t *testing.T) {
	s, home, _ := newSecondaryTestServer(t)
	writeConfig(t, home)

	if rr := postSecondary(t, s, `{"provider":"none"}`); rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	cfg := loadSavedConfig(t, home)
	if cfg.Secondary.Provider != config.ProviderNone {
		t.Errorf("secondary provider = %q, want %q", cfg.Secondary.Provider, config.ProviderNone)
	}
	if cfg.BedrockBaseURL != "" {
		t.Errorf("bedrock_base_url = %q, want it cleared", cfg.BedrockBaseURL)
	}
	// The real proof: reload through the same path the gateway uses.
	reloaded, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Secondary.Provider != config.ProviderNone {
		t.Fatalf("after reload the secondary came back as %q -- 'none' did not stick", reloaded.Secondary.Provider)
	}
}

// TestSecondaryRejectsBadInput keeps the validation at the form rather than
// at gateway startup, where a bad value means a gateway that will not start
// and a dashboard that is gone with it.
func TestSecondaryRejectsBadInput(t *testing.T) {
	cases := []struct{ name, body string }{
		{"no base url", `{"provider":"openai-compatible","model":"glm-4.6"}`},
		{"no model", `{"provider":"openai-compatible","base_url":"https://api.z.ai/v4"}`},
		{"bare host", `{"provider":"openai-compatible","base_url":"api.z.ai/v4","model":"glm-4.6"}`},
		{"no scheme", `{"provider":"openai-compatible","base_url":"//api.z.ai/v4","model":"glm-4.6"}`},
		{"unknown provider", `{"provider":"vertex","base_url":"https://x.example/v1","model":"m"}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s, home, _ := newSecondaryTestServer(t)
			writeConfig(t, home)
			rr := postSecondary(t, s, c.body)
			if rr.Code != http.StatusBadRequest {
				t.Fatalf("status=%d, want 400 (body=%s)", rr.Code, rr.Body.String())
			}
		})
	}
}

// TestStateReportsSecondaryCredential covers the half of this the form reads
// rather than writes: the dashboard must be able to say "a key exists" (and
// under which names) without the key ever crossing the wire.
func TestStateReportsSecondaryCredential(t *testing.T) {
	s, home, stored := newSecondaryTestServer(t)
	writeConfig(t, home)
	stored["claude-burst-openrouter"] = "sk-live"

	if rr := postSecondary(t, s, `{"provider":"openai-compatible","base_url":"https://openrouter.ai/api/v1",
		"model":"z-ai/glm-4.6","keychain_service":"claude-burst-openrouter"}`); rr.Code != http.StatusOK {
		t.Fatalf("save failed: %s", rr.Body.String())
	}

	req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1/api/state", nil)
	req.Host = "127.0.0.1"
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var st stateResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &st); err != nil {
		t.Fatal(err)
	}
	if st.Secondary.KeychainService != "claude-burst-openrouter" {
		t.Errorf("keychain_service = %q", st.Secondary.KeychainService)
	}
	if st.Secondary.KeyEnvVar != "OPENROUTER_API_KEY" {
		t.Errorf("key_env_var = %q, want OPENROUTER_API_KEY", st.Secondary.KeyEnvVar)
	}
	if !st.Secondary.KeyPresent {
		t.Error("key_present = false, but a key is stored under that service")
	}
	if strings.Contains(rr.Body.String(), "sk-live") {
		t.Fatal("/api/state leaked the API key")
	}
	// A primary with no credential of its own must report no env var at
	// all, not an empty-looking "missing key" -- the UI distinguishes the
	// two on exactly this field.
	if st.Primary.KeyEnvVar != "" {
		t.Errorf("primary key_env_var = %q, want empty for oauth-passthrough", st.Primary.KeyEnvVar)
	}
}

// TestCredentialNamesMatchesBuildProvider pins the defaulting to the same
// rules router.buildProvider applies. If these drift, the form offers to
// store a key under a service the gateway never looks in, and nothing says
// so until a failover finds no credentials.
func TestCredentialNamesMatchesBuildProvider(t *testing.T) {
	cases := []struct {
		name           string
		rc             config.RouteConfig
		defaultService string
		wantService    string
		wantEnvVar     string
	}{
		{"openai-compatible explicit", config.RouteConfig{Provider: "openai-compatible", KeychainService: "claude-burst-zai"}, "claude-burst-bedrock", "claude-burst-zai", "ZAI_API_KEY"},
		{"openai-compatible default", config.RouteConfig{Provider: "openai-compatible"}, "claude-burst-bedrock", "claude-burst-together", "TOGETHER_API_KEY"},
		{"bedrock explicit", config.RouteConfig{Provider: "bedrock", KeychainService: "custom"}, "claude-burst-bedrock", "custom", "AWS_BEARER_TOKEN_BEDROCK"},
		{"bedrock default", config.RouteConfig{Provider: "bedrock"}, "claude-burst-bedrock", "claude-burst-bedrock", "AWS_BEARER_TOKEN_BEDROCK"},
		{"oauth-passthrough has none", config.RouteConfig{Provider: "oauth-passthrough"}, "claude-burst-bedrock", "", ""},
		{"none has none", config.RouteConfig{Provider: config.ProviderNone}, "claude-burst-bedrock", "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			service, envVar := credentialNames(c.rc, c.defaultService)
			if service != c.wantService || envVar != c.wantEnvVar {
				t.Fatalf("credentialNames = (%q, %q), want (%q, %q)", service, envVar, c.wantService, c.wantEnvVar)
			}
		})
	}
}

// TestSecondarySwitchingVendorDoesNotInheritKeychainService is the
// regression test for a bug this form had on its first run: with a blank
// Keychain-service field it carried the PREVIOUS secondary's service name
// forward regardless of vendor, so switching a bedrock secondary to an
// openai-compatible one derived "claude-burst-bedrock" -> $BEDROCK_API_KEY
// and offered to store the new provider's key under Bedrock's entry.
// `security add-generic-password -U` overwrites, so saving would have
// destroyed the Bedrock credential -- the same vendor collision
// cmd/claude-burst's keychainTarget doc comment describes.
func TestSecondarySwitchingVendorDoesNotInheritKeychainService(t *testing.T) {
	s, home, stored := newSecondaryTestServer(t)
	writeConfig(t, home) // config.Default() resolves to a bedrock secondary
	stored["claude-burst-bedrock"] = "bedrock-token"

	rr := postSecondary(t, s, `{"provider":"openai-compatible","base_url":"https://api.z.ai/v4","model":"glm-4.6","api_key":"sk-glm"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if stored["claude-burst-bedrock"] != "bedrock-token" {
		t.Fatalf("the Bedrock key was overwritten with the new provider's: %q", stored["claude-burst-bedrock"])
	}
	if cfg := loadSavedConfig(t, home); cfg.Secondary.KeychainService == "claude-burst-bedrock" {
		t.Fatal("openai-compatible secondary inherited Bedrock's keychain service")
	}
}
