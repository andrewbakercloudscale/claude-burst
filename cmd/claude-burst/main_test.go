package main

import (
	"testing"

	"github.com/andrewbaker/claude-burst/internal/config"
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
