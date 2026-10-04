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
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
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
}

// persist saves the limits and windows; called with g.mu held. A failure
// costs only the tab's readings after a restart, so it is logged, not fatal.
func (g *Gateway) persist() {
	b, err := json.Marshal(saved{Limits: g.limits, Windows: g.windows})
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
}

// New builds the gateway; metricsPath is where each Codex turn is recorded,
// and codex-state.json beside it keeps the last limits and windows.
func New(upstream, metricsPath string, logger *log.Logger) (*Gateway, error) {
	u, err := url.Parse(upstream)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("codex upstream %q is not a URL", upstream)
	}
	g := &Gateway{upstream: u, metrics: metrics.New(metricsPath), logger: logger,
		statePath: filepath.Join(filepath.Dir(metricsPath), "codex-state.json")}
	if b, err := os.ReadFile(g.statePath); err == nil {
		var sv saved
		if json.Unmarshal(b, &sv) == nil {
			g.limits, g.windows = sv.Limits, sv.Windows
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
			logger.Printf("codex: %s %s failed: %v", r.Method, r.URL.Path, err)
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
	r = r.WithContext(context.WithValue(r.Context(), startKey{}, time.Now()))
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
		return nil
	}
	g.noteLimits(resp.Header)
	if resp.StatusCode >= 400 {
		// An error body is small and read whole: it says why, and the
		// usage-limit refusal is told apart by it.
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		resp.Body.Close()
		resp.Body = io.NopCloser(bytes.NewReader(b))
		g.noteRefusal(resp.StatusCode, resp.Header, b)
		g.record(r, resp.StatusCode, usage{}, snippet(b))
		return nil
	}
	g.noteBackWithin()
	resp.Body = &sseTap{rc: resp.Body, done: func(u usage) { g.record(r, resp.StatusCode, u, "") }}
	return nil
}

// noteModels keeps each model's context window from the model list. The
// list is a few hundred KB of instructions and is read whole, then handed
// on to Codex unchanged.
func (g *Gateway) noteModels(resp *http.Response) {
	b, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	resp.Body.Close()
	resp.Body = io.NopCloser(bytes.NewReader(b))
	if err != nil {
		return
	}
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
	Model  string
	Input  int64 // including cached, as OpenAI counts it
	Cached int64
	Output int64
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
	// One line per turn, metadata only, so the gateway log (and with it
	// scripts/diagnose.sh) shows Codex's traffic beside Claude Code's.
	g.logger.Printf("codex: turn status=%d model=%q in=%d cached=%d out=%d ms=%d session=%s", status, u.Model, uncached, u.Cached, u.Output, e.DurationMS, e.SessionID)
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
	done func(usage)
	once sync.Once
}

const maxLine = 4 << 20

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

func (t *sseTap) finish() { t.once.Do(func() { t.done(t.u) }) }

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
	if t.skip {
		return
	}
	if len(t.line)+len(b) > maxLine {
		t.skip, t.line = true, t.line[:0]
		return
	}
	t.line = append(t.line, b...)
}

func (t *sseTap) endLine() {
	line := t.line
	t.line, t.skip = t.line[:0], false
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
