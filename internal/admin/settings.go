package admin

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"reflect"
	"sort"
	"strings"

	"github.com/andrewbakercloudscale/claude-burst/internal/config"
	"github.com/andrewbakercloudscale/claude-burst/internal/hotspot"
	"github.com/andrewbakercloudscale/claude-burst/internal/keychain"
	"github.com/andrewbakercloudscale/claude-burst/internal/metrics"
)

// The Settings API: every config.json setting the dashboard edits that has
// no section of its own. Each field is validated here, never written raw,
// and the response says which saved values the running gateway has not
// picked up yet, so the page can offer the restart instead of leaving a
// change that silently does nothing.

type modelPrice struct {
	Model string             `json:"model"`
	Price *config.ModelPrice `json:"price"`
	// Default is the built-in price. Built-in prices are merged into every
	// load, so removing one only resets it to this.
	Default  *config.ModelPrice `json:"default,omitempty"`
	Requests int                `json:"requests"` // last 30 days
	USD      float64            `json:"usd"`
	Unpriced bool               `json:"unpriced"` // served with tokens and no price
}

type advancedSettings struct {
	ResetGraceSeconds            int   `json:"reset_grace_seconds"`
	UnknownResetSeconds          int   `json:"unknown_reset_seconds"`
	ResponseHeaderTimeoutSeconds int   `json:"response_header_timeout_seconds"`
	MaxRequestMB                 int64 `json:"max_request_mb"`
}

type hotspotView struct {
	config.HotspotConfig
	Known          []string `json:"known"`
	LidClosed      bool     `json:"lid_closed"`
	Online         bool     `json:"online"`
	PasswordStored bool     `json:"password_stored"`
	Events         []string `json:"events"`
}

type settingsView struct {
	Pricing         []modelPrice                 `json:"pricing"`
	FallbackChain   map[string][]string          `json:"fallback_chain"`
	MeteredFailover config.MeteredFailoverConfig `json:"metered_failover"`
	MeteredDefaults config.MeteredFailoverConfig `json:"metered_defaults"`
	Advanced        advancedSettings             `json:"advanced"`
	AdvancedDefault advancedSettings             `json:"advanced_defaults"`
	Listen          string                       `json:"listen"`
	AdminListen     string                       `json:"admin_listen"`
	AdminHostname   string                       `json:"admin_hostname"`
	PeerLog         bool                         `json:"peer_log"`
	Notify          config.NotifyConfig          `json:"notify"`
	Hotspot         hotspotView                  `json:"hotspot"`
	RestartNeeded   []string                     `json:"restart_needed"`
}

func advancedOf(c config.Config) advancedSettings {
	return advancedSettings{c.ResetGraceSeconds, c.UnknownResetSeconds, c.ResponseHeaderTimeoutSeconds, c.MaxRequestMB}
}

// restartFields are the settings the gateway reads once, at startup.
// Compaction, pruning, notifications and the hotspot are read live and are
// not listed.
func restartNeeded(running, disk config.Config) []string {
	var out []string
	check := func(name string, a, b any) {
		if !reflect.DeepEqual(a, b) {
			out = append(out, name)
		}
	}
	check("pricing", running.Pricing, disk.Pricing)
	check("fallback chain", running.FallbackChain, disk.FallbackChain)
	check("failover thresholds", running.MeteredFailover, disk.MeteredFailover)
	check("failover strategy", running.Primary.FailoverStrategy, disk.Primary.FailoverStrategy)
	check("intercept mode", running.Intercept.Mode, disk.Intercept.Mode)
	check("secondary", running.Secondary, disk.Secondary)
	check("timeouts and limits", advancedOf(running), advancedOf(disk))
	return out
}

