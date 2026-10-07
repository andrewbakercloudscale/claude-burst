package admin

import (
	_ "embed"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/andrewbakercloudscale/claude-burst/internal/claudesettings"
	"github.com/andrewbakercloudscale/claude-burst/internal/config"
	"github.com/andrewbakercloudscale/claude-burst/internal/handover"
	"github.com/andrewbakercloudscale/claude-burst/internal/metrics"
	"github.com/andrewbakercloudscale/claude-burst/internal/router"
	"github.com/andrewbakercloudscale/claude-burst/internal/tlsca"
	"github.com/andrewbakercloudscale/claude-burst/internal/tlswatch"
)

type stateResponse struct {
	Version string `json:"version"`
	// PID tells a restarted gateway from the old one still draining: both
	// answer /api/state while the old one finishes its replies.
	PID int `json:"pid"`
	// ConfigError, when set, means config.json failed to load: every other
	// field below is a zero value and must not be trusted for anything.
	// Reported as a normal 200 response rather than an HTTP error status,
	// because an HTTP error on THIS endpoint blanks the whole dashboard
	// (it's what every page load and every Refresh click fetches first) --
	// exactly the moment a working dashboard matters most, since a broken
	// config.json is itself the thing someone would come here to diagnose.
	ConfigError string          `json:"config_error,omitempty"`
	Route       string          `json:"route"`
	Overflow    bool            `json:"overflow"`
	Until       string          `json:"until,omitempty"`
	Claim       string          `json:"claim,omitempty"`
	Reason      string          `json:"reason,omitempty"`
	Gateway     string          `json:"gateway"`
	Primary     routeInfo       `json:"primary"`
	Secondary   routeInfo       `json:"secondary"`
	Intercept   interceptInfo   `json:"intercept"`
	Totals      metrics.Summary `json:"totals"`
	Today       metrics.Summary `json:"today"`

	// Downgrade describes the fallback chain: whether it is on, what it
	// would do, and which models are currently inside a rejection window of
	// their own. Without the last part the toggle is a claim with nothing
	// behind it -- the useful question on this page is not "is downgrade
	// enabled" but "what is Fable doing right now".
	Downgrade downgradeInfo `json:"downgrade"`

	// Context is prompt caching and overflow-request pruning: the switches
	// and whether they are working. See context.go.
	Context contextInfo `json:"context"`

	// Handover is the HANDOFF.md hooks: installed or not, their settings,
	// and the log of what they wrote. See handover.go.
	Handover handover.Status `json:"handover"`

	// PrimaryHealth is whether Anthropic is answering right now, as opposed
	// to the 14-day error rate, which an outage of minutes cannot move.
	PrimaryHealth router.PrimaryHealth `json:"primary_health"`

	// SecondaryReady is whether the RUNNING gateway has a secondary it can
	// fail over to: built, with its credential. False on a single plan.
	SecondaryReady bool `json:"secondary_ready"`

	// ClientTLS is the client side of the gateway's TLS handshakes over the
	// last five minutes: how many completed, how many failed and why. Absent
	// in base-url mode. It is the only field here that can see Claude Code
	// refusing the gateway's certificate; every other check is the gateway
	// looking at itself.
	ClientTLS *tlswatch.Snapshot `json:"client_tls,omitempty"`
}

type downgradeInfo struct {
	Enabled bool                `json:"enabled"`
	Chain   map[string][]string `json:"chain,omitempty"`
	// Rejected lists each model with an open window, soonest first.
	Rejected []rejectedModel `json:"rejected,omitempty"`
}

type rejectedModel struct {
	Model string `json:"model"`
	Until string `json:"until"`
	// FallsBackTo is the rung the next request for this model will actually
	// take -- "" meaning the secondary, because every rung is itself
	// rejected or none is configured.
	FallsBackTo string `json:"falls_back_to,omitempty"`
}

