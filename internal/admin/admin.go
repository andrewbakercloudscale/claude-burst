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
	"errors"
	"fmt"
	"github.com/andrewbakercloudscale/claude-burst/internal/codex"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/andrewbakercloudscale/claude-burst/internal/autocompact"
	"github.com/andrewbakercloudscale/claude-burst/internal/config"
	"github.com/andrewbakercloudscale/claude-burst/internal/keychain"
	"github.com/andrewbakercloudscale/claude-burst/internal/metrics"
	"github.com/andrewbakercloudscale/claude-burst/internal/router"
	"github.com/andrewbakercloudscale/claude-burst/internal/tlswatch"
	"github.com/andrewbakercloudscale/claude-burst/internal/touchid"
)

//go:embed admin.html
var indexHTML []byte

type Server struct {
	gateway    *router.Server
	modNotices modNotices
	// learned is Intelligent Compaction Mode's state, under learnMu.
	learnMu     sync.Mutex
	learned     autocompact.State
	metricsPath string
	version     string
	// extraHost is an optional friendly hostname accepted in addition to the
	// loopback names. See config.AdminHostname for the trade-off it makes.
	extraHost string
	// repos maps sessions to repositories for spend per repo.
	repos *repoResolver
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
	keyInfo  func(service, envVar string) keychain.Info
	loadKey  func(service, envVar string) (string, error)
	// authenticate gates the one endpoint that hands out a secret. A field
	// so tests can drive both answers: a gate that is only ever exercised
	// in its allow direction is not a gate.
	authenticate func(reason string) error

	// handshakes counts the gateway listener's TLS handshakes; nil in
	// base-url mode, where the listener speaks plain HTTP.
	handshakes *tlswatch.Watcher

	// codex is the Codex gateway, nil when its listener is not running.
	codex    *codex.Gateway
	codexErr string

	// trace holds the Send test message hops' outside dependencies, empty
	// in production (traceDeps fills the defaults); see trace.go.
	trace traceDeps
}

// SetHandshakes attaches the gateway listener's handshake counter, so
// /api/state can report whether clients are accepting its certificate.
func (s *Server) SetHandshakes(w *tlswatch.Watcher) { s.handshakes = w }

