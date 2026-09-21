package router

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/andrewbakercloudscale/claude-burst/internal/config"
	"github.com/andrewbakercloudscale/claude-burst/internal/metrics"
)

type ctxKey int

const requestIDKey ctxKey = 0

// newRequestID returns a short hex identifier used to correlate a single
// inbound request across the text log and metrics.jsonl. It is never derived
// from, or a container for, request content.
func newRequestID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		// Extremely unlikely; fall back to a time-based id rather than an empty one.
		return fmt.Sprintf("t%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

func requestIDFrom(ctx context.Context) string {
	if v, ok := ctx.Value(requestIDKey).(string); ok {
		return v
	}
	return "-"
}

type State struct {
	// OverflowUntil is the ACCOUNT-WIDE window: only ForceOverflow sets it,
	// because "route everything to the secondary" is the one situation we
	// know covers every model. A limit Anthropic reported never lands here
	// -- see ModelOverflow.
	OverflowUntil int64  `json:"overflow_until"`
	LimitClaim    string `json:"limit_claim,omitempty"`
	LastReason    string `json:"last_reason,omitempty"`

	// ModelOverflow maps a requested Claude model to the unix time its own
	// rejection window ends.
	//
	// Anthropic's claim headers name the bucket that was exhausted
	// (five_hour, seven_day_opus, seven_day_overage_included, ...) but
	// nothing states which MODELS that bucket covers, and we do not guess:
	// only the model that was actually refused gets a window. If a limit
	// really is account-wide, the next model discovers that for itself on
	// its first request, at the cost of one rejection that bills nothing.
	// Guessing the other way is what cost real money -- one Fable rejection
	// on 2026-09-20 sent every model to a paid secondary for two days while
	// Opus was answering normally.
	ModelOverflow map[string]int64 `json:"model_overflow,omitempty"`

	// ModelClaim is what armed each model's window: an Anthropic limit claim
	// (five_hour, seven_day_overage_included, ...) or metered_* for an outage.
	// The two need different handling -- see releaseOutageWindow.
	ModelClaim map[string]string `json:"model_claim,omitempty"`

	// DowngradeDisabled turns the fallback chain off without editing
	// config.json, from the dashboard, while the gateway runs. Negative so
	// the zero value keeps the chain on: a user who has configured a chain
	// has already opted in, and an empty state file must not silently mean
	// "off".
	DowngradeDisabled bool `json:"downgrade_disabled,omitempty"`
}

type Server struct {
	cfg             config.Config
	primary         Provider
	primaryDetector FailoverDetector
	secondary       Provider // nil if no secondary is configured
	client          *http.Client
	// probe measures local network health after a transport failure. A field so
	// tests can say "the network is down" or "up" without depending on the
	// machine they run on having working DNS.
	probe func() netProbe
	// passthroughClient serves non-inference paths; identical to client but
	// with no ResponseHeaderTimeout, for long-polling control-plane requests.
	passthroughClient *http.Client
	metrics           *metrics.Writer
	statePath         string
	state             State
	mu                sync.RWMutex
	recent            recentRing
	logger            *log.Logger
	// warnedUnpriced deduplicates the "no pricing entry" warning per served
	// model. Without it a whole overflow window logs one line per request.
	warnedUnpriced sync.Map
}

func New(cfg config.Config, statePath, metricsPath string, logger *log.Logger) (*Server, error) {
	cfg.ResolveRoutes()

	primary, err := buildProvider(cfg.Primary, cfg.KeychainService, cfg.ModelMap, logger)
	if err != nil {
		return nil, fmt.Errorf("primary provider: %w", err)
	}
	primaryDetector, err := buildDetector(cfg.Primary.FailoverStrategy, cfg.MeteredFailover)
	if err != nil {
		return nil, fmt.Errorf("primary failover strategy: %w", err)
	}

	var secondary Provider
	if cfg.Secondary.Provider != "" && cfg.Secondary.Provider != config.ProviderNone {
		secondary, err = buildProvider(cfg.Secondary, cfg.KeychainService, cfg.ModelMap, logger)
		if err != nil {
			return nil, fmt.Errorf("secondary provider: %w", err)
		}
	}

	timeout := time.Duration(cfg.ResponseHeaderTimeoutSeconds) * time.Second

	newTransport := func(responseHeaderTimeout time.Duration) *http.Transport {
		t := &http.Transport{ResponseHeaderTimeout: responseHeaderTimeout}
		if cfg.Intercept.Transparent() {
			// /etc/hosts now points the upstream hostname at this process, so
			// the standard resolver would make the gateway call itself. See
			// interceptResolver.
			t.DialContext = newInterceptResolver(
				cfg.Intercept.Host, cfg.Intercept.ResolverDoH, cfg.Intercept.UpstreamAddr,
			).DialContext
		}
		return t
	}

	s := &Server{
		cfg:             cfg,
		primary:         primary,
		primaryDetector: primaryDetector,
		secondary:       secondary,
		client:          &http.Client{Timeout: 0, Transport: newTransport(timeout)},
		probe:           probeNetwork,
		// Control-plane traffic (notably Claude Code's Remote Control, which
		// registers and then long-polls for work) can legitimately hold a
		// connection open for minutes before sending any response header.
		// ResponseHeaderTimeout would kill exactly that, presenting as Remote
		// Control dropping every ~60s and looking like a network fault, so
		// non-inference paths get a client without it. Inference keeps the
		// bounded client, where the timeout is load-bearing for the
		// metered-failures failover strategy.
		passthroughClient: &http.Client{Timeout: 0, Transport: newTransport(0)},
		metrics:           metrics.New(metricsPath),
		statePath:         statePath,
		logger:            logger,
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
// configured strategy.
func buildDetector(strategy string, mf config.MeteredFailoverConfig) (FailoverDetector, error) {
	switch strategy {
	case "", "subscription-limit":
		return subscriptionLimitDetector{}, nil
	case "metered-failures":
		return newMeteredFailureDetector(mf.WindowSeconds, mf.MinFailures, mf.TransportErrorMinFailures), nil
	case "subscription-limit+metered-failures":
		return newCombinedDetector(mf), nil
	case "none":
		return noFailoverDetector{}, nil
	default:
		return nil, fmt.Errorf("unknown failover_strategy %q", strategy)
	}
}

func (s *Server) loadState() {
	b, err := os.ReadFile(s.statePath)
	if err != nil {
		if !os.IsNotExist(err) {
			s.logger.Printf("error stage=load_state path=%s err=%v (starting with no overflow state)", s.statePath, err)
		}
		return
	}
	var st State
	if err := json.Unmarshal(b, &st); err != nil {
		s.logger.Printf("error stage=load_state action=parse path=%s err=%v (starting with no overflow state)", s.statePath, err)
		return
	}
	// One-time migration. Before model scoping, any limit Anthropic reported
	// armed the account-wide window, so a legacy state file can carry a
	// days-long window that was only ever evidence about ONE model -- and
	// keeping it would send every model to the paid secondary for the rest
	// of it. A forced window is different: it was a deliberate instruction
	// and is honoured as written.
	if st.OverflowUntil > time.Now().Unix() && st.LimitClaim != "forced" && len(st.ModelOverflow) == 0 {
		s.logger.Printf("dropping pre-model-scoping overflow window (until=%s claim=%s): it recorded one model's rejection as account-wide. The next request per model re-establishes the truth.",
			time.Unix(st.OverflowUntil, 0).Format(time.RFC3339), st.LimitClaim)
		st.OverflowUntil, st.LimitClaim, st.LastReason = 0, "", ""
	}
	s.state = st
}

func (s *Server) saveStateLocked() {
	b, err := json.MarshalIndent(s.state, "", "  ")
	if err != nil {
		s.logger.Printf("error stage=save_state action=marshal err=%v", err)
		return
	}
	if err := os.WriteFile(s.statePath, append(b, '\n'), 0600); err != nil {
		s.logger.Printf("error stage=save_state action=write path=%s err=%v", s.statePath, err)
	}
}

func (s *Server) Status() State {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.state
}

// HasSecondary reports whether THIS running gateway process actually has a
// secondary Provider built, as opposed to what config.json on disk currently
// says. The two can disagree: config is only read at process start (see
// handleConfig's own "restart it for this to take effect"), so a secondary
// added or removed on disk after startup, without a restart, does not change
// s.secondary. Callers that need to know whether failing over to the
// secondary will actually do anything -- notably admin's "Force -> secondary"
// button -- must check this, not a freshly re-loaded config file.
func (s *Server) HasSecondary() bool {
	return s.secondary != nil
}

// isOutageClaim reports whether a failover was armed by failures rather than by
// Anthropic saying a limit was reached.
func isOutageClaim(claim string) bool { return strings.HasPrefix(claim, "metered_") }

// releaseOutageWindow ends a model's window if -- and only if -- an outage armed
// it. A rate-limit window is left alone: the primary would just refuse again.
func (s *Server) releaseOutageWindow(model string) {
	if model == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !isOutageClaim(s.state.ModelClaim[model]) || s.state.ModelOverflow[model] == 0 {
		return
	}
	delete(s.state.ModelOverflow, model)
	delete(s.state.ModelClaim, model)
	s.saveStateLocked()
	s.logger.Printf("released outage window for model=%q: the secondary also failed at the transport level, so the next request tries the primary", model)
}

// ClearOverflow reopens every route: the forced account-wide window and each
// model's own. DowngradeDisabled deliberately survives -- it is a policy the
// user set, not a window that expires, and "Back to primary" silently
// re-enabling a chain they turned off would be a setting that undoes itself.
func (s *Server) ClearOverflow() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state = State{DowngradeDisabled: s.state.DowngradeDisabled}
	s.saveStateLocked()
}

// inOverflow reports whether ANY window is open -- the forced account-wide
// one, or any single model's. Routing never asks this question (it always has
// a model in hand, and asks modelInOverflow); it is for status output, where
// "is anything diverted right now" is the useful summary.
func (s *Server) inOverflow(now time.Time) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.state.OverflowUntil > now.Unix() {
		return true
	}
	for _, until := range s.state.ModelOverflow {
		if until > now.Unix() {
			return true
		}
	}
	return false
}

// forcedOverflow is the account-wide window. It deliberately bypasses the
// fallback chain: someone who pressed "Force -> secondary" is exercising the
// secondary, and quietly serving them a different Claude model instead would
// defeat the only test the secondary path ever gets.
func (s *Server) forcedOverflow(now time.Time) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.state.OverflowUntil > now.Unix()
}

