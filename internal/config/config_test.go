package config

import "testing"

func TestConfigBackwardCompat_LegacyFieldsSynthesizePrimarySecondary(t *testing.T) {
	cfg := Default()
	cfg.ResolveRoutes()
	if cfg.Primary.Provider != "oauth-passthrough" {
		t.Fatalf("expected legacy config to synthesize oauth-passthrough primary, got %q", cfg.Primary.Provider)
	}
	if cfg.Primary.BaseURL != cfg.AnthropicBaseURL {
		t.Fatalf("primary base URL not synthesized from legacy field: got %q want %q", cfg.Primary.BaseURL, cfg.AnthropicBaseURL)
	}
	if cfg.Primary.FailoverStrategy != "subscription-limit" {
		t.Fatalf("expected subscription-limit failover strategy, got %q", cfg.Primary.FailoverStrategy)
	}
	if cfg.Secondary.Provider != "bedrock" {
		t.Fatalf("expected legacy config to synthesize bedrock secondary, got %q", cfg.Secondary.Provider)
	}
	if cfg.Secondary.BaseURL != cfg.BedrockBaseURL {
		t.Fatalf("secondary base URL not synthesized from legacy field: got %q want %q", cfg.Secondary.BaseURL, cfg.BedrockBaseURL)
	}
}

func TestConfigNewSchema_ExplicitProviderSelectionIsNotOverwritten(t *testing.T) {
	cfg := Default()
	cfg.Primary = RouteConfig{Provider: "bedrock", BaseURL: cfg.BedrockBaseURL, KeychainService: cfg.KeychainService, ModelMap: cfg.ModelMap}
	cfg.Secondary = RouteConfig{Provider: "anthropic-api-key", BaseURL: cfg.AnthropicBaseURL, FailoverStrategy: "none"}
	cfg.ResolveRoutes()
	if cfg.Primary.Provider != "bedrock" {
		t.Fatalf("explicit primary provider was overwritten: got %q", cfg.Primary.Provider)
	}
	if cfg.Secondary.Provider != "anthropic-api-key" {
		t.Fatalf("explicit secondary provider was overwritten: got %q", cfg.Secondary.Provider)
	}
}

func TestConfigNoSecondaryWhenBedrockBaseURLEmpty(t *testing.T) {
	cfg := Default()
	cfg.BedrockBaseURL = ""
	cfg.ResolveRoutes()
	if cfg.Secondary.Provider != "" {
		t.Fatalf("expected no secondary to be synthesized when BedrockBaseURL is empty, got %q", cfg.Secondary.Provider)
	}
}

func TestResolveRoutesDefaultsMeteredFailoverAndTimeout(t *testing.T) {
	cfg := Default()
	cfg.ResolveRoutes()
	if cfg.MeteredFailover.WindowSeconds != 60 {
		t.Fatalf("expected default window_seconds=60, got %d", cfg.MeteredFailover.WindowSeconds)
	}
	if cfg.MeteredFailover.MinFailures != 3 {
		t.Fatalf("expected default min_failures=3, got %d", cfg.MeteredFailover.MinFailures)
	}
	if cfg.ResponseHeaderTimeoutSeconds != 60 {
		t.Fatalf("expected default response_header_timeout_seconds=60, got %d", cfg.ResponseHeaderTimeoutSeconds)
	}
}

func TestResolveRoutesIsIdempotent(t *testing.T) {
	cfg := Default()
	cfg.ResolveRoutes()
	provider, baseURL, strategy := cfg.Primary.Provider, cfg.Primary.BaseURL, cfg.Primary.FailoverStrategy
	cfg.ResolveRoutes()
	if cfg.Primary.Provider != provider || cfg.Primary.BaseURL != baseURL || cfg.Primary.FailoverStrategy != strategy {
		t.Fatalf("ResolveRoutes must be idempotent: got %+v then %+v", provider, cfg.Primary)
	}
}

// `configure --secondary none` used to be a silent no-op: it cleared Secondary
// to the zero value, which writes nothing (omitempty), and ResolveRoutes then
// rebuilt a bedrock secondary from the defaults Load seeds in. The marker must
// survive a save/load round trip.
func TestSecondaryNoneSurvivesResolveRoutes(t *testing.T) {
	cfg := Default() // Default() supplies BedrockBaseURL, which is what used to resurrect it
	cfg.Secondary = RouteConfig{Provider: ProviderNone}
	cfg.BedrockBaseURL = ""
	cfg.ResolveRoutes()
	if cfg.Secondary.Provider != ProviderNone {
		t.Fatalf("secondary = %q, want %q", cfg.Secondary.Provider, ProviderNone)
	}

	// Even with the legacy field still populated, an explicit none wins.
	cfg2 := Default()
	cfg2.Secondary = RouteConfig{Provider: ProviderNone}
	cfg2.ResolveRoutes()
	if cfg2.Secondary.Provider != ProviderNone {
		t.Errorf("legacy bedrock_base_url resurrected the secondary: got %q", cfg2.Secondary.Provider)
	}
}