func New(gateway *router.Server, metricsPath, version, extraHost, rootHelper string) *Server {
	return &Server{gateway: gateway, metricsPath: metricsPath, version: version, repos: newRepoResolver(),
		extraHost: strings.ToLower(extraHost), rootHelper: rootHelper,
		storeKey: keychain.Store, keyInfo: keychain.Describe, loadKey: keychain.Load,
		authenticate: touchid.AuthenticateIsolated}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleIndex)
	mux.HandleFunc("/api/state", s.readOnly(s.handleState))
	mux.HandleFunc("/api/mod", s.readOnly(s.handleMod))
	mux.HandleFunc("/api/mod-status", s.readOnly(s.handleModStatus))
	mux.HandleFunc("/api/mod-action", s.mutating(s.handleModAction))
	mux.HandleFunc("/api/requests", s.readOnly(s.handleRequests))
	mux.HandleFunc("/api/codex", s.readOnly(s.handleCodex))
	mux.HandleFunc("/api/codex/insights", s.readOnly(s.handleCodexInsights))
	mux.HandleFunc("/api/codex/enable", s.mutating(s.handleCodexRoute(true)))
	mux.HandleFunc("/api/codex/disable", s.mutating(s.handleCodexRoute(false)))
	mux.HandleFunc("/api/codex/trace", s.mutating(s.handleCodexTrace))
	mux.HandleFunc("/api/codex/test-turn", s.mutating(s.handleCodexTestTurn))
	mux.HandleFunc("/api/codex/alert-test", s.mutating(s.handleCodexAlertTest))
	mux.HandleFunc("/api/codex/alert-test/status", s.readOnly(s.handleCodexAlertTestStatus))
	mux.HandleFunc("/api/responses", s.readOnly(s.handleResponses))
	mux.HandleFunc("/api/history", s.readOnly(s.handleHistory))
	mux.HandleFunc("/api/usage", s.readOnly(s.handleUsage))
	mux.HandleFunc("/api/test-connection", s.readOnly(s.handleTestConnection))
	mux.HandleFunc("/api/log", s.readOnly(s.handleLog))
	mux.HandleFunc("/api/audit", readOnly(handleAudit))
	mux.HandleFunc("/api/audit/context", readOnly(handleAuditContext))
	mux.HandleFunc("/audit.js", readOnly(handleAuditJS))
	mux.HandleFunc("/api/reset", s.mutating(s.handleReset))
	mux.HandleFunc("/api/force", s.mutating(s.handleForce))
	mux.HandleFunc("/api/downgrade", s.mutating(s.handleDowngrade))
	mux.HandleFunc("/api/config", s.mutating(s.handleConfig))
	mux.HandleFunc("/api/pruning", s.mutating(s.handlePruning))
	mux.HandleFunc("/api/compaction", s.mutating(s.handleCompaction))
	mux.HandleFunc("/api/automask", s.readOnly(s.handleAutomask))
	mux.HandleFunc("/api/inspect", s.readOnly(s.handleInspect))
	mux.HandleFunc("/api/inspect-item", s.readOnly(s.handleInspectItem))
	mux.HandleFunc("/api/inspect/remove", s.mutating(s.handleInspectRemove))
	mux.HandleFunc("/api/inspect/prune", s.mutating(s.handleInspectPrune))
	mux.HandleFunc("/api/inspect/refresh", s.mutating(s.handleInspectRefresh))
	mux.HandleFunc("/api/automask-save", s.mutating(s.handleAutomaskSave))
	mux.HandleFunc("/api/compaction/repo", s.mutating(s.handleCompactionRepo))
	mux.HandleFunc("/api/compaction/drop", s.mutating(s.handleCompactionDrop))
	mux.HandleFunc("/api/intelligent-compaction", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			s.mutating(s.handleIntelligent)(w, r)
			return
		}
		s.readOnly(s.handleIntelligent)(w, r)
	})
	// The usage panel's call, named as it is asked for.
	mux.HandleFunc("/api/GetAutoCompactionThreshold", s.readOnly(s.handleThreshold))
	mux.HandleFunc("/api/prompt-notice", s.mutating(s.handlePromptNotice))
	mux.HandleFunc("/api/prompt-notice-test", s.mutating(s.handlePromptNoticeTest))
	mux.HandleFunc("/api/coordination", s.readOnly(s.handleCoordination))
	mux.HandleFunc("/api/coordination-save", s.mutating(s.handleCoordination))
	mux.HandleFunc("/api/coordination-act", s.mutating(s.handleCoordinationAct))
	mux.HandleFunc("/api/handover", s.mutating(s.handleHandover))
	mux.HandleFunc("/api/handover-install", s.mutating(s.handleHandoverInstall))
	mux.HandleFunc("/api/handover-audit", s.readOnly(s.handleHandoverAudit))
	mux.HandleFunc("/api/handover-file", s.readOnly(s.handleHandoverFile))
	mux.HandleFunc("/api/handover-delete", s.mutating(s.handleHandoverDelete))
	mux.HandleFunc("/api/handover-reveal", s.mutating(s.handleHandoverReveal))
	mux.HandleFunc("/api/mac", s.readOnly(s.handleMac))
	mux.HandleFunc("/api/settings", s.readOnly(s.handleSettingsGet))
	mux.HandleFunc("/api/alert-test", s.mutating(s.handleAlertTest))
	mux.HandleFunc("/api/alert", s.mutating(s.handleAlertPublish))
	mux.HandleFunc("/api/alert-spend", s.mutating(s.handleAlertSpend))
	mux.HandleFunc("/api/statusline", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			s.readOnly(s.handleStatusLineGet)(w, r)
			return
		}
		s.mutating(s.handleStatusLinePost)(w, r)
	})
	mux.HandleFunc("/api/settings-save", s.mutating(s.handleSettingsPost))
	mux.HandleFunc("/api/hotspot-join", s.mutating(s.handleHotspotJoin))
	mux.HandleFunc("/api/hotspot-password", s.mutating(s.handleHotspotPassword))
	mux.HandleFunc("/api/keep-awake", s.mutating(s.handleKeepAwake))
	mux.HandleFunc("/api/panel-options", s.mutating(s.handlePanelOptions))
	mux.HandleFunc("/api/panel-install", s.mutating(s.handlePanelInstall))
	mux.HandleFunc("/api/finder", s.readOnly(s.handleFinder))
	mux.HandleFunc("/api/permissions", s.readOnly(s.handlePermissions))
	mux.HandleFunc("/api/permissions-check", s.mutating(s.handlePermissionsCheck))
	mux.HandleFunc("/api/signing-setup", s.mutating(s.handleSigningSetup))
	mux.HandleFunc("/api/open-privacy-settings", s.mutating(s.handleOpenPrivacySettings))
	mux.HandleFunc("/api/upgrade-status", s.readOnly(s.handleUpgradeStatus))
	mux.HandleFunc("/api/upgrade", s.mutating(s.handleUpgrade))
	mux.HandleFunc("/api/finder-install", s.mutating(s.handleFinderInstall))
	mux.HandleFunc("/docs/usage-panel.png", s.readOnly(s.handlePanelShot))
	mux.HandleFunc("/api/secondary", s.mutating(s.handleSecondary))
	mux.HandleFunc("/api/secondary-key", s.mutating(s.handleSecondaryKey))
	mux.HandleFunc("/api/test-secondary", s.mutating(s.handleTestSecondary))
	mux.HandleFunc("/api/trace", s.mutating(s.handleTrace))
	mux.HandleFunc("/api/revert", s.mutating(s.handleRevert))
	mux.HandleFunc("/api/restart", s.mutating(s.handleRestart))
	mux.HandleFunc("/api/install", s.mutating(s.handleInstall))
	mux.HandleFunc("/api/pf-heal-log", s.readOnly(s.handlePFHealLog))
	mux.HandleFunc("/api/pf-heal-install", s.mutating(s.handlePFHealInstall))
	mux.HandleFunc("/api/self-heal-log", s.readOnly(s.handleSelfHealLog))
	mux.HandleFunc("/api/self-heal-install", s.mutating(s.handleSelfHealInstall))
	return s.guard(mux)
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
	// Always fetch the page fresh: a tab kept from before a deploy ran the old
	// script, and buttons added since did nothing.
	w.Header().Set("Cache-Control", "no-store")
	// No inline-script CSP exemption needed: the page's script is inline but
	// this server is the only origin that can reach it.
	_, _ = w.Write(indexHTML)
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
		events = []metrics.Event{} // the page reads .length, so an empty log is [] and never null
	}
	writeJSON(w, events)
}

