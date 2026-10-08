// Package codex is Claude Burst's gateway for OpenAI's Codex.
//
// Codex signed in with ChatGPT sends its model calls to
// chatgpt.com/backend-api/codex. A model provider in ~/.codex/config.toml
// (see config.go) points those calls at this gateway instead, over plain
// HTTP on loopback, with Codex's own ChatGPT token on every request. The
// gateway forwards each one unchanged to chatgpt.com and, on the way back,
// reads what it needs: the tokens a turn used, the plan limits ChatGPT
// reports in its x-codex-* headers, and a usage-limit refusal.
//
// Verified against codex-cli 0.159.2 (2026-10-04): a custom provider with
// requires_openai_auth sends the ChatGPT bearer token and account id, over
// http:// to loopback, and only /models and /responses go through it. The
// other route, chatgpt_base_url, is refused unless it is https and also
// carries plugins, MCP and account calls, so it was not taken.
//
// Nothing here changes a request. No failover, compaction or masking yet:
// those are Claude Code features built on Anthropic's wire format.
package codex

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/andrewbakercloudscale/claude-burst/internal/ctxview"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/andrewbakercloudscale/claude-burst/internal/metrics"
	"github.com/andrewbakercloudscale/claude-burst/internal/notice"
)

// alertLimit is the notice kind for the ChatGPT plan's usage limit, so the
// "back within limits" OK resolves the warning it follows.
const alertLimit = "codex-limit"

// Limits is the plan usage ChatGPT last reported, from the x-codex-*
// headers on a /responses reply: the same numbers Codex's own /status shows.
type Limits struct {
	Seen    time.Time         `json:"seen"`
	Headers map[string]string `json:"headers"`
}

// Gateway is the Codex listener's handler.
type Gateway struct {
	upstream *url.URL
	proxy    *httputil.ReverseProxy
	metrics  *metrics.Writer
	logger   *log.Logger

	inFlight atomic.Int64

	mu      sync.Mutex
	limits  Limits
	limited bool
	last    time.Time
	// windows is each model's context window, in tokens, from the model
	// list ChatGPT sends Codex when a session starts.
	windows map[string]int64

	statePath string

	inspect  inspectStore
	removals *ctxview.Store

	// Listener state, set by whoever serves the gateway: bound, or why not.
	// "starting" until the first report.
	listening bool
	listenErr string

	refused Refused
	// failures is the latest of what failed and is not a turn, newest
	// last: a refused connection, a model list ChatGPT would not give.
	failures []metrics.Event
}

// SetListenState records whether the gateway's port is bound, and why not.
func (g *Gateway) SetListenState(bound bool, why string) {
	g.mu.Lock()
	g.listening, g.listenErr = bound, why
	g.mu.Unlock()
}

// ListenState reports whether the port is bound; why is "" while starting.
func (g *Gateway) ListenState() (bound bool, why string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.listening, g.listenErr
}

// persist saves the limits and windows; called with g.mu held. A failure
// costs only the tab's readings after a restart, so it is logged, not fatal.
func (g *Gateway) persist() {
	b, err := json.Marshal(saved{Limits: g.limits, Windows: g.windows, Failures: g.failures})
	if err != nil {
		return
	}
	tmp := g.statePath + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err == nil {
		err = os.Rename(tmp, g.statePath)
		if err == nil {
			return
		}
	}
	g.logger.Printf("codex: could not save %s", g.statePath)
}

type startKey struct{}

// saved is what outlives a restart: the plan limits and context windows
// last reported, so the Codex tab is not blank until Codex next sends.
type saved struct {
	Limits  Limits           `json:"limits"`
	Windows map[string]int64 `json:"windows"`
	// Failures are kept so the requests table still shows them after a
	// restart; a turn's are in codex-metrics.jsonl already.
	Failures []metrics.Event `json:"failures,omitempty"`
}

