package admin

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/andrewbakercloudscale/claude-burst/internal/config"
	"github.com/andrewbakercloudscale/claude-burst/internal/router"
)

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

// hostOf is a base URL's host, lower case, "" when it has none.
func hostOf(base string) string {
	u, err := url.Parse(base)
	if err != nil {
		return ""
	}
	return strings.ToLower(u.Host)
}

// movesStoredKey reports a save that points an endpoint already in use at a
// different host, so a key stored for the first would go to the second.
func movesStoredKey(from, to string) bool {
	return hostOf(from) != "" && hostOf(to) != "" && hostOf(from) != hostOf(to)
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
	// Showing the stored key needs Touch ID, so sending it somewhere new
	// must too: without this, a new base URL and a test request hand the
	// key to a host of the caller's choosing. Asked before the config lock
	// is taken, since the sheet waits for a person.
	if cur, err := config.Load(); err == nil && strings.TrimSpace(req.APIKey) == "" {
		if next, service, envVar, err := secondaryRoute(cur, req); err == nil && service != "" &&
			movesStoredKey(cur.Secondary.BaseURL, next.BaseURL) && s.keyInfo(service, envVar).Present {
			if err := s.authenticate("send the API key stored for Claude Burst to " + hostOf(next.BaseURL)); err != nil {
				http.Error(w, "authentication was not completed, so the secondary was not changed: "+err.Error(), http.StatusForbidden)
				return
			}
		}
	}
	var (
		rc              config.RouteConfig
		service, envVar string
	)
	if _, ok := updateConfig(w, func(cfg *config.Config) error {
		var err error
		rc, service, envVar, err = secondaryRoute(*cfg, req)
		if err != nil {
			return badRequest(err)
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
				return badRequest(errors.New("provider " + rc.Provider + " takes no API key"))
			}
			if err := s.storeKey(service, key); err != nil {
				return serverError("could not store the API key in the Keychain, so nothing was saved: " + err.Error())
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
		return nil
	}); !ok {
		return
	}

	resp := secondaryResponse{}
	if rc.Provider == config.ProviderNone {
		resp.OK = "secondary provider removed, overflow now has nowhere to go, and requests will stay on the primary"
	} else {
		resp.OK = fmt.Sprintf("saved: secondary is %s (%s)", rc.Provider, rc.BaseURL)
		if rc.Model != "" {
			resp.OK += " → " + rc.Model
		}
		if envVar != "" && !s.keyInfo(service, envVar).Present {
			resp.Warning = fmt.Sprintf("no API key found for this provider: nothing is stored in the Keychain under %q and $%s is unset. "+
				"The secondary is configured but will fail the moment it is used, paste the key above and save again.", service, envVar)
		}
	}
	// The running gateway built its Provider set at startup (see
	// router.Server.HasSecondary) and has not seen any of this. Saying so
	// is not boilerplate: "Force -> secondary" checks the RUNNING gateway,
	// so without a restart it will correctly refuse to use what this form
	// just saved.
	resp.Restart = "the gateway reads config at startup, restart it (button below) before this secondary can actually serve anything"
	writeJSON(w, resp)
}

type secondaryKeyResponse struct {
	Value string `json:"value"`
	// Source is "keychain" or "environment" -- the same distinction
	// /api/state draws, and it matters here too: an env-var value is not
	// what a saved configuration is relying on, and editing the box would
	// write a Keychain entry that the env var then keeps overriding.
	Source string `json:"source"`
}

// handleSecondaryKey returns the secondary's API key in the clear, for the
// dashboard's Show button.
//
// This is the one endpoint that hands out a secret, so it is deliberately
// POST-and-mutation-guarded rather than a read-only GET, despite reading
// nothing. The custom header forces a CORS preflight that this server never
// answers, so a cross-origin page cannot reach it even if it gets past the
// loopback Host check -- whereas a plain GET would need only the Host check
// to hold. It is not a mutation; it is guarded like one because the cost of
// being wrong is a leaked credential rather than a changed setting.
//
// Nothing here logs the value, and the response is marked no-store so it
// does not settle into a disk cache.
func (s *Server) handleSecondaryKey(w http.ResponseWriter, r *http.Request) {
	cfg, err := config.Load()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	service, envVar := credentialNames(cfg.Secondary, cfg.KeychainService)
	if envVar == "" {
		http.Error(w, "the configured secondary ("+cfg.Secondary.Provider+") has no API key of its own", http.StatusBadRequest)
		return
	}
	// Touch ID (or the login password) before the key is even read, not
	// after -- a denied prompt must not have fetched the secret at all.
	//
	// Deliberately fails CLOSED: no prompt, no key. That is only safe
	// because the sheet accepts the login password as well as a
	// fingerprint, so a Mac with no sensor, or a user who has enrolled no
	// finger, still has a way through. Gating on biometrics alone would
	// have locked someone out of their own credential.
	//
	// The gateway's own read of this key, on the failover path, does NOT
	// come through here. Failover happens unattended; a prompt there would
	// simply time out and take the secondary down with it.
	if err := s.authenticate("reveal the " + cfg.Secondary.Provider + " API key stored for Claude Burst"); err != nil {
		http.Error(w, "authentication was not completed, so the key was not read: "+err.Error(), http.StatusForbidden)
		return
	}

	value, err := s.loadKey(service, envVar)
	if err != nil {
		// keychain.Load's error names the service and env var it looked in
		// and never contains the value.
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	source := "keychain"
	if os.Getenv(envVar) != "" {
		source = "environment"
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, secondaryKeyResponse{Value: value, Source: source})
}

type testSecondaryResponse struct {
	OK     bool               `json:"ok"`
	Error  string             `json:"error,omitempty"`
	Result router.ProbeResult `json:"result"`
}

// handleTestSecondary sends one trivial completion through the secondary
// provider and reports what came back.
//
// POST, and mutation-guarded, because it is not a read: it spends real money
// on a metered provider every time it is pressed. Everything it can tell you
// -- reachable, authenticated, model translated, response well-formed --
// only becomes true when a request actually goes down the wire, which is why
// the dashboard could not answer it from config.
//
// Deliberately NOT a 500 when the probe fails. The failure detail IS the
// answer here, and an HTTP error status would leave the UI showing a bare
// status line instead of the upstream's own message, which is the one thing
// worth reading when a key is wrong.
func (s *Server) handleTestSecondary(w http.ResponseWriter, r *http.Request) {
	// Bounded well inside any browser timeout, so a hung provider produces a
	// real answer on the page rather than a spinner that never resolves.
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()

	res, err := s.gateway.ProbeSecondary(ctx)
	if err != nil {
		writeJSON(w, testSecondaryResponse{OK: false, Error: err.Error(), Result: res})
		return
	}
	writeJSON(w, testSecondaryResponse{OK: true, Result: res})
}