func (s *Server) modelInOverflow(model string, now time.Time) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.state.ModelOverflow[model] > now.Unix()
}

// DowngradeEnabled reports whether the fallback chain is live.
func (s *Server) DowngradeEnabled() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return !s.state.DowngradeDisabled
}

// SetDowngradeEnabled turns the fallback chain on or off for the running
// gateway and persists the choice, so the dashboard toggle survives a
// restart without a config edit.
func (s *Server) SetDowngradeEnabled(on bool) {
	s.mu.Lock()
	s.state.DowngradeDisabled = !on
	s.saveStateLocked()
	s.mu.Unlock()
	s.logger.Printf("model downgrade before secondary: %v", on)
}

// FallbackChain returns the configured rungs for a model, for display.
func (s *Server) FallbackChain() map[string][]string { return s.cfg.FallbackChain }

// ModelOverflow returns a copy of the per-model windows, for display.
func (s *Server) ModelOverflow() map[string]int64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string]int64, len(s.state.ModelOverflow))
	for k, v := range s.state.ModelOverflow {
		out[k] = v
	}
	return out
}

// ladderFor returns the rungs still worth trying for a requested model: the
// configured chain minus any model already inside its own rejection window,
// and minus the requested model itself (a chain that loops back would retry
// the model that was just refused).
func (s *Server) ladderFor(model string, now time.Time) []string {
	if model == "" || !s.DowngradeEnabled() {
		return nil
	}
	var out []string
	for _, rung := range s.cfg.FallbackChain[model] {
		if rung == "" || rung == model || s.modelInOverflow(rung, now) {
			continue
		}
		out = append(out, rung)
	}
	return out
}