// New builds the gateway; metricsPath is where each Codex turn is recorded,
// and codex-state.json beside it keeps the last limits and windows.
func New(upstream, metricsPath string, logger *log.Logger) (*Gateway, error) {
	u, err := url.Parse(upstream)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("codex upstream %q is not a URL", upstream)
	}
	g := &Gateway{upstream: u, metrics: metrics.New(metricsPath), logger: logger,
		statePath: filepath.Join(filepath.Dir(metricsPath), "codex-state.json"),
		removals:  ctxview.Shared(ctxview.RemovalsPath(filepath.Dir(metricsPath)))}
	if b, err := os.ReadFile(g.statePath); err == nil {
		var sv saved
		if json.Unmarshal(b, &sv) == nil {
			g.limits, g.windows, g.failures = sv.Limits, sv.Windows, sv.Failures
		}
	}
	g.proxy = &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(u)
			pr.Out.Host = u.Host
			// Rewrite adds X-Forwarded-* only when asked; none are wanted:
			// chatgpt.com should see what Codex itself would have sent.
			// A turn's reply is read on the way back, so it must arrive
			// uncompressed: with no Accept-Encoding the transport asks for
			// gzip itself and undoes it. Codex sends none today anyway.
			if isTurn(pr.In) {
				pr.Out.Header.Del("Accept-Encoding")
			}
		},
		// Codex streams its replies as server-sent events: every chunk goes
		// on the moment it arrives.
		FlushInterval:  -1,
		ModifyResponse: g.observe,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			// Codex hung up first (it cancels a model list it no longer
			// needs): nothing failed, and nobody is left to answer.
			if errors.Is(err, context.Canceled) && r.Context().Err() != nil {
				g.record(r, metrics.StatusClientClosed, usage{}, "Codex closed the request")
				return
			}
			logger.Printf("codex: %s %s failed: %v", r.Method, r.URL.Path, err)
			g.noteFailure(r, http.StatusBadGateway, "upstream unreachable: "+err.Error())
			g.record(r, http.StatusBadGateway, usage{}, "upstream unreachable: "+err.Error())
			http.Error(w, "Claude Burst could not reach "+u.Host+": "+err.Error(), http.StatusBadGateway)
		},
	}
	return g, nil
}

// InFlight is how many Codex requests are open, for the drain on restart.
func (g *Gateway) InFlight() int64 { return g.inFlight.Load() }

// Limits returns the plan usage ChatGPT last reported.
func (g *Gateway) Limits() Limits {
	g.mu.Lock()
	defer g.mu.Unlock()
	h := make(map[string]string, len(g.limits.Headers))
	for k, v := range g.limits.Headers {
		h[k] = v
	}
	return Limits{Seen: g.limits.Seen, Headers: h}
}

// LastRequest is when Codex last sent anything through the gateway.
func (g *Gateway) LastRequest() time.Time {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.last
}

func (g *Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	g.inFlight.Add(1)
	defer g.inFlight.Add(-1)
	g.mu.Lock()
	g.last = time.Now()
	g.mu.Unlock()
	// A WebSocket is passed through but its turns are not read: logged, so
	// diagnose shows when Codex uses one and the counts would be short.
	if strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		g.logger.Printf("codex: websocket %s (passed through; its turns are not counted)", r.URL.Path)
	}
	r = r.WithContext(context.WithValue(r.Context(), startKey{}, time.Now()))
	if isTurn(r) {
		g.prepareTurn(r)
	}
	g.proxy.ServeHTTP(w, r)
}

// isTurn reports whether a request is a model call: the only ones counted.
func isTurn(r *http.Request) bool {
	return r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/responses")
}

// observe reads a reply on its way back to Codex without holding it up.
func (g *Gateway) observe(resp *http.Response) error {
	r := resp.Request
	if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/models") && resp.StatusCode == http.StatusOK {
		g.noteModels(resp)
		return nil
	}
	if !isTurn(r) {
		// Not counted, but a refusal is logged: on 8 Oct 2026 ChatGPT
		// answered Codex's model list with 401 "token has expired" for two
		// hours and this log had nothing to tell it from a fault of Burst's.
		if resp.StatusCode >= 400 {
			why := snippet(peekBody(resp, 4<<10))
			g.logger.Printf("codex: %s %s answered %d by ChatGPT (passed on to Codex as it came): %s", r.Method, r.URL.Path, resp.StatusCode, why)
			g.noteFailure(r, resp.StatusCode, why)
		}
		return nil
	}
	g.noteLimits(resp.Header)
	if resp.StatusCode >= 400 {
		// An error body is small and read whole: it says why, and the
		// usage-limit refusal is told apart by it.
		b := peekBody(resp, 64<<10)
		g.noteRefusal(resp.StatusCode, resp.Header, b)
		g.record(r, resp.StatusCode, usage{}, snippet(b))
		return nil
	}
	g.noteBackWithin()
	resp.Body = &sseTap{rc: resp.Body, done: func(u usage, completed bool) {
		// A stream that ended without response.completed is a turn that did
		// not finish (Esc in Codex, a dropped connection), never a success.
		switch {
		case completed:
			g.record(r, resp.StatusCode, u, "")
		case r.Context().Err() != nil:
			g.record(r, metrics.StatusClientClosed, u, "Codex closed the turn before it finished")
		default:
			g.record(r, http.StatusBadGateway, u, "the stream ended before the turn finished")
		}
	}}
	return nil
}

