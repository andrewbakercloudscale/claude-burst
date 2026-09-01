package admin

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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