// ForceOverflow routes inference to the secondary for d, regardless of what
// the upstream is actually saying.
//
// This exists because a subscription primary only fails over on genuine
// exhaustion signals, which cannot be provoked on demand -- so without it the
// secondary path is untestable until the day it is needed, which is the worst
// possible moment to discover it is misconfigured. The claim is recorded as
// "forced" so the metrics and status output never imply Anthropic reported a
// limit that it did not.
func (s *Server) ForceOverflow(d time.Duration, reason string) time.Time {
	if d <= 0 {
		d = 15 * time.Minute
	}
	until := time.Now().Add(d)
	s.mu.Lock()
	// Same care as ClearOverflow: replacing the whole struct here silently
	// reset the downgrade toggle, so forcing the secondary for 15 minutes
	// also turned a chain the user had switched off back on, permanently.
	s.state = State{OverflowUntil: until.Unix(), LimitClaim: "forced", LastReason: reason,
		DowngradeDisabled: s.state.DowngradeDisabled}
	s.saveStateLocked()
	s.mu.Unlock()
	s.logger.Printf("FORCED to secondary until %s reason=%s", until.Format(time.RFC3339), reason)
	return until
}

// activateOverflow records that ONE model was refused, until resetAt. It no
// longer touches the account-wide window: see State.ModelOverflow for why a
// claim header is not evidence about models it does not name.
func (s *Server) activateOverflow(model string, resetAt int64, claim, reason string) {
	if resetAt <= time.Now().Unix() {
		ttl := time.Duration(s.cfg.UnknownResetSeconds) * time.Second
		if isOutageClaim(claim) {
			// An outage has no reset time to read, and the unknown-reset default
			// (5 minutes) is sized for a rate limit that genuinely lasts that
			// long. An outage that has passed should not keep steering traffic
			// to a secondary: hold it only as long as the failures that armed it
			// were counted over.
			ttl = time.Duration(s.cfg.MeteredFailover.WindowSeconds) * time.Second
		}
		resetAt = time.Now().Add(ttl).Unix()
	}
	resetAt += int64(s.cfg.ResetGraceSeconds)
	s.mu.Lock()
	if s.state.ModelOverflow == nil {
		s.state.ModelOverflow = map[string]int64{}
	}
	if s.state.ModelClaim == nil {
		s.state.ModelClaim = map[string]string{}
	}
	if model != "" {
		s.state.ModelClaim[model] = claim
	}
	// An empty model means the body carried none to read. Scoping that to ""
	// would arm a window nothing ever matches, so it falls back to the
	// account-wide behaviour it had before -- diverting too much is bad, but
	// silently diverting nothing while believing otherwise is worse.
	if model == "" {
		s.state.OverflowUntil = resetAt
	} else {
		s.state.ModelOverflow[model] = resetAt
	}
	s.state.LimitClaim, s.state.LastReason = claim, reason
	s.saveStateLocked()
	s.mu.Unlock()
	s.logger.Printf("model=%q rejected until %s claim=%s reason=%s", model, time.Unix(resetAt, 0).Format(time.RFC3339), claim, reason)
}

// ServeHTTP is the entrypoint Go's http package calls for every request. It
// never contains business logic itself: its only jobs are to (1) assign a
// request id so every log line and metrics event for this request can be
// correlated, (2) guarantee that a panic anywhere below is logged with a
// stack trace and turned into a 500 instead of crashing the gateway or
// hanging Claude Code's connection, and (3) log a single start/finish
// summary line for every request so "what happened" is always visible in
// the text log even when nothing went wrong.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rid := newRequestID()
	ctx := context.WithValue(r.Context(), requestIDKey, rid)
	r = r.WithContext(ctx)
	start := time.Now()

	sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}

	defer func() {
		if rec := recover(); rec != nil {
			s.logger.Printf("req=%s PANIC method=%q path=%q err=%v\n%s", rid, r.Method, r.URL.Path, rec, debug.Stack())
			if !sw.wroteHeader {
				http.Error(sw, "internal error", http.StatusInternalServerError)
			}
		}
		// %q (not %s) for method/path: both are attacker-influenced (the
		// path arrives already percent-decoded, so e.g. %0A becomes a real
		// newline) and this line is the audit trail the whole log design
		// exists for -- an unquoted newline would let a caller forge what
		// looks like a second, distinct log line.
		s.logger.Printf("req=%s done method=%q path=%q status=%d dur_ms=%d",
			rid, r.Method, r.URL.Path, sw.status, time.Since(start).Milliseconds())
	}()

	s.logger.Printf("req=%s start method=%q path=%q", rid, r.Method, r.URL.Path)
	s.handle(sw, r)
}

