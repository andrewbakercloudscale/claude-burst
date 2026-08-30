package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type ModelPrice struct {
	InputPerMTok  float64 `json:"input_per_mtok"`
	OutputPerMTok float64 `json:"output_per_mtok"`
}

// RouteConfig describes one provider slot (primary or secondary). Provider
// selects the implementation ("oauth-passthrough", "anthropic-api-key",
// "bedrock", or "openai-compatible"); FailoverStrategy selects how failures
// on THIS slot are evaluated to decide whether to move traffic to the other
// slot ("subscription-limit", "metered-failures",
// "subscription-limit+metered-failures", or "none"). KeychainService applies
// to "bedrock" and "openai-compatible". ModelMap applies to both: for
// "bedrock" it is required (every Claude model must have an entry); for
// "openai-compatible" it is optional and selects "consistent failover"
// (per-Claude-model target, falling back to Model for any unmapped model)
// -- leaving it empty keeps the simpler "fixed failover" behavior of always
// targeting Model regardless of which Claude model was requested.
type RouteConfig struct {
	Provider         string            `json:"provider,omitempty"`
	BaseURL          string            `json:"base_url,omitempty"`
	FailoverStrategy string            `json:"failover_strategy,omitempty"`
	KeychainService  string            `json:"keychain_service,omitempty"`
	ModelMap         map[string]string `json:"model_map,omitempty"` // bedrock: required per-Claude-model mapping; openai-compatible: optional "consistent failover" mapping
	Model            string            `json:"model,omitempty"`     // openai-compatible: fixed/fallback target model
}

type MeteredFailoverConfig struct {
	WindowSeconds int `json:"window_seconds,omitempty"`
	MinFailures   int `json:"min_failures,omitempty"`
}

// Intercept modes -- how Claude Code is persuaded to send its traffic here.
const (
	// InterceptBaseURL points Claude Code at the gateway by setting
	// ANTHROPIC_BASE_URL in ~/.claude/settings.json. Simple, needs no root and
	// no certificates. Cost: Claude Code disables Remote Control whenever that
	// variable names anything other than api.anthropic.com, so this mode gives
	// up /remote-control. This is the default and the original behaviour.
	InterceptBaseURL = "base-url"

	// InterceptTransparent leaves ANTHROPIC_BASE_URL unset and instead puts the
	// gateway in the path at the DNS layer (/etc/hosts) with local TLS
	// termination, so Claude Code believes it is talking to api.anthropic.com
	// directly and Remote Control keeps working. Costs a local CA, an
	// /etc/hosts entry and a pf redirect -- i.e. root, and a machine-wide
	// change rather than a per-user one.
	InterceptTransparent = "transparent"
)

// ProviderNone marks a slot as deliberately empty.
//
// It has to be an explicit value rather than an absent one. The legacy flat
// fields (BedrockBaseURL and friends) are seeded from Default() on every Load
// and are `omitempty`, so a secondary cleared to the zero RouteConfig wrote
// nothing to disk and ResolveRoutes then rebuilt it from those defaults on the
// next load -- `configure --secondary none` silently did nothing at all.
const ProviderNone = "none"

// InterceptConfig is opt-in. A config.json with no "intercept" block behaves
// exactly as it did before this existed.
type InterceptConfig struct {
	Mode        string `json:"mode,omitempty"`         // InterceptBaseURL (default) | InterceptTransparent
	Host        string `json:"host,omitempty"`         // hostname to impersonate locally
	TLSPort     int    `json:"tls_port,omitempty"`     // port Claude Code connects to (443)
	CADir       string `json:"ca_dir,omitempty"`       // where the local CA and leaf live
	CABundle    string `json:"ca_bundle,omitempty"`    // NODE_EXTRA_CA_CERTS file to append the CA to
	ResolverDoH string `json:"resolver_doh,omitempty"` // DNS-over-HTTPS endpoint used to resolve Host upstream

	// UpstreamAddr optionally pins the upstream IP, skipping DoH entirely.
	// Escape hatch for a network where DoH is blocked; normally empty.
	UpstreamAddr string `json:"upstream_addr,omitempty"`
}

// Transparent reports whether transparent interception is active. Callers
// should use this rather than comparing Mode by hand, so that an empty Mode
// consistently reads as the default.
func (i InterceptConfig) Transparent() bool {
	return i.Mode == InterceptTransparent
}

