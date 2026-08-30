package main

import (
	"testing"

	"github.com/andrewbakercloudscale/claude-burst/internal/config"
)

// TestBaseURLForProviderNeverCrossesVendors is a regression test for a real
// bug: configure() used to always set cfg.Primary.BaseURL = cfg.AnthropicBaseURL
// regardless of which --primary provider name was given, so
// `configure --primary bedrock` would build a BedrockProvider pointed at
// api.anthropic.com -- sending the Keychain-stored Bedrock credential to the
// wrong host. baseURLForProvider must always derive the base URL from the
// chosen provider's own field.
func TestBaseURLForProviderNeverCrossesVendors(t *testing.T) {
	cfg := config.Default()
	cfg.AnthropicBaseURL = "https://api.anthropic.com"
	cfg.BedrockBaseURL = "https://bedrock-runtime.us-east-1.amazonaws.com/anthropic"

	cases := []struct {
		provider string
		want     string
	}{
		{"oauth-passthrough", cfg.AnthropicBaseURL},
		{"anthropic-api-key", cfg.AnthropicBaseURL},
		{"bedrock", cfg.BedrockBaseURL},
	}
	for _, c := range cases {
		got, _, err := baseURLForProvider(cfg, c.provider)
		if err != nil {
			t.Fatalf("%s: unexpected error: %v", c.provider, err)
		}
		if got != c.want {
			t.Fatalf("%s: base URL = %q, want %q", c.provider, got, c.want)
		}
	}
}

func TestBaseURLForProviderRejectsUnknownName(t *testing.T) {
	cfg := config.Default()
	if _, _, err := baseURLForProvider(cfg, "together-ai"); err == nil {
		t.Fatal("expected an error for an unknown provider name, got nil")
	}
}

// clearBaseURL must remove the gateway's own value and nothing else. The old
// implementation only matched a "http://127.0.0.1:" prefix, so a non-loopback
// --listen left the key stranded and Claude Code stayed pointed at a gateway
// the user had just disabled.
func TestClearBaseURL(t *testing.T) {
	cases := []struct {
		name    string
		env     map[string]any
		listen  string
		want    bool
		leftKey bool // ANTHROPIC_BASE_URL still present afterwards
	}{
		{"loopback default", map[string]any{"ANTHROPIC_BASE_URL": "http://127.0.0.1:7777"}, "127.0.0.1:7777", true, false},
		{"non-loopback listen", map[string]any{"ANTHROPIC_BASE_URL": "http://192.168.1.9:7777"}, "192.168.1.9:7777", true, false},
		{"https listen", map[string]any{"ANTHROPIC_BASE_URL": "https://127.0.0.1:443"}, "127.0.0.1:443", true, false},
		{"someone else's proxy is left alone", map[string]any{"ANTHROPIC_BASE_URL": "https://corp-gateway.example.com"}, "127.0.0.1:7777", false, true},
		{"no key", map[string]any{"OTHER": "x"}, "127.0.0.1:7777", false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := map[string]any{"env": tc.env}
			if got := clearBaseURL(root, tc.listen); got != tc.want {
				t.Fatalf("clearBaseURL = %v, want %v", got, tc.want)
			}
			env, _ := root["env"].(map[string]any)
			_, present := env["ANTHROPIC_BASE_URL"]
			if present != tc.leftKey {
				t.Errorf("key present = %v, want %v", present, tc.leftKey)
			}
		})
	}
}

// Unrelated env vars and top-level settings must survive; this file holds the
// user's hooks, statusLine and permissions.
func TestClearBaseURLPreservesEverythingElse(t *testing.T) {
	root := map[string]any{
		"model": "opus[1m]",
		"hooks": map[string]any{"Stop": "something"},
		"env":   map[string]any{"ANTHROPIC_BASE_URL": "http://127.0.0.1:7777", "NODE_EXTRA_CA_CERTS": "/path/to.pem"},
	}
	if !clearBaseURL(root, "127.0.0.1:7777") {
		t.Fatal("expected removal")
	}
	if root["model"] != "opus[1m]" || root["hooks"] == nil {
		t.Error("unrelated top-level settings were lost")
	}
	env := root["env"].(map[string]any)
	if env["NODE_EXTRA_CA_CERTS"] != "/path/to.pem" {
		t.Error("unrelated env var was lost")
	}
}

// When the gateway's key was the only one, the empty env block is dropped
// rather than left behind as `"env": {}`.
func TestClearBaseURLDropsEmptyEnvBlock(t *testing.T) {
	root := map[string]any{"env": map[string]any{"ANTHROPIC_BASE_URL": "http://127.0.0.1:7777"}}
	clearBaseURL(root, "127.0.0.1:7777")
	if _, present := root["env"]; present {
		t.Error("empty env block should have been removed")
	}
}

func TestPortOf(t *testing.T) {
	for in, want := range map[string]string{
		"127.0.0.1:7777": "7777",
		"0.0.0.0:443":    "443",
		"7777":           "7777",
	} {
		if got := portOf(in); got != want {
			t.Errorf("portOf(%q) = %q, want %q", in, got, want)
		}
	}
}