// statusWriter records the status code actually written so the deferred
// logging line in ServeHTTP always reflects what the client received, even
// when the status was set deep inside forward/relay.
type statusWriter struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (sw *statusWriter) WriteHeader(code int) {
	if !sw.wroteHeader {
		sw.status = code
		sw.wroteHeader = true
	}
	sw.ResponseWriter.WriteHeader(code)
}

func (sw *statusWriter) Write(b []byte) (int, error) {
	if !sw.wroteHeader {
		sw.status = http.StatusOK
		sw.wroteHeader = true
	}
	return sw.ResponseWriter.Write(b)
}

// Flush lets the SSE relay loop keep flushing through the wrapper.
func (sw *statusWriter) Flush() {
	if fl, ok := sw.ResponseWriter.(http.Flusher); ok {
		fl.Flush()
	}
}

// isInference reports whether a path carries a model call, as opposed to the
// control-plane and probe traffic that is simply passed through.
func isInference(path string) bool {
	return strings.HasPrefix(path, "/v1/messages")
}

func (s *Server) handle(w http.ResponseWriter, r *http.Request) {
	rid := requestIDFrom(r.Context())

	if r.URL.Path == "/healthz" {
		w.Header().Set("content-type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "overflow": s.inOverflow(time.Now()), "state": s.Status()})
		return
	}

	// Read the body for every request, not just inference: Remote Control's
	// register call (and any other control-plane POST) needs its body
	// forwarded too, not just GETs/HEADs like the /api/hello warm-up probe.
	maxBytes := s.cfg.MaxRequestMB * 1024 * 1024
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBytes+1))
	if err != nil {
		s.logger.Printf("req=%s error stage=read_body err=%v", rid, err)
		http.Error(w, "failed reading request", http.StatusBadRequest)
		return
	}
	if int64(len(body)) > maxBytes {
		s.logger.Printf("req=%s error stage=read_body reason=too_large limit_mb=%d actual_bytes=%d", rid, s.cfg.MaxRequestMB, len(body))
		http.Error(w, fmt.Sprintf("request exceeds configured max_request_mb=%d", s.cfg.MaxRequestMB), http.StatusRequestEntityTooLarge)
		return
	}

	// ONE RULE, applied twice below: a request that cannot fail over does not
	// feed the failover detector either. Only /v1/messages can fail over, so
	// only /v1/messages decides when to.
	//
	// Both halves of that were wrong before, and both were observed in real
	// logs on this machine (2026-09-03):
	//
	//   - Control-plane paths passed allowFailover=true, so a run of failures
	//     replayed them to the secondary -- which has no such endpoint. 30
	//     requests to /v1/code/sessions/<id>/worker/events/stream and
	//     /api/claude_code/settings were handed to the OpenAI translator,
	//     which rejected each one with "request body is not valid JSON"
	//     because they are GETs with no body. The client got a 502 blaming
	//     the secondary for a request the secondary could never serve.
	//   - Worse, their outcomes counted. A single dropped Remote Control
	//     heartbeat ("connection reset by peer" on .../worker/heartbeat) was
	//     enough to arm an overflow window -- transport_error_min_failures
	//     defaults to 1 -- which then routed INFERENCE to a paid provider.
	//     A long-poll losing its connection is not evidence that inference is
	//     failing, and must not be able to spend money.
	//
	// The nil detector matters as much as allowFailover=false, and in the
	// opposite direction. forward() calls OnSuccess() whenever fd != nil,
	// regardless of allowFailover, so leaving the detector wired here would
	// let Remote Control's constant successful long-polls RESET a genuine run
	// of inference failures -- masking a real Anthropic outage rather than
	// reacting to it. Failures that cannot count and successes that still
	// reset is the worst of both. One signal source, both directions.
	//
	// count_tokens gets its own branch because isInference matches it
	// (prefix /v1/messages) and it has an extra reason of its own: it is
	// Anthropic-specific, with no equivalent shape on an openai-compatible
	// secondary. Routing it there makes the translator turn a count-only body
	// into a full chat-completion -- paying for a real generation to answer
	// "how many tokens is this" -- and then hang until the response-header
	// timeout before 502ing, since the reply never resembles a count.
	if r.URL.Path == "/v1/messages/count_tokens" {
		s.forward(w, r, body, "primary", s.primary, nil, false, "", nil)
		return
	}

	if !isInference(r.URL.Path) {
		s.forward(w, r, body, "primary", s.primary, nil, false, "", nil)
		return
	}

	now := time.Now()
	reqModel := requestModel(body)
	ladder := s.ladderFor(reqModel, now)

	// A model inside its own rejection window is not asked again until the
	// window ends -- but that is a statement about THAT model, so the chain
	// is tried before any money is spent.
	if !s.forcedOverflow(now) && s.modelInOverflow(reqModel, now) && len(ladder) > 0 {
		rung, rest := ladder[0], ladder[1:]
		downgraded, err := withModel(body, rung)
		if err == nil {
			s.logger.Printf("req=%s downgrade model=%q -> %q reason=%q (its rejection window is still open)", rid, reqModel, rung, "window open")
			s.forward(w, r, downgraded, "primary", s.primary, s.primaryDetector, true, "downgraded from "+reqModel, rest)
			return
		}
		s.logger.Printf("req=%s error stage=downgrade model=%q err=%v (falling through to the secondary)", rid, reqModel, err)
	}

	if s.forcedOverflow(now) || s.modelInOverflow(reqModel, now) {
		if s.secondary == nil {
			// An overflow window is armed (forced from the admin UI, or left
			// over in state.json from before a restart) but this process has
			// no secondary Provider built -- routing "secondary" would pass a
			// nil Provider into forward(), which panics on the first method
			// call. Surface a clear, actionable error instead of a bare 500;
			// see HasSecondary's doc comment for how the two can drift apart.
			s.logger.Printf("req=%s error stage=route reason=overflow_active_no_secondary", rid)
			http.Error(w, "gateway is in a forced/overflow window but has no secondary provider configured on this running process -- configure a secondary and restart the gateway, or clear the overflow window", http.StatusBadGateway)
			s.writeMetric(r, "secondary", "none", "", "", 0, time.Now(), tokenUsage{}, "", 0, "overflow active but no live secondary provider", "")
			return
		}
		s.forward(w, r, body, "secondary", s.secondary, nil, false, "overflow window active", nil)
		return
	}
	// allowFailover is true when there is anywhere to go: a secondary, or a
	// rung on the chain. Before the chain existed this was `s.secondary !=
	// nil`, which meant a user with no secondary configured got no downgrade
	// either, though it costs nothing and needs no third party.
	s.forward(w, r, body, "primary", s.primary, s.primaryDetector, s.secondary != nil || len(ladder) > 0, "", ladder)
}

