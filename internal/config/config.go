package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/andrewbakercloudscale/claude-burst/internal/atomicfile"
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

// PruneConfig tunes SecondaryPruning. Everything is on unless switched off:
// Disabled is the master switch, NoStub and NoCap turn off one technique
// each. Zero numbers take the defaults in router/prune.go (keep the last 10
// tool results intact, move the pruning boundary 10 results at a time, only
// stub results of 1 KB or more, cap any single result at 40 KB).
type PruneConfig struct {
	Disabled           bool `json:"disabled,omitempty"`
	NoStub             bool `json:"no_stub,omitempty"`
	NoCap              bool `json:"no_cap,omitempty"`
	KeepRecent         int  `json:"keep_recent,omitempty"`
	Step               int  `json:"step,omitempty"`
	StubMinBytes       int  `json:"stub_min_bytes,omitempty"`
	MaxToolResultBytes int  `json:"max_tool_result_bytes,omitempty"`
}

// CompactionConfig tunes PrimaryCompaction. Zero numbers take the defaults
// below: compact at 300k tokens of context, warn at 80% of that, and compact
// any one session at most once per 30 minutes.
type CompactionConfig struct {
	Enabled         bool  `json:"enabled,omitempty"`
	CompactAtTokens int64 `json:"compact_at_tokens,omitempty"`
	// WarnAtPercent is the warning as a percentage of CompactAtTokens, so
	// moving the threshold moves the warning with it.
	WarnAtPercent int `json:"warn_at_percent,omitempty"`
	// WarnAtTokens is derived by Resolved from the two above and is never
	// stored: a warn_at_tokens left in an older config.json is ignored.
	WarnAtTokens  int64 `json:"-"`
	WindowMinutes int   `json:"window_minutes,omitempty"`
	// NoPromptNotice turns off the lines Claude Code shows under a prompt
	// when a compaction starts, is ready, swaps in or fails. On by default.
	NoPromptNotice bool `json:"no_prompt_notice,omitempty"`
	// NoIdleCompaction turns off the summary written for a session that
	// has been idle for 50 minutes, before its one-hour cache expires
	// (router/compact_idle.go). On by default.
	NoIdleCompaction bool `json:"no_idle_compaction,omitempty"`
	// MidTurn swaps a ready summary in on the next request, even inside a
	// turn, instead of waiting for the next plain prompt. The turn in
	// progress keeps its thinking; only what came before it is summarised.
	// Off by default: whether the API accepts a running turn's thinking
	// after the history before it has changed is not documented, so the
	// gateway undoes the swap and resends on a 400 (see compact_run.go).
	MidTurn bool `json:"mid_turn,omitempty"`
	// RepoOverrides give particular repositories their own Compact at, or
	// none at all. Everything else (the warning percentage, the delay
	// between compactions, mid-turn) applies to the repository's own limit.
	RepoOverrides []RepoCompaction `json:"repo_overrides,omitempty"`
	// Mode is how Compact at is chosen. "" is fixed: CompactAtTokens for
	// every repository without an override. CompactionIntelligent learns a
	// Compact at for each repository from what its compactions cost and
	// saved (internal/autocompact), adjusted once a day, never below
	// FloorTokens nor above CompactAtTokens. A repository's override still
	// wins, and WindowMinutes still spaces a session's compactions.
	Mode        string `json:"mode,omitempty"`
	FloorTokens int64  `json:"floor_tokens,omitempty"`
	// BufferPercent is added on top of the size the learner works out as
	// cheapest, so a learned Compact at is less eager than the money alone
	// says: the log does not measure what a summary loses. nil is the
	// default; 0 is no buffer.
	BufferPercent *int `json:"buffer_percent,omitempty"`
	// Learned is each repository's learned Compact at, by root. Memory
	// only: internal/autocompact keeps it, with how it got there, in its
	// own file, and it applies only in the intelligent mode.
	Learned map[string]int64 `json:"-"`
}