type routeInfo struct {
	Provider string `json:"provider"`
	BaseURL  string `json:"base_url"`
	Model    string `json:"model,omitempty"`
	Strategy string `json:"strategy,omitempty"`

	// KeychainService, KeyEnvVar and KeyPresent describe the credential
	// this slot would use. The key itself is never sent -- only whether one
	// can be found, and under which names, which is what someone filling in
	// the secondary form needs to know and is the whole answer to "why did
	// failover produce an auth error?".
	//
	// KeyEnvVar doubles as the applicability flag: it is empty for a slot
	// that has no key at all (oauth-passthrough), which is a different
	// thing from a slot whose key is missing, and the UI must not show the
	// two the same way.
	KeychainService string `json:"keychain_service,omitempty"`
	KeyEnvVar       string `json:"key_env_var,omitempty"`
	KeyPresent      bool   `json:"key_present,omitempty"`
	// KeySource is "keychain" or "environment". A green "key found" that
	// cannot tell the two apart is not evidence a save landed: an
	// unrelated env var answers identically, and the Keychain write could
	// have gone nowhere.
	KeySource string `json:"key_source,omitempty"`
	// KeyUpdated is when the Keychain entry was last written, RFC3339 with
	// this machine's offset. It is the page's only proof that the key you
	// just saved is the key it can see -- the value itself is never sent,
	// so without a timestamp "key found" is indistinguishable from "key
	// found, from six weeks ago, your save silently failed".
	KeyUpdated string `json:"key_updated,omitempty"`
}

// credentialNames returns the Keychain service and env var a route slot's
// provider draws its API key from, or ("", "") for a provider that needs no
// key of its own. It mirrors buildProvider's defaulting exactly (an empty
// keychain_service means cfg.KeychainService for bedrock and
// "claude-burst-together" for openai-compatible), because a form that
// reported a different service name than the gateway actually reads would
// invite storing a key where nothing looks for it.
func credentialNames(rc config.RouteConfig, defaultKeychainService string) (service, envVar string) {
	switch rc.Provider {
	case "bedrock":
		service = rc.KeychainService
		if service == "" {
			service = defaultKeychainService
		}
		return service, "AWS_BEARER_TOKEN_BEDROCK"
	case "openai-compatible":
		service = rc.KeychainService
		if service == "" {
			service = "claude-burst-together"
		}
		_, envVar = router.OpenAICompatibleIdentity(service)
		return service, envVar
	default:
		return "", ""
	}
}

type interceptInfo struct {
	Mode        string `json:"mode"`
	Host        string `json:"host,omitempty"`
	CATrusted   bool   `json:"ca_trusted"`
	HostsEntry  bool   `json:"hosts_entry"`
	RemoteCtrl  bool   `json:"remote_control_expected"`
	SettingsURL string `json:"settings_base_url"`
	// Active answers a different question from every field above it: not
	// "is Claude Burst configured?" but "is Claude Code's traffic actually
	// going through it right now?" Those came apart in practice -- the
	// dashboard reported a healthy PRIMARY route, with a gateway version
	// and a live request table, while nothing had ever been enabled and
	// every real request went straight to Anthropic. Everything on the page
	// was true; none of it answered the question the user had.
	//
	// This is the cheap local answer (what is on disk). The expensive live
	// one is /api/test-connection, which actually puts a request down the
	// wire -- see its doc comment for why config-on-disk still isn't proof.
	Active bool `json:"active"`
	// InactiveReason names the specific missing piece, because "not active"
	// with three possible causes is a prompt to go guessing.
	InactiveReason string `json:"inactive_reason,omitempty"`
	// PFHeal reports the root daemon that guards the pf rdr rule. Only
	// meaningful in transparent mode, and only populated there -- base-url
	// mode has no pf rule to lose.
	PFHeal *pfHealInfo `json:"pf_heal,omitempty"`
	// SelfHeal reports the user LaunchAgent that keeps the gateway itself
	// alive. Populated in BOTH modes, unlike PFHeal: a dead gateway breaks
	// base-url mode just as thoroughly, it simply breaks it for one user
	// instead of for the whole machine.
	SelfHeal *pfHealInfo `json:"self_heal,omitempty"`
	// BailoutCmd is the ready-to-run command that undoes the machine-wide
	// redirect (pf + /etc/hosts). Only meaningful in transparent mode, and
	// only ever needs root -- the dashboard can't run it itself, but it can
	// make sure the one command that gets this Mac back to a known-good
	// state is always visible rather than something you have to go dig out
	// of scripts/ or ask for while every request is failing.
	BailoutCmd string `json:"bailout_cmd,omitempty"`
}

