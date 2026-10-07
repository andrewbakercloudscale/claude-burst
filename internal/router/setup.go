package router

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"github.com/andrewbakercloudscale/claude-burst/internal/ctxview"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/andrewbakercloudscale/claude-burst/internal/config"
	"github.com/andrewbakercloudscale/claude-burst/internal/metrics"
	"github.com/andrewbakercloudscale/claude-burst/internal/repo"
	"github.com/andrewbakercloudscale/claude-burst/internal/tlsca"
)

func New(cfg config.Config, statePath, metricsPath string, logger *log.Logger) (*Server, error) {
	cfg.ResolveRoutes()

	primary, err := buildProvider(cfg.Primary, cfg.KeychainService, cfg.ModelMap, logger)
	if err != nil {
		return nil, fmt.Errorf("primary provider: %w", err)
	}
	primaryDetector, err := buildDetector(cfg.Primary.FailoverStrategy, cfg.MeteredFailover, logger.Printf)
	if err != nil {
		return nil, fmt.Errorf("primary failover strategy: %w", err)
	}

	var secondary Provider
	if cfg.Secondary.Provider != "" && cfg.Secondary.Provider != config.ProviderNone {
		secondary, err = buildProvider(cfg.Secondary, cfg.KeychainService, cfg.ModelMap, logger)
		if err != nil {
			return nil, fmt.Errorf("secondary provider: %w", err)
		}
		// Set here rather than in buildProvider: pruning belongs to the
		// secondary SLOT, and an openai-compatible primary must never be
		// pruned. See prune.go.
		if op, ok := secondary.(*OpenAICompatibleProvider); ok {
			op.setPruning(cfg.SecondaryPruning)
		}
	}

	timeout := time.Duration(cfg.ResponseHeaderTimeoutSeconds) * time.Second

	// One resolver shared by both transports, so a lookup either one makes
	// warms the cache for the other.
	var resolver *interceptResolver
	if cfg.Intercept.Transparent() {
		resolver = newInterceptResolver(
			cfg.Intercept.Host, cfg.Intercept.ResolverDoH, cfg.Intercept.UpstreamAddr,
		).withFallbacks(resolverCachePath(statePath))
	}
	newTransport := func(responseHeaderTimeout time.Duration) *http.Transport {
		// Cloned from the default, not a bare Transport: a bare one ignores
		// HTTPS_PROXY, and on a corporate network the egress proxy is often
		// the only way out. RootCAs adds the corporate CAs Claude Code
		// trusts through NODE_EXTRA_CA_CERTS (an internal Portkey gateway is
		// signed by one), which Go would otherwise never read.
		t := http.DefaultTransport.(*http.Transport).Clone()
		t.Proxy = upstreamProxy
		t.ResponseHeaderTimeout = responseHeaderTimeout
		t.TLSClientConfig = &tls.Config{RootCAs: upstreamRoots(cfg.Intercept.CABundle, logger)}
		if resolver != nil {
			// /etc/hosts now points the upstream hostname at this process, so
			// the standard resolver would make the gateway call itself. See
			// interceptResolver.
			t.DialContext = resolver.DialContext
		}
		return t
	}

	s := &Server{
		compaction:      newCompactor(cfg.PrimaryCompaction, compactionStatePath(statePath), logger),
		automask:        newMasker(cfg.Automask),
		inspect:         newInspectStore(),
		removals:        ctxview.Shared(removalsPath(statePath)),
		repos:           repo.New(),
		cfg:             cfg,
		primary:         primary,
		primaryDetector: primaryDetector,
		secondary:       secondary,
		client:          &http.Client{Timeout: 0, Transport: newTransport(timeout), CheckRedirect: neverFollow},
		probe:           probeNetwork,
		// Control-plane traffic (notably Claude Code's Remote Control, which
		// registers and then long-polls for work) can legitimately hold a
		// connection open for minutes before sending any response header.
		// ResponseHeaderTimeout would kill exactly that, presenting as Remote
		// Control dropping every ~60s and looking like a network fault, so
		// non-inference paths get a client without it. Inference keeps the
		// bounded client, where the timeout is load-bearing for the
		// metered-failures failover strategy.
		passthroughClient: &http.Client{Timeout: 0, Transport: newTransport(0), CheckRedirect: neverFollow},
		metrics:           metrics.New(metricsPath),
		statePath:         statePath,
		logger:            logger,
		resolver:          resolver,
	}
	s.loadState()
	return s, nil
}

