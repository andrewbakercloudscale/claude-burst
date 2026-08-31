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
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/andrewbakercloudscale/claude-burst/internal/claudesettings"
	"github.com/andrewbakercloudscale/claude-burst/internal/config"
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
}

func New(gateway *router.Server, metricsPath, version, extraHost, rootHelper string) *Server {
	return &Server{gateway: gateway, metricsPath: metricsPath, version: version,
		extraHost: strings.ToLower(extraHost), rootHelper: rootHelper}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleIndex)
	mux.HandleFunc("/api/state", s.readOnly(s.handleState))
	mux.HandleFunc("/api/requests", s.readOnly(s.handleRequests))
	mux.HandleFunc("/api/responses", s.readOnly(s.handleResponses))
	mux.HandleFunc("/api/reset", s.mutating(s.handleReset))
	mux.HandleFunc("/api/force", s.mutating(s.handleForce))
	mux.HandleFunc("/api/config", s.mutating(s.handleConfig))
	mux.HandleFunc("/api/revert", s.mutating(s.handleRevert))
	mux.HandleFunc("/api/restart", s.mutating(s.handleRestart))
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
	Version   string          `json:"version"`
	Route     string          `json:"route"`
	Overflow  bool            `json:"overflow"`
	Until     string          `json:"until,omitempty"`
	Claim     string          `json:"claim,omitempty"`
	Reason    string          `json:"reason,omitempty"`
	Gateway   string          `json:"gateway"`
	Primary   routeInfo       `json:"primary"`
	Secondary routeInfo       `json:"secondary"`
	Intercept interceptInfo   `json:"intercept"`
	Totals    metrics.Summary `json:"totals"`
	Today     metrics.Summary `json:"today"`
}

type routeInfo struct {
	Provider string `json:"provider"`
	BaseURL  string `json:"base_url"`
	Model    string `json:"model,omitempty"`
	Strategy string `json:"strategy,omitempty"`
}

type interceptInfo struct {
	Mode        string `json:"mode"`
	Host        string `json:"host,omitempty"`
	CATrusted   bool   `json:"ca_trusted"`
	HostsEntry  bool   `json:"hosts_entry"`
	RemoteCtrl  bool   `json:"remote_control_expected"`
	SettingsURL string `json:"settings_base_url"`
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
		http.Error(w, err.Error(), http.StatusInternalServerError)
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
	if cfg.Intercept.Transparent() {
		if b, err := os.ReadFile(cfg.Intercept.CABundle); err == nil {
			ii.CATrusted = tlsca.HasBlock(string(b))
		}
		if h, err := os.ReadFile("/etc/hosts"); err == nil {
			ii.HostsEntry = strings.Contains(string(h), "# BEGIN claude-burst hosts")
		}
		ii.BailoutCmd = "sudo " + s.rootHelper + " remove"
	}
	// Claude Code disables Remote Control whenever ANTHROPIC_BASE_URL names a
	// host other than api.anthropic.com; an unset value is the default.
	ii.RemoteCtrl = settingsURL == ""

	resp := stateResponse{
		Version: s.version, Route: route, Overflow: overflow,
		Claim: st.LimitClaim, Reason: st.LastReason, Gateway: cfg.Listen,
		Primary:   routeInfo{cfg.Primary.Provider, cfg.Primary.BaseURL, cfg.Primary.Model, cfg.Primary.FailoverStrategy},
		Secondary: routeInfo{cfg.Secondary.Provider, cfg.Secondary.BaseURL, cfg.Secondary.Model, cfg.Secondary.FailoverStrategy},
		Intercept: ii, Totals: total, Today: today,
	}
	if overflow {
		resp.Until = time.Unix(st.OverflowUntil, 0).Format(time.RFC3339)
	}
	writeJSON(w, resp)
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

func (s *Server) handleReset(w http.ResponseWriter, r *http.Request) {
	s.gateway.ClearOverflow()
	writeJSON(w, map[string]string{"ok": "overflow cleared; the next request tries the primary"})
}

type forceRequest struct {
	Minutes int `json:"minutes"`
}

func (s *Server) handleForce(w http.ResponseWriter, r *http.Request) {
	var req forceRequest
	_ = json.NewDecoder(r.Body).Decode(&req)
	cfg, err := config.Load()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if cfg.Secondary.Provider == "" || cfg.Secondary.Provider == config.ProviderNone {
		http.Error(w, "no secondary provider is configured, so there is nothing to fail over to", http.StatusBadRequest)
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
		"ok": fmt.Sprintf("inference now goes to %s (%s) until %s. Clear it any time with Clear overflow.",
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

type revertResponse struct {
	Steps       []string `json:"steps"`
	NeedsRoot   bool     `json:"needs_root"`
	RootCommand string   `json:"root_command,omitempty"`
	Warning     string   `json:"warning,omitempty"`
}

// handleRevert puts Claude Code back on the stock Anthropic endpoint.
//
// It edits surgically rather than restoring a backup file. Restoring
// settings.json wholesale would silently roll back anything the user changed
// since the snapshot -- their model choice, a new hook -- which is a
// surprising thing for a button labelled "revert the proxy" to do.
func (s *Server) handleRevert(w http.ResponseWriter, r *http.Request) {
	cfg, err := config.Load()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	out := revertResponse{}

	p, err := claudesettings.Path()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	root, err := claudesettings.Read(p)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if claudesettings.ClearBaseURL(root, cfg.Listen) {
		if err := claudesettings.Write(p, root); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		out.Steps = append(out.Steps, "removed ANTHROPIC_BASE_URL from settings.json")
	} else {
		out.Steps = append(out.Steps, "settings.json already had no gateway URL")
	}

	if cfg.Intercept.CABundle != "" {
		if b, err := os.ReadFile(cfg.Intercept.CABundle); err == nil && tlsca.HasBlock(string(b)) {
			if err := tlsca.RemoveFromBundle(cfg.Intercept.CABundle); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			out.Steps = append(out.Steps, "removed the local CA from the trust bundle (other certificates untouched)")
		}
	}

	// The machine-wide part needs root, which this process deliberately does
	// not have. Say so loudly: while a hosts redirect points at a gateway the
	// user is trying to turn off, Anthropic is unreachable for EVERY process
	// on this Mac, not just Claude Code.
	if h, err := os.ReadFile("/etc/hosts"); err == nil && strings.Contains(string(h), "# BEGIN claude-burst hosts") {
		out.NeedsRoot = true
		out.RootCommand = "sudo scripts/transparent-root.sh remove"
		out.Warning = "An /etc/hosts redirect is still installed. Until you run the command above, traffic to " +
			cfg.Intercept.Host + " from every process on this Mac still goes to the gateway."
	}
	out.Steps = append(out.Steps, "restart Claude Code for this to take effect")
	writeJSON(w, out)
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
