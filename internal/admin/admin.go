// Package admin serves a small local control panel for the gateway.
//
// It runs on its own listener, separate from the gateway's. That is not
// cosmetic: in transparent intercept mode the gateway IS https://api.anthropic.com
// as far as this machine is concerned, and admin routes sharing that listener
// would be reachable at that hostname.
//
// There is no login, by design -- it binds loopback and is meant to be opened
// in a browser on this Mac. "Loopback with no auth" is not automatically safe,
// though: a malicious web page can point a hostname it controls at 127.0.0.1
// (DNS rebinding) and drive this UI from the user's own browser. The two
// cheap defences that do not cost a password are applied to every request:
// the Host header must name loopback, and mutations must carry a custom header
// (which forces a CORS preflight that a cross-origin page cannot satisfy,
// since no CORS headers are ever returned).
package admin

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/andrewbakercloudscale/claude-burst/internal/claudesettings"
	"github.com/andrewbakercloudscale/claude-burst/internal/config"
	"github.com/andrewbakercloudscale/claude-burst/internal/keychain"
	"github.com/andrewbakercloudscale/claude-burst/internal/metrics"
	"github.com/andrewbakercloudscale/claude-burst/internal/router"
	"github.com/andrewbakercloudscale/claude-burst/internal/tlsca"
)

//go:embed admin.html
var indexHTML []byte

// mutationHeader must be present on any state-changing request. A cross-origin
// page cannot set it without a successful preflight, and this server answers
// no preflights.
const mutationHeader = "X-Claude-Burst-Admin"

type Server struct {
	gateway     *router.Server
	metricsPath string
	version     string
	// extraHost is an optional friendly hostname accepted in addition to the
	// loopback names. See config.AdminHostname for the trade-off it makes.
	extraHost string
	// rootHelper is the resolved path to transparent-root.sh, computed once
	// by the caller (cmd/claude-burst/main.go's rootHelperPath) rather than
	// re-derived here -- that search-the-likely-locations logic already
	// lives in exactly one place, and the dashboard's bail-out command needs
	// to be a path that actually exists, same as the CLI's own messages.
	rootHelper string

	// storeKey and hasKey are the two Keychain operations the secondary
	// form needs, held as fields purely so tests can substitute them. A
	// test that called keychain.Store directly would write a real entry
	// into the developer's login Keychain -- and, worse, `security
	// add-generic-password -U` overwrites, so a test using a plausible
	// service name could silently replace the key a live secondary is
	// running on.
	storeKey func(service, value string) error
	hasKey   func(service, envVar string) bool
}

func New(gateway *router.Server, metricsPath, version, extraHost, rootHelper string) *Server {
	return &Server{gateway: gateway, metricsPath: metricsPath, version: version,
		extraHost: strings.ToLower(extraHost), rootHelper: rootHelper,
		storeKey: keychain.Store, hasKey: keychain.Has}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleIndex)
	mux.HandleFunc("/api/state", s.readOnly(s.handleState))
	mux.HandleFunc("/api/requests", s.readOnly(s.handleRequests))
	mux.HandleFunc("/api/responses", s.readOnly(s.handleResponses))
	mux.HandleFunc("/api/test-connection", s.readOnly(s.handleTestConnection))
	mux.HandleFunc("/api/log", s.readOnly(s.handleLog))
	mux.HandleFunc("/api/reset", s.mutating(s.handleReset))
	mux.HandleFunc("/api/force", s.mutating(s.handleForce))
	mux.HandleFunc("/api/config", s.mutating(s.handleConfig))
	mux.HandleFunc("/api/secondary", s.mutating(s.handleSecondary))
	mux.HandleFunc("/api/revert", s.mutating(s.handleRevert))
	mux.HandleFunc("/api/restart", s.mutating(s.handleRestart))
	mux.HandleFunc("/api/install", s.mutating(s.handleInstall))
	mux.HandleFunc("/api/pf-heal-log", s.readOnly(s.handlePFHealLog))
	mux.HandleFunc("/api/pf-heal-install", s.mutating(s.handlePFHealInstall))
	mux.HandleFunc("/api/self-heal-log", s.readOnly(s.handleSelfHealLog))
	mux.HandleFunc("/api/self-heal-install", s.mutating(s.handleSelfHealInstall))
	return s.guard(mux)
}

