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
	"github.com/andrewbakercloudscale/claude-burst/internal/metrics"
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

func getJSON(t *testing.T, s *Server, path string, out any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1"+path, nil)
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("%s status=%d body=%s", path, rr.Code, rr.Body.String())
	}
	if err := json.Unmarshal(rr.Body.Bytes(), out); err != nil {
		t.Fatal(err)
	}
}

func TestWorkerCallsAppearInRequestsAsTokenShunts(t *testing.T) {
	s, _ := shuntServer(t, true)
	lp, _ := config.ShuntLogPath()
	now := time.Now()
	shunt.Append(lp, shunt.Event{Time: now.Add(-3 * time.Minute), Kind: shunt.KindRead, OK: true, Files: 2, BytesIn: 400_000, BytesOut: 2_000,
		InputTokens: 100_000, OutputTokens: 400, Model: "zai-org/GLM-5.3", Destination: "https://api.together.xyz/v1/chat/completions", USD: 0.14, DurationMS: 1800})
	shunt.Append(lp, shunt.Event{Time: now.Add(-2 * time.Minute), Kind: shunt.KindWrite, OK: true, BytesOut: 9_000, Model: "zai-org/GLM-5.3", PricingUnknown: true})
	shunt.Append(lp, shunt.Event{Time: now.Add(-1 * time.Minute), Kind: shunt.KindRead, OK: false, Model: "zai-org/GLM-5.3", Note: "worker returned HTTP 500"})
	shunt.Append(lp, shunt.Event{Time: now, Kind: shunt.KindDeny, OK: true, BytesIn: 90_000, Note: "Read"})

	var rows []requestRow
	getJSON(t, s, "/api/requests?limit=50", &rows)

	if len(rows) != 3 {
		t.Fatalf("worker calls belong in the table, a refusal does not (no worker call followed): got %d rows", len(rows))
	}
	// newest first
	if rows[0].Shunt != "read" || rows[0].ShuntOK == nil || *rows[0].ShuntOK {
		t.Errorf("newest row should be the failed read: %+v", rows[0])
	}
	if rows[0].Note != "worker returned HTTP 500" {
		t.Errorf("a failure must show its reason, got %q", rows[0].Note)
	}
	for _, r := range rows {
		if r.Slot != SlotShunt || r.Route != "token-shunt" {
			t.Errorf("row not tagged as a Token Shunt: %+v", r)
		}
		if r.Model != "zai-org/GLM-5.3" {
			t.Errorf("the row must name the WORKER model, got %q", r.Model)
		}
	}
	last := rows[2]
	if !strings.Contains(last.Note, "2 file(s)") || !strings.Contains(last.Note, "kept out of context") || last.Destination == "" {
		t.Errorf("read row: %+v", last)
	}
	if !rows[1].PricingUnknown || !strings.Contains(rows[1].Note, "straight to disk") {
		t.Errorf("write row: %+v", rows[1])
	}
}

// A worker call is not gateway traffic. If it leaked into metrics.jsonl the
// request counts, the secondary's share and the activity chart would all count
// calls Claude Code never made.
func TestWorkerCallsNeverInflateGatewayTotals(t *testing.T) {
	s, _ := shuntServer(t, true)
	lp, _ := config.ShuntLogPath()
	for i := 0; i < 5; i++ {
		shunt.Append(lp, shunt.Event{Kind: shunt.KindRead, OK: true, BytesIn: 100_000, Model: "glm", USD: 0.05})
	}
	if st := stateOf(t, s); st.Totals.Requests != 0 || st.Today.Requests != 0 || st.Totals.APIEquivalentUSD != 0 {
		t.Errorf("shunt calls must not appear in gateway totals: %+v / %+v", st.Totals, st.Today)
	}
	var h struct {
		Window struct{ Requests int }
	}
	getJSON(t, s, "/api/history?days=14", &h)
	if h.Window.Requests != 0 {
		t.Errorf("the activity chart must not count shunt calls, got %d", h.Window.Requests)
	}
}

func TestRequestsMergeRespectsLimitAndOrdering(t *testing.T) {
	now := time.Now()
	events := []metrics.Event{{Time: now.Add(-10 * time.Second), Route: "anthropic"}, {Time: now.Add(-50 * time.Second), Route: "anthropic"}}
	calls := []shunt.Event{{Time: now.Add(-30 * time.Second), Kind: shunt.KindRead, OK: true}, {Time: now.Add(-5 * time.Second), Kind: shunt.KindWrite, OK: true}}
	rows := mergeRequests(events, calls, 3)
	if len(rows) != 3 {
		t.Fatalf("limit not applied: %d", len(rows))
	}
	want := []string{"write", "", "read"} // -5s shunt, -10s gateway, -30s shunt; the -50s row is cut
	for i, w := range want {
		if rows[i].Shunt != w {
			t.Errorf("row %d: shunt=%q want %q", i, rows[i].Shunt, w)
		}
	}
	// ordinary gateway rows serialise exactly as before: no shunt fields leak in
	b, _ := json.Marshal(rows[1])
	if strings.Contains(string(b), "shunt") {
		t.Errorf("a gateway row must not carry shunt fields: %s", b)
	}
}

func TestShuntActivityListsCallsAndRefusalsNewestFirst(t *testing.T) {
	s, _ := shuntServer(t, true)
	lp, _ := config.ShuntLogPath()
	now := time.Now()
	shunt.Append(lp, shunt.Event{Time: now.Add(-2 * time.Minute), Kind: shunt.KindRead, OK: true, BytesIn: 400_000, BytesOut: 2_000, Model: "glm"})
	shunt.Append(lp, shunt.Event{Time: now.Add(-1 * time.Minute), Kind: shunt.KindDeny, OK: true, BytesIn: 90_000, Note: "Read", Cwd: "/Users/x/proj/wporg-ready"})
	shunt.Append(lp, shunt.Event{Time: now, Kind: shunt.KindWrite, OK: false, Note: "output rejected"})

	var rows []shuntActivityRow
	getJSON(t, s, "/api/shunt-activity", &rows)
	if len(rows) != 3 || rows[0].Kind != "write" || rows[1].Kind != "deny" || rows[2].Kind != "read" {
		t.Fatalf("want write, deny, read newest first, got %+v", rows)
	}
	if rows[1].Cwd != "/Users/x/proj/wporg-ready" {
		t.Errorf("a refusal must say which project it came from: %+v", rows[1])
	}
	if rows[2].KeptOutTokens != shunt.EstimateTokens(398_000) {
		t.Errorf("kept-out %d", rows[2].KeptOutTokens)
	}
	if rows[0].KeptOutTokens != 0 || rows[1].KeptOutTokens != 0 {
		t.Errorf("a failure and a refusal kept nothing out on their own: %+v", rows[:2])
	}
	var limited []shuntActivityRow
	getJSON(t, s, "/api/shunt-activity?limit=1", &limited)
	if len(limited) != 1 || limited[0].Kind != "write" {
		t.Errorf("limit not honoured: %+v", limited)
	}
}

func TestShuntActivityIsEmptyNotNull(t *testing.T) {
	s, _ := shuntServer(t, true)
	req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1/api/shunt-activity", nil)
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if strings.TrimSpace(rr.Body.String()) != "[]" {
		t.Errorf("no log must serialise as [], which the page iterates, not null: %q", rr.Body.String())
	}
}