type Config struct {
	Listen                       string `json:"listen"`
	ResetGraceSeconds            int    `json:"reset_grace_seconds"`
	UnknownResetSeconds          int    `json:"unknown_reset_seconds"`
	ResponseHeaderTimeoutSeconds int    `json:"response_header_timeout_seconds,omitempty"`
	MaxRequestMB                 int64  `json:"max_request_mb"`

	// Legacy flat fields. Kept so existing config.json files (and code that
	// builds a Config in-memory rather than via Load, e.g. tests) keep
	// working unchanged. ResolveRoutes synthesizes Primary/Secondary from
	// these whenever the new blocks below are absent.
	AnthropicBaseURL string                `json:"anthropic_base_url,omitempty"`
	BedrockBaseURL   string                `json:"bedrock_base_url,omitempty"`
	KeychainService  string                `json:"keychain_service"`
	ModelMap         map[string]string     `json:"model_map"`
	Pricing          map[string]ModelPrice `json:"pricing"`

	Primary         RouteConfig           `json:"primary,omitempty"`
	Secondary       RouteConfig           `json:"secondary,omitempty"`
	MeteredFailover MeteredFailoverConfig `json:"metered_failover,omitempty"`
	Intercept       InterceptConfig       `json:"intercept,omitempty"`

	// AdminListen is the local control panel's address. Deliberately a
	// separate listener from Listen: in transparent mode the gateway serves
	// the intercepted hostname, and admin routes must not be reachable there.
	// Empty disables the panel entirely.
	AdminListen string `json:"admin_listen,omitempty"`
}

func Default() Config {
	return Config{
		Listen:              "127.0.0.1:7777",
		AdminListen:         "127.0.0.1:7788",
		AnthropicBaseURL:    "https://api.anthropic.com",
		BedrockBaseURL:      "https://bedrock-runtime.us-east-1.amazonaws.com/anthropic",
		ResetGraceSeconds:   10,
		UnknownResetSeconds: 300,
		MaxRequestMB:        128,
		KeychainService:     "claude-burst-bedrock",
		ModelMap: map[string]string{
			"claude-sonnet-5":                "global.anthropic.claude-sonnet-5",
			"claude-opus-5":                  "global.anthropic.claude-opus-5",
			"claude-fable-5":                 "global.anthropic.claude-fable-5",
			"claude-haiku-4-5-20251001":      "global.anthropic.claude-haiku-4-5-20251001-v1:0",
			"claude-haiku-4-5-20251001-v1:0": "global.anthropic.claude-haiku-4-5-20251001-v1:0",
		},
		Pricing: map[string]ModelPrice{
			"claude-sonnet-5":                                 {InputPerMTok: 2, OutputPerMTok: 10},
			"global.anthropic.claude-sonnet-5":                {InputPerMTok: 2, OutputPerMTok: 10},
			"claude-opus-5":                                   {InputPerMTok: 5, OutputPerMTok: 25},
			"global.anthropic.claude-opus-5":                  {InputPerMTok: 5, OutputPerMTok: 25},
			"claude-fable-5":                                  {InputPerMTok: 10, OutputPerMTok: 50},
			"global.anthropic.claude-fable-5":                 {InputPerMTok: 10, OutputPerMTok: 50},
			"claude-haiku-4-5-20251001":                       {InputPerMTok: 1, OutputPerMTok: 5},
			"global.anthropic.claude-haiku-4-5-20251001-v1:0": {InputPerMTok: 1, OutputPerMTok: 5},
		},
	}
}

// ResolveRoutes synthesizes Primary/Secondary from the legacy flat fields
// when the new provider blocks are absent, and applies defaults for the
// metered-failover window and the response-header timeout. Safe to call
// more than once. router.New calls it defensively even after config.Load
// already has, since some callers (tests) build a Config in-memory and
// bypass Load entirely.
func (c *Config) ResolveRoutes() {
	if c.Primary.Provider == "" {
		c.Primary = RouteConfig{
			Provider:         "oauth-passthrough",
			BaseURL:          c.AnthropicBaseURL,
			FailoverStrategy: "subscription-limit",
		}
	}
	if c.Secondary.Provider == ProviderNone {
		c.Secondary = RouteConfig{Provider: ProviderNone}
	}
	if c.Secondary.Provider == "" && c.BedrockBaseURL != "" {
		c.Secondary = RouteConfig{
			Provider:        "bedrock",
			BaseURL:         c.BedrockBaseURL,
			KeychainService: c.KeychainService,
			ModelMap:        c.ModelMap,
		}
	}
	if c.MeteredFailover.WindowSeconds <= 0 {
		c.MeteredFailover.WindowSeconds = 60
	}
	if c.MeteredFailover.MinFailures <= 0 {
		c.MeteredFailover.MinFailures = 3
	}
	if c.ResponseHeaderTimeoutSeconds <= 0 {
		c.ResponseHeaderTimeoutSeconds = 60
	}
	c.Intercept.applyDefaults()
}

