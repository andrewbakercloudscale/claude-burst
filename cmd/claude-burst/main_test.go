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

// TestKeychainTargetKeepsProvidersSeparate is a regression test for a real
// bug found live on 2026-08-31: `keychain-set --provider openrouter`, run
// while the active secondary was Together AI, silently overwrote the
// Together AI Keychain entry with the OpenRouter key, because the default
// service name was inferred from whatever cfg.Secondary.KeychainService
// happened to be rather than from the requested provider. A first fix that
// tried to guard that inference by comparing derived labels still broke for
// a genuinely custom service name (nothing to derive a label back out of),
// which is why keychainTarget no longer consults config at all -- storing
// several providers' keys side by side and swapping which one is active
// must always be independent, non-destructive operations.
func TestKeychainTargetKeepsProvidersSeparate(t *testing.T) {
	if service, envVar, _ := keychainTarget("together", "", "claude-burst-bedrock"); service != "claude-burst-together" || envVar != "TOGETHER_API_KEY" {
		t.Fatalf("together: service=%q envVar=%q, want claude-burst-together / TOGETHER_API_KEY", service, envVar)
	}
	if service, envVar, _ := keychainTarget("openrouter", "", "claude-burst-bedrock"); service != "claude-burst-openrouter" || envVar != "OPENROUTER_API_KEY" {
		t.Fatalf("openrouter: service=%q envVar=%q, want its own claude-burst-openrouter / OPENROUTER_API_KEY, independent of whatever else is configured -- getting a different provider's entry is the overwrite bug", service, envVar)
	}
	if service, envVar, _ := keychainTarget("bedrock", "", "claude-burst-bedrock"); service != "claude-burst-bedrock" || envVar != "AWS_BEARER_TOKEN_BEDROCK" {
		t.Fatalf("bedrock: service=%q envVar=%q, want claude-burst-bedrock / AWS_BEARER_TOKEN_BEDROCK", service, envVar)
	}

	// An explicit --service always wins, and only affects the provider it
	// was passed for.
	if service, _, _ := keychainTarget("together", "my-custom-together-service", "claude-burst-bedrock"); service != "my-custom-together-service" {
		t.Fatalf("explicit --service for together: got %q, want my-custom-together-service", service)
	}
	if service, _, _ := keychainTarget("openrouter", "", "claude-burst-bedrock"); service != "claude-burst-openrouter" {
		t.Fatalf("openrouter must not inherit a different call's explicit --service: got %q, want claude-burst-openrouter", service)
	}

	// Bedrock's default comes from cfg.KeychainService (a real, documented
	// config.json field) -- unlike the openai-compatible case, this one
	// legitimately does vary by config, not by guesswork.
	if service, _, _ := keychainTarget("bedrock", "", "claude-burst-my-custom-bedrock"); service != "claude-burst-my-custom-bedrock" {
		t.Fatalf("bedrock should use cfg.KeychainService as its default: got %q, want claude-burst-my-custom-bedrock", service)
	}
}

func TestBaseURLForProviderRejectsUnknownName(t *testing.T) {
	cfg := config.Default()
	if _, _, err := baseURLForProvider(cfg, "together-ai"); err == nil {
		t.Fatal("expected an error for an unknown provider name, got nil")
	}
}

// clearBaseURL's tests moved to internal/claudesettings/settings_test.go:
// main.go no longer has its own copy of this logic (see
// internal/claudesettings's package doc -- it was already meant to be the
// one place this lived, and now actually is).

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
