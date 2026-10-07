package router

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"github.com/andrewbakercloudscale/claude-burst/internal/ctxview"
	"io"
	"log"
	"net/http"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/andrewbakercloudscale/claude-burst/internal/config"
	"github.com/andrewbakercloudscale/claude-burst/internal/keepawake"
	"github.com/andrewbakercloudscale/claude-burst/internal/metrics"
	"github.com/andrewbakercloudscale/claude-burst/internal/repo"
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

type Server struct {
	cfg             config.Config
	primary         Provider
	primaryDetector FailoverDetector
	secondary       Provider // nil if no secondary is configured

	// inflight counts inference requests (/v1/messages) being served right
	// now. A graceful restart waits for it to reach zero; see InFlight.
	inflight atomic.Int64

	// held are the control-plane streams open now (held.go).
	held heldStreams

	// compaction is proxy-side compaction of long primary sessions
	// (compact.go, compact_run.go). Always present; off unless enabled.
	compaction *compactor
	automask   *masker
	inspect    *inspectStore
	removals   *ctxview.Store
	// quiet counts the requests whose start and done lines are left out of
	// the log, and repeats says when a line that would repeat for every
	// request of an outage was last written: see quietlog.go.
	quiet   quietCounter
	repeats repeatLimiter
	// snapMu guards the last fully logged network snapshot (logSnapshot).
	snapMu    sync.Mutex
	snapState string
	snapAt    time.Time
	// repos names each session's repository, for per-repository Compact at.
	repos  *repo.Resolver
	client *http.Client
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

	// failoverNotices are lines waiting to be shown in Claude Code's window
	// at the next prompt of any session: a switch to the paid secondary is
	// never silent. Held by failoverMu; not part of state.json.
	failoverMu sync.Mutex
	// alerts is what the on-screen alerts need to say a problem ended.
	alerts          alertState
	failoverNotices []string

	// health is whether the primary is answering right now, for the
	// dashboard. Held by healthMu; not part of state.json.
	// readyMu guards the cached answer to "does the secondary have its
	// credential"; see secondaryReady.
	readyMu  sync.Mutex
	readyAt  time.Time
	readyErr error

	healthMu sync.Mutex
	health   PrimaryHealth
	recent   recentRing
	logger   *log.Logger
	// warnedUnpriced deduplicates the "no pricing entry" warning per served
	// model. Without it a whole overflow window logs one line per request.
	warnedUnpriced sync.Map
	// guardCounters rate-limit the request guard's refusal log; see guard.go.
	guardCounters
	// traceRegistry and clientAuth back the dashboard's test message; see
	// trace.go.
	traceRegistry
	clientAuth
	// resolver resolves the intercepted host bypassing /etc/hosts; nil
	// outside transparent mode.
	resolver *interceptResolver
}