// guard rejects requests whose Host header is not loopback. This is the
// DNS-rebinding defence: the attacker controls DNS, not the Host header the
// browser sends, so a page on evil.example resolving to 127.0.0.1 still
// arrives here with Host: evil.example.
func (s *Server) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.Host
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		host = strings.ToLower(host)
		allowed := host == "127.0.0.1" || host == "localhost" || host == "::1" || host == "[::1]" ||
			(s.extraHost != "" && host == s.extraHost)
		if !allowed {
			http.Error(w, "admin UI only accepts loopback Host headers (got "+r.Host+")", http.StatusForbidden)
			return
		}
		// Never emit CORS headers: without them a cross-origin page cannot
		// read a response even if it manages to send the request.
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(w, r)
	})
}

func (s *Server) readOnly(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		h(w, r)
	}
}

func (s *Server) mutating(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if r.Header.Get(mutationHeader) == "" {
			http.Error(w, "missing "+mutationHeader+" header", http.StatusForbidden)
			return
		}
		h(w, r)
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// No inline-script CSP exemption needed: the page's script is inline but
	// this server is the only origin that can reach it.
	_, _ = w.Write(indexHTML)
}

type stateResponse struct {
	Version string `json:"version"`
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
		writeJSON(w, stateResponse{Version: s.version, ConfigError: err.Error()})
		return
	}
	st := s.gateway.Status()
	overflow := st.OverflowUntil > time.Now().Unix()

	route := "PRIMARY"
	if overflow {
		route = "SECONDARY"
	}

	total, _ := metrics.Summarize(s.metricsPath, time.Time{})
	today, _ := metrics.Summarize(s.metricsPath, time.Now().Add(-24*time.Hour))

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
		secondary.KeyPresent = s.hasKey(secondary.KeychainService, secondary.KeyEnvVar)
	}

	resp := stateResponse{
		Version: s.version, Route: route, Overflow: overflow,
		Claim: st.LimitClaim, Reason: st.LastReason, Gateway: cfg.Listen,
		Primary:   primary,
		Secondary: secondary,
		Intercept: ii, Totals: total, Today: today,
	}
	if overflow {
		resp.Until = time.Unix(st.OverflowUntil, 0).Format(time.RFC3339)
	}
	writeJSON(w, resp)
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

func (s *Server) handleRequests(w http.ResponseWriter, r *http.Request) {
	limit := 50
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 500 {
			limit = n
		}
	}
	events, err := metrics.Recent(s.metricsPath, limit)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if events == nil {
		events = []metrics.Event{}
	}
	writeJSON(w, events)
}

func (s *Server) handleResponses(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, s.gateway.RecentResponses())
}

type testConnectionResponse struct {
	OK     bool   `json:"ok"`
	Mode   string `json:"mode"`
	Detail string `json:"detail"`
}