func (s *Server) readSettings() (settingsView, error) {
	disk, err := config.Load()
	if err != nil {
		return settingsView{}, err
	}
	def := config.Default()
	// Load fills defaults in; Default() alone does not resolve these.
	defResolved := def
	defResolved.ResolveRoutes()

	seen := map[string]metrics.ModelUse{}
	if h, err := metrics.Daily(s.metricsPath, 30); err == nil {
		for _, m := range h.Models {
			seen[m.Model] = m
		}
	}
	names := map[string]bool{}
	for m := range disk.Pricing {
		names[m] = true
	}
	for m := range seen {
		names[m] = true
	}
	var pricing []modelPrice
	for m := range names {
		mp := modelPrice{Model: m, Requests: seen[m].Requests, USD: seen[m].USD, Unpriced: seen[m].Unpriced}
		if p, ok := disk.Pricing[m]; ok {
			p := p
			mp.Price = &p
		}
		if d, ok := def.Pricing[m]; ok {
			d := d
			mp.Default = &d
		}
		pricing = append(pricing, mp)
	}
	// Unpriced first, then most used, then by name.
	sort.Slice(pricing, func(i, j int) bool {
		a, b := pricing[i], pricing[j]
		if a.Unpriced != b.Unpriced {
			return a.Unpriced
		}
		if a.Requests != b.Requests {
			return a.Requests > b.Requests
		}
		return a.Model < b.Model
	})

	pwStored := false
	if _, err := keychain.Load(hotspot.KeychainService, "CLAUDE_BURST_HOTSPOT_PASSWORD"); err == nil {
		pwStored = true
	}
	hs := disk.Hotspot
	if hs.When == "" {
		hs.When = config.HotspotLidClosed
	}
	running := s.gateway.StartupConfig()
	return settingsView{
		Pricing:         pricing,
		FallbackChain:   disk.FallbackChain,
		MeteredFailover: disk.MeteredFailover,
		MeteredDefaults: config.MeteredFailoverConfig{WindowSeconds: 60, MinFailures: 3, TransportErrorMinFailures: 1},
		Advanced:        advancedOf(disk),
		AdvancedDefault: advancedSettings{def.ResetGraceSeconds, def.UnknownResetSeconds, 60, def.MaxRequestMB},
		Listen:          disk.Listen,
		AdminListen:     disk.AdminListen,
		AdminHostname:   disk.AdminHostname,
		PeerLog:         os.Getenv("CLAUDE_BURST_LOG_TLS_PEERS") == "1",
		Notify:          disk.Notify,
		Hotspot: hotspotView{
			HotspotConfig:  hs,
			Known:          hotspot.KnownNetworks(),
			LidClosed:      hotspot.LidClosed(),
			Online:         hotspot.Online(),
			PasswordStored: pwStored,
			Events:         hotspot.RecentEvents(12),
		},
		RestartNeeded: restartNeeded(running, disk),
	}, nil
}