// withModel rewrites the "model" field of an Anthropic request body, leaving
// everything else byte-for-byte as the client sent it.
func withModel(body []byte, model string) ([]byte, error) {
	var v map[string]any
	if err := json.Unmarshal(body, &v); err != nil {
		return nil, fmt.Errorf("cannot retarget request body: %w", err)
	}
	v["model"] = model
	return json.Marshal(v)
}

// forward drives one hop of the proxy through Provider p: build the outbound
// request, call it, and either relay success, pass an ordinary error straight
// back to the client, or -- when allowFailover is true and fd decides the
// failure warrants it -- activate the overflow window and replay the same
// request to s.secondary. The secondary hop is always invoked with
// allowFailover=false, so a failure never chains past two hops.
// clientFor picks the bounded client for inference and the unbounded one for
// pass-through control-plane traffic. Built defensively: a Server assembled in
// a test without passthroughClient still works.
func (s *Server) clientFor(path string) *http.Client {
	if !isInference(path) && s.passthroughClient != nil {
		return s.passthroughClient
	}
	return s.client
}

func (s *Server) forward(w http.ResponseWriter, in *http.Request, body []byte, slot string, p Provider, fd FailoverDetector, allowFailover bool, note string, ladder []string) {
	rid := requestIDFrom(in.Context())
	start := time.Now()

	req, model, err := p.Prepare(in.Context(), in, body)
	if err != nil {
		status, stage, reqModel := http.StatusBadGateway, "build_request", ""
		var perr *ProviderError
		if errors.As(err, &perr) {
			status, stage, reqModel = perr.Status, perr.Stage, perr.Model
		}
		s.logger.Printf("req=%s error stage=%s route=%s requested_model=%q err=%v", rid, stage, p.Name(), reqModel, err)
		http.Error(w, err.Error(), status)
		s.writeMetric(in, slot, p.Name(), reqModel, reqModel, 0, start, tokenUsage{}, "", 0, stage+" failed: "+err.Error(), "")
		return
	}

	// The actual outbound URL this hop is sent to -- scheme+host+path, no
	// query -- so metrics.jsonl and the admin UI can show which real backend
	// served a request rather than just the configured slot name, which is
	// what the "still going to primary?" confusion in practice turns out to
	// be: the slot label was right, but nothing showed the URL to check it
	// against.
	destination := req.URL.Scheme + "://" + req.URL.Host + req.URL.Path

	// The model that actually served, for metrics and the admin view. Falls
	// back to the requested model for passthrough providers, which serve with
	// exactly what was asked for.
	serveModel := model
	if sm, ok := p.(ServeModeler); ok {
		serveModel = sm.ServeModel(model)
	}

	resp, err := s.clientFor(in.URL.Path).Do(req)
	if err != nil && isStaleWriteFailure(err) && in.Context().Err() == nil {
		// The kept-alive connection died under us (a network switch), and the
		// write onto it failed, so the server never saw the request. Drop the
		// pooled connections and send it again on a fresh one, once, BEFORE
		// treating it as a failure: counting this against the primary is what
		// armed a failover window on 2026-09-21 that then sent a healthy
		// primary's traffic to a secondary that was also unreachable.
		s.clientFor(in.URL.Path).CloseIdleConnections()
		if retry, _, perr := p.Prepare(in.Context(), in, body); perr == nil {
			s.logger.Printf("req=%s retry route=%s reason=%q -> one more attempt on a fresh connection", rid, p.Name(), err)
			req = retry
			resp, err = s.clientFor(in.URL.Path).Do(req)
		}
	}
	if resp != nil {
		s.recent.add(RecentResponse{
			Time: start, RequestID: rid, Method: in.Method, Path: in.URL.Path,
			Slot: slot, Route: p.Name(), Model: serveModel, Status: resp.StatusCode,
			DurationMS: time.Since(start).Milliseconds(), Headers: filterHeaders(resp.Header),
			Destination: destination,
		})
	}
	if err != nil {
		// Logged on every transport-level failure, not just the ones that
		// exhaust failover: on 2026-09-03 primary timed out mid-connection and
		// secondary's DNS lookup failed outright ~90s later, and the log gave
		// no way to tell whether that was one continuous network outage or two
		// unrelated failures. This snapshot answers that next time.
		probe := s.probe
		if probe == nil {
			probe = probeNetwork
		}
		np := probe()
		s.logger.Printf("req=%s %s", rid, np.snapshot(p.Name(), err))
		// The client's own context is what Prepare was given, so if it is
		// done, this request died because the CALLER went away -- not because
		// the upstream failed. Checked here as well as in the detector
		// because this is the authoritative signal (the detector only sees a
		// wrapped error string away from it), and because the consequence
		// here is concrete: replaying to the secondary would spend money on a
		// paid provider generating a response that nobody is left to read.
		if in.Context().Err() != nil {
			s.logger.Printf("req=%s client_gone route=%s err=%v (no failover, not replayed)", rid, p.Name(), err)
			s.writeMetric(in, slot, p.Name(), serveModel, model, 0, start, tokenUsage{}, "", 0, "client cancelled: "+err.Error(), destination)
			return
		}
		if slot == "secondary" {
			// The secondary just failed at the transport level. If the window
			// that sent traffic here was armed by an outage (not by a rate limit,
			// which the primary would only refuse again), it is not helping:
			// release it so the next request tries the primary rather than
			// queueing behind a secondary that cannot answer.
			//
			// Keyed by the model the CLIENT asked for, read from the body: what
			// Prepare returns for a secondary is that provider's own model id,
			// and the window was armed under the requested one.
			s.releaseOutageWindow(requestModel(body))
		}
		if !np.dnsOK && !peerAnswered(err) && !isClientCancellation(err) {
			// Silence from the far side while this machine cannot resolve any
			// name: the network is down, and the secondary is behind the same
			// network. Failing over would only make the request wait on a second
			// dead host (four Together timeouts in a row, 2026-09-21), and it
			// would arm a window blaming a model for a laptop changing WiFi.
			s.logger.Printf("req=%s no_failover route=%s reason=%q (local network unavailable: control DNS failed)", rid, p.Name(), "network down")
			http.Error(w, "local network unavailable (DNS is failing on this machine) -- not failing over, since the secondary is behind the same network: "+err.Error(), http.StatusBadGateway)
			s.writeMetric(in, slot, p.Name(), serveModel, model, 0, start, tokenUsage{}, "", 0, "local network unavailable; not failed over: "+err.Error(), destination)
			return
		}
		if allowFailover {
			if d := fd.OnError(err); d.Failover {
				s.activateOverflow(model, d.ResetAt, d.Claim, d.Reason)
				s.replayElsewhere(w, in, body, slot, p, model, serveModel, destination, start, 0, d, ladder,
					fmt.Sprintf("transport error: %v", err))
				return
			}
		}
		s.logger.Printf("req=%s error stage=upstream_call route=%s err=%v", rid, p.Name(), err)
		http.Error(w, p.Name()+" upstream error: "+err.Error(), http.StatusBadGateway)
		s.writeMetric(in, slot, p.Name(), serveModel, model, 0, start, tokenUsage{}, "", 0, "upstream call failed: "+err.Error(), destination)
		return
	}

	// Successful responses must stream immediately; don't buffer them.
	if resp.StatusCode < 400 {
		if fd != nil {
			fd.OnSuccess()
		}
		var tok tokenUsage
		if t, ok := p.(Translator); ok {
			tok, err = t.TranslateResponse(w, resp, model)
			if err != nil {
				// The response has likely already started writing to the
				// client by this point (translation is itself streaming);
				// there's nothing safe left to do but log it.
				s.logger.Printf("req=%s error stage=translate_response route=%s model=%q err=%v", rid, p.Name(), model, err)
			}
		} else {
			tok = s.relay(w, resp, model)
		}
		s.logger.Printf("req=%s ok route=%s model=%q status=%d dur_ms=%d in_tok=%d out_tok=%d note=%q",
			rid, p.Name(), model, resp.StatusCode, time.Since(start).Milliseconds(), tok.input, tok.output, note)
		s.writeMetric(in, slot, p.Name(), serveModel, model, resp.StatusCode, start, tok, "", 0, note, destination)
		return
	}

	errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 2*1024*1024))
	_ = resp.Body.Close()

	if allowFailover {
		if d := fd.OnResponse(resp.StatusCode, resp.Header, errBody); d.Failover {
			s.activateOverflow(model, d.ResetAt, d.Claim, d.Reason)
			s.replayElsewhere(w, in, body, slot, p, model, serveModel, destination, start, resp.StatusCode, d, ladder,
				fmt.Sprintf("status=%d", resp.StatusCode))
			return
		}
	}

	s.logger.Printf("req=%s upstream_error route=%s model=%q status=%d note=%q", rid, p.Name(), model, resp.StatusCode, note)
	copyResponseHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(errBody)
	s.writeMetric(in, slot, p.Name(), serveModel, model, resp.StatusCode, start, tokenUsage{}, "", 0, "upstream error; no failover", destination)
}