// handleTestConnection answers the question the dashboard could not
// otherwise answer on its own: is Claude Code's REAL traffic actually
// reaching this gateway right now? Everything else in /api/state (CA
// trusted, hosts entry present) describes configuration, not live behavior --
// on 2026-09-03 the hosts-entry check even had a bug that reported "yes"
// for an empty marker block, and independent of that bug, config being
// correct on disk still doesn't prove Claude Code's actual requests are
// landing here rather than sailing past to the real Anthropic. This runs
// the same "real traffic path" probe deploy.sh and watchdog.sh already use
// via gateway_healthy() in health-diagnostics.sh -- hit the intercepted
// hostname over plain HTTPS, exactly as Claude Code itself would, and check
// whether the body is actually THIS gateway's (it stamps its own "overflow"
// field into /healthz) rather than Anthropic's real one.
func (s *Server) handleTestConnection(w http.ResponseWriter, r *http.Request) {
	cfg, err := config.Load()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if !cfg.Intercept.Transparent() {
		// base-url mode: Claude Code reaches this gateway because
		// ANTHROPIC_BASE_URL in settings.json names it directly, not via
		// DNS -- there is no separate "real path" to test independent of
		// that URL being correct, which /api/state already reports.
		writeJSON(w, testConnectionResponse{
			OK: true, Mode: "base-url",
			Detail: "base-url mode: Claude Code reaches this gateway via ANTHROPIC_BASE_URL in settings.json, not DNS -- there's no separate live path to test.",
		})
		return
	}

	client := &http.Client{Timeout: 5 * time.Second}
	url := "https://" + cfg.Intercept.Host + "/healthz"
	resp, err := client.Get(url)
	if err != nil {
		writeJSON(w, testConnectionResponse{
			OK: false, Mode: "transparent",
			Detail: fmt.Sprintf("could not reach %s at all: %v", url, err),
		})
		return
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))

	if strings.Contains(string(body), `"overflow"`) {
		writeJSON(w, testConnectionResponse{
			OK: true, Mode: "transparent",
			Detail: fmt.Sprintf("confirmed: %s currently resolves to THIS gateway (HTTP %d) -- real Claude Code traffic is passing through it.", url, resp.StatusCode),
		})
		return
	}
	writeJSON(w, testConnectionResponse{
		OK: false, Mode: "transparent",
		Detail: fmt.Sprintf("%s answered (HTTP %d) but NOT from this gateway -- the machine-wide redirect is not installed, so real Claude Code traffic is going straight to Anthropic, bypassing burst entirely. Run: sudo %s install --host %s --gateway-port %s",
			url, resp.StatusCode, s.rootHelper, cfg.Intercept.Host, gatewayPort(cfg.Listen)),
	})
}

// gatewayPort extracts the port from a "host:port" listen address, falling
// back to the address itself if it doesn't parse -- only used to compose a
// copy-pasteable install command in the test-connection failure message.
func gatewayPort(listen string) string {
	if _, port, err := net.SplitHostPort(listen); err == nil {
		return port
	}
	return listen
}

// utcToLocalCutover names the release that switched the gateway's own log from
// UTC to local time, so a reader of a file spanning both knows where the seam
// is rather than assuming a gap or a stalled logger.
const utcToLocalCutover = "2026-09-08"

func utcOffsetLabel(now time.Time) string {
	_, offset := now.Zone()
	h := offset / 3600
	if h == 0 {
		return "the same"
	}
	return fmt.Sprintf("%dh", h)
}

// logTailBytes caps how much of claude-burst.log a single /api/log request
// serves. Under rotation the file can now grow to 200MB (see main.go's
// logMaxBytes/logMaxBackups) -- loading that whole thing into a browser tab
// would hang it, so this always serves just the tail, which is what anyone
// debugging a live issue actually wants: recent lines, not the full history.
const logTailBytes = 512 * 1024

// handleLog serves the tail of the gateway's own text log as plain text, so
// "what's actually happening" is one click from the dashboard instead of a
// terminal command against a path you have to already know
// (~/.config/claude-burst/claude-burst.log).
func (s *Server) handleLog(w http.ResponseWriter, r *http.Request) {
	path, err := config.LogPath()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	f, err := os.Open(path)
	if err != nil {
		http.Error(w, fmt.Sprintf("could not open %s: %v", path, err), http.StatusInternalServerError)
		return
	}
	defer f.Close()

	st, err := f.Stat()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")

	// New lines are local time (see main.go). Lines already on disk from
	// before that change are UTC, and a file with two clocks in it silently
	// mis-reads unless it says so -- which is the same failure, one layer
	// along, as the one that made this change necessary.
	now := time.Now()
	fmt.Fprintf(w, "(timestamps are LOCAL time; it is now %s. Lines written before %s are UTC -- older entries will look %s behind.)\n",
		now.Format("2006-01-02 15:04:05 MST"), utcToLocalCutover, utcOffsetLabel(now))
	if st.Size() > logTailBytes {
		if _, err := f.Seek(-logTailBytes, io.SeekEnd); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		fmt.Fprintf(w, "(showing the last %dKB of %s -- %d bytes total; older lines are in claude-burst.log.1, .2, ... via rotation)\n",
			logTailBytes/1024, path, st.Size())
	}
	fmt.Fprintln(w)
	_, _ = io.Copy(w, f)
}

