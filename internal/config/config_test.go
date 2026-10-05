package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
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
		"claude-opus-5-5", "claude-opus-5", "claude-opus-4-8", "claude-opus-4-7", "claude-opus-4-6",
		"claude-sonnet-5-5", "claude-sonnet-5", "claude-sonnet-4-6",
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

// keep_awake_lid_closed is written even when false so the switch is visible
// in config.json, and defaults to false for a config that never mentions it.
func TestKeepAwakeLidClosedDefaultsFalseAndIsVisible(t *testing.T) {
	if Default().KeepAwakeLidClosed {
		t.Fatal("KeepAwakeLidClosed must default to false")
	}
	b, err := json.Marshal(Default())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"keep_awake_lid_closed":false`) {
		t.Fatalf("keep_awake_lid_closed missing from marshalled config: %s", b)
	}
}

// The power mode defaults to mains-only, both for a fresh config and for one
// written before the field existed, and an unknown value is refused.
func TestKeepAwakePowerDefaultsToACAndRejectsUnknown(t *testing.T) {
	if got := Default().KeepAwakeLidClosedPower; got != KeepAwakeOnAC {
		t.Fatalf("default power mode = %q, want %q", got, KeepAwakeOnAC)
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".config", "claude-burst")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	write := func(body string) {
		if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write(`{"keep_awake_lid_closed": true}`)
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.KeepAwakeLidClosedPower != KeepAwakeOnAC {
		t.Fatalf("legacy config power mode = %q, want %q", cfg.KeepAwakeLidClosedPower, KeepAwakeOnAC)
	}
	write(`{"keep_awake_lid_closed_power": "battery"}`)
	if _, err := Load(); err == nil {
		t.Fatal("unknown keep_awake_lid_closed_power was accepted")
	}
}

// A fallback chain in config.json replaces the default instead of merging
// into it; before, a removed default entry came back on every load, and an
// emptied chain could not be saved at all.
func TestFallbackChainInFileReplacesDefault(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.FallbackChain) == 0 {
		t.Fatal("no config file: the default chain should apply")
	}
	cfg.FallbackChain = map[string][]string{"claude-opus-5-5": {"claude-opus-5"}}
	if err := Save(cfg); err != nil {
		t.Fatal(err)
	}
	got, _ := Load()
	if len(got.FallbackChain) != 1 || got.FallbackChain["claude-opus-5-5"][0] != "claude-opus-5" {
		t.Fatalf("chain after reload: %v", got.FallbackChain)
	}
	got.FallbackChain = map[string][]string{}
	if err := Save(got); err != nil {
		t.Fatal(err)
	}
	if again, _ := Load(); len(again.FallbackChain) != 0 {
		t.Fatalf("an emptied chain came back: %v", again.FallbackChain)
	}
}

// Token shunting was removed on 2026-10-02, but config.json files written
// while it existed still carry a "shunt" block, some with read and write on.
// Load must keep reading them: a parse error here would take the gateway down
// on the first start after an upgrade. The block is ignored, and the next Save
// drops it.
func TestLoadIgnoresALegacyShuntBlock(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_BURST_BACKUP_DIR", "")
	dir := filepath.Join(home, ".config", "claude-burst")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	body := `{"listen":"127.0.0.1:7999","shunt":{"read":true,"write":true,"min_lines":350,"chunk_lines":6000,"timeout_seconds":120,"model":"zai-org/GLM-5.3"}}`
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load()
	if err != nil {
		t.Fatalf("a config with a legacy shunt block must still load: %v", err)
	}
	if cfg.Listen != "127.0.0.1:7999" {
		t.Errorf("the rest of the file was not read: listen=%q", cfg.Listen)
	}
	if err := Save(cfg); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), `"shunt"`) {
		t.Errorf("Save should drop the legacy shunt block:\n%s", b)
	}
}

// The warning follows Compact at as a percentage, and a warn_at_tokens left
// in an older config.json no longer decides it.
func TestCompactionWarnIsAPercentOfCompactAt(t *testing.T) {
	var c CompactionConfig
	if err := json.Unmarshal([]byte(`{"enabled":true,"warn_at_tokens":300000,"compact_at_tokens":500000}`), &c); err != nil {
		t.Fatal(err)
	}
	r := c.Resolved()
	if r.WarnAtPercent != 80 || r.WarnAtTokens != 400_000 || r.WindowMinutes != 30 {
		t.Fatalf("defaults: %+v", r)
	}
	c.WarnAtPercent = 50
	if r := c.Resolved(); r.WarnAtTokens != 250_000 {
		t.Fatalf("50%% of 500k: %+v", r)
	}
	out, _ := json.Marshal(c.Resolved())
	if strings.Contains(string(out), "warn_at_tokens") {
		t.Fatalf("the derived token count must not be stored: %s", out)
	}
}

// A learned Compact at applies only in the intelligent mode, and never over
// the user's own override for a repository.
func TestForRepoTakesTheLearnedCompactAtInTheIntelligentMode(t *testing.T) {
	c := CompactionConfig{Enabled: true, CompactAtTokens: 300_000,
		RepoOverrides: []RepoCompaction{{Repo: "/src/pinned", CompactAtTokens: 500_000}},
		Learned:       map[string]int64{"/src/learned": 150_000, "/src/pinned": 120_000}}
	if got, o := c.ForRepo("/src/learned"); got.CompactAtTokens != 300_000 || o != nil {
		t.Fatalf("fixed mode: %d %+v, want the fixed Compact at", got.CompactAtTokens, o)
	}
	c.Mode = CompactionIntelligent
	got, o := c.ForRepo("/src/learned")
	if got.CompactAtTokens != 150_000 || got.WarnAtTokens != NeverTokens || o == nil || !o.Learned {
		t.Fatalf("intelligent: %d warn %d %+v", got.CompactAtTokens, got.WarnAtTokens, o)
	}
	if got, o := c.ForRepo("/src/pinned"); got.CompactAtTokens != 500_000 || o == nil || o.Learned {
		t.Fatalf("an override must win: %d %+v", got.CompactAtTokens, o)
	}
	if got, o := c.ForRepo("/src/other"); got.CompactAtTokens != 300_000 || o != nil {
		t.Fatalf("nothing learned: %d %+v", got.CompactAtTokens, o)
	}
	if c.Resolved().FloorTokens != DefaultCompactionFloor || DefaultCompactionFloor != 100_000 {
		t.Fatal("the floor defaults to 100k")
	}
}

func TestIntelligentModeHasNoWarning(t *testing.T) {
	c := CompactionConfig{Enabled: true, Mode: CompactionIntelligent, Learned: map[string]int64{"/r": 160_000}}.Resolved()
	if c.WarnAtTokens != NeverTokens {
		t.Fatalf("intelligent mode warns at %d, want never", c.WarnAtTokens)
	}
	if got, _ := c.ForRepo("/r"); got.CompactAtTokens != 160_000 || got.WarnAtTokens != NeverTokens {
		t.Fatalf("learned repo: compact at %d, warn at %d", got.CompactAtTokens, got.WarnAtTokens)
	}
	if fixed := (CompactionConfig{Enabled: true}).Resolved(); fixed.WarnAtTokens != 240_000 {
		t.Fatalf("static mode warns at %d, want 240000", fixed.WarnAtTokens)
	}
}
