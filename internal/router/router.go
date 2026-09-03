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
	"net/http"
	"net/url"
	"os"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
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
	OverflowUntil int64  `json:"overflow_until"`
	LimitClaim    string `json:"limit_claim,omitempty"`
	LastReason    string `json:"last_reason,omitempty"`
}

type Server struct {
	cfg             config.Config
	primary         Provider
	primaryDetector FailoverDetector
	secondary       Provider // nil if no secondary is configured
	client          *http.Client
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

	primary, err := buildProvider(cfg.Primary, cfg.KeychainService, cfg.ModelMap)
	if err != nil {
		return nil, fmt.Errorf("primary provider: %w", err)
	}
	primaryDetector, err := buildDetector(cfg.Primary.FailoverStrategy, cfg.MeteredFailover)
	if err != nil {
		return nil, fmt.Errorf("primary failover strategy: %w", err)
	}

	var secondary Provider
	if cfg.Secondary.Provider != "" && cfg.Secondary.Provider != config.ProviderNone {
		secondary, err = buildProvider(cfg.Secondary, cfg.KeychainService, cfg.ModelMap)
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
func buildProvider(rc config.RouteConfig, defaultKeychainService string, defaultModelMap map[string]string) (Provider, error) {
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
		label, envVar := openAICompatibleIdentity(ks)
		return NewOpenAICompatibleProvider(label, base, rc.Model, rc.ModelMap, ks, envVar), nil
	default:
		return nil, fmt.Errorf("unknown provider %q", rc.Provider)
	}
}

// EnvVarForProvider derives the environment-variable name claude-burst
// expects to hold an openai-compatible secondary's API key, from a short
// vendor label -- "together" -> "TOGETHER_API_KEY", "openrouter" ->
// "OPENROUTER_API_KEY". Exported because cmd/claude-burst's keychain-set
// needs this same half of the convention (label -> env var) to know which
// env var to read for `--provider <label>`; openAICompatibleIdentity below
// covers the other half (keychain service -> label) and only buildProvider
// needs that one.
func EnvVarForProvider(label string) string {
	return strings.ToUpper(strings.ReplaceAll(label, "-", "_")) + "_API_KEY"
}

// openAICompatibleIdentity derives the short vendor label and API-key env
// var for an openai-compatible secondary from its keychain service name --
// e.g. "claude-burst-together" -> ("together", "TOGETHER_API_KEY"),
// "claude-burst-openrouter" -> ("openrouter", "OPENROUTER_API_KEY"). No
// vendor is hardcoded here: a config that only ever set (or defaulted to)
// "claude-burst-together" gets back exactly the identity it had before.
func openAICompatibleIdentity(keychainService string) (label, envVar string) {
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
		return newMeteredFailureDetector(mf.WindowSeconds, mf.MinFailures), nil
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

func (s *Server) ClearOverflow() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state = State{}
	s.saveStateLocked()
}

func (s *Server) inOverflow(now time.Time) bool {
	s.mu.RLock()
	until := s.state.OverflowUntil
	s.mu.RUnlock()
	return until > now.Unix()
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
	s.state = State{OverflowUntil: until.Unix(), LimitClaim: "forced", LastReason: reason}
	s.saveStateLocked()
	s.mu.Unlock()
	s.logger.Printf("FORCED to secondary until %s reason=%s", until.Format(time.RFC3339), reason)
	return until
}

func (s *Server) activateOverflow(resetAt int64, claim, reason string) {
	if resetAt <= time.Now().Unix() {
		resetAt = time.Now().Add(time.Duration(s.cfg.UnknownResetSeconds) * time.Second).Unix()
	}
	resetAt += int64(s.cfg.ResetGraceSeconds)
	s.mu.Lock()
	s.state = State{OverflowUntil: resetAt, LimitClaim: claim, LastReason: reason}
	s.saveStateLocked()
	s.mu.Unlock()
	s.logger.Printf("switching to secondary until %s claim=%s reason=%s", time.Unix(resetAt, 0).Format(time.RFC3339), claim, reason)
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

	// /v1/messages/count_tokens is Anthropic-specific: no equivalent request
	// shape exists on an openai-compatible secondary's wire protocol. Routing
	// it there (as isInference below would, during an overflow window) makes
	// the OpenAI translator mistranslate the count-only body into a full
	// chat-completion request -- paying for a real generation just to answer
	// "how many tokens is this" -- and it hangs until the response-header
	// timeout before 502ing, since the translated reply never resembles a
	// count response. So this always goes to primary, overflow or not, and
	// never fails over: there is nowhere correct to fail over to.
	if r.URL.Path == "/v1/messages/count_tokens" {
		s.forward(w, r, body, "primary", s.primary, s.primaryDetector, false, "")
		return
	}

	if !isInference(r.URL.Path) {
		s.forward(w, r, body, "primary", s.primary, s.primaryDetector, s.secondary != nil, "")
		return
	}

	if s.inOverflow(time.Now()) {
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
		s.forward(w, r, body, "secondary", s.secondary, nil, false, "overflow window active")
		return
	}
	s.forward(w, r, body, "primary", s.primary, s.primaryDetector, s.secondary != nil, "")
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

func (s *Server) forward(w http.ResponseWriter, in *http.Request, body []byte, slot string, p Provider, fd FailoverDetector, allowFailover bool, note string) {
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
	if resp != nil {
		s.recent.add(RecentResponse{
			Time: start, RequestID: rid, Method: in.Method, Path: in.URL.Path,
			Slot: slot, Route: p.Name(), Model: serveModel, Status: resp.StatusCode,
			DurationMS: time.Since(start).Milliseconds(), Headers: filterHeaders(resp.Header),
			Destination: destination,
		})
	}
	if err != nil {
		if allowFailover {
			if d := fd.OnError(err); d.Failover {
				s.writeMetric(in, slot, p.Name(), serveModel, model, 0, start, tokenUsage{}, d.Claim, d.ResetAt, d.Reason+"; request replayed to secondary", destination)
				s.activateOverflow(d.ResetAt, d.Claim, d.Reason)
				s.logger.Printf("req=%s failover route=%s claim=%s reason=%q (transport error: %v) -> replaying to secondary", rid, p.Name(), d.Claim, d.Reason, err)
				s.forward(w, in, body, "secondary", s.secondary, nil, false, d.Reason)
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
			s.writeMetric(in, slot, p.Name(), serveModel, model, resp.StatusCode, start, tokenUsage{}, d.Claim, d.ResetAt, d.Reason+"; request replayed to secondary", destination)
			s.activateOverflow(d.ResetAt, d.Claim, d.Reason)
			s.logger.Printf("req=%s failover route=%s model=%q status=%d claim=%s reason=%q -> replaying to secondary",
				rid, p.Name(), model, resp.StatusCode, d.Claim, d.Reason)
			s.forward(w, in, body, "secondary", s.secondary, nil, false, d.Reason)
			return
		}
	}

	s.logger.Printf("req=%s upstream_error route=%s model=%q status=%d note=%q", rid, p.Name(), model, resp.StatusCode, note)
	copyResponseHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(errBody)
	s.writeMetric(in, slot, p.Name(), serveModel, model, resp.StatusCode, start, tokenUsage{}, "", 0, "upstream error; no failover", destination)
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