// applyDefaults fills the intercept block's blanks. Mode is deliberately left
// alone when empty: Transparent() treats "" as base-url, so an absent block
// stays the original behaviour without this function having to write a value
// into every config that never asked for the feature.
func (i *InterceptConfig) applyDefaults() {
	// Only materialise defaults for a config that actually opted in. Filling
	// them in regardless would bake this machine's absolute home-directory
	// paths into every config.json, including those of users who will never
	// turn transparent mode on.
	if i.Mode != InterceptTransparent {
		return
	}
	if i.Host == "" {
		i.Host = "api.anthropic.com"
	}
	if i.TLSPort <= 0 || i.TLSPort > 65535 {
		i.TLSPort = 443
	}
	if i.ResolverDoH == "" {
		// Resolves Host upstream while /etc/hosts points it at us. This
		// hostname must NOT be one we redirect, or the gateway would resolve
		// its own upstream to itself.
		i.ResolverDoH = "https://cloudflare-dns.com/dns-query"
	}
	if i.CADir == "" {
		if d, err := ConfigDir(); err == nil {
			i.CADir = filepath.Join(d, "ca")
		}
	}
	if i.CABundle == "" {
		// Prefer the bundle Claude Code is already being told to trust --
		// on a machine behind a corporate proxy this file exists and holds
		// the employer's CAs, and we must append to it rather than replace it.
		if env := os.Getenv("NODE_EXTRA_CA_CERTS"); env != "" {
			i.CABundle = env
		} else if h, err := HomeDir(); err == nil {
			i.CABundle = filepath.Join(h, ".claude", "certs", "node-extra-ca-certs.pem")
		}
	}
}

// ValidateIntercept rejects an unrecognised mode rather than quietly falling
// back to base-url. A silent fallback would leave the user believing Remote
// Control was preserved while the gateway had actually done the opposite --
// the failure would surface much later, as a missing feature rather than an error.
func (c *Config) ValidateIntercept() error {
	switch c.Intercept.Mode {
	case "", InterceptBaseURL, InterceptTransparent:
		return nil
	default:
		return fmt.Errorf("intercept.mode %q is not recognised (want %q or %q)",
			c.Intercept.Mode, InterceptBaseURL, InterceptTransparent)
	}
}

func HomeDir() (string, error) {
	h, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return h, nil
}

func ConfigDir() (string, error) {
	h, err := HomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(h, ".config", "claude-burst"), nil
}

func ConfigPath() (string, error) {
	d, err := ConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "config.json"), nil
}

func StatePath() (string, error) {
	d, err := ConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "state.json"), nil
}

func MetricsPath() (string, error) {
	d, err := ConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "metrics.jsonl"), nil
}

func LogPath() (string, error) {
	d, err := ConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "claude-burst.log"), nil
}

func EnsureDir() error {
	d, err := ConfigDir()
	if err != nil {
		return err
	}
	return os.MkdirAll(d, 0700)
}

func Load() (Config, error) {
	cfg := Default()
	p, err := ConfigPath()
	if err != nil {
		return cfg, err
	}
	b, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		cfg.ResolveRoutes()
		return cfg, nil
	}
	if err != nil {
		return cfg, err
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		return cfg, fmt.Errorf("parse %s: %w", p, err)
	}
	cfg.AnthropicBaseURL = strings.TrimRight(cfg.AnthropicBaseURL, "/")
	cfg.BedrockBaseURL = strings.TrimRight(cfg.BedrockBaseURL, "/")
	cfg.Primary.BaseURL = strings.TrimRight(cfg.Primary.BaseURL, "/")
	cfg.Secondary.BaseURL = strings.TrimRight(cfg.Secondary.BaseURL, "/")
	if cfg.Listen == "" {
		cfg.Listen = "127.0.0.1:7777"
	}
	if cfg.MaxRequestMB <= 0 || cfg.MaxRequestMB > 1024 {
		// The upper bound also guards against int64 overflow in
		// MaxRequestMB * 1024 * 1024 turning negative and rejecting every
		// request -- a self-inflicted denial of service from a garbage
		// config value.
		cfg.MaxRequestMB = 128
	}
	if cfg.ResetGraceSeconds < 0 {
		cfg.ResetGraceSeconds = 0
	}
	if cfg.UnknownResetSeconds <= 0 {
		cfg.UnknownResetSeconds = 300
	}
	if cfg.KeychainService == "" {
		cfg.KeychainService = "claude-burst-bedrock"
	}
	if cfg.ModelMap == nil {
		cfg.ModelMap = map[string]string{}
	}
	if cfg.Pricing == nil {
		cfg.Pricing = map[string]ModelPrice{}
	}
	cfg.ResolveRoutes()
	if err := cfg.ValidateIntercept(); err != nil {
		return cfg, fmt.Errorf("parse %s: %w", p, err)
	}
	return cfg, nil
}

func Save(cfg Config) error {
	if err := EnsureDir(); err != nil {
		return err
	}
	p, err := ConfigPath()
	if err != nil {
		return err
	}
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(p, append(b, '\n'), 0600)
}
