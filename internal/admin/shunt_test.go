package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/andrewbakercloudscale/claude-burst/internal/config"
	"github.com/andrewbakercloudscale/claude-burst/internal/keychain"
	"github.com/andrewbakercloudscale/claude-burst/internal/shunt"
)

// shuntServer returns a server whose secondary is an openai-compatible
// endpoint, with the Keychain stubbed to report a key as present or absent.
func shuntServer(t *testing.T, keyPresent bool) (*Server, string) {
	t.Helper()
	s := newTestServer(t)
	home := os.Getenv("HOME")
	cfg := config.Default()
	cfg.Secondary = config.RouteConfig{
		Provider: "openai-compatible", BaseURL: "https://api.example.test/v1",
		Model: "glm-x", KeychainService: "claude-burst-fake",
	}
	if err := config.Save(cfg); err != nil {
		t.Fatal(err)
	}
	s.keyInfo = func(service, envVar string) keychain.Info { return keychain.Info{Present: keyPresent} }
	s.shuntBin = "/opt/bin/claude-burst"
	return s, home
}

func settingsOf(t *testing.T, home string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(home, ".claude", "settings.json"))
	if os.IsNotExist(err) {
		return map[string]any{}
	}
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func seedUserHook(t *testing.T, home string) {
	t.Helper()
	dir := filepath.Join(home, ".claude")
	os.MkdirAll(dir, 0o700)
	os.WriteFile(filepath.Join(dir, "settings.json"), []byte(`{"model":"sonnet","hooks":{"PreToolUse":[{"matcher":"Bash","hooks":[{"type":"command","command":"~/theirs.sh"}]}]}}`), 0o600)
}

