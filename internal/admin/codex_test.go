package admin

import (
	"context"
	"encoding/json"
	"github.com/andrewbakercloudscale/claude-burst/internal/codex"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/andrewbakercloudscale/claude-burst/internal/config"
	"github.com/andrewbakercloudscale/claude-burst/internal/metrics"
)

// withCodexGateway gives s a running Codex gateway on a free port, named in
// this test's config.json, so nothing touches the real 7779.
func withCodexGateway(t *testing.T, s *Server) string {
	t.Helper()
	g, err := codex.New("http://127.0.0.1:1", filepath.Join(t.TempDir(), "codex-metrics.jsonl"), log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	gw := httptest.NewServer(g)
	t.Cleanup(gw.Close)
	addr := strings.TrimPrefix(gw.URL, "http://")
	setCodexListen(t, addr)
	g.SetListenState(true, "")
	s.SetCodex(g, "")
	return addr
}

func setCodexListen(t *testing.T, addr string) {
	t.Helper()
	home, _ := os.UserHomeDir()
	dir := filepath.Join(home, ".config", "claude-burst")
	os.MkdirAll(dir, 0o700)
	cfg := config.Default()
	cfg.Codex.Listen = addr
	b, _ := json.Marshal(cfg)
	if err := os.WriteFile(filepath.Join(dir, "config.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// A listener that failed to start is never routed to: 409, file untouched.
func TestCodexEnableRefusedWhenListenerFailed(t *testing.T) {
	s := newTestServer(t)
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	orig := "notify = 1\n"
	os.WriteFile(filepath.Join(codexHome, "config.toml"), []byte(orig), 0o600)
	setCodexListen(t, "127.0.0.1:1")
	s.SetCodex(nil, "bind 127.0.0.1:1: address already in use")
	rr := mutate(t, s, "/api/codex/enable", "")
	if rr.Code != http.StatusConflict || !strings.Contains(rr.Body.String(), "address already in use") {
		t.Fatalf("got %d %s", rr.Code, rr.Body)
	}
	if b, _ := os.ReadFile(filepath.Join(codexHome, "config.toml")); string(b) != orig {
		t.Errorf("config.toml changed: %q", b)
	}
}

func codexStateOf(t *testing.T, s *Server) codexState {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1/api/codex", nil)
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("/api/codex: %d %s", rr.Code, rr.Body)
	}
	var st codexState
	if err := json.Unmarshal(rr.Body.Bytes(), &st); err != nil {
		t.Fatal(err)
	}
	return st
}

// The dashboard's Enable and Disable edit Codex's config.toml (here a temp
// CODEX_HOME, never the real ~/.codex) and /api/codex reports the result.
func TestCodexEnableDisableFromDashboard(t *testing.T) {
	s := newTestServer(t)
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	orig := "notify = 1\n"
	os.WriteFile(filepath.Join(codexHome, "config.toml"), []byte(orig), 0o600)
	addr := withCodexGateway(t, s)

	if st := codexStateOf(t, s); st.Status.Enabled || st.Listen != addr {
		t.Fatalf("before: %+v", st)
	}
	if rr := mutate(t, s, "/api/codex/enable", ""); rr.Code != http.StatusOK {
		t.Fatalf("enable: %d %s", rr.Code, rr.Body)
	}
	st := codexStateOf(t, s)
	if !st.Status.Enabled || st.Status.BaseURL != "http://"+addr+"/backend-api/codex" {
		t.Errorf("after enable: %+v", st.Status)
	}
	if rr := mutate(t, s, "/api/codex/disable", ""); rr.Code != http.StatusOK {
		t.Fatalf("disable: %d %s", rr.Code, rr.Body)
	}
	if b, _ := os.ReadFile(filepath.Join(codexHome, "config.toml")); string(b) != orig {
		t.Errorf("disable left %q", b)
	}
}

// A provider the user chose is refused with the reason, file untouched.
func TestCodexEnableConflictIsReported(t *testing.T) {
	s := newTestServer(t)
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	os.WriteFile(filepath.Join(codexHome, "config.toml"), []byte("model_provider = \"ollama\"\n"), 0o600)
	withCodexGateway(t, s)
	rr := mutate(t, s, "/api/codex/enable", "")
	if rr.Code != http.StatusConflict || !strings.Contains(rr.Body.String(), "ollama") {
		t.Fatalf("got %d %s", rr.Code, rr.Body)
	}
}

// Enable needs the dashboard's mutation header like every other change.
func TestCodexEnableNeedsMutationHeader(t *testing.T) {
	s := newTestServer(t)
	t.Setenv("CODEX_HOME", t.TempDir())
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/api/codex/enable", nil)
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("got %d", rr.Code)
	}
}

// Context per session: the newest successful turn's whole input plus its
// output, against the model's window; errors carry no tokens and are skipped.
func TestCodexSessionContext(t *testing.T) {
	now := time.Now()
	evs := []metrics.Event{ // newest first, as metrics.Recent returns them
		{Time: now, SessionID: "a", Model: "gpt-6.1-sol", HTTPStatus: 429},
		{Time: now.Add(-time.Minute), SessionID: "a", Model: "gpt-6.1-sol", HTTPStatus: 200, InputTokens: 1000, CacheReadTokens: 135000, OutputTokens: 1000},
		{Time: now.Add(-2 * time.Minute), SessionID: "b", Model: "unknown-model", HTTPStatus: 200, InputTokens: 500, OutputTokens: 10},
		{Time: now.Add(-3 * time.Minute), SessionID: "a", Model: "gpt-6.1-sol", HTTPStatus: 200, InputTokens: 9000, OutputTokens: 100},
	}
	got := codexSessions(evs, map[string]int64{"gpt-6.1-sol": 272000})
	if len(got) != 2 || got[0].ID != "a" || got[1].ID != "b" {
		t.Fatalf("sessions %+v", got)
	}
	a := got[0]
	if a.Context != 137000 || a.Window != 272000 || a.Turns != 3 || a.Errors != 1 || a.Percent < 50.3 || a.Percent > 50.4 {
		t.Errorf("a = %+v", a)
	}
	if b := got[1]; b.Context != 510 || b.Window != 0 || b.Percent != 0 {
		t.Errorf("b = %+v", b)
	}
}

// Windows come from Codex's own model cache when the gateway has not seen
// the model list since it started.
func TestCodexWindowsFromCodexCache(t *testing.T) {
	s := newTestServer(t)
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	os.WriteFile(filepath.Join(codexHome, "models_cache.json"), []byte(`{"models":[{"slug":"gpt-6.1-sol","context_window":272000}]}`), 0o600)
	if w := s.codexWindows(); w["gpt-6.1-sol"] != 272000 {
		t.Errorf("windows %v", w)
	}
}

// The trace walks every hop against a fake ChatGPT and gateway: the signed-in
// hop sends Codex's own token, and the token never appears in the result.
func TestCodexTraceHops(t *testing.T) {
	s := newTestServer(t)
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	os.WriteFile(filepath.Join(codexHome, "auth.json"), []byte(`{"auth_mode":"chatgpt","tokens":{"access_token":"secret-token","account_id":"acct"}}`), 0o600)

	var gotAuth string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		gotAuth = r.Header.Get("Authorization")
		w.Write([]byte(`{"models":[{"slug":"m","context_window":1}]}`))
	}))
	defer up.Close()
	cfg := config.Default()
	cfg.Codex.Listen = strings.TrimPrefix(up.URL, "http://") // the "gateway" is the fake too
	cfg.Codex.Upstream = up.URL
	s.trace.lookupHost = func(context.Context, string) ([]string, error) { return []string{"203.0.113.1"}, nil }
	s.trace.findMtr = func() string { return "" }

	res := s.runCodexTrace(context.Background(), cfg)
	var states []string
	for _, h := range res.Hops {
		states = append(states, h.Key+"="+h.State)
	}
	want := "config=warn gateway=ok login=ok dns=ok direct=ok through=ok network=skip"
	if strings.Join(states, " ") != want {
		t.Errorf("hops %s\nwant %s", strings.Join(states, " "), want)
	}
	if gotAuth != "Bearer secret-token" {
		t.Errorf("signed-in hop sent %q", gotAuth)
	}
	b, _ := json.Marshal(res)
	if strings.Contains(string(b), "secret-token") {
		t.Error("the token leaked into the trace")
	}
}