func (s *Server) handleReset(w http.ResponseWriter, r *http.Request) {
	s.gateway.ClearOverflow()
	writeJSON(w, map[string]string{"ok": "back to primary — the next request will use it"})
}

type forceRequest struct {
	Minutes int `json:"minutes"`
}

func (s *Server) handleForce(w http.ResponseWriter, r *http.Request) {
	var req forceRequest
	_ = json.NewDecoder(r.Body).Decode(&req)
	// Validate against the RUNNING gateway's actual secondary, not a
	// freshly re-read config.json. The gateway only builds its Provider set
	// once, at startup (see router.New/buildProvider), so config.json can
	// say a secondary exists while the live process still has none --
	// typically because a secondary was added (or removed) without
	// restarting claude-burst. Checking disk here let this button report
	// "ok" and arm the overflow window while the next real request had
	// nowhere to go, failing on every retry until the gateway was
	// restarted. See router.Server.HasSecondary's doc comment.
	if !s.gateway.HasSecondary() {
		http.Error(w, "no secondary provider is configured on the running gateway, so there is nothing to fail over to "+
			"(if you just added or changed the secondary in config, restart the gateway first -- it only reads config at startup)", http.StatusBadRequest)
		return
	}
	cfg, err := config.Load()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if req.Minutes <= 0 {
		req.Minutes = 15
	}
	if req.Minutes > 720 {
		http.Error(w, "maximum is 720 minutes", http.StatusBadRequest)
		return
	}
	until := s.gateway.ForceOverflow(time.Duration(req.Minutes)*time.Minute, "forced from the admin UI")
	writeJSON(w, map[string]string{
		"ok": fmt.Sprintf("inference now goes to %s (%s) until %s. Clear it any time with Back to primary.",
			cfg.Secondary.Provider, cfg.Secondary.Model, until.Format("15:04:05")),
	})
}

type configRequest struct {
	SecondaryModel   string `json:"secondary_model"`
	FailoverStrategy string `json:"failover_strategy"`
	InterceptMode    string `json:"intercept_mode"`
}

func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	var req configRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request body", http.StatusBadRequest)
		return
	}
	cfg, err := config.Load()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	var changed []string
	if req.SecondaryModel != "" && req.SecondaryModel != cfg.Secondary.Model {
		if cfg.Secondary.Provider != "openai-compatible" {
			http.Error(w, "secondary model only applies to an openai-compatible secondary", http.StatusBadRequest)
			return
		}
		cfg.Secondary.Model = req.SecondaryModel
		changed = append(changed, "secondary model")
	}
	if req.FailoverStrategy != "" && req.FailoverStrategy != cfg.Primary.FailoverStrategy {
		switch req.FailoverStrategy {
		case "subscription-limit", "metered-failures", "subscription-limit+metered-failures", "none":
			cfg.Primary.FailoverStrategy = req.FailoverStrategy
			changed = append(changed, "failover strategy")
		default:
			http.Error(w, "unknown failover strategy", http.StatusBadRequest)
			return
		}
	}
	if req.InterceptMode != "" && req.InterceptMode != cfg.Intercept.Mode {
		cfg.Intercept.Mode = req.InterceptMode
		if err := cfg.ValidateIntercept(); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		cfg.ResolveRoutes()
		changed = append(changed, "intercept mode")
	}
	if len(changed) == 0 {
		writeJSON(w, map[string]string{"ok": "nothing to change"})
		return
	}
	if err := config.Save(cfg); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]string{
		"ok":      "saved: " + strings.Join(changed, ", "),
		"restart": "the gateway reads config at startup -- restart it for this to take effect",
	})
}

type secondaryRequest struct {
	Provider        string `json:"provider"`
	BaseURL         string `json:"base_url"`
	Model           string `json:"model"`
	KeychainService string `json:"keychain_service"`
	// APIKey is write-only and never echoed back. Empty means "leave the
	// stored key alone", so re-saving a base URL or model does not require
	// re-typing the secret -- and, more importantly, cannot wipe it.
	APIKey string `json:"api_key"`
}