// buildProvider constructs the Provider for one configured route slot.
func buildProvider(rc config.RouteConfig, defaultKeychainService string, defaultModelMap map[string]string, logger *log.Logger) (Provider, error) {
	switch rc.Provider {
	case "", "oauth-passthrough":
		base, err := validateBaseURL("oauth-passthrough", rc.BaseURL)
		if err != nil {
			return nil, err
		}
		return NewPassthroughProvider("anthropic", base), nil
	case "anthropic-api-key":
		base, err := validateBaseURL("anthropic-api-key", rc.BaseURL)
		if err != nil {
			return nil, err
		}
		return NewPassthroughProvider("anthropic-api-key", base), nil
	case "bedrock":
		base, err := validateBaseURL("bedrock", rc.BaseURL)
		if err != nil {
			return nil, err
		}
		ks := rc.KeychainService
		if ks == "" {
			ks = defaultKeychainService
		}
		mm := rc.ModelMap
		if mm == nil {
			mm = defaultModelMap
		}
		return NewBedrockProvider(base, mm, ks), nil
	case "openai-compatible":
		base, err := validateBaseURL("openai-compatible", rc.BaseURL)
		if err != nil {
			return nil, err
		}
		if rc.Model == "" {
			return nil, fmt.Errorf("openai-compatible provider requires a configured model")
		}
		ks := rc.KeychainService
		if ks == "" {
			ks = "claude-burst-together"
		}
		label, envVar := OpenAICompatibleIdentity(ks)
		p := NewOpenAICompatibleProvider(label, base, rc.Model, rc.ModelMap, ks, envVar)
		p.logger = logger
		return p, nil
	default:
		return nil, fmt.Errorf("unknown provider %q", rc.Provider)
	}
}

// EnvVarForProvider derives the environment-variable name claude-burst
// expects to hold an openai-compatible secondary's API key, from a short
// vendor label -- "together" -> "TOGETHER_API_KEY", "openrouter" ->
// "OPENROUTER_API_KEY". Exported because cmd/claude-burst's keychain-set
// needs this same half of the convention (label -> env var) to know which
// env var to read for `--provider <label>`; OpenAICompatibleIdentity below
// covers the other half (keychain service -> label).
func EnvVarForProvider(label string) string {
	return strings.ToUpper(strings.ReplaceAll(label, "-", "_")) + "_API_KEY"
}

// OpenAICompatibleIdentity derives the short vendor label and API-key env
// var for an openai-compatible secondary from its keychain service name --
// e.g. "claude-burst-together" -> ("together", "TOGETHER_API_KEY"),
// "claude-burst-openrouter" -> ("openrouter", "OPENROUTER_API_KEY"). No
// vendor is hardcoded here: a config that only ever set (or defaulted to)
// "claude-burst-together" gets back exactly the identity it had before.
//
// Exported for the same reason as EnvVarForProvider: buildProvider is no
// longer the only caller. The admin UI's secondary-provider form has to
// name the env var it would read for a given keychain service, and a second
// copy of this convention there is exactly how the two would drift apart --
// the form would offer to store a key under a name the gateway never looks
// for, and nothing would say so until a failover found no credentials.
func OpenAICompatibleIdentity(keychainService string) (label, envVar string) {
	label = strings.TrimPrefix(keychainService, "claude-burst-")
	if label == "" {
		label = "openai-compatible"
	}
	return label, EnvVarForProvider(label)
}

// validateBaseURL rejects the kind of malformed/incomplete base_url that
// url.Parse alone lets through silently (e.g. "", a bare host with no
// scheme, or a relative path) -- turning what would otherwise be a
// confusing runtime request failure into a clear startup error naming which
// route slot is misconfigured.
func validateBaseURL(provider, raw string) (*url.URL, error) {
	base, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("%s base_url: %w", provider, err)
	}
	if (base.Scheme != "http" && base.Scheme != "https") || base.Host == "" {
		return nil, fmt.Errorf("%s base_url must be an absolute http(s) URL, got %q", provider, raw)
	}
	return base, nil
}

// buildDetector constructs the FailoverDetector for a route slot's
// configured strategy. logf receives the detector's own lines (a failure held
// back on a phone hotspot).
func buildDetector(strategy string, mf config.MeteredFailoverConfig, logf func(string, ...any)) (FailoverDetector, error) {
	switch strategy {
	case "", "subscription-limit":
		return subscriptionLimitDetector{}, nil
	case "metered-failures":
		return newMeteredFailureDetector(mf.WindowSeconds, mf.MinFailures, mf.TransportErrorMinFailures).
			withHotspot(mf.HotspotMultiplier(), logf), nil
	case "subscription-limit+metered-failures":
		d := newCombinedDetector(mf)
		d.metered.withHotspot(mf.HotspotMultiplier(), logf)
		return d, nil
	case "none":
		return noFailoverDetector{}, nil
	default:
		return nil, fmt.Errorf("unknown failover_strategy %q", strategy)
	}
}