func (s *Server) handleSettingsGet(w http.ResponseWriter, r *http.Request) {
	v, err := s.readSettings()
	if err != nil {
		http.Error(w, "config.json does not parse: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, v)
}

// settingsUpdate is a partial update: only the parts present change.
type settingsUpdate struct {
	// Pricing sets a model's price; null removes it.
	Pricing         map[string]*config.ModelPrice `json:"pricing"`
	FallbackChain   *map[string][]string          `json:"fallback_chain"`
	MeteredFailover *config.MeteredFailoverConfig `json:"metered_failover"`
	Advanced        *advancedSettings             `json:"advanced"`
	Notify          *config.NotifyConfig          `json:"notify"`
	Hotspot         *config.HotspotConfig         `json:"hotspot"`
	// HotspotPassword stores (non-empty) the hotspot's password in the
	// login Keychain; "-" deletes it. Never echoed back.
	HotspotPassword string `json:"hotspot_password"`
}

func between(name string, v, lo, hi int64) error {
	if v < lo || v > hi {
		return fmt.Errorf("%s must be between %d and %d", name, lo, hi)
	}
	return nil
}

func validatePrice(m string, p config.ModelPrice) error {
	for _, f := range []float64{p.InputPerMTok, p.OutputPerMTok, p.CacheReadPerMTok, p.CacheWritePerMTok} {
		if f < 0 || f > 1000 {
			return fmt.Errorf("%s: prices are per million tokens, between 0 and 1000", m)
		}
	}
	if p.InputPerMTok == 0 && p.OutputPerMTok == 0 {
		return fmt.Errorf("%s: set at least the input and output price", m)
	}
	return nil
}

func validModelName(m string) bool {
	return m != "" && len(m) <= 200 && !strings.ContainsAny(m, " \t\n\"'")
}

func (s *Server) handleSettingsPost(w http.ResponseWriter, r *http.Request) {
	var u settingsUpdate
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&u); err != nil {
		http.Error(w, "bad request body: "+err.Error(), http.StatusBadRequest)
		return
	}
	cfg, err := config.Load()
	if err != nil {
		http.Error(w, "config.json does not parse, fix it before changing this: "+err.Error(), http.StatusInternalServerError)
		return
	}
	bad := func(err error) { http.Error(w, err.Error(), http.StatusBadRequest) }
	var changed []string

	if u.Pricing != nil {
		if cfg.Pricing == nil {
			cfg.Pricing = map[string]config.ModelPrice{}
		}
		for m, p := range u.Pricing {
			if !validModelName(m) {
				bad(fmt.Errorf("invalid model name %q", m))
				return
			}
			if p == nil {
				delete(cfg.Pricing, m)
				continue
			}
			if err := validatePrice(m, *p); err != nil {
				bad(err)
				return
			}
			cfg.Pricing[m] = *p
		}
		changed = append(changed, "pricing")
	}
	if u.FallbackChain != nil {
		chain := map[string][]string{}
		for m, rungs := range *u.FallbackChain {
			if !validModelName(m) {
				bad(fmt.Errorf("invalid model name %q", m))
				return
			}
			var clean []string
			for _, r := range rungs {
				r = strings.TrimSpace(r)
				if r == "" {
					continue
				}
				if !validModelName(r) || r == m {
					bad(fmt.Errorf("fallback for %s: %q is not a usable model", m, r))
					return
				}
				clean = append(clean, r)
			}
			if len(clean) > 0 {
				chain[m] = clean
			}
		}
		cfg.FallbackChain = chain
		changed = append(changed, "fallback chain")
	}
	if mf := u.MeteredFailover; mf != nil {
		for _, e := range []error{
			between("window", int64(mf.WindowSeconds), 10, 3600),
			between("failures", int64(mf.MinFailures), 1, 100),
			between("connection failures", int64(mf.TransportErrorMinFailures), 1, 100),
		} {
			if e != nil {
				bad(e)
				return
			}
		}
		cfg.MeteredFailover = *mf
		changed = append(changed, "failover thresholds")
	}
	if a := u.Advanced; a != nil {
		for _, e := range []error{
			between("reset grace", int64(a.ResetGraceSeconds), 0, 600),
			between("unknown reset", int64(a.UnknownResetSeconds), 30, 86400),
			between("response header timeout", int64(a.ResponseHeaderTimeoutSeconds), 10, 600),
			between("max request size", a.MaxRequestMB, 8, 1024),
		} {
			if e != nil {
				bad(e)
				return
			}
		}
		cfg.ResetGraceSeconds, cfg.UnknownResetSeconds = a.ResetGraceSeconds, a.UnknownResetSeconds
		cfg.ResponseHeaderTimeoutSeconds, cfg.MaxRequestMB = a.ResponseHeaderTimeoutSeconds, a.MaxRequestMB
		changed = append(changed, "timeouts and limits")
	}
	if u.Notify != nil {
		cfg.Notify = *u.Notify
		changed = append(changed, "notifications")
	}
	if h := u.Hotspot; h != nil {
		if h.When != "" && h.When != config.HotspotLidClosed && h.When != config.HotspotAlways {
			bad(fmt.Errorf("when must be %s or %s", config.HotspotLidClosed, config.HotspotAlways))
			return
		}
		if len(h.SSID) > 64 || strings.ContainsAny(h.SSID, "\n\r\x00") {
			bad(fmt.Errorf("that is not a Wi-Fi network name"))
			return
		}
		// 0 means the default; anything else must be in range.
		for _, c := range []struct {
			name      string
			v, lo, hi int
			unit      string
		}{
			{"check every", h.CheckSeconds, config.MinHotspotCheckSeconds, config.MaxHotspotCheckSeconds, "seconds"},
			{"failed checks", h.OfflineChecks, 1, config.MaxHotspotOfflineChecks, ""},
			{"gap between tries", h.RetrySeconds, config.MinHotspotRetrySeconds, config.MaxHotspotRetrySeconds, "seconds"},
			{"keep trying for", h.GiveUpMinutes, 1, config.MaxHotspotGiveUpMinutes, "minutes"},
		} {
			if c.v != 0 && (c.v < c.lo || c.v > c.hi) {
				bad(fmt.Errorf("%s must be between %d and %d %s", c.name, c.lo, c.hi, c.unit))
				return
			}
		}
		// The password is required: without it macOS refuses a join made
		// by a background process (error -3900), every time.
		typed := u.HotspotPassword != "" && u.HotspotPassword != "-"
		if h.SSID != "" && !typed && (u.HotspotPassword == "-" || !hotspotPasswordStored()) {
			bad(fmt.Errorf("type the hotspot's password: without it macOS refuses the join"))
			return
		}
		cfg.Hotspot = *h
		changed = append(changed, "hotspot")
	}
	if u.HotspotPassword == "-" && u.Hotspot == nil && cfg.Hotspot.SSID != "" {
		bad(fmt.Errorf("the password is required while a hotspot is chosen: set Network to Off first"))
		return
	}
	switch u.HotspotPassword {
	case "":
	case "-":
		_ = keychain.Delete(hotspot.KeychainService)
		changed = append(changed, "hotspot password removed")
	default:
		if err := keychain.Store(hotspot.KeychainService, u.HotspotPassword); err != nil {
			http.Error(w, "storing the password in the Keychain: "+err.Error(), http.StatusInternalServerError)
			return
		}
		changed = append(changed, "hotspot password stored in the Keychain")
	}
	if len(changed) == 0 {
		writeJSON(w, map[string]string{"ok": "nothing to change"})
		return
	}
	if err := config.Save(cfg); err != nil {
		http.Error(w, "saving config.json: "+err.Error(), http.StatusInternalServerError)
		return
	}
	resp := map[string]any{"ok": "saved: " + strings.Join(changed, ", ")}
	if rn := restartNeeded(s.gateway.StartupConfig(), cfg); len(rn) > 0 {
		resp["restart_needed"] = rn
	}
	writeJSON(w, resp)
}

// handleHotspotJoin joins a network now, as a test: the one in the body if
// given (so a choice can be tried before saving), else the saved one. Only a
// network this Mac already knows is accepted, so the name reaching
// networksetup is one macOS itself listed.
func (s *Server) handleHotspotJoin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SSID string `json:"ssid"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	ssid := req.SSID
	if ssid == "" {
		if cfg, err := config.Load(); err == nil {
			ssid = cfg.Hotspot.SSID
		}
	}
	if ssid == "" {
		http.Error(w, "choose a network first", http.StatusBadRequest)
		return
	}
	known := false
	for _, n := range hotspot.KnownNetworks() {
		known = known || n == ssid
	}
	if !known {
		http.Error(w, fmt.Sprintf("%q is not a network this Mac has joined before; join it once from the Wi-Fi menu first", ssid), http.StatusBadRequest)
		return
	}
	if !hotspotPasswordStored() {
		http.Error(w, "type the hotspot's password first: without it macOS refuses the join (error -3900)", http.StatusBadRequest)
		return
	}
	// One JSON object per line as each stage starts and ends, then the
	// outcome, so the page can show every attempt while the next one runs.
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Cache-Control", "no-store")
	flusher, _ := w.(http.Flusher)
	enc := json.NewEncoder(w)
	steps := hotspot.JoinSteps(ssid, func(st hotspot.Step) {
		_ = enc.Encode(map[string]any{"step": st})
		if flusher != nil {
			flusher.Flush()
		}
	})
	ok := len(steps) > 0
	for _, st := range steps {
		ok = ok && st.OK
	}
	// A failed attempt followed by a working one is still a pass.
	if n := len(steps); n > 0 && steps[n-1].OK && steps[n-1].Name == "Internet through it" {
		ok = true
	}
	_ = enc.Encode(map[string]any{"done": true, "ok": ok, "ssid": ssid})
}

// hotspotPasswordStored is a variable so tests never read the real Keychain.
var hotspotPasswordStored = func() bool {
	_, err := keychain.Load(hotspot.KeychainService, "CLAUDE_BURST_HOTSPOT_PASSWORD")
	return err == nil
}

// handleHotspotPassword hands the stored hotspot password back, after Touch
// ID or the login password, so it can be checked by eye.
func (s *Server) handleHotspotPassword(w http.ResponseWriter, r *http.Request) {
	if err := s.authenticate("show the hotspot password stored for Claude Burst"); err != nil {
		http.Error(w, "authentication was not completed, so the password was not read: "+err.Error(), http.StatusForbidden)
		return
	}
	v, err := keychain.Load(hotspot.KeychainService, "CLAUDE_BURST_HOTSPOT_PASSWORD")
	if err != nil {
		http.Error(w, "no hotspot password is stored", http.StatusNotFound)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, map[string]string{"value": v})
}