// CompactionIntelligent is CompactionConfig.Mode for a learned Compact at.
const CompactionIntelligent = "intelligent"

// Intelligent reports whether Compact at is learned per repository.
func (c CompactionConfig) Intelligent() bool { return c.Mode == CompactionIntelligent }

// RepoCompaction is one repository's override. Repo is the repository's
// root, an absolute path: two checkouts of the same project can differ.
type RepoCompaction struct {
	Repo            string `json:"repo"`
	CompactAtTokens int64  `json:"compact_at_tokens,omitempty"`
	// Off never compacts this repository's sessions on its own;
	// /compact-async still does, since the user chose the moment.
	Off bool `json:"off,omitempty"`
	// Learned marks a limit ForRepo took from CompactionConfig.Learned, not
	// one the user set. Never stored.
	Learned bool `json:"-"`
}

// NeverTokens is the Compact at of a repository with compaction off: no
// context reaches it.
const NeverTokens = int64(1) << 62

// ForRepo is c (resolved) as it applies to sessions in the repository at
// root, and the override that applied, nil when none did. root "" (a
// session whose repository is unknown) gets the default.
func (c CompactionConfig) ForRepo(root string) (CompactionConfig, *RepoCompaction) {
	c = c.Resolved()
	if root == "" {
		return c, nil
	}
	root = filepath.Clean(root)
	for i := range c.RepoOverrides {
		o := c.RepoOverrides[i]
		if o.Repo == "" || filepath.Clean(o.Repo) != root {
			continue
		}
		switch {
		case o.Off:
			c.CompactAtTokens, c.WarnAtTokens = NeverTokens, NeverTokens
		case o.CompactAtTokens > 0:
			c.CompactAtTokens = o.CompactAtTokens
			c.WarnAtTokens = c.warnAt(o.CompactAtTokens)
		default:
			continue
		}
		return c, &o
	}
	if at := c.Learned[root]; c.Intelligent() && at > 0 {
		c.CompactAtTokens = at
		c.WarnAtTokens = c.warnAt(at)
		return c, &RepoCompaction{Repo: root, CompactAtTokens: at, Learned: true}
	}
	return c, nil
}

const (
	DefaultCompactionWarnPercent = 80
	DefaultCompactionCompactAt   = 300_000
	DefaultCompactionWindow      = 30
	DefaultCompactionFloor       = 100_000
	DefaultCompactionBuffer      = 0
)

// warnAt is the warning size for a Compact at. Intelligent mode has none:
// its sizes are learned and move daily, so "80% of it" warned about a
// compaction the mode had already decided was the cheap thing to do.
func (c CompactionConfig) warnAt(at int64) int64 {
	if c.Intelligent() {
		return NeverTokens
	}
	return at * int64(c.WarnAtPercent) / 100
}

// Resolved returns c with its zero numbers replaced by the defaults.
func (c CompactionConfig) Resolved() CompactionConfig {
	if c.CompactAtTokens <= 0 {
		c.CompactAtTokens = DefaultCompactionCompactAt
	}
	if c.WarnAtPercent <= 0 {
		c.WarnAtPercent = DefaultCompactionWarnPercent
	}
	c.WarnAtTokens = c.warnAt(c.CompactAtTokens)
	if c.WindowMinutes <= 0 {
		c.WindowMinutes = DefaultCompactionWindow
	}
	if c.FloorTokens <= 0 {
		c.FloorTokens = DefaultCompactionFloor
	}
	if c.BufferPercent == nil {
		b := DefaultCompactionBuffer
		c.BufferPercent = &b
	}
	return c
}

// CoordinationConfig is session coordination: Claude Code hooks that let
// sessions on this Mac share files without losing or sweeping up each
// other's work (internal/coord). Off by default. Zero minutes take the
// defaults: a master idle 15 minutes hands its files on, and one sitting on
// other sessions' uncommitted changes 10 minutes is nudged to commit.
type CoordinationConfig struct {
	Enabled           bool `json:"enabled,omitempty"`
	MasterIdleMinutes int  `json:"master_idle_minutes,omitempty"`
	NudgeMinutes      int  `json:"nudge_minutes,omitempty"`
}