// A dead Codex port stops the trace there with a failure, not a pass.
func TestCodexTraceDeadPort(t *testing.T) {
	s := newTestServer(t)
	t.Setenv("CODEX_HOME", t.TempDir())
	cfg := config.Default()
	cfg.Codex.Listen = "127.0.0.1:1"
	res := s.runCodexTrace(context.Background(), cfg)
	if res.State != hopBad || !strings.Contains(res.Verdict, "nothing answers") {
		t.Errorf("%+v", res)
	}
}

// No Codex CLI: the test turn says so instead of failing obscurely.
func TestCodexTestTurnWithoutCLI(t *testing.T) {
	s := newTestServer(t)
	s.trace.findCodex = func() string { return "" }
	res := s.runCodexTestTurn(context.Background(), config.Default())
	if res.OK || !strings.Contains(res.Detail, "not found") {
		t.Errorf("%+v", res)
	}
}

// Codex runs and exits 0, but nothing reached the gateway: a failure.
func TestCodexTestTurnMustPassThroughBurst(t *testing.T) {
	s := newTestServer(t)
	s.trace.findCodex = func() string { return "/fake/codex" }
	var args []string
	s.trace.run = func(ctx context.Context, env []string, name string, a ...string) ([]byte, []byte, error) {
		args = a
		return []byte("pong"), nil, nil
	}
	res := s.runCodexTestTurn(context.Background(), config.Default())
	if res.OK || !strings.Contains(res.Detail, "no turn reached Burst") {
		t.Errorf("%+v", res)
	}
	if !strings.Contains(strings.Join(args, " "), `base_url="http://127.0.0.1:7779/backend-api/codex"`) || !strings.Contains(strings.Join(args, " "), "--ignore-user-config") {
		t.Errorf("args %q", args)
	}
}

// The test turn is matched by the session id Codex prints: another session's
// turn recorded meanwhile is not taken for it.
func TestCodexTestTurnMatchesItsOwnSession(t *testing.T) {
	s := newTestServer(t)
	s.trace.findCodex = func() string { return "/fake/codex" }
	mp, _ := config.CodexMetricsPath()
	os.MkdirAll(filepath.Dir(mp), 0o700)
	w := metrics.New(mp)
	s.trace.run = func(ctx context.Context, env []string, name string, a ...string) ([]byte, []byte, error) {
		// A busy real session's turn lands while the test runs; the test's own never does.
		w.Write(metrics.Event{Time: time.Now(), SessionID: "someone-else", Model: "gpt-x", HTTPStatus: 200, InputTokens: 9})
		return []byte("pong"), []byte("session id: test-session\n"), nil
	}
	res := s.runCodexTestTurn(context.Background(), config.Default())
	if res.OK || !strings.Contains(res.Detail, "no turn reached Burst") {
		t.Errorf("%+v", res)
	}
}