// Logf writes one line to the gateway log, for other components (the
// dashboard's notifier) whose events belong beside the request log.
func (s *Server) Logf(format string, a ...any) { s.logger.Printf(format, a...) }

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
	if isHeldStream(r) {
		var done func()
		r, done = s.held.hold(r)
		defer done()
	}

	sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}

	if isInference(r.URL.Path) {
		s.inflight.Add(1)
		defer s.inflight.Add(-1)
	}

	// Health checks, heartbeats and the other calls that are not a model
	// request were 56% of the log's lines (6 Oct 2026), a start and a done
	// each, around the one request in a hundred anyone looks for. They are
	// counted now and one line a minute says how many; one that fails or
	// is slow is still logged, with its request id.
	quiet, panicked := !isInference(r.URL.Path), false
	defer func() {
		if rec := recover(); rec != nil {
			panicked = true
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
		dur := time.Since(start)
		if quiet && !panicked && sw.status < 400 && dur < quietSlow {
			s.quiet.add(s.logger, time.Now())
			return
		}
		s.logger.Printf("req=%s done method=%q path=%q status=%d dur_ms=%d",
			rid, r.Method, r.URL.Path, sw.status, dur.Milliseconds())
	}()

	if !quiet {
		s.logger.Printf("req=%s start method=%q path=%q", rid, r.Method, r.URL.Path)
	}
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
// InFlight is how many inference requests are being served right now. Only
// inference counts: a Remote Control long-poll or a heartbeat holds a
// connection open with nothing at stake, reconnects on its own, and would
// otherwise keep a restart waiting forever.
func (s *Server) InFlight() int64 { return s.inflight.Load() }

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

	// Before the body is read: a refused request must cost nothing, and
	// above all must never reach a provider. See guard.go.
	if reason := s.requestRefusal(r); reason != "" {
		s.logRefusal(rid, reason, r)
		http.Error(w, "claude-burst: request refused: "+reason, http.StatusForbidden)
		return
	}
	// Removes TraceHeader whatever it holds, so it is never forwarded.
	r = s.withTrace(r, rid)

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
		// Masked like the turn it measures: /context sends the whole
		// conversation, and automask promises nothing personal leaves.
		body = s.applyAutomask(r, body)
		body = s.applyRemovals(r.Header.Get("x-claude-code-session-id"), body)
		s.forward(w, r, body, "primary", s.primary, nil, false, "", nil)
		return
	}

	if !isInference(r.URL.Path) {
		s.forward(w, r, body, "primary", s.primary, nil, false, "", nil)
		return
	}

	// A real turn: the keep-awake idle window counts from here.
	keepawake.Touch()
	if traceFrom(r.Context()) == nil {
		s.noteClientAuth(r.Header)
	}

	now := time.Now()
	// Before routing, so a compacted history goes wherever the request
	// goes. count_tokens returned above, uncompacted: it must count what
	// Claude Code sent. Masked first: a summary, the compaction hash and the secondary all
	// see only the masked history.
	body = s.applyAutomask(r, body)
	body, r = s.applyCompaction(r, body)
	// A thread with a compaction to make is asked for its history.
	if compactInfoFrom(r.Context()).replay != "" {
		s.askForHistory(w, r)
		return
	}
	// After compaction, so a removal never changes the history compaction
	// hashes: it only changes what is sent.
	body = s.applyRemovals(r.Header.Get("x-claude-code-session-id"), body)
	s.captureForInspect(r.Header.Get("x-claude-code-session-id"), body)
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
			traceRoute(r.Context(), "primary", "downgraded to "+rung+": the rejection window for "+reqModel+" is still open")
			s.forward(w, r, downgraded, "primary", s.primary, s.primaryDetector, s.secondaryReady() || len(rest) > 0, "downgraded from "+reqModel, rest)
			return
		}
		s.logger.Printf("req=%s error stage=downgrade model=%q err=%v (falling through to the secondary)", rid, reqModel, err)
	}

	if s.forcedOverflow(now) || s.modelInOverflow(reqModel, now) {
		if s.secondaryReady() {
			why := "an overflow window for " + reqModel + " is open"
			if s.forcedOverflow(now) {
				why = "an overflow window is open for every model"
				if st := s.Status(); st.LastReason != "" {
					why += " (" + st.LastReason + ")"
				}
			}
			traceRoute(r.Context(), "secondary", why)
			s.forward(w, r, body, "secondary", s.secondary, nil, false, "overflow window active", nil)
			return
		}
		traceRoute(r.Context(), "primary", "an overflow window is open, but there is no usable secondary, so Anthropic is asked anyway")
		// A window is open (forced from the admin UI, or left in state.json
		// by a secondary since removed) but there is nowhere to send the
		// request. It used to get a 502 for the rest of the window; asking
		// Anthropic costs nothing, and its answer, a refusal included, is
		// the right one to return.
		s.logger.Printf("req=%s route=primary reason=%q (overflow window open, no usable secondary)", rid, "no secondary")
	}
	// allowFailover is true when there is anywhere to go: a secondary, or a
	// rung on the chain. Before the chain existed this was `s.secondary !=
	// nil`, which meant a user with no secondary configured got no downgrade
	// either, though it costs nothing and needs no third party.
	traceRoute(r.Context(), "primary", "no overflow window is open, so the subscription serves it")
	s.forward(w, r, body, "primary", s.primary, s.primaryDetector, s.secondaryReady() || len(ladder) > 0, "", ladder)
}