func (s *Server) handleResponses(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, s.gateway.RecentResponses())
}

// handleHistory backs the activity chart: the same events /api/requests
// returns one by one, bucketed per local day and split by slot.
//
// The bucketing happens here rather than in the page because the window is
// six figures of events on a busy machine -- 131,000 over thirty days on
// the machine this was written on -- and shipping all of them to a browser
// so it can produce thirty numbers is a lot of JSON to throw away. It reads
// the rotated backups too, which is why it can report more than the
// all-time card does; see metrics.Daily.
func (s *Server) handleHistory(w http.ResponseWriter, r *http.Request) {
	days := 14
	if v := r.URL.Query().Get("days"); v != "" {
		// Capped at 90: beyond that the bars are thinner than the gaps
		// between them, and the read cost is unbounded by anything except
		// how long the gateway has been running.
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 90 {
			days = n
		}
	}
	h, err := metrics.Daily(s.metricsPath, days)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if s.repos != nil {
		// Compaction's saving per session over the same days as the spend,
		// so the two columns describe one window.
		y, m, d := time.Now().AddDate(0, 0, -(days - 1)).Date()
		saved := map[string]float64{}
		if cs, err := metrics.CompactionStatsSince(s.metricsPath, time.Date(y, m, d, 0, 0, 0, 0, time.Local)); err == nil {
			for _, c := range cs.Sessions {
				saved[c.Session] += c.NetUSD
			}
		}
		h.Repos = s.repos.repoSpend(h.SessionUse, saved)
	}
	writeJSON(w, h)
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
			Detail: fmt.Sprintf("confirmed: %s currently resolves to THIS gateway (HTTP %d). Real Claude Code traffic is passing through it.", url, resp.StatusCode),
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
	writeJSON(w, map[string]string{"ok": "back to primary, the next request will use it"})
}

type forceRequest struct {
	Minutes int `json:"minutes"`
	// Model, when set, forces only that requested model to the secondary;
	// every other model stays on the primary. Empty forces everything.
	Model string `json:"model"`
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
	if req.Model != "" {
		until := s.gateway.ForceModelOverflow(req.Model, time.Duration(req.Minutes)*time.Minute, "forced from the admin UI")
		writeJSON(w, map[string]string{
			"ok": fmt.Sprintf("%s now goes to %s (%s) until %s; other models stay on the primary. Clear it any time with Force primary.",
				req.Model, cfg.Secondary.Provider, cfg.Secondary.Model, until.Format("15:04:05")),
		})
		return
	}
	until := s.gateway.ForceOverflow(time.Duration(req.Minutes)*time.Minute, "forced from the admin UI")
	writeJSON(w, map[string]string{
		"ok": fmt.Sprintf("inference now goes to %s (%s) until %s. Clear it any time with Force primary.",
			cfg.Secondary.Provider, cfg.Secondary.Model, until.Format("15:04:05")),
	})
}