// noteModels keeps each model's context window from the model list. The
// list is a few hundred KB of instructions and is read whole, then handed
// on to Codex unchanged.
func (g *Gateway) noteModels(resp *http.Response) {
	b := peekBody(resp, 16<<20)
	if w := ParseWindows(b); len(w) > 0 {
		g.mu.Lock()
		g.windows = w
		g.persist()
		g.mu.Unlock()
	}
}

// ParseWindows reads model context windows from a Codex model list: the
// /models reply, or ~/.codex/models_cache.json, which has the same shape.
func ParseWindows(b []byte) map[string]int64 {
	var list struct {
		Models []struct {
			Slug          string `json:"slug"`
			ContextWindow int64  `json:"context_window"`
		} `json:"models"`
	}
	if json.Unmarshal(b, &list) != nil {
		return nil
	}
	w := map[string]int64{}
	for _, m := range list.Models {
		if m.Slug != "" && m.ContextWindow > 0 {
			w[m.Slug] = m.ContextWindow
		}
	}
	return w
}

// Windows returns the context windows learned from the model list.
func (g *Gateway) Windows() map[string]int64 {
	g.mu.Lock()
	defer g.mu.Unlock()
	w := make(map[string]int64, len(g.windows))
	for k, v := range g.windows {
		w[k] = v
	}
	return w
}

func (g *Gateway) noteLimits(h http.Header) {
	got := map[string]string{}
	for k, v := range h {
		lk := strings.ToLower(k)
		// turn-state is an opaque token Codex hands back on its next turn,
		// not a reading, and is no business of the dashboard's.
		if strings.HasPrefix(lk, "x-codex-") && lk != "x-codex-turn-state" && len(v) > 0 {
			got[lk] = v[0]
		}
	}
	if len(got) == 0 {
		return
	}
	g.mu.Lock()
	g.limits = Limits{Seen: time.Now(), Headers: got}
	g.persist()
	g.mu.Unlock()
}