type secondaryResponse struct {
	OK      string `json:"ok"`
	Restart string `json:"restart,omitempty"`
	// Warning carries the case that is saved-but-not-yet-working: a
	// provider written to config.json with no credential anywhere. That is
	// a legitimate thing to do (store the key later, or supply it through
	// the env var), so it is not an error -- but a form that answered a
	// plain "saved" here would send someone away believing failover was
	// armed, and they would find out at the exact moment the primary ran
	// out, which is the worst possible moment to discover it.
	Warning string `json:"warning,omitempty"`
}

// validateSecondaryBaseURL rejects the malformed base URL that url.Parse
// alone accepts silently -- a bare host, a relative path, an empty string.
// router.validateBaseURL does the same check at gateway startup, but that
// is hours too late and in the wrong place: the gateway would refuse to
// start, and the dashboard that wrote the bad value would be gone with it.
func validateSecondaryBaseURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("base URL: %w", err)
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("base URL must be an absolute http(s) URL, e.g. https://api.together.xyz/v1 (got %q)", raw)
	}
	return nil
}

// secondaryRoute turns a form submission into the RouteConfig to save, plus
// the Keychain service and env var that provider's credential lives under.
//
// Pure, and separate from handleSecondary, so the validation and defaulting
// rules -- which have to match cmd/claude-burst's `configure --secondary`
// and router.buildProvider exactly -- are testable without a Keychain, an
// HTTP server, or a config.json on disk.
func secondaryRoute(cfg config.Config, req secondaryRequest) (rc config.RouteConfig, service, envVar string, err error) {
	switch req.Provider {
	case config.ProviderNone, "":
		// The explicit marker, never the zero RouteConfig: see
		// config.ProviderNone for why an absent value silently resurrects
		// whatever the legacy flat fields hold.
		return config.RouteConfig{Provider: config.ProviderNone}, "", "", nil

	case "openai-compatible":
		base := strings.TrimRight(strings.TrimSpace(req.BaseURL), "/")
		model := strings.TrimSpace(req.Model)
		if base == "" || model == "" {
			return rc, "", "", fmt.Errorf("an openai-compatible secondary needs both a base URL and a model")
		}
		if err := validateSecondaryBaseURL(base); err != nil {
			return rc, "", "", err
		}
		ks := strings.TrimSpace(req.KeychainService)
		if ks == "" && cfg.Secondary.Provider == "openai-compatible" {
			// Carry the existing service forward only when the slot was
			// ALREADY openai-compatible. Inheriting it from any secondary
			// would hand this provider the previous one's credential name:
			// switching from bedrock with a blank field derived
			// "claude-burst-bedrock" -> $BEDROCK_API_KEY, so the form would
			// offer to store a GLM key under Bedrock's Keychain entry --
			// the same vendor collision cmd/claude-burst's keychainTarget
			// doc comment describes, and `security add-generic-password -U`
			// overwrites, so it would have destroyed the Bedrock key.
			ks = cfg.Secondary.KeychainService
		}
		if ks == "" {
			ks = "claude-burst-together" // matches buildProvider's default; not a vendor requirement
		}
		rc = config.RouteConfig{Provider: "openai-compatible", BaseURL: base, Model: model, KeychainService: ks}
		service, envVar = credentialNames(rc, cfg.KeychainService)
		return rc, service, envVar, nil

	case "bedrock":
		base := strings.TrimRight(strings.TrimSpace(req.BaseURL), "/")
		if base == "" {
			base = cfg.BedrockBaseURL
		}
		if err := validateSecondaryBaseURL(base); err != nil {
			return rc, "", "", err
		}
		ks := strings.TrimSpace(req.KeychainService)
		if ks == "" {
			ks = cfg.KeychainService
		}
		// ModelMap is required for bedrock (every Claude model needs an
		// entry), and the form does not edit it -- carry the configured one
		// through rather than writing a slot the gateway will reject.
		rc = config.RouteConfig{Provider: "bedrock", BaseURL: base, KeychainService: ks, ModelMap: cfg.ModelMap}
		service, envVar = credentialNames(rc, cfg.KeychainService)
		return rc, service, envVar, nil

	default:
		return rc, "", "", fmt.Errorf("unknown provider %q (want openai-compatible, bedrock, or none)", req.Provider)
	}
}