const (
	DefaultCoordMasterIdle = 15
	DefaultCoordNudge      = 10
)

// Resolved returns c with its zero numbers replaced by the defaults.
func (c CoordinationConfig) Resolved() CoordinationConfig {
	if c.MasterIdleMinutes <= 0 {
		c.MasterIdleMinutes = DefaultCoordMasterIdle
	}
	if c.NudgeMinutes <= 0 {
		c.NudgeMinutes = DefaultCoordNudge
	}
	return c
}

type ModelPrice struct {
	InputPerMTok  float64 `json:"input_per_mtok"`
	OutputPerMTok float64 `json:"output_per_mtok"`
	// Cache rates are optional. When absent, CacheRates derives them: for a
	// Claude model from Anthropic's published multipliers (reads 0.1x input,
	// 5-minute writes 1.25x), for anything else at the full input rate --
	// an unknown discount is priced as no discount, so spend is overstated
	// rather than hidden.
	CacheReadPerMTok  float64 `json:"cache_read_per_mtok,omitempty"`
	CacheWritePerMTok float64 `json:"cache_write_per_mtok,omitempty"`
	// CacheWrite1hPerMTok is a write to the one-hour cache, which Claude
	// Code asks for on a subscription. When absent: twice the input rate for
	// a Claude model (Anthropic's published multiplier), the ordinary write
	// rate for anything else.
	CacheWrite1hPerMTok float64 `json:"cache_write_1h_per_mtok,omitempty"`
	// LongPromptOverTokens and LongPromptMultiplier price a model that
	// charges more for a long prompt: every rate is multiplied when the
	// prompt (input, cache reads and cache writes together) is over that
	// many tokens. Claude Haiku 5.5 is five times the price over 100,000.
	LongPromptOverTokens int64   `json:"long_prompt_over_tokens,omitempty"`
	LongPromptMultiplier float64 `json:"long_prompt_multiplier,omitempty"`
}

// promptRate is what a prompt of that many tokens multiplies every rate by:
// 1 unless the model charges more for a long prompt and this is one.
func (p ModelPrice) promptRate(prompt int64) float64 {
	if p.LongPromptOverTokens > 0 && p.LongPromptMultiplier > 0 && prompt > p.LongPromptOverTokens {
		return p.LongPromptMultiplier
	}
	return 1
}

// LongWriteExtraUSD is what tokens written to the one-hour cache cost over
// the ordinary write rate PriceTokens charged them at. Until 7 Oct 2026
// every write was priced as a five-minute one, 1.25 times input, while this
// Mac's sessions wrote to the one-hour cache at 2 times: 14 days of writes
// read as $105 and were $168. It is given no prompt size, so on a model
// with a long prompt price the extra is counted at the short prompt rate.
func (c Config) LongWriteExtraUSD(model string, tokens int64) float64 {
	if tokens <= 0 {
		return 0
	}
	price := c.Pricing[model]
	_, write := price.CacheRates(model)
	long := price.CacheWrite1hPerMTok
	if long == 0 {
		long = write
		if strings.Contains(model, "claude") {
			long = price.InputPerMTok * 2
		}
	}
	return float64(tokens) / 1_000_000 * math.Max(0, long-write)
}

// CacheRates returns the per-MTok price of cache reads and cache writes for
// model. See ModelPrice for the defaults.
// PriceTokens is the API-equivalent cost of tokens served by model at the
// configured rates, and whether model has a pricing entry at all (a missing
// entry prices at $0, which callers must not report as free).
func (c Config) PriceTokens(model string, input, output, cacheRead, cacheWrite int64) (float64, bool) {
	price, priced := c.Pricing[model]
	readRate, writeRate := price.CacheRates(model)
	usd := (float64(input)/1_000_000)*price.InputPerMTok + (float64(output)/1_000_000)*price.OutputPerMTok +
		(float64(cacheRead)/1_000_000)*readRate + (float64(cacheWrite)/1_000_000)*writeRate
	return usd * price.promptRate(input+cacheRead+cacheWrite), priced
}