type downgradeRequest struct {
	Enabled bool `json:"enabled"`
}

// handleDowngrade toggles the fallback chain on the RUNNING gateway. It is
// deliberately not a config.json edit: every other setting on this page says
// "restart for this to take effect", and the moment you want this one is
// mid-limit, when a restart is the last thing you want to do.
func (s *Server) handleDowngrade(w http.ResponseWriter, r *http.Request) {
	var req downgradeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request body", http.StatusBadRequest)
		return
	}
	s.gateway.SetDowngradeEnabled(req.Enabled)
	if !req.Enabled {
		writeJSON(w, map[string]string{"ok": "downgrade off, a refused model now goes straight to the secondary"})
		return
	}
	cfg, err := config.Load()
	if err != nil {
		writeJSON(w, map[string]string{"ok": "downgrade on"})
		return
	}
	if len(cfg.FallbackChain) == 0 {
		writeJSON(w, map[string]string{"ok": "downgrade on, but fallback_chain in config.json is empty, so there is nothing to fall back to yet"})
		return
	}
	var pairs []string
	for model, chain := range cfg.FallbackChain {
		if len(chain) > 0 {
			pairs = append(pairs, model+" → "+strings.Join(chain, " → "))
		}
	}
	sort.Strings(pairs)
	writeJSON(w, map[string]string{"ok": "downgrade on, " + strings.Join(pairs, ", ")})
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
	var changed []string
	if _, ok := updateConfig(w, func(cfg *config.Config) error {
		if req.SecondaryModel != "" && req.SecondaryModel != cfg.Secondary.Model {
			if cfg.Secondary.Provider != "openai-compatible" {
				return badRequest(errors.New("secondary model only applies to an openai-compatible secondary"))
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
				return badRequest(errors.New("unknown failover strategy"))
			}
		}
		if req.InterceptMode != "" && req.InterceptMode != cfg.Intercept.Mode {
			cfg.Intercept.Mode = req.InterceptMode
			if err := cfg.ValidateIntercept(); err != nil {
				return badRequest(err)
			}
			cfg.ResolveRoutes()
			changed = append(changed, "intercept mode")
		}
		if len(changed) == 0 {
			return errNothingToChange
		}
		return nil
	}); !ok {
		return
	}
	writeJSON(w, map[string]string{
		"ok":      "saved: " + strings.Join(changed, ", "),
		"restart": "the gateway reads config at startup -- restart it for this to take effect",
	})
}

// restartSelf asks this process to stop the way a deploy does: SIGTERM,
// which the gateway's drain handler (cmd/claude-burst/drain.go) answers by
// exiting once nothing is streaming, at most 50s later. A variable so tests
// can see it called without stopping the test binary.
var restartSelf = func() error { return syscall.Kill(os.Getpid(), syscall.SIGTERM) }

// handleRestart stops the process after its in-flight replies finish.
// launchd's KeepAlive brings it straight back with the current config, which
// is how a config change takes effect. If the gateway is not running under
// launchd, this simply stops it -- so the UI says as much before offering
// the button.
//
// Until 2026-10-03 this called os.Exit after 250ms, skipping the drain, so
// a Restart cut every Claude Code session mid-reply.
func (s *Server) handleRestart(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]string{"ok": "restarting once in-flight replies finish (at most 50s); if managed by launchd it will be back in a moment"})
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	go func() {
		time.Sleep(250 * time.Millisecond)
		if err := restartSelf(); err != nil {
			log.Printf("admin: restart: signalling self: %v", err)
		}
	}()
}

// ListenAndServe runs the admin UI. It never returns nil early: a bind failure
// is reported to the caller rather than silently leaving no admin server.
func (s *Server) ListenAndServe(addr string) error {
	refreshLaunchers()
	srv := &http.Server{
		Addr:              addr,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	return srv.ListenAndServe()
}

// Describe returns the URL to print at startup.
func Describe(addr string) string { return fmt.Sprintf("http://%s", addr) }