// HasSecondary reports whether THIS running gateway process actually has a
// secondary Provider built, as opposed to what config.json on disk currently
// says. The two can disagree: config is only read at process start (see
// handleConfig's own "restart it for this to take effect"), so a secondary
// added or removed on disk after startup, without a restart, does not change
// s.secondary. Callers that need to know whether failing over to the
// secondary will actually do anything -- notably admin's "Force -> secondary"
// button -- must check this, not a freshly re-loaded config file.
//
// A secondary with no credential counts as none: routing to it only turns
// Anthropic's answer into a 503.
func (s *Server) HasSecondary() bool {
	return s.secondaryReady()
}

// secondaryReadyTTL bounds how stale the credential check may be: a key
// added with keychain-set is picked up within this, without a restart.
const secondaryReadyTTL = time.Minute

// secondaryReady is whether there is a secondary that can actually serve:
// built, and with its credential loadable.
//
// Someone on one plan only (Claude Enterprise, or any single subscription)
// has no secondary, and often not even a deliberate "none": a config with
// no secondary block resolves to a Bedrock secondary with no key. Before
// this, a subscription limit there was "failed over" to that Bedrock, which
// answered 503, and the window it armed sent every request after it to the
// same dead end until the limit reset. With nowhere to go, Anthropic's own
// response goes back to Claude Code unchanged, and Claude Code handles it
// the way it does without Burst.
func (s *Server) secondaryReady() bool {
	if s.secondary == nil {
		return false
	}
	c, ok := s.secondary.(credentialChecker)
	if !ok {
		return true
	}
	s.readyMu.Lock()
	defer s.readyMu.Unlock()
	if !s.readyAt.IsZero() && time.Since(s.readyAt) < secondaryReadyTTL {
		return s.readyErr == nil
	}
	err := c.CredentialReady()
	if err != nil && (s.readyAt.IsZero() || s.readyErr == nil) {
		s.logger.Printf("secondary route=%s has no usable credential (%v): treating it as absent, so Anthropic's own limits pass through to Claude Code", s.secondary.Name(), err)
	}
	s.readyAt, s.readyErr = time.Now(), err
	s.alertSecondaryKey(err)
	return err == nil
}

// upstreamProxy is http.ProxyFromEnvironment, except that a proxy on this
// Mac's loopback is ignored: that is Claude Burst's own address in older
// installs, and using it would send the gateway's requests back to itself.
func upstreamProxy(req *http.Request) (*url.URL, error) {
	u, err := http.ProxyFromEnvironment(req)
	if err != nil {
		return nil, err
	}
	return withoutLoopback(u), nil
}

// withoutLoopback returns nil for a proxy on this Mac's loopback, else u.
func withoutLoopback(u *url.URL) *url.URL {
	if u == nil {
		return nil
	}
	if ip := net.ParseIP(u.Hostname()); (ip != nil && ip.IsLoopback()) || strings.EqualFold(u.Hostname(), "localhost") {
		return nil
	}
	return u
}

// upstreamRoots is the system trust store plus the certificates in the
// NODE_EXTRA_CA_CERTS bundle, without Claude Burst's own block (its CA
// signs the gateway's leaf and has no business vouching for an upstream).
// Any failure falls back to the system store alone, which is what the
// gateway used before.
func upstreamRoots(bundle string, logger *log.Logger) *x509.CertPool {
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}
	if bundle == "" {
		return pool
	}
	b, err := os.ReadFile(bundle)
	if err != nil {
		return pool
	}
	rest, _ := tlsca.StripBlock(string(b))
	if strings.Contains(rest, "BEGIN CERTIFICATE") && !pool.AppendCertsFromPEM([]byte(rest)) && logger != nil {
		logger.Printf("WARNING: %s has certificates the gateway could not parse; upstreams signed by them will fail TLS", bundle)
	}
	return pool
}

// neverFollow hands a redirect back to the client as it came, the way the
// upstream sent it. Following it here would replay the request, headers and
// all, to whatever host the Location names: Go strips Authorization on a
// cross-host redirect but not x-api-key, so an API key would go with it.
// Claude Code follows (or refuses) redirects itself.
func neverFollow(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
