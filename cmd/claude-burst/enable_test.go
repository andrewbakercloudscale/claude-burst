package main

// enable and disable, run in-process against a temp HOME. Neither touches
// launchctl, sudo or the root helper: enable only edits settings.json (and,
// in transparent mode, the CA bundle) and prints the root step. Whether the
// gateway is healthy before enable runs is its callers' job; that ordering is
// tested by running install-proxy.sh in internal/integration.

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/andrewbakercloudscale/claude-burst/internal/claudesettings"
	"github.com/andrewbakercloudscale/claude-burst/internal/config"
	"github.com/andrewbakercloudscale/claude-burst/internal/tlsca"
)

// A settings.json with things that are not ours, which must survive both.
const userSettings = `{
  "model": "opus",
  "env": {"FOO": "bar"},
  "permissions": {"allow": ["Bash(ls:*)"]}
}`

func tempHome(t *testing.T, configJSON string) (settingsPath string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("NODE_EXTRA_CA_CERTS", "")
	// Never read this Mac's real managed settings.
	saved := managedSettingsPath
	managedSettingsPath = filepath.Join(home, "managed-settings.json")
	t.Cleanup(func() { managedSettingsPath = saved })
	// Nor this Mac's real Claude Code sessions.
	savedProcs := runningClaude
	runningClaude = func() map[string]time.Time { return nil }
	t.Cleanup(func() { runningClaude = savedProcs })
	if configJSON != "" {
		d := filepath.Join(home, ".config", "claude-burst")
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, "config.json"), []byte(configJSON), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	p, err := claudesettings.Path()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(p, home) {
		t.Fatalf("settings path %s is outside the temp HOME", p)
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(userSettings), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func readJSON(t *testing.T, p string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	m := map[string]any{}
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("%s: %v\n%s", p, err, b)
	}
	return m
}

func TestEnableThenDisableRestoresSettings(t *testing.T) {
	p := tempHome(t, "")
	var orig map[string]any
	if err := json.Unmarshal([]byte(userSettings), &orig); err != nil {
		t.Fatal(err)
	}

	enable(nil)
	got := readJSON(t, p)
	if url := claudesettings.BaseURL(got); url != "http://127.0.0.1:7777" {
		t.Fatalf("after enable ANTHROPIC_BASE_URL = %q", url)
	}
	env := got["env"].(map[string]any)
	if env["FOO"] != "bar" || got["model"] != "opus" || got["permissions"] == nil {
		t.Fatalf("enable lost the user's settings: %v", got)
	}
	if _, ok := env["ANTHROPIC_API_KEY"]; ok {
		t.Fatal("enable must never set a credential")
	}

	disable(nil)
	if got := readJSON(t, p); !reflect.DeepEqual(got, orig) {
		t.Fatalf("disable did not restore the settings:\n got %v\nwant %v", got, orig)
	}
}

// A base URL the user set themselves, to something that is not this
// gateway, is not ours to remove.
func TestDisableLeavesSomeoneElsesBaseURL(t *testing.T) {
	p := tempHome(t, "")
	other := `{"env": {"ANTHROPIC_BASE_URL": "https://llm-proxy.example.com"}}`
	if err := os.WriteFile(p, []byte(other), 0o600); err != nil {
		t.Fatal(err)
	}
	disable(nil)
	if url := claudesettings.BaseURL(readJSON(t, p)); url != "https://llm-proxy.example.com" {
		t.Fatalf("disable removed a base URL it did not set: %q", url)
	}
}

// Transparent mode exists so the variable stays unset: Claude Code disables
// Remote Control whenever it names another host. enable must clear one left
// from base-url mode, never set one, and leave the rest alone.
func TestEnableInTransparentModeNeverSetsBaseURL(t *testing.T) {
	p := tempHome(t, `{"intercept": {"mode": "transparent"}}`)
	withURL := `{"model": "opus", "env": {"FOO": "bar", "ANTHROPIC_BASE_URL": "http://127.0.0.1:7777"}}`
	if err := os.WriteFile(p, []byte(withURL), 0o600); err != nil {
		t.Fatal(err)
	}
	enable(nil)
	got := readJSON(t, p)
	if url := claudesettings.BaseURL(got); url != "" {
		t.Fatalf("transparent enable left ANTHROPIC_BASE_URL = %q", url)
	}
	if got["env"].(map[string]any)["FOO"] != "bar" || got["model"] != "opus" {
		t.Fatalf("enable lost the user's settings: %v", got)
	}
	bundle := filepath.Join(os.Getenv("HOME"), ".claude", "certs", "node-extra-ca-certs.pem")
	if b, err := os.ReadFile(bundle); err != nil || !strings.Contains(string(b), "BEGIN CERTIFICATE") {
		t.Fatalf("CA not added to the temp bundle %s: %v", bundle, err)
	}

	disable(nil)
	if b, _ := os.ReadFile(bundle); strings.Contains(string(b), "BEGIN CERTIFICATE") {
		t.Fatal("disable left the local CA in the bundle")
	}
}

// An enterprise install: Claude Code already goes to Portkey with its own
// headers. enable puts Burst in front with Portkey as the primary, keeps
// the headers, and disable leaves the file exactly as it was found.
func TestEnableAdoptsAnEnterpriseGatewayAndDisablePutsItBack(t *testing.T) {
	p := tempHome(t, "")
	portkey := `{"model": "opus", "env": {"ANTHROPIC_BASE_URL": "https://ai.portkey.corp.example/v1", "ANTHROPIC_CUSTOM_HEADERS": "x-portkey-config: pc-1"}}`
	if err := os.WriteFile(p, []byte(portkey), 0o600); err != nil {
		t.Fatal(err)
	}
	var orig map[string]any
	json.Unmarshal([]byte(portkey), &orig)

	enable(nil)
	got := readJSON(t, p)
	if url := claudesettings.BaseURL(got); url != "http://127.0.0.1:7777" {
		t.Fatalf("after enable ANTHROPIC_BASE_URL = %q, want the gateway", url)
	}
	if got["env"].(map[string]any)["ANTHROPIC_CUSTOM_HEADERS"] != "x-portkey-config: pc-1" {
		t.Fatal("enable lost the Portkey headers")
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Primary.BaseURL != "https://ai.portkey.corp.example/v1" || cfg.AdoptedBaseURL != "https://ai.portkey.corp.example/v1" {
		t.Fatalf("Portkey not adopted as the primary: primary=%q adopted=%q", cfg.Primary.BaseURL, cfg.AdoptedBaseURL)
	}

	disable(nil)
	if got := readJSON(t, p); !reflect.DeepEqual(got, orig) {
		t.Fatalf("disable did not put the enterprise setup back:\n got %v\nwant %v", got, orig)
	}
}

// Managed settings override the user's, so a base URL there cannot be
// taken over; enable must say so rather than appear to work.
func TestRefuseManagedBaseURL(t *testing.T) {
	tempHome(t, "")
	if err := refuseManagedBaseURL(); err != nil {
		t.Fatalf("no managed settings, got %v", err)
	}
	os.WriteFile(managedSettingsPath, []byte(`{"env": {"ANTHROPIC_BASE_URL": "https://gw.corp.example"}}`), 0o600)
	if err := refuseManagedBaseURL(); err == nil || !strings.Contains(err.Error(), "gw.corp.example") {
		t.Fatalf("managed base URL not refused: %v", err)
	}
}

// Transparent mode cannot see traffic Claude Code sends to an enterprise
// gateway, so enable refuses rather than install a redirect that catches
// nothing. Tested through the binary, since fatal exits.
func TestTransparentEnableRefusesAnEnterpriseGateway(t *testing.T) {
	if os.Getenv("BURST_TEST_SUBPROCESS") == "1" {
		tempHome(t, `{"intercept": {"mode": "transparent"}}`)
		p, _ := claudesettings.Path()
		os.WriteFile(p, []byte(`{"env": {"ANTHROPIC_BASE_URL": "https://ai.portkey.corp.example"}}`), 0o600)
		enable(nil)
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestTransparentEnableRefusesAnEnterpriseGateway$")
	cmd.Env = append(os.Environ(), "BURST_TEST_SUBPROCESS=1")
	out, err := cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(out), "base-url mode") {
		t.Fatalf("transparent enable did not refuse an enterprise gateway: err=%v\n%s", err, out)
	}
}

func TestParseEtime(t *testing.T) {
	for in, want := range map[string]time.Duration{
		"36:12":      36*time.Minute + 12*time.Second,
		"03:29:49":   3*time.Hour + 29*time.Minute + 49*time.Second,
		"2-01:00:05": 49*time.Hour + 5*time.Second,
	} {
		if got, ok := parseEtime(in); !ok || got != want {
			t.Errorf("parseEtime(%q) = %v %v, want %v", in, got, ok, want)
		}
	}
	if _, ok := parseEtime("x"); ok {
		t.Error("garbage parsed")
	}
}

// 2026-10-02 13:22: re-enabling with sessions open that started before the
// CA was in the bundle failed every one. Those sessions are named; ones
// started after are not; with no CA in the bundle, every running one is.
func TestSessionsNotTrusting(t *testing.T) {
	tempHome(t, "")
	bundle := filepath.Join(t.TempDir(), "bundle.pem")
	caPEM := []byte("-----BEGIN CERTIFICATE-----\nAAAA\n-----END CERTIFICATE-----\n")
	now := time.Now()
	runningClaude = func() map[string]time.Time {
		return map[string]time.Time{"100": now.Add(-time.Hour), "200": now.Add(time.Minute)}
	}
	if got := sessionsNotTrusting(bundle, caPEM); strings.Join(got, ",") != "100,200" {
		t.Fatalf("no CA in the bundle: every session refuses, got %v", got)
	}
	if err := tlsca.EnsureInBundle(bundle, caPEM); err != nil {
		t.Fatal(err)
	}
	if got := sessionsNotTrusting(bundle, caPEM); strings.Join(got, ",") != "100" {
		t.Fatalf("only the session started before the CA arrived, got %v", got)
	}
	// Re-running enable rewrites nothing, so the date does not move and
	// sessions that already trust the CA are not flagged afterwards.
	runningClaude = func() map[string]time.Time { return map[string]time.Time{"300": now.Add(time.Second)} }
	tlsca.EnsureInBundle(bundle, caPEM)
	if got := sessionsNotTrusting(bundle, caPEM); len(got) != 0 {
		t.Fatalf("a session that trusts the CA was flagged: %v", got)
	}
}