func (s *Server) handleState(w http.ResponseWriter, r *http.Request) {
	cfg, err := config.Load()
	if err != nil {
		writeJSON(w, stateResponse{Version: s.version, PID: os.Getpid(), ConfigError: err.Error()})
		return
	}
	st := s.gateway.Status()
	overflow := st.OverflowUntil > time.Now().Unix()

	route := "PRIMARY"
	if overflow {
		route = "SECONDARY"
	}

	total, _ := cachedScan("total|"+s.metricsPath, func() (metrics.Summary, error) { return metrics.Summarize(s.metricsPath, time.Time{}) })
	today, _ := cachedScan("today|"+s.metricsPath, func() (metrics.Summary, error) {
		return metrics.Summarize(s.metricsPath, time.Now().Add(-24*time.Hour))
	})

	mode := cfg.Intercept.Mode
	if mode == "" {
		mode = config.InterceptBaseURL
	}

	var settingsURL string
	if p, err := claudesettings.Path(); err == nil {
		if root, err := claudesettings.Read(p); err == nil {
			settingsURL = claudesettings.BaseURL(root)
		}
	}

	ii := interceptInfo{Mode: mode, Host: cfg.Intercept.Host, SettingsURL: settingsURL}
	scriptsForGuards, _ := s.scriptsDir()
	sh := selfHealStatus(scriptsForGuards)
	ii.SelfHeal = &sh
	if cfg.Intercept.Transparent() {
		if b, err := os.ReadFile(cfg.Intercept.CABundle); err == nil {
			ii.CATrusted = tlsca.HasBlock(string(b))
		}
		if h, err := os.ReadFile("/etc/hosts"); err == nil {
			ii.HostsEntry = config.HostsRedirectActive(h, cfg.Intercept.Host)
		}
		ii.BailoutCmd = "sudo " + s.rootHelper + " remove"
		dir, _ := s.scriptsDir()
		ph := pfHealStatus(dir)
		ii.PFHeal = &ph
	}
	// Claude Code disables Remote Control whenever ANTHROPIC_BASE_URL names a
	// host other than api.anthropic.com; an unset value is the default.
	ii.RemoteCtrl = settingsURL == ""
	ii.Active, ii.InactiveReason = interceptActive(cfg, ii)

	primary := routeInfo{Provider: cfg.Primary.Provider, BaseURL: cfg.Primary.BaseURL,
		Model: cfg.Primary.Model, Strategy: cfg.Primary.FailoverStrategy}
	secondary := routeInfo{Provider: cfg.Secondary.Provider, BaseURL: cfg.Secondary.BaseURL,
		Model: cfg.Secondary.Model, Strategy: cfg.Secondary.FailoverStrategy}
	secondary.KeychainService, secondary.KeyEnvVar = credentialNames(cfg.Secondary, cfg.KeychainService)
	if secondary.KeyEnvVar != "" {
		info := s.keyInfo(secondary.KeychainService, secondary.KeyEnvVar)
		secondary.KeyPresent, secondary.KeySource = info.Present, info.Source
		if !info.Modified.IsZero() {
			secondary.KeyUpdated = info.Modified.Format(time.RFC3339)
		}
	}

	resp := stateResponse{
		PID:     os.Getpid(),
		Version: s.version, Route: route, Overflow: overflow,
		Claim: st.LimitClaim, Reason: st.LastReason, Gateway: cfg.Listen,
		Primary:   primary,
		Secondary: secondary,
		Intercept: ii, Totals: total, Today: today,
	}
	if overflow {
		resp.Until = time.Unix(st.OverflowUntil, 0).Format(time.RFC3339)
	}
	resp.Downgrade = s.downgradeInfo(cfg)
	resp.Context = s.contextInfo(cfg)
	resp.Handover = handover.GetStatus()
	resp.PrimaryHealth = s.gateway.Health()
	resp.SecondaryReady = s.gateway.HasSecondary()
	if s.handshakes != nil {
		snap := s.handshakes.Snapshot()
		resp.ClientTLS = &snap
	}
	writeJSON(w, resp)
}