// noteRefusal raises the on-screen alert when ChatGPT refuses for the
// plan's usage limit: Codex itself only says so inside its own window.
func (g *Gateway) noteRefusal(status int, h http.Header, body []byte) {
	if status != http.StatusTooManyRequests && !bytes.Contains(body, []byte("usage_limit")) {
		return
	}
	detail := "ChatGPT refused Codex's request for the plan's usage limit."
	var e struct {
		Error struct {
			Type            string `json:"type"`
			Message         string `json:"message"`
			ResetsInSeconds int64  `json:"resets_in_seconds"`
			ResetsAt        int64  `json:"resets_at"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &e) == nil {
		switch {
		case e.Error.ResetsAt > 0:
			detail += " It resets at " + time.Unix(e.Error.ResetsAt, 0).Local().Format("15:04 on Mon 2 Jan") + "."
		case e.Error.ResetsInSeconds > 0:
			detail += " It resets at " + time.Now().Add(time.Duration(e.Error.ResetsInSeconds)*time.Second).Format("15:04 on Mon 2 Jan") + "."
		}
	} else if s := h.Get("Retry-After"); s != "" {
		if n, err := strconv.Atoi(s); err == nil {
			detail += " It resets at " + time.Now().Add(time.Duration(n)*time.Second).Format("15:04") + "."
		}
	}
	g.mu.Lock()
	first := !g.limited
	g.limited = true
	g.mu.Unlock()
	if first {
		g.logger.Printf("codex: usage limit reached (HTTP %d)", status)
		notice.Publish(alertLimit, notice.Warn, "Codex hit its ChatGPT usage limit", detail)
	}
}

func (g *Gateway) noteBackWithin() {
	g.mu.Lock()
	was := g.limited
	g.limited = false
	g.mu.Unlock()
	if was {
		notice.Publish(alertLimit, notice.OK, "Codex is answering again", "ChatGPT accepted a Codex request after the usage limit.")
	}
}

func snippet(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 300 {
		s = s[:300] + "..."
	}
	return s
}

// usage is what a finished turn reports in its response.completed event.
type usage struct {
	// Unknown: the turn completed but its usage could not be read.
	Unknown bool
	Model   string
	Input   int64 // including cached, as OpenAI counts it
	Cached  int64
	Output  int64
}

// maxFailures is how many failures that are not turns are kept.
const maxFailures = 50

// Failures returns what failed and is not a turn, oldest first. They are
// kept apart from the turns: no count, error rate or check reads them, so
// a model list refused every few minutes cannot pass for failing turns.
func (g *Gateway) Failures() []metrics.Event {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]metrics.Event(nil), g.failures...)
}

// noteFailure keeps a request that failed and is not a turn, with which
// Codex client sent it: the desktop app and its helpers sign in apart.
func (g *Gateway) noteFailure(r *http.Request, status int, why string) {
	if isTurn(r) {
		return
	}
	start, _ := r.Context().Value(startKey{}).(time.Time)
	if start.IsZero() {
		start = time.Now()
	}
	note := "not a model call: " + r.Method + " " + r.URL.Path
	if o := r.Header.Get("Originator"); o != "" {
		note += ", sent by " + o
	}
	g.keepFailure(metrics.Event{
		Time:        start,
		RequestID:   r.Header.Get("X-Client-Request-Id"),
		SessionID:   r.Header.Get("Session-Id"),
		Slot:        "primary",
		Route:       "PRIMARY",
		Destination: g.upstream.Scheme + "://" + g.upstream.Host + r.URL.Path,
		HTTPStatus:  status,
		DurationMS:  time.Since(start).Milliseconds(),
		Note:        note + ": " + why,
	})
}

func (g *Gateway) keepFailure(e metrics.Event) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.failures = append(g.failures, e)
	if n := len(g.failures) - maxFailures; n > 0 {
		g.failures = append([]metrics.Event(nil), g.failures[n:]...)
	}
	g.persist()
}

func (g *Gateway) record(r *http.Request, status int, u usage, note string) {
	if !isTurn(r) {
		return
	}
	start, _ := r.Context().Value(startKey{}).(time.Time)
	if start.IsZero() {
		start = time.Now()
	}
	uncached := u.Input - u.Cached
	if uncached < 0 {
		uncached = 0
	}
	e := metrics.Event{
		Time:            start,
		RequestID:       r.Header.Get("X-Client-Request-Id"),
		SessionID:       r.Header.Get("Session-Id"),
		Slot:            "primary",
		Route:           "PRIMARY",
		Model:           u.Model,
		Destination:     g.upstream.Scheme + "://" + g.upstream.Host + r.URL.Path,
		HTTPStatus:      status,
		DurationMS:      time.Since(start).Milliseconds(),
		InputTokens:     uncached,
		OutputTokens:    u.Output,
		CacheReadTokens: u.Cached,
		Note:            note,
	}
	if u.Unknown && e.Note == "" {
		e.Note = "tokens unknown: the completed event was too large to read whole and its usage was not found"
	}
	// One line per turn, metadata only, so the gateway log (and with it
	// scripts/diagnose.sh) shows Codex's traffic beside Claude Code's.
	// req is the id codex-metrics.jsonl has as request_id, so a turn's line
	// here and its record there are one lookup apart.
	g.logger.Printf("codex: turn req=%s status=%d model=%q in=%d cached=%d out=%d ms=%d session=%s", e.RequestID, status, u.Model, uncached, u.Cached, u.Output, e.DurationMS, e.SessionID)
	if err := g.metrics.Write(e); err != nil {
		g.logger.Printf("codex: metrics write failed: %v", err)
	}
}

// sseTap passes a streamed reply through unchanged while watching it for
// the response.completed event, which carries the turn's model and usage.
// Lines are only collected up to maxLine; a longer one (a huge output item)
// is let through unread, since usage never travels in one.
type sseTap struct {
	rc   io.ReadCloser
	line []byte
	skip bool
	u    usage
	got  bool
	done func(u usage, completed bool)
	once sync.Once
	// An oversized line (a completed event carrying a huge output) is not
	// held whole: its start (with the model) is kept in line, its last
	// tailMax bytes in tail, and usage is decoded from whichever holds it.
	tail []byte
}

const (
	maxLine = 4 << 20
	tailMax = 256 << 10
)

func (t *sseTap) Read(p []byte) (int, error) {
	n, err := t.rc.Read(p)
	t.scan(p[:n])
	if err != nil {
		t.finish()
	}
	return n, err
}

func (t *sseTap) Close() error {
	t.finish()
	return t.rc.Close()
}

func (t *sseTap) finish() { t.once.Do(func() { t.done(t.u, t.got) }) }

func (t *sseTap) scan(b []byte) {
	for len(b) > 0 {
		i := bytes.IndexByte(b, '\n')
		if i < 0 {
			t.add(b)
			return
		}
		t.add(b[:i])
		t.endLine()
		b = b[i+1:]
	}
}

func (t *sseTap) add(b []byte) {
	if !t.skip && len(t.line)+len(b) > maxLine {
		t.skip = true
		t.tail = t.tail[:0]
	}
	if !t.skip {
		t.line = append(t.line, b...)
		return
	}
	t.tail = append(t.tail, b...)
	if len(t.tail) > 2*tailMax {
		t.tail = append(t.tail[:0], t.tail[len(t.tail)-tailMax:]...)
	}
}

var (
	usageKey = []byte(`"usage":`)
	modelRe  = regexp.MustCompile(`"model"\s*:\s*"([^"]+)"`)
)