// handleSecondary writes the secondary provider slot -- provider, endpoint,
// model, and the API key that goes with it.
//
// The key is stored in the macOS Keychain, exactly where `claude-burst
// keychain-set` puts it and under the same service name the gateway derives
// at startup; config.json only ever holds that service name. Nothing here
// writes a secret to disk in the clear, and no response ever contains one.
func (s *Server) handleSecondary(w http.ResponseWriter, r *http.Request) {
	var req secondaryRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request body", http.StatusBadRequest)
		return
	}
	cfg, err := config.Load()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	rc, service, envVar, err := secondaryRoute(cfg, req)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// Store the key BEFORE saving config, and abort the save if it fails.
	// The two orderings fail very differently: a Keychain entry with no
	// config pointing at it is inert, while a config naming a provider
	// whose key was never stored is a secondary that looks configured on
	// this page and produces an auth error the first time the primary runs
	// out -- i.e. it defers the failure to the one moment it cannot be
	// tolerated. Same reasoning as the enable-ordering fix in the install
	// path: do the step that can fail harmlessly first.
	if key := strings.TrimSpace(req.APIKey); key != "" {
		if service == "" {
			http.Error(w, "provider "+rc.Provider+" takes no API key", http.StatusBadRequest)
			return
		}
		if err := s.storeKey(service, key); err != nil {
			http.Error(w, "could not store the API key in the Keychain, so nothing was saved: "+err.Error(), http.StatusInternalServerError)
			return
		}
	}

	cfg.Secondary = rc
	if rc.Provider == config.ProviderNone {
		// ResolveRoutes rebuilds a bedrock secondary from this legacy flat
		// field whenever the slot is empty, so leaving it set makes "none"
		// silently undo itself on the next load -- the same trap
		// `configure --secondary none` had to clear.
		cfg.BedrockBaseURL = ""
	}
	if rc.Provider == "bedrock" {
		cfg.BedrockBaseURL = rc.BaseURL
	}
	cfg.ResolveRoutes()
	if err := config.Save(cfg); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	resp := secondaryResponse{}
	if rc.Provider == config.ProviderNone {
		resp.OK = "secondary provider removed — overflow now has nowhere to go, and requests will stay on the primary"
	} else {
		resp.OK = fmt.Sprintf("saved: secondary is %s (%s)", rc.Provider, rc.BaseURL)
		if rc.Model != "" {
			resp.OK += " → " + rc.Model
		}
		if envVar != "" && !s.hasKey(service, envVar) {
			resp.Warning = fmt.Sprintf("no API key found for this provider: nothing is stored in the Keychain under %q and $%s is unset. "+
				"The secondary is configured but will fail the moment it is used — paste the key above and save again.", service, envVar)
		}
	}
	// The running gateway built its Provider set at startup (see
	// router.Server.HasSecondary) and has not seen any of this. Saying so
	// is not boilerplate: "Force -> secondary" checks the RUNNING gateway,
	// so without a restart it will correctly refuse to use what this form
	// just saved.
	resp.Restart = "the gateway reads config at startup — restart it (button below) before this secondary can actually serve anything"
	writeJSON(w, resp)
}

// handleRestart exits the process. launchd's KeepAlive brings it straight back
// with the current config, which is how a config change takes effect. If the
// gateway is not running under launchd, this simply stops it -- so the UI says
// as much before offering the button.
func (s *Server) handleRestart(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]string{"ok": "restarting; if managed by launchd it will be back in a moment"})
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	go func() {
		time.Sleep(250 * time.Millisecond)
		os.Exit(0)
	}()
}

// ListenAndServe runs the admin UI. It never returns nil early: a bind failure
// is reported to the caller rather than silently leaving no admin server.
func (s *Server) ListenAndServe(addr string) error {
	srv := &http.Server{
		Addr:              addr,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	return srv.ListenAndServe()
}

// Describe returns the URL to print at startup.
func Describe(addr string) string { return fmt.Sprintf("http://%s", addr) }