// replayElsewhere is where a failover decision turns into a second hop: down
// the fallback chain to another Claude model on the subscription, or, when
// the chain is exhausted or empty, out to the paid secondary.
//
// The chain is tried first on purpose. A rejection names one model; the rungs
// are models the subscription may still be serving, and they cost nothing.
// Only when none is available does this spend money. If neither is possible
// the upstream's own error goes back to the client unchanged, which is the
// behaviour a gateway with no secondary always had.
func (s *Server) replayElsewhere(w http.ResponseWriter, in *http.Request, body []byte,
	slot string, p Provider, model, serveModel, destination string, start time.Time, status int,
	d FailoverDecision, ladder []string, trigger string) {

	rid := requestIDFrom(in.Context())

	for len(ladder) > 0 {
		rung := ladder[0]
		ladder = ladder[1:]
		if s.modelInOverflow(rung, time.Now()) {
			continue // refused in the time this request has been in flight
		}
		downgraded, err := withModel(body, rung)
		if err != nil {
			s.logger.Printf("req=%s error stage=downgrade model=%q err=%v", rid, rung, err)
			break
		}
		s.writeMetric(in, slot, p.Name(), serveModel, model, status, start, tokenUsage{}, d.Claim, d.ResetAt,
			d.Reason+"; request replayed to "+rung, destination)
		s.logger.Printf("req=%s failover route=%s model=%q claim=%s reason=%q (%s) -> replaying on the subscription as %q",
			rid, p.Name(), model, d.Claim, d.Reason, trigger, rung)
		// allowFailover stays true: the rung can be refused too, and when it
		// is, this same path carries on to the next rung or the secondary.
		s.forward(w, in, downgraded, "primary", s.primary, s.primaryDetector, true, "downgraded from "+model, ladder)
		return
	}

	if s.secondary == nil {
		s.logger.Printf("req=%s no_failover_target route=%s model=%q claim=%s reason=%q (%s): fallback chain exhausted and no secondary configured",
			rid, p.Name(), model, d.Claim, d.Reason, trigger)
		http.Error(w, p.Name()+" refused this model ("+d.Reason+") and there is no fallback chain rung or secondary provider left to try", http.StatusServiceUnavailable)
		s.writeMetric(in, slot, p.Name(), serveModel, model, status, start, tokenUsage{}, d.Claim, d.ResetAt,
			d.Reason+"; no fallback target", destination)
		return
	}

	s.writeMetric(in, slot, p.Name(), serveModel, model, status, start, tokenUsage{}, d.Claim, d.ResetAt,
		d.Reason+"; request replayed to secondary", destination)
	s.logger.Printf("req=%s failover route=%s model=%q claim=%s reason=%q (%s) -> replaying to secondary",
		rid, p.Name(), model, d.Claim, d.Reason, trigger)
	s.forward(w, in, body, "secondary", s.secondary, nil, false, d.Reason, nil)
}