func TestShuntEnableInstallsHookAndSkillAndDisableRemovesThem(t *testing.T) {
	s, home := shuntServer(t, true)
	seedUserHook(t, home)

	rr := mutate(t, s, "/api/shunt", `{"read":true,"write":true}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	cfg, _ := config.Load()
	if !cfg.Shunt.Read || !cfg.Shunt.Write {
		t.Fatalf("config not saved: %+v", cfg.Shunt)
	}
	root := settingsOf(t, home)
	if !shunt.HookInstalled(root) {
		t.Fatalf("guard hook missing from settings.json: %v", root)
	}
	groups := root["hooks"].(map[string]any)["PreToolUse"].([]any)
	if len(groups) != 2 || root["model"] != "sonnet" {
		t.Errorf("the user's own hook and settings must survive: %v", root)
	}
	if !shunt.SkillInstalled() {
		t.Errorf("skill not installed")
	}
	if st := stateOf(t, s).Shunt; !st.Read || !st.Write || !st.HookInstalled || !st.SkillInstalled || !st.WorkerReady {
		t.Errorf("state should describe the installed feature: %+v", st)
	}

	if rr := mutate(t, s, "/api/shunt", `{"read":false,"write":false}`); rr.Code != http.StatusOK {
		t.Fatalf("disable status=%d body=%s", rr.Code, rr.Body.String())
	}
	root = settingsOf(t, home)
	if shunt.HookInstalled(root) || len(root["hooks"].(map[string]any)["PreToolUse"].([]any)) != 1 {
		t.Errorf("disable must remove only our hook: %v", root)
	}
	if shunt.SkillInstalled() {
		t.Errorf("skill should be removed once nothing is on")
	}
}

func TestShuntHookFollowsReadNotWrite(t *testing.T) {
	s, home := shuntServer(t, true)
	if rr := mutate(t, s, "/api/shunt", `{"read":true,"write":false}`); rr.Code != http.StatusOK {
		t.Fatalf("%d %s", rr.Code, rr.Body.String())
	}
	if !shunt.HookInstalled(settingsOf(t, home)) {
		t.Errorf("read on needs the hook")
	}
	p, _ := shunt.SkillPath()
	b, _ := os.ReadFile(p)
	if !strings.Contains(string(b), "shunt read") || strings.Contains(string(b), "shunt write") {
		t.Errorf("skill must describe only what is on")
	}
	// write only: nothing to guard, so no hook running on every Read and Bash
	if rr := mutate(t, s, "/api/shunt", `{"read":false,"write":true}`); rr.Code != http.StatusOK {
		t.Fatalf("%d %s", rr.Code, rr.Body.String())
	}
	if shunt.HookInstalled(settingsOf(t, home)) {
		t.Errorf("write-only must not leave a guard hook installed")
	}
	if st := stateOf(t, s).Shunt; !st.Write || st.HookInstalled {
		t.Errorf("state: %+v", st)
	}
	b, _ = os.ReadFile(p)
	if !strings.Contains(string(b), "shunt write") || strings.Contains(string(b), "shunt read --question") {
		t.Errorf("skill must follow the switches")
	}
}

func TestShuntCannotBeEnabledWithoutAWorker(t *testing.T) {
	t.Run("no key", func(t *testing.T) {
		s, home := shuntServer(t, false)
		rr := mutate(t, s, "/api/shunt", `{"read":true,"write":false}`)
		if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), "keychain-set") {
			t.Fatalf("want 400 naming the fix, got %d %s", rr.Code, rr.Body.String())
		}
		if cfg, _ := config.Load(); cfg.Shunt.Read {
			t.Errorf("a refused enable must not be saved")
		}
		if shunt.HookInstalled(settingsOf(t, home)) {
			t.Errorf("a refused enable must not install a hook that blocks reads")
		}
	})
	t.Run("bedrock secondary", func(t *testing.T) {
		s := newTestServer(t)
		s.keyInfo = func(string, string) keychain.Info { return keychain.Info{Present: true} }
		rr := mutate(t, s, "/api/shunt", `{"read":true}`)
		if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), "openai-compatible") {
			t.Fatalf("want 400 explaining the secondary, got %d %s", rr.Code, rr.Body.String())
		}
	})
}

// The guard must always be removable. A broken secondary or a lost key is
// exactly when someone comes here to switch it off.
func TestShuntCanAlwaysBeDisabled(t *testing.T) {
	s, home := shuntServer(t, true)
	if rr := mutate(t, s, "/api/shunt", `{"read":true,"write":true}`); rr.Code != http.StatusOK {
		t.Fatalf("%d %s", rr.Code, rr.Body.String())
	}
	s.keyInfo = func(string, string) keychain.Info { return keychain.Info{} } // key vanishes

	st := stateOf(t, s).Shunt
	if st.WorkerReady || st.WorkerError == "" {
		t.Errorf("state must surface the lost key: %+v", st)
	}
	if rr := mutate(t, s, "/api/shunt", `{"read":false,"write":false}`); rr.Code != http.StatusOK {
		t.Fatalf("disable must work without a worker, got %d %s", rr.Code, rr.Body.String())
	}
	if shunt.HookInstalled(settingsOf(t, home)) {
		t.Errorf("hook still installed")
	}
}

func TestShuntThreshold(t *testing.T) {
	s, _ := shuntServer(t, true)
	for _, bad := range []string{`{"read":true,"min_lines":5}`, `{"read":true,"min_lines":999999}`, `{"read":true,"min_lines":-1}`} {
		if rr := mutate(t, s, "/api/shunt", bad); rr.Code != http.StatusBadRequest {
			t.Errorf("%s should be rejected, got %d", bad, rr.Code)
		}
	}
	if rr := mutate(t, s, "/api/shunt", `{"read":true,"min_lines":500}`); rr.Code != http.StatusOK {
		t.Fatalf("%d %s", rr.Code, rr.Body.String())
	}
	if got := stateOf(t, s).Shunt.MinLines; got != 500 {
		t.Errorf("threshold %d want 500", got)
	}
	// omitting it leaves it alone
	mutate(t, s, "/api/shunt", `{"read":true,"write":true}`)
	if got := stateOf(t, s).Shunt.MinLines; got != 500 {
		t.Errorf("an omitted threshold must not reset it, got %d", got)
	}
}

// "On" in config.json over a missing hook is the state the panel exists to
// expose: a green switch over nothing enforcing it.
func TestShuntStateExposesDriftBetweenConfigAndSettings(t *testing.T) {
	s, home := shuntServer(t, true)
	mutate(t, s, "/api/shunt", `{"read":true,"write":false}`)

	os.WriteFile(filepath.Join(home, ".claude", "settings.json"), []byte(`{}`), 0o600) // hook wiped by something else
	st := stateOf(t, s).Shunt
	if !st.Read || st.HookInstalled {
		t.Fatalf("state must show read on with no hook: %+v", st)
	}
	// re-applying the same state repairs it
	if rr := mutate(t, s, "/api/shunt", `{"read":true,"write":false}`); rr.Code != http.StatusOK {
		t.Fatalf("%d %s", rr.Code, rr.Body.String())
	}
	if !stateOf(t, s).Shunt.HookInstalled {
		t.Errorf("re-saving the same state must reinstall the hook")
	}
}

func TestShuntStateReportsSavings(t *testing.T) {
	s, _ := shuntServer(t, true)
	lp, _ := config.ShuntLogPath()
	shunt.Append(lp, shunt.Event{Kind: shunt.KindRead, OK: true, BytesIn: 400_000, BytesOut: 2_000, InputTokens: 100_000, OutputTokens: 500, USD: 0.15})
	shunt.Append(lp, shunt.Event{Kind: shunt.KindDeny, OK: true, BytesIn: 90_000})
	shunt.Append(lp, shunt.Event{Time: time.Now().Add(-10 * 24 * time.Hour), Kind: shunt.KindWrite, OK: true, BytesOut: 8_000, PricingUnknown: true})

	st := stateOf(t, s).Shunt
	if st.Today.Reads != 1 || st.Today.Writes != 0 || st.Today.Denials != 1 {
		t.Errorf("today: %+v", st.Today)
	}
	if st.Last30.Writes != 1 || st.Last30.Reads != 1 || st.Last30.Unpriced != 1 {
		t.Errorf("30d: %+v", st.Last30)
	}
	if want := shunt.EstimateTokens(398_000); st.Today.KeptOutTokens != want {
		t.Errorf("kept-out %d want %d", st.Today.KeptOutTokens, want)
	}
}

func TestShuntEndpointNeedsTheMutationHeader(t *testing.T) {
	s, _ := shuntServer(t, true)
	req, _ := http.NewRequest(http.MethodPost, "http://127.0.0.1/api/shunt", strings.NewReader(`{"read":true}`))
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Errorf("a cross-origin page must not be able to switch this: got %d", rr.Code)
	}
	if cfg, _ := config.Load(); cfg.Shunt.Read {
		t.Errorf("refused request changed config")
	}
}
