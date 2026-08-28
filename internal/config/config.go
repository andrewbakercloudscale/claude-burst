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
// selects the implementation ("oauth-passthrough", "anthropic-api-key", or
// "bedrock"); FailoverStrategy selects how failures on THIS slot are
// evaluated to decide whether to move traffic to the other slot
// ("subscription-limit", "metered-failures", or "none"). KeychainService and
// ModelMap only apply to the "bedrock" provider.
type RouteConfig struct {
	Provider         string            `json:"provider,omitempty"`
	BaseURL          string            `json:"base_url,omitempty"`
	FailoverStrategy string            `json:"failover_strategy,omitempty"`
	KeychainService  string            `json:"keychain_service,omitempty"`
	ModelMap         map[string]string `json:"model_map,omitempty"`
}

type MeteredFailoverConfig struct {
	WindowSeconds int `json:"window_seconds,omitempty"`
	MinFailures   int `json:"min_failures,omitempty"`
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
}

func Default() Config {
	return Config{
		Listen:              "127.0.0.1:7777",
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
