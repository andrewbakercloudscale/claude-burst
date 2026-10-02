package main

// enable and disable, run in-process against a temp HOME. Neither touches
// launchctl, sudo or the root helper: enable only edits settings.json (and,
// in transparent mode, the CA bundle) and prints the root step. Whether the
// gateway is healthy before enable runs is its callers' job; that ordering is
// tested by running install-proxy.sh in internal/integration.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/andrewbakercloudscale/claude-burst/internal/claudesettings"
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
