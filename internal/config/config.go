package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/andrewbakercloudscale/claude-burst/internal/backup"
)

// HostsRedirectActive reports whether /etc/hosts contains a live loopback
// redirect for host, as opposed to merely the claude-burst marker block
// existing. transparent-root.sh's own `remove` deletes the whole block,
// markers included -- an empty-but-present block only happens by hand -- so
// checking for the marker string alone (as this used to) reports "installed"
// for a host that actually resolves straight to the real Anthropic IP. That
// gap left claude-burst's own status output and admin UI claiming the
// redirect was active while an hour of real Claude Code traffic had quietly
// gone direct, no gateway involved at all -- confirmed live 2026-09-03.
func HostsRedirectActive(hostsContent []byte, host string) bool {
	host = strings.ToLower(strings.TrimSpace(host))
	for _, line := range strings.Split(string(hostsContent), "\n") {
		if idx := strings.IndexByte(line, '#'); idx >= 0 {
			line = line[:idx]
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		ip := fields[0]
		if ip != "127.0.0.1" && ip != "::1" {
			continue
		}
		for _, h := range fields[1:] {
			if strings.ToLower(h) == host {
				return true
			}
		}
	}
	return false
}

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
	// TransportErrorMinFailures is the number of pure transport-level
	// failures (dial/connect/timeout -- Anthropic could not be reached at
	// all) needed within WindowSeconds before failing over. Deliberately
	// separate from, and defaulting lower than, MinFailures: an HTTP error
	// response means Anthropic answered and could be a transient blip worth
	// a few tries before routing to a second, paid provider; a transport
	// failure means the connection couldn't even be established, which is a
	// much stronger "Anthropic is down" signal -- most noticeable as a
	// broken session right when Claude Code starts up. Local-connectivity
	// failures (DNS failure, no route to host) never reach this counter at
	// all; see isLocalConnectivityFailure.
	TransportErrorMinFailures int `json:"transport_error_min_failures,omitempty"`
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

// Shunt defaults. 350 lines is where a delegated read starts to pay for its
// 10-30 seconds of latency; below that the direct Read is already cheap and a
// delegation costs more than it saves.
const (
	DefaultShuntMinLines       = 350
	DefaultShuntChunkLines     = 6000
	DefaultShuntTimeoutSeconds = 120
)

// ShuntConfig controls the two independent delegations. Read and Write are
// separate switches on purpose: reading is low-risk and is where nearly all of
// the saving is, whereas Write puts generated code on disk without the
// frontier model ever seeing it.
type ShuntConfig struct {
	Read  bool `json:"read,omitempty"`  // block whole-file reads above MinLines and route them to the worker
	Write bool `json:"write,omitempty"` // allow `shunt write` to generate boilerplate straight to disk

	MinLines       int `json:"min_lines,omitempty"`       // whole-file reads at or above this are delegated
	ChunkLines     int `json:"chunk_lines,omitempty"`     // lines per worker call before a file is sliced
	TimeoutSeconds int `json:"timeout_seconds,omitempty"` // per worker call

	// Model overrides the worker model. Empty means the secondary's own model.
	Model string `json:"model,omitempty"`
}

// Enabled reports whether either delegation is on.
func (s ShuntConfig) Enabled() bool { return s.Read || s.Write }

func (s ShuntConfig) MinLinesOrDefault() int {
	if s.MinLines > 0 {
		return s.MinLines
	}
	return DefaultShuntMinLines
}

func (s ShuntConfig) ChunkLinesOrDefault() int {
	if s.ChunkLines > 0 {
		return s.ChunkLines
	}
	return DefaultShuntChunkLines
}

func (s ShuntConfig) TimeoutOrDefault() int {
	if s.TimeoutSeconds > 0 {
		return s.TimeoutSeconds
	}
	return DefaultShuntTimeoutSeconds
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

	// FallbackChain is the ordered list of OTHER Claude models to try on the
	// primary, on the subscription, before spending money on the secondary.
	//
	// Anthropic meters Fable separately from Opus: a rejected Fable request
	// does not mean Opus is unavailable, and on 2026-09-20 a single Fable
	// rejection armed a two-day window that sent every model to a paid
	// secondary while Opus was answering normally. A rung is only taken when
	// the model it names has no rejection window of its own.
	//
	// Keyed by the model Claude Code asked for. An empty or absent entry
	// means "no downgrade, go straight to the secondary".
	FallbackChain map[string][]string `json:"fallback_chain,omitempty"`

	Primary         RouteConfig           `json:"primary,omitempty"`
	Secondary       RouteConfig           `json:"secondary,omitempty"`
	MeteredFailover MeteredFailoverConfig `json:"metered_failover,omitempty"`
	Intercept       InterceptConfig       `json:"intercept,omitempty"`

	// Shunt is the token-shunting feature: bulk file reads and boilerplate
	// generation are handed to the secondary provider so the frontier model
	// never carries them in its context. Off unless explicitly enabled.
	Shunt ShuntConfig `json:"shunt,omitempty"`

	// AdminListen is the local control panel's address. Deliberately a
	// separate listener from Listen: in transparent mode the gateway serves
	// the intercepted hostname, and admin routes must not be reachable there.
	// Empty disables the panel entirely.
	AdminListen string `json:"admin_listen,omitempty"`

	// AdminHostname is an optional friendly name for the admin UI, e.g.
	// "cloudscale-claudeburst.test", paired with an /etc/hosts entry pointing
	// it at 127.0.0.1.
	//
	// It is accepted in addition to the loopback names, never instead of them.
	// Note the trade: the Host-header check is a DNS-rebinding defence, and a
	// hostname a hostile page can simply guess and navigate to weakens it. What
	// still holds is that mutations require a custom header (so a cross-origin
	// page needs a preflight this server never answers) and no CORS headers are
	// ever returned (so responses cannot be read cross-origin).
	AdminHostname string `json:"admin_hostname,omitempty"`
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
		// Fable -> Opus only. Opus -> Sonnet is a far larger capability drop
		// than a cost saving justifies by default, so it is left for the
		// user to add deliberately rather than shipped on.
		FallbackChain: map[string][]string{
			"claude-fable-5-1": {"claude-opus-5-5"},
			"claude-fable-5":   {"claude-opus-5-5"},
		},
		ModelMap: map[string]string{
			"claude-sonnet-5":                "global.anthropic.claude-sonnet-5",
			"claude-opus-5":                  "global.anthropic.claude-opus-5",
			"claude-fable-5":                 "global.anthropic.claude-fable-5",
			"claude-haiku-4-5-20251001":      "global.anthropic.claude-haiku-4-5-20251001-v1:0",
			"claude-haiku-4-5-20251001-v1:0": "global.anthropic.claude-haiku-4-5-20251001-v1:0",
		},
		// Anthropic first-party rates, per million tokens. These are the
		// models the PRIMARY slot serves, so they belong in the shipped
		// defaults -- unlike a third-party secondary's pricing, which is
		// provider-specific (the same GLM id costs different amounts through
		// Together, OpenRouter and Z.ai direct) and so lives in the user's
		// own config.json instead.
		//
		// The global.anthropic.* keys are BEDROCK ids and are listed here at
		// first-party rates, which is not necessarily correct: Bedrock is
		// partner-operated and separately priced. Left as-is rather than
		// silently "corrected" to numbers nobody has checked -- see the
		// pricing note in README. Anyone billing against Bedrock should
		// verify these against the AWS price list.
		Pricing: map[string]ModelPrice{
			"claude-fable-5-1":                                {InputPerMTok: 10, OutputPerMTok: 50},
			"claude-opus-4-8":                                 {InputPerMTok: 5, OutputPerMTok: 25},
			"claude-opus-4-7":                                 {InputPerMTok: 5, OutputPerMTok: 25},
			"claude-opus-4-6":                                 {InputPerMTok: 5, OutputPerMTok: 25},
			"claude-sonnet-4-6":                               {InputPerMTok: 3, OutputPerMTok: 15},
			"claude-haiku-4-5":                                {InputPerMTok: 1, OutputPerMTok: 5},
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
	if c.MeteredFailover.TransportErrorMinFailures <= 0 {
		c.MeteredFailover.TransportErrorMinFailures = 1
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

// ShuntLogPath is where the shunt records each delegation and each refused
// read. Separate from metrics.jsonl: those events are gateway requests, and a
// worker call is not one -- mixing them would inflate request counts and the
// secondary's share in every existing summary.
func ShuntLogPath() (string, error) {
	d, err := ConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "shunt.jsonl"), nil
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
	if cfg.FallbackChain == nil {
		cfg.FallbackChain = Default().FallbackChain
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
	// Best-effort throughout: see internal/backup's doc comment for why every
	// writer needs this, not just the ones that remember to run
	// scripts/backup-config.sh first, and why the restore point is set AFTER
	// the write, from what was actually written, rather than before it.
	_ = backup.Snapshot(p) // archive the outgoing version, for history
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	full := append(b, '\n')
	if err := os.WriteFile(p, full, 0600); err != nil {
		return err
	}
	_ = backup.SetLatest(p, full)
	return nil
}
