package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/andrewbakercloudscale/claude-burst/internal/config"
	"github.com/andrewbakercloudscale/claude-burst/internal/hotspot"
	"github.com/andrewbakercloudscale/claude-burst/internal/keychain"
)

func postSettings(t *testing.T, s *Server, body string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:7788/api/settings-save", strings.NewReader(body))
	req.Header.Set("X-Claude-Burst-Admin", "1")
	s.Handler().ServeHTTP(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec, out
}

func loadCfg(t *testing.T) config.Config {
	t.Helper()
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

// Every rejected body must leave config.json exactly as it was: validation
// runs before anything is written.
func TestSettingsSaveRejectsBadInput(t *testing.T) {
	s := newTestServer(t)
	if rec, _ := postSettings(t, s, `{"notify":{"failover":true}}`); rec.Code != http.StatusOK {
		t.Fatalf("seed: %d %s", rec.Code, rec.Body)
	}
	p, _ := config.ConfigPath()
	before, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]string{
		"negative price":          `{"pricing":{"m1":{"input_per_mtok":-1,"output_per_mtok":5}}}`,
		"price over 1000":         `{"pricing":{"m1":{"input_per_mtok":1001,"output_per_mtok":5}}}`,
		"cache price over 1000":   `{"pricing":{"m1":{"input_per_mtok":1,"output_per_mtok":5,"cache_read_per_mtok":5000}}}`,
		"input and output zero":   `{"pricing":{"m1":{"input_per_mtok":0,"output_per_mtok":0,"cache_read_per_mtok":1}}}`,
		"model name with space":   `{"pricing":{"bad model":{"input_per_mtok":1,"output_per_mtok":5}}}`,
		"model name with quote":   `{"pricing":{"bad\"m":{"input_per_mtok":1,"output_per_mtok":5}}}`,
		"empty model name":        `{"pricing":{"":{"input_per_mtok":1,"output_per_mtok":5}}}`,
		"fallback self loop":      `{"fallback_chain":{"claude-a":["claude-a"]}}`,
		"fallback bad rung":       `{"fallback_chain":{"claude-a":["has space"]}}`,
		"fallback bad key":        `{"fallback_chain":{"bad key":["claude-b"]}}`,
		"window too short":        `{"metered_failover":{"window_seconds":5,"min_failures":3,"transport_error_min_failures":1}}`,
		"window too long":         `{"metered_failover":{"window_seconds":3601,"min_failures":3,"transport_error_min_failures":1}}`,
		"zero failures":           `{"metered_failover":{"window_seconds":60,"min_failures":0,"transport_error_min_failures":1}}`,
		"too many conn failures":  `{"metered_failover":{"window_seconds":60,"min_failures":3,"transport_error_min_failures":101}}`,
		"reset grace too long":    `{"advanced":{"reset_grace_seconds":601,"unknown_reset_seconds":300,"response_header_timeout_seconds":60,"max_request_mb":128}}`,
		"unknown reset too short": `{"advanced":{"reset_grace_seconds":10,"unknown_reset_seconds":29,"response_header_timeout_seconds":60,"max_request_mb":128}}`,
		"header timeout too low":  `{"advanced":{"reset_grace_seconds":10,"unknown_reset_seconds":300,"response_header_timeout_seconds":9,"max_request_mb":128}}`,
		"request size too big":    `{"advanced":{"reset_grace_seconds":10,"unknown_reset_seconds":300,"response_header_timeout_seconds":60,"max_request_mb":1025}}`,
		"request size too small":  `{"advanced":{"reset_grace_seconds":10,"unknown_reset_seconds":300,"response_header_timeout_seconds":60,"max_request_mb":7}}`,
		"hotspot bad when":        `{"hotspot":{"ssid":"Phone","when":"sometimes"}}`,
		"hotspot ssid too long":   `{"hotspot":{"ssid":"` + strings.Repeat("x", 65) + `"}}`,
		"hotspot ssid newline":    `{"hotspot":{"ssid":"a\nb"}}`,
		"unknown field":           `{"listen":"0.0.0.0:7777"}`,
		"not json":                `pricing please`,
	}
	for name, body := range cases {
		rec, _ := postSettings(t, s, body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400 (%s)", name, rec.Code, strings.TrimSpace(rec.Body.String()))
		}
		after, _ := os.ReadFile(p)
		if string(after) != string(before) {
			t.Fatalf("%s: config.json changed on a rejected request", name)
		}
	}
}

func TestSettingsSaveNeedsTheAdminHeader(t *testing.T) {
	s := newTestServer(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:7788/api/settings-save", strings.NewReader(`{"notify":{"failover":true}}`))
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status %d, want 403", rec.Code)
	}
	if loadCfg(t).Notify.Failover {
		t.Fatal("saved without the header")
	}
}

// A partial update changes its own part and nothing else.
func TestSettingsSavePartialUpdates(t *testing.T) {
	s := newTestServer(t)
	base := loadCfg(t)

	if rec, _ := postSettings(t, s, `{"notify":{"failover":true,"guards":true}}`); rec.Code != http.StatusOK {
		t.Fatalf("notify: %d %s", rec.Code, rec.Body)
	}
	cfg := loadCfg(t)
	if !cfg.Notify.Failover || cfg.Notify.Compaction || !cfg.Notify.Guards {
		t.Fatalf("notify = %+v", cfg.Notify)
	}
	if cfg.MeteredFailover != base.MeteredFailover || cfg.Hotspot != base.Hotspot ||
		cfg.ResetGraceSeconds != base.ResetGraceSeconds || len(cfg.Pricing) != len(base.Pricing) {
		t.Fatal("a notify update touched other settings")
	}

	if rec, _ := postSettings(t, s, `{"metered_failover":{"window_seconds":120,"min_failures":5,"transport_error_min_failures":2}}`); rec.Code != http.StatusOK {
		t.Fatalf("metered: %d %s", rec.Code, rec.Body)
	}
	cfg = loadCfg(t)
	want := config.MeteredFailoverConfig{WindowSeconds: 120, MinFailures: 5, TransportErrorMinFailures: 2}
	if cfg.MeteredFailover != want || !cfg.Notify.Failover || !cfg.Notify.Guards {
		t.Fatalf("metered = %+v, notify = %+v", cfg.MeteredFailover, cfg.Notify)
	}

	if rec, _ := postSettings(t, s, `{"advanced":{"reset_grace_seconds":20,"unknown_reset_seconds":600,"response_header_timeout_seconds":90,"max_request_mb":256}}`); rec.Code != http.StatusOK {
		t.Fatalf("advanced: %d %s", rec.Code, rec.Body)
	}
	cfg = loadCfg(t)
	if cfg.ResetGraceSeconds != 20 || cfg.UnknownResetSeconds != 600 || cfg.ResponseHeaderTimeoutSeconds != 90 || cfg.MaxRequestMB != 256 {
		t.Fatalf("advanced not saved: %+v", advancedOf(cfg))
	}
	if cfg.MeteredFailover != want || cfg.Listen != base.Listen || cfg.AdminListen != base.AdminListen {
		t.Fatal("an advanced update touched other settings")
	}

	// The password is required.
	stored := false
	old := hotspotPasswordStored
	hotspotPasswordStored = func() bool { return stored }
	defer func() { hotspotPasswordStored = old }()
	if rec, _ := postSettings(t, s, `{"hotspot":{"ssid":"My Phone","when":"always"}}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("a hotspot with no password must be refused: %d %s", rec.Code, rec.Body)
	}
	// Not tested here: forgetting ("-") and storing, which call the real
	// Keychain.
	stored = true
	if rec, _ := postSettings(t, s, `{"hotspot":{"ssid":"My Phone","when":"always"}}`); rec.Code != http.StatusOK {
		t.Fatalf("hotspot: %d %s", rec.Code, rec.Body)
	}
	cfg = loadCfg(t)
	if cfg.Hotspot.SSID != "My Phone" || cfg.Hotspot.When != config.HotspotAlways || cfg.ResetGraceSeconds != 20 {
		t.Fatalf("hotspot = %+v", cfg.Hotspot)
	}
	// Choosing no network is how the feature is switched off.
	if rec, _ := postSettings(t, s, `{"hotspot":{"ssid":"","when":"lid-closed"}}`); rec.Code != http.StatusOK {
		t.Fatalf("hotspot off: %d %s", rec.Code, rec.Body)
	}
	if loadCfg(t).Hotspot.SSID != "" {
		t.Fatal("hotspot not switched off")
	}

	// Fallback chain replaces the whole map, trims rungs and drops empties.
	if rec, _ := postSettings(t, s, `{"fallback_chain":{"claude-a":[" claude-b ","","claude-c"],"claude-d":[]}}`); rec.Code != http.StatusOK {
		t.Fatalf("fallback: %d %s", rec.Code, rec.Body)
	}
	// Checked in the file, not through config.Load: Load unmarshals into
	// Default(), whose FallbackChain map the saved one is MERGED into, so
	// the default entries always reappear on load (a config.Load issue,
	// reported separately).
	p, _ := config.ConfigPath()
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	var onDisk struct {
		FallbackChain map[string][]string `json:"fallback_chain"`
	}
	if err := json.Unmarshal(raw, &onDisk); err != nil {
		t.Fatal(err)
	}
	fc := onDisk.FallbackChain
	if len(fc) != 1 || !slices.Equal(fc["claude-a"], []string{"claude-b", "claude-c"}) {
		t.Fatalf("fallback chain written = %v", fc)
	}
}

func TestSettingsSaveNothingIsANoOp(t *testing.T) {
	s := newTestServer(t)
	rec, out := postSettings(t, s, `{}`)
	if rec.Code != http.StatusOK || out["ok"] != "nothing to change" {
		t.Fatalf("%d %v", rec.Code, out)
	}
	// An empty password means "leave the Keychain alone": it must not be
	// stored or deleted. (Store and Delete touch the real login Keychain,
	// so only the no-op path is exercised here.)
	rec, out = postSettings(t, s, `{"hotspot_password":""}`)
	if rec.Code != http.StatusOK || out["ok"] != "nothing to change" {
		t.Fatalf("%d %v", rec.Code, out)
	}
	if p, _ := config.ConfigPath(); fileExists(p) {
		t.Fatal("a no-op save wrote config.json")
	}
}


func TestSettingsPricingSetDeleteAndRestartNeeded(t *testing.T) {
	s := newTestServer(t)
	if rn := restartNeeded(s.gateway.StartupConfig(), loadCfg(t)); slices.Contains(rn, "pricing") {
		t.Fatalf("pricing flagged before any change: %v", rn)
	}

	rec, out := postSettings(t, s, `{"pricing":{"test-model-x":{"input_per_mtok":3,"output_per_mtok":15,"cache_read_per_mtok":0.3}}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	p, ok := loadCfg(t).Pricing["test-model-x"]
	if !ok || p.InputPerMTok != 3 || p.OutputPerMTok != 15 || p.CacheReadPerMTok != 0.3 {
		t.Fatalf("price not saved: %+v %v", p, ok)
	}
	rn, _ := out["restart_needed"].([]any)
	found := false
	for _, x := range rn {
		found = found || x == "pricing"
	}
	if !found {
		t.Fatalf("restart_needed = %v, want it to include pricing", out["restart_needed"])
	}
	if _, running := s.gateway.StartupConfig().Pricing["test-model-x"]; running {
		t.Fatal("the running gateway picked up the price without a restart; restart_needed would be wrong")
	}

	// Other models' prices survive an update to one.
	before := len(loadCfg(t).Pricing)
	if rec, _ := postSettings(t, s, `{"pricing":{"test-model-y":{"input_per_mtok":1,"output_per_mtok":2}}}`); rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if got := len(loadCfg(t).Pricing); got != before+1 {
		t.Fatalf("pricing entries %d, want %d", got, before+1)
	}

	// null deletes that one model's price and nothing else.
	if rec, _ := postSettings(t, s, `{"pricing":{"test-model-x":null}}`); rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	cfg := loadCfg(t)
	if _, still := cfg.Pricing["test-model-x"]; still {
		t.Fatal("null did not delete the price")
	}
	if _, kept := cfg.Pricing["test-model-y"]; !kept {
		t.Fatal("deleting one price removed another")
	}
}

func TestRestartNeededOnlyListsStartupSettings(t *testing.T) {
	a := config.Default()
	b := a
	b.Notify = config.NotifyConfig{Failover: true}
	b.Hotspot = config.HotspotConfig{SSID: "Phone"}
	b.KeepAwakeLidClosed = true
	if rn := restartNeeded(a, b); len(rn) != 0 {
		t.Fatalf("live settings flagged as needing a restart: %v", rn)
	}
	b.MaxRequestMB = a.MaxRequestMB + 1
	b.MeteredFailover.MinFailures = 9
	rn := restartNeeded(a, b)
	if !slices.Contains(rn, "timeouts and limits") || !slices.Contains(rn, "failover thresholds") || len(rn) != 2 {
		t.Fatalf("restart_needed = %v", rn)
	}
}

// stubMacForSettings answers everything the settings view asks the Mac:
// the Keychain through a stub security that records its calls, and the
// hotspot's networksetup, ioreg and online probe with fixed answers. It
// returns how many times the stub security ran.
func stubMacForSettings(t *testing.T) func() int {
	t.Helper()
	dir := t.TempDir()
	calls := filepath.Join(dir, "calls")
	stub := filepath.Join(dir, "security")
	body := "#!/bin/sh\necho \"$*\" >> " + calls + "\nexit 44\n" // 44: item not found
	if err := os.WriteFile(stub, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(keychain.SetSecurityPathForTesting(stub))
	t.Setenv("CLAUDE_BURST_HOTSPOT_PASSWORD", "")
	known, lid, online := hotspot.KnownNetworks, hotspot.LidClosed, hotspot.Online
	hotspot.KnownNetworks = func() []string { return []string{"Test Phone"} }
	hotspot.LidClosed = func() bool { return false }
	hotspot.Online = func() bool { return true }
	t.Cleanup(func() { hotspot.KnownNetworks, hotspot.LidClosed, hotspot.Online = known, lid, online })
	return func() int {
		b, _ := os.ReadFile(calls)
		return strings.Count(string(b), "\n")
	}
}

// The settings view's shape, built without the real Keychain, network or
// hotspot commands: every Mac answer below comes from stubMacForSettings.
func TestSettingsGetShape(t *testing.T) {
	s := newTestServer(t)
	securityCalls := stubMacForSettings(t)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://127.0.0.1:7788/api/settings", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	var v settingsView
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	if v.MeteredDefaults.WindowSeconds != 60 || v.AdvancedDefault.MaxRequestMB != config.Default().MaxRequestMB {
		t.Fatalf("defaults = %+v %+v", v.MeteredDefaults, v.AdvancedDefault)
	}
	if v.Hotspot.When != config.HotspotLidClosed {
		t.Fatalf("hotspot when defaults to %q", v.Hotspot.When)
	}
	if v.Listen == "" || v.AdminListen == "" {
		t.Fatal("listen addresses missing")
	}
	if !v.Hotspot.Online || len(v.Hotspot.Known) != 1 || v.Hotspot.Known[0] != "Test Phone" || v.Hotspot.PasswordStored {
		t.Fatalf("hotspot view did not come from the stubs: %+v", v.Hotspot)
	}
	if securityCalls() == 0 {
		t.Fatal("the password check must go through the stub security, not the real Keychain")
	}
}