func (p ModelPrice) CacheRates(model string) (read, write float64) {
	read, write = p.CacheReadPerMTok, p.CacheWritePerMTok
	claude := strings.Contains(model, "claude")
	if read == 0 {
		read = p.InputPerMTok
		if claude {
			read = p.InputPerMTok * 0.1
		}
	}
	if write == 0 {
		write = p.InputPerMTok
		if claude {
			write = p.InputPerMTok * 1.25
		}
	}
	return read, write
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

	// HotspotTransportMultiplier multiplies TransportErrorMinFailures while
	// the Mac reaches the internet through an iPhone Personal Hotspot
	// (172.20.10.0/28). On a phone the mobile uplink is by far the likeliest
	// thing to have failed, and the secondary sits behind that same uplink.
	// 2026-10-02 12:04: a 258k-token request on the hotspot died with "write:
	// broken pipe", its fresh-connection retry did too, and the window that
	// armed sent six turns to the paid secondary for a dropped mobile link.
	// Only transport failures are scaled: a 429 or 5xx is Anthropic
	// answering, which a phone cannot fake, so MinFailures is unchanged.
	// 0 means the default, DefaultHotspotTransportMultiplier; 1 turns the
	// extra tolerance off.
	HotspotTransportMultiplier int `json:"hotspot_transport_multiplier,omitempty"`
}

// DefaultHotspotTransportMultiplier doubles the transport-failure threshold
// on a phone hotspot: with the default threshold of 1, two failed requests
// inside the window, not one.
const DefaultHotspotTransportMultiplier = 2