// networkSnapshot captures a point-in-time read of local network health at
// the moment an upstream call failed at the transport layer (dial/DNS/i-o
// timeout -- never an HTTP-level error, which already carries its own status
// code and needs no further diagnosis). It exists to answer one question a
// bare "dial tcp: i/o timeout" or "no such host" line cannot: was this route
// specifically broken, or was the machine's network itself down for
// everything at that instant?
//
//   - local_ifaces: non-loopback addresses currently bound. Empty means the
//     network interface itself is down -- the most total failure possible,
//     and distinguishable from "this one host is unreachable".
//   - control_dns: a lookup of a hostname unrelated to any configured
//     provider or DoH endpoint, through the plain system resolver (bypassing
//     DoH entirely, unlike interceptResolver). If this also fails, DNS is
//     broken machine-wide, not just for the provider that just errored.
func networkSnapshot(route string, triggerErr error) string {
	return probeNetwork().snapshot(route, triggerErr)
}

// netProbe is one measurement of local network health. It is taken once per
// transport failure and used twice: written to the log, and consulted to
// decide whether failing over could possibly help.
type netProbe struct {
	ifaces []string
	dnsOK  bool
	dnsErr error
	dnsDur time.Duration
}

func probeNetwork() netProbe {
	var p netProbe
	if addrs, ierr := net.InterfaceAddrs(); ierr == nil {
		for _, a := range addrs {
			ipnet, ok := a.(*net.IPNet)
			if !ok || ipnet.IP.IsLoopback() {
				continue
			}
			p.ifaces = append(p.ifaces, ipnet.IP.String())
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	start := time.Now()
	_, p.dnsErr = net.DefaultResolver.LookupHost(ctx, "www.apple.com")
	p.dnsDur = time.Since(start)
	p.dnsOK = p.dnsErr == nil
	return p
}

func (p netProbe) snapshot(route string, triggerErr error) string {
	ifaceState := "NONE (network interface appears down)"
	if len(p.ifaces) > 0 {
		ifaceState = strings.Join(p.ifaces, ",")
	}
	dnsState := fmt.Sprintf("ok (%s)", p.dnsDur.Round(time.Millisecond))
	if p.dnsErr != nil {
		dnsState = fmt.Sprintf("FAILED (%s): %v", p.dnsDur.Round(time.Millisecond), p.dnsErr)
	}
	return fmt.Sprintf("network-snapshot route=%s trigger_err=%q local_ifaces=%s control_dns=%s",
		route, triggerErr, ifaceState, dnsState)
}

// peerAnswered reports whether err proves something on the far side of the
// network actually replied. A refused or reset connection is a peer speaking;
// a timeout or an EOF is silence, and silence is the ambiguous case where the
// probe above is worth consulting.
func peerAnswered(err error) bool {
	return errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.ECONNRESET)
}

// isStaleWriteFailure reports whether err is a WRITE onto a connection that
// had already died, which is what a laptop moving between networks produces:
// the kept-alive connection to Anthropic is still in the pool, the interface
// it rode on is gone, and the first write onto it fails with EPIPE.
//
// It is scoped to writes on purpose. A failed write means the server cannot
// have received the whole request, so sending it again on a fresh connection
// cannot run the same generation twice. A read-side reset or EOF gives no such
// guarantee and is deliberately not retried.
func isStaleWriteFailure(err error) bool {
	var op *net.OpError
	if !errors.As(err, &op) || op.Op != "write" {
		return false
	}
	return errors.Is(err, syscall.EPIPE) || errors.Is(err, syscall.ECONNRESET)
}

func copyResponseHeaders(dst http.Header, src http.Header) {
	for k, vals := range src {
		lk := strings.ToLower(k)
		if lk == "content-length" || lk == "connection" || lk == "content-encoding" {
			continue
		}
		for _, v := range vals {
			dst.Add(k, v)
		}
	}
}

type tokenUsage struct{ input, output int64 }

func (s *Server) relay(w http.ResponseWriter, resp *http.Response, model string) tokenUsage {
	defer resp.Body.Close()
	copyResponseHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	fl, _ := w.(http.Flusher)

	ct := strings.ToLower(resp.Header.Get("content-type"))
	if !strings.Contains(ct, "text/event-stream") {
		if _, err := io.Copy(w, resp.Body); err != nil {
			// Almost always means the client (Claude Code) disconnected
			// mid-response. Not a proxy bug, but worth having in the log
			// when someone is debugging a truncated response.
			s.logger.Printf("relay copy error (client likely disconnected): %v", err)
		}
		if fl != nil {
			fl.Flush()
		}
		return tokenUsage{}
	}

	br := bufio.NewReader(resp.Body)
	var tok tokenUsage
	for {
		line, err := br.ReadString('\n')
		if len(line) > 0 {
			if _, werr := io.WriteString(w, line); werr != nil {
				s.logger.Printf("relay SSE write error (client likely disconnected): %v", werr)
				break
			}
			if fl != nil {
				fl.Flush()
			}
			parseSSEUsage(line, &tok)
		}
		if err != nil {
			if err != io.EOF {
				s.logger.Printf("relay SSE read error: %v", err)
			}
			break
		}
	}
	return tok
}

func parseSSEUsage(line string, tok *tokenUsage) {
	line = strings.TrimSpace(line)
	if !strings.HasPrefix(line, "data:") {
		return
	}
	raw := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
	if raw == "" || raw == "[DONE]" {
		return
	}
	var v map[string]any
	if json.Unmarshal([]byte(raw), &v) != nil {
		return
	}
	// message_start: {message:{usage:{input_tokens:...}}}
	if m, ok := v["message"].(map[string]any); ok {
		if u, ok := m["usage"].(map[string]any); ok {
			if n := number(u["input_tokens"]); n > tok.input {
				tok.input = n
			}
			if n := number(u["output_tokens"]); n > tok.output {
				tok.output = n
			}
		}
	}
	// message_delta: {usage:{output_tokens:...}}
	if u, ok := v["usage"].(map[string]any); ok {
		if n := number(u["input_tokens"]); n > tok.input {
			tok.input = n
		}
		if n := number(u["output_tokens"]); n > tok.output {
			tok.output = n
		}
	}
}

func number(v any) int64 {
	switch n := v.(type) {
	case float64:
		return int64(n)
	case json.Number:
		i, _ := n.Int64()
		return i
	case string:
		i, _ := strconv.ParseInt(n, 10, 64)
		return i
	}
	return 0
}

func requestModel(body []byte) string {
	var v struct {
		Model string `json:"model"`
	}
	if json.Unmarshal(body, &v) == nil {
		return v.Model
	}
	return ""
}

func (s *Server) writeMetric(in *http.Request, slot, route, model, requestedModel string, status int, start time.Time, tok tokenUsage, claim string, reset int64, note, destination string) {

	// Two-value lookup, not a bare index. A missing key yields the zero
	// ModelPrice, so indexing alone silently prices an unknown model at
	// $0/Mtok -- which is exactly what happened once the served model
	// started being recorded correctly: the secondary's real model id is
	// not in the default pricing table, so every overflow request recorded
	// api_equivalent_usd=0 and `stats` reported no secondary spend at all.
	// A zero that means "not priced" must not look like a zero that means
	// "free".
	price, priced := s.cfg.Pricing[model]
	equiv := (float64(tok.input)/1_000_000)*price.InputPerMTok + (float64(tok.output)/1_000_000)*price.OutputPerMTok
	// Only tokens make a missing price a problem. Events with no token
	// counts (failover notes, upstream errors, control-plane passthrough)
	// legitimately cost nothing and must not be flagged.
	unpriced := !priced && (tok.input > 0 || tok.output > 0)
	if unpriced {
		if _, seen := s.warnedUnpriced.LoadOrStore(model, true); !seen {
			s.logger.Printf("warn stage=pricing model=%q no pricing entry; cost for this model is not being counted -- add it to `pricing` in config.json", model)
		}
	}
	rid := requestIDFrom(in.Context())
	err := s.metrics.Write(metrics.Event{
		Time: time.Now(), RequestID: rid, SessionID: in.Header.Get("x-claude-code-session-id"), AgentID: in.Header.Get("x-claude-code-agent-id"),
		Slot: slot, Route: route, Model: model, RequestedModel: requestedModel, HTTPStatus: status, DurationMS: time.Since(start).Milliseconds(),
		InputTokens: tok.input, OutputTokens: tok.output, APIEquivalentUSD: equiv, LimitClaim: claim, ResetAt: reset, Note: note, Destination: destination,
		PricingUnknown: unpriced,
	})
	if err != nil {
		// The request has already been served to Claude Code by this point;
		// a metrics-write failure (e.g. disk full, permissions) must never
		// take the gateway down. Log it loudly so it's diagnosable, and move on.
		s.logger.Printf("req=%s error stage=write_metric err=%v", rid, err)
	}
}