func (s *Server) downgradeInfo(cfg config.Config) downgradeInfo {
	enabled := s.gateway.DowngradeEnabled()
	di := downgradeInfo{Enabled: enabled, Chain: cfg.FallbackChain}
	now := time.Now().Unix()
	open := s.gateway.ModelOverflow()
	for model, until := range open {
		if until <= now {
			continue
		}
		r := rejectedModel{Model: model, Until: time.Unix(until, 0).Format(time.RFC3339)}
		if enabled {
			for _, rung := range cfg.FallbackChain[model] {
				if rung != model && open[rung] <= now {
					r.FallsBackTo = rung
					break
				}
			}
		}
		di.Rejected = append(di.Rejected, r)
	}
	sort.Slice(di.Rejected, func(i, j int) bool { return di.Rejected[i].Until < di.Rejected[j].Until })
	return di
}

// interceptActive reports whether Claude Code's traffic is actually being
// routed through this gateway, and if not, which piece is missing.
//
// The two modes fail in completely different places, which is why this
// cannot be one boolean read off config.json:
//
//   - base-url: settings.json must name this gateway in ANTHROPIC_BASE_URL.
//     Writing the mode into config.json does nothing on its own; `enable`
//     is what puts it in the path.
//   - transparent: settings.json must NOT name it (that is the whole point
//     -- it keeps Remote Control), so the evidence is elsewhere: the
//     /etc/hosts redirect sends the hostname to loopback, and the local CA
//     is trusted so the TLS handshake against it succeeds. With the hosts
//     entry but no CA trust, traffic arrives here and is REJECTED, which is
//     worse than not intercepting at all -- so that counts as inactive, and
//     says so.
//
// Deliberately a disk-only check with no network call: /api/state is
// fetched on every page load and every Refresh, and a probe with a timeout
// on that path would make the whole dashboard hang whenever the network is
// the thing that is broken.
func interceptActive(cfg config.Config, ii interceptInfo) (bool, string) {
	if cfg.Intercept.Transparent() {
		switch {
		case !ii.HostsEntry && !ii.CATrusted:
			return false, "transparent mode is configured but not installed: no /etc/hosts redirect and the local CA is not trusted. Claude Code is talking to Anthropic directly."
		case !ii.HostsEntry:
			return false, "the /etc/hosts redirect is missing, so nothing sends " + ii.Host + " to this gateway. Claude Code is talking to Anthropic directly."
		case !ii.CATrusted:
			return false, "the redirect is installed but the local CA is not trusted, so requests reach this gateway and then fail TLS. This is worse than not intercepting -- fix or remove the redirect."
		}
		return true, ""
	}
	if ii.SettingsURL == "" {
		return false, "ANTHROPIC_BASE_URL is not set in settings.json, so Claude Code is talking to Anthropic directly."
	}
	if !strings.Contains(ii.SettingsURL, cfg.Listen) {
		return false, "ANTHROPIC_BASE_URL points at " + ii.SettingsURL + ", which is not this gateway (" + cfg.Listen + ")."
	}
	return true, ""
}