// HotspotMultiplier is HotspotTransportMultiplier with the default applied.
func (m MeteredFailoverConfig) HotspotMultiplier() int {
	if m.HotspotTransportMultiplier <= 0 {
		return DefaultHotspotTransportMultiplier
	}
	return m.HotspotTransportMultiplier
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

// Values for Config.KeepAwakeLidClosedPower.
const (
	KeepAwakeOnAC   = "ac"
	KeepAwakeAlways = "always"
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

// AutomaskConfig: Enabled is the master switch. Rules holds only the rules
// switched away from their default (internal/automask.Rules), so a new rule
// arrives with its own default rather than off.
type AutomaskConfig struct {
	Enabled bool            `json:"enabled,omitempty"`
	Rules   map[string]bool `json:"rules,omitempty"`
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
	FallbackChain map[string][]string `json:"fallback_chain"`

	Primary RouteConfig `json:"primary,omitempty"`
	// AdoptedBaseURL is the ANTHROPIC_BASE_URL Claude Code had before
	// enable pointed it at this gateway: an enterprise gateway such as
	// Portkey, which then becomes Primary.BaseURL. disable and rollback.sh
	// put it back, so Claude Code is left exactly as it was found.
	AdoptedBaseURL  string                `json:"adopted_base_url,omitempty"`
	Secondary       RouteConfig           `json:"secondary,omitempty"`
	MeteredFailover MeteredFailoverConfig `json:"metered_failover,omitempty"`
	Intercept       InterceptConfig       `json:"intercept,omitempty"`

	// SecondaryPruning trims old and oversized tool output from requests sent
	// to an openai-compatible secondary, where every token is paid for and
	// nothing is cached for us. On unless Disabled; see router/prune.go.
	SecondaryPruning PruneConfig `json:"secondary_pruning,omitempty"`

	// PrimaryCompaction summarises the old part of a long primary session
	// once and sends the summary in place of those messages from then on.
	// Experimental and off by default; see router/compact.go.
	PrimaryCompaction CompactionConfig `json:"primary_compaction,omitempty"`

	// Automask masks personal data (card numbers, ID numbers, ...) in every
	// request before it leaves the Mac. Off by default; see AutomaskConfig.
	Automask AutomaskConfig `json:"automask,omitempty"`

	// SessionCoordination: see CoordinationConfig.
	SessionCoordination CoordinationConfig `json:"session_coordination,omitempty"`

	// Codex: see CodexConfig.
	Codex CodexConfig `json:"codex,omitempty"`

	// AdminListen is the local control panel's address. Deliberately a
	// separate listener from Listen: in transparent mode the gateway serves
	// the intercepted hostname, and admin routes must not be reachable there.
	// Empty disables the panel entirely.
	AdminListen string `json:"admin_listen,omitempty"`

	// ConsoleListen is the support console's address (`claude-burst
	// console`, its own process): audit, log and repair buttons that stay
	// up when the gateway and dashboard do not. "off" disables it.
	Console string `json:"console_listen,omitempty"`

	// KeepAwakeLidClosed keeps the Mac awake with the lid shut, so a Claude
	// Code session in Ghostty keeps working and Remote Control stays reachable.
	// Off by default: a closed laptop that never sleeps drains its battery and
	// runs hot in a bag. Applied by `configure --keep-awake-lid-closed`, which
	// sets pmset SleepDisabled (root, scripts/lid-awake-root.sh) and turns off
	// App Nap for Ghostty. `status` reports drift between this and the machine.
	KeepAwakeLidClosed bool `json:"keep_awake_lid_closed"`

	// KeepAwakeLidClosedPower picks WHEN KeepAwakeLidClosed applies:
	// KeepAwakeOnAC (default) only while plugged in -- a root LaunchDaemon
	// follows the power source, because SleepDisabled is one global value
	// with no per-power-source form -- or KeepAwakeAlways.
	KeepAwakeLidClosedPower string `json:"keep_awake_lid_closed_power"`

	// KeepAwakeIdleMinutes narrows KeepAwakeLidClosed to "while in use": the
	// Mac stays awake with the lid shut only this many minutes after Claude
	// Code was last used or the lid was last open, then sleeps as usual.
	// 0 (default) keeps it awake for as long as the power mode applies.
	KeepAwakeIdleMinutes int `json:"keep_awake_idle_minutes,omitempty"`

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

	// Notify raises macOS notifications for events worth knowing about while
	// looking elsewhere. Read live by the gateway, no restart needed.
	Notify NotifyConfig `json:"notify,omitempty"`

	// AlertDailySpendUSD puts an on-screen alert up when today's
	// API-equivalent spend through the gateway passes this many dollars,
	// once per day per level. 0 is off. Read live.
	AlertDailySpendUSD float64 `json:"alert_daily_spend_usd,omitempty"`

	// Hotspot joins a named Wi-Fi network when this Mac is offline, so a
	// session left running with the lid shut can reach Anthropic from a
	// phone's hotspot. Read live, no restart needed. See internal/hotspot.
	Hotspot HotspotConfig `json:"hotspot,omitempty"`
}

// NotifyConfig is retired: Burst no longer sends macOS notifications (the
// usage panel's on-screen alerts replaced them on 3 Oct 2026). Kept so a
// config.json that still has a "notify" block reads and saves unchanged.
type NotifyConfig struct {
	Failover   bool `json:"failover,omitempty"`   // requests move to the secondary, and back
	Compaction bool `json:"compaction,omitempty"` // Burst compacted a session
	Guards     bool `json:"guards,omitempty"`     // a guard repaired or removed the redirect
}

// Values for HotspotConfig.When.
const (
	HotspotLidClosed = "lid-closed"
	HotspotAlways    = "always"
)

type HotspotConfig struct {
	// SSID is the network to join; empty turns the feature off.
	SSID string `json:"ssid,omitempty"`
	// When is HotspotLidClosed (default: only while the lid is shut, so an
	// open laptop is left to the user) or HotspotAlways.
	When string `json:"when,omitempty"`
	// The watcher's timing; 0 means the default. CheckSeconds: how often
	// the internet is probed. OfflineChecks: failed probes in a row before
	// the first join. RetrySeconds: the gap after one attempt ends and the
	// next starts. GiveUpMinutes: how long it keeps trying, from its first
	// attempt in an offline spell.
	CheckSeconds  int `json:"check_seconds,omitempty"`
	OfflineChecks int `json:"offline_checks,omitempty"`
	RetrySeconds  int `json:"retry_seconds,omitempty"`
	GiveUpMinutes int `json:"give_up_minutes,omitempty"`
}

const (
	DefaultHotspotCheckSeconds  = 5
	DefaultHotspotOfflineChecks = 2
	DefaultHotspotRetrySeconds  = 60
	DefaultHotspotGiveUpMinutes = 30

	MinHotspotCheckSeconds  = 2
	MaxHotspotCheckSeconds  = 120
	MaxHotspotOfflineChecks = 10
	MinHotspotRetrySeconds  = 15 // below this a phone is hammered
	MaxHotspotRetrySeconds  = 30 * 60
	MaxHotspotGiveUpMinutes = 24 * 60
)

func orDefault(v, def int) int {
	if v <= 0 {
		return def
	}
	return v
}

// CheckEvery, OfflineAfter and RetryEvery are the watcher's timing with
// the defaults filled in.
func (h HotspotConfig) CheckEvery() time.Duration {
	return time.Duration(orDefault(h.CheckSeconds, DefaultHotspotCheckSeconds)) * time.Second
}
func (h HotspotConfig) OfflineAfter() int {
	return orDefault(h.OfflineChecks, DefaultHotspotOfflineChecks)
}
func (h HotspotConfig) RetryEvery() time.Duration {
	return time.Duration(orDefault(h.RetrySeconds, DefaultHotspotRetrySeconds)) * time.Second
}

// GiveUp is GiveUpMinutes as a duration, with the default filled in.
func (h HotspotConfig) GiveUp() time.Duration {
	if h.GiveUpMinutes <= 0 {
		return DefaultHotspotGiveUpMinutes * time.Minute
	}
	return time.Duration(h.GiveUpMinutes) * time.Minute
}

// CodexConfig is the Codex gateway: a second listener, plain HTTP on
// loopback, that Codex reaches through a model provider in
// ~/.codex/config.toml (see internal/codex). Whether Codex USES it is decided
// in that file, not here, so the listener runs whenever Burst does: a Codex
// session started while it was on keeps sending here until it exits.
type CodexConfig struct {
	// Listen is the Codex listener's address; "off" turns it off.
	Listen string `json:"listen,omitempty"`
	// Upstream is where Codex's requests go on to: the ChatGPT backend
	// that a ChatGPT login talks to.
	Upstream string `json:"upstream,omitempty"`
}

// DefaultCodexListen and DefaultCodexUpstream apply when config.json names
// none, which is every config written before the Codex gateway existed.
const (
	DefaultCodexListen   = "127.0.0.1:7779"
	DefaultCodexUpstream = "https://chatgpt.com"
)

// CodexListen is the Codex listener's address, "" when it is off.
func (c Config) CodexListen() string {
	switch c.Codex.Listen {
	case "off":
		return ""
	case "":
		return DefaultCodexListen
	}
	return c.Codex.Listen
}

// DefaultConsoleListen is the support console's address unless configured.
const DefaultConsoleListen = "127.0.0.1:7789"

// ConsoleListen is the support console's address, "" when off.
func (c Config) ConsoleListen() string {
	switch c.Console {
	case "off":
		return ""
	case "":
		return DefaultConsoleListen
	}
	return c.Console
}

// CodexUpstream is where the Codex listener forwards to.
func (c Config) CodexUpstream() string {
	if c.Codex.Upstream == "" {
		return DefaultCodexUpstream
	}
	return strings.TrimRight(c.Codex.Upstream, "/")
}

// CodexMetricsPath is the Codex gateway's request log: kept apart from
// metrics.jsonl so no Claude Code total ever includes a Codex request.
func CodexMetricsPath() (string, error) {
	d, err := ConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "codex-metrics.jsonl"), nil
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

		KeepAwakeLidClosedPower: KeepAwakeOnAC,
		KeychainService:         "claude-burst-bedrock",
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
			// Fable 5.1, Opus 5.5 and Sonnet 5.5 cache reads are listed prices
			// ($0.25, $0.20 and $0.10/MTok), under the 0.1x-of-input default
			// CacheRates uses. Haiku 5.5 is priced by prompt length: these
			// rates up to 100,000 tokens, five times them over that.
			"claude-fable-5-1":                                {InputPerMTok: 10, OutputPerMTok: 50, CacheReadPerMTok: 0.25},
			"claude-opus-5-5":                                 {InputPerMTok: 4, OutputPerMTok: 20, CacheReadPerMTok: 0.20},
			"claude-sonnet-5-5":                               {InputPerMTok: 2, OutputPerMTok: 10, CacheReadPerMTok: 0.10},
			"claude-haiku-5-5":                                {InputPerMTok: 0.10, OutputPerMTok: 0.50, LongPromptOverTokens: 100_000, LongPromptMultiplier: 5},
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

// MaxKeepAwakeIdleMinutes is a day, the most lid-awake-root.sh accepts.
const MaxKeepAwakeIdleMinutes = 1440

// ValidateKeepAwakePower rejects an unknown power mode rather than guessing:
// "always" read as "ac" would let the Mac sleep when the user expected it not
// to, and the reverse would keep it awake on battery in a bag.
func ValidateKeepAwakePower(mode string) error {
	switch mode {
	case KeepAwakeOnAC, KeepAwakeAlways:
		return nil
	default:
		return fmt.Errorf("keep_awake_lid_closed_power %q is not recognised (want %q or %q)",
			mode, KeepAwakeOnAC, KeepAwakeAlways)
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
	p, err := ConfigPath()
	if err != nil {
		return Default(), err
	}
	return loadFile(p)
}

// loadFile is Load for the file at p.
func loadFile(p string) (Config, error) {
	cfg := Default()
	b, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		cfg.ResolveRoutes()
		return cfg, nil
	}
	if err != nil {
		return cfg, err
	}
	// Decoding into a map merges into what is there, so the default chain
	// would come back on every load and a fallback removed in the dashboard
	// could never stay removed. A chain in the file replaces the default;
	// a file without one gets the default below. Pricing is left to merge on
	// purpose: new built-in prices reach old config files that way.
	cfg.FallbackChain = nil
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
	if cfg.KeepAwakeLidClosedPower == "" {
		cfg.KeepAwakeLidClosedPower = KeepAwakeOnAC
	}
	cfg.ResolveRoutes()
	if err := cfg.ValidateIntercept(); err != nil {
		return cfg, fmt.Errorf("parse %s: %w", p, err)
	}
	if err := ValidateKeepAwakePower(cfg.KeepAwakeLidClosedPower); err != nil {
		return cfg, fmt.Errorf("parse %s: %w", p, err)
	}
	if cfg.KeepAwakeIdleMinutes < 0 || cfg.KeepAwakeIdleMinutes > MaxKeepAwakeIdleMinutes {
		return cfg, fmt.Errorf("parse %s: keep_awake_idle_minutes must be 0 to %d, got %d", p, MaxKeepAwakeIdleMinutes, cfg.KeepAwakeIdleMinutes)
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
	if err := atomicfile.Write(p, full, 0600); err != nil {
		return err
	}
	_ = backup.SetLatest(p, full)
	return nil
}
