package config

import (
	"os"
	"path/filepath"
	"testing"
)

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

// TestHostsRedirectActive_EmptyMarkerBlockIsNotActive is a regression test
// for a real incident (2026-09-03): transparent-root.sh's own `remove`
// deletes the whole marker block, so an empty-but-present block only ever
// happens by hand -- and the old check (a bare substring match on the BEGIN
// marker) reported the redirect as active regardless, while an hour of real
// Claude Code traffic had quietly gone direct to Anthropic with no gateway
// involved at all.
func TestHostsRedirectActive_EmptyMarkerBlockIsNotActive(t *testing.T) {
	hosts := "127.0.0.1 localhost\n# BEGIN claude-burst hosts\n# END claude-burst hosts\n"
	if HostsRedirectActive([]byte(hosts), "api.anthropic.com") {
		t.Fatal("an empty marker block must not report the redirect as active")
	}
}

func TestHostsRedirectActive_RealRedirectIsActive(t *testing.T) {
	hosts := "127.0.0.1 localhost\n# BEGIN claude-burst hosts\n127.0.0.1 api.anthropic.com\n# END claude-burst hosts\n"
	if !HostsRedirectActive([]byte(hosts), "api.anthropic.com") {
		t.Fatal("a real loopback redirect line must report the redirect as active")
	}
}

func TestHostsRedirectActive_OtherHostsEntriesDontCount(t *testing.T) {
	hosts := "127.0.0.1 localhost\n127.0.0.1 some-other-host.test\n"
	if HostsRedirectActive([]byte(hosts), "api.anthropic.com") {
		t.Fatal("a loopback entry for an unrelated host must not count as the redirect being active")
	}
}

// TestLoadMergesPricingRatherThanReplacing pins behaviour the default
// pricing table silently depends on.
//
// Load() seeds the struct from Default() and then unmarshals config.json
// over it. encoding/json reuses a non-nil map rather than allocating a new
// one, so a file carrying its own `pricing` block ADDS to the defaults
// instead of replacing them -- which is why adding a model here reaches
// every existing installation without anyone editing their config.
//
// If that ever changed -- a `cfg.Pricing = map[...]{}` reset before
// Unmarshal, say -- every default price would vanish for anyone with a
// pricing block, and the only symptom would be cost quietly reading $0.00
// on models that used to be priced. A zero that means "not priced" looking
// exactly like a zero that means "free" is the failure this repo keeps
// re-learning, so it gets a test rather than a comment.
func TestLoadMergesPricingRatherThanReplacing(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".config", "claude-burst")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	// A user config naming exactly one model, as a real one would.
	body := `{"listen":"127.0.0.1:7777","pricing":{"my-vendor/some-model":{"input_per_mtok":1.5,"output_per_mtok":6}}}`
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(body), 0600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Pricing["my-vendor/some-model"]; got.InputPerMTok != 1.5 || got.OutputPerMTok != 6 {
		t.Errorf("the file's own entry was lost: %+v", got)
	}
	// The point of the test: a default the file never mentions must survive.
	for _, model := range []string{"claude-opus-5", "claude-sonnet-5", "claude-haiku-4-5"} {
		if got := cfg.Pricing[model]; got.InputPerMTok == 0 {
			t.Errorf("default pricing for %q was replaced by the file's block; "+
				"its cost would now silently record as $0.00", model)
		}
	}
}

// TestDefaultPricingCoversCurrentModels guards against the table drifting
// behind the model line-up. An unpriced model records tokens with zero cost,
// which reads as free rather than as unknown.
func TestDefaultPricingCoversCurrentModels(t *testing.T) {
	p := Default().Pricing
	for _, model := range []string{
		"claude-fable-5-1", "claude-fable-5",
		"claude-opus-5", "claude-opus-4-8", "claude-opus-4-7", "claude-opus-4-6",
		"claude-sonnet-5", "claude-sonnet-4-6",
		"claude-haiku-4-5",
	} {
		got, ok := p[model]
		if !ok {
			t.Errorf("no default pricing for %q", model)
			continue
		}
		if got.InputPerMTok <= 0 || got.OutputPerMTok <= 0 {
			t.Errorf("%q priced at %+v; a zero rate is indistinguishable from free", model, got)
		}
		if got.OutputPerMTok <= got.InputPerMTok {
			t.Errorf("%q has output (%v) <= input (%v), which no Claude model does -- likely transposed",
				model, got.OutputPerMTok, got.InputPerMTok)
		}
	}
}