// endBigLine reads an oversized completed event from its kept start and end.
func (t *sseTap) endBigLine() {
	head, tail := t.line, t.tail
	t.line, t.tail, t.skip = t.line[:0], t.tail[:0], false
	if !bytes.Contains(head, []byte(`"response.completed"`)) {
		return
	}
	t.got = true // a completed turn, whether or not its usage is found
	if m := modelRe.FindSubmatch(head); m != nil {
		t.u.Model = string(m[1])
	}
	for _, part := range [][]byte{tail, head} {
		i := bytes.LastIndex(part, usageKey)
		if i < 0 {
			continue
		}
		var u struct {
			InputTokens        int64 `json:"input_tokens"`
			OutputTokens       int64 `json:"output_tokens"`
			InputTokensDetails struct {
				CachedTokens int64 `json:"cached_tokens"`
			} `json:"input_tokens_details"`
		}
		if json.NewDecoder(bytes.NewReader(part[i+len(usageKey):])).Decode(&u) == nil {
			t.u.Input, t.u.Output, t.u.Cached = u.InputTokens, u.OutputTokens, u.InputTokensDetails.CachedTokens
			return
		}
	}
	t.u.Unknown = true
}

func (t *sseTap) endLine() {
	if t.skip {
		t.endBigLine()
		return
	}
	line := t.line
	t.line = t.line[:0]
	data, ok := bytes.CutPrefix(bytes.TrimRight(line, "\r"), []byte("data: "))
	if !ok || !bytes.Contains(data, []byte(`"response.completed"`)) {
		return
	}
	var ev struct {
		Type     string `json:"type"`
		Response struct {
			Model string `json:"model"`
			Usage struct {
				InputTokens        int64 `json:"input_tokens"`
				OutputTokens       int64 `json:"output_tokens"`
				InputTokensDetails struct {
					CachedTokens int64 `json:"cached_tokens"`
				} `json:"input_tokens_details"`
			} `json:"usage"`
		} `json:"response"`
	}
	if json.Unmarshal(data, &ev) != nil || ev.Type != "response.completed" {
		return
	}
	t.u = usage{
		Model:  ev.Response.Model,
		Input:  ev.Response.Usage.InputTokens,
		Cached: ev.Response.Usage.InputTokensDetails.CachedTokens,
		Output: ev.Response.Usage.OutputTokens,
	}
	t.got = true
}

// peekBody returns up to n bytes of a reply for inspection and puts them
// back in front of the rest, so the client still gets the whole body,
// however long, at the length its Content-Length promised.
func peekBody(resp *http.Response, n int64) []byte {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, n))
	resp.Body = struct {
		io.Reader
		io.Closer
	}{io.MultiReader(bytes.NewReader(b), resp.Body), resp.Body}
	return b
}
