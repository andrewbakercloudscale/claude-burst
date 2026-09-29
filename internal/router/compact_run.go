package router

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/andrewbakercloudscale/claude-burst/internal/config"
)

// The runtime half of proxy-side compaction; compact.go has the pure
// history rewrite and the reasoning.

// compactionSummaryPrompt is Anthropic's recommended client-side compaction
// prompt (claude-api skill, model-migration.md). Its last sentence matters:
// the summary request carries the session's tools, and without it the
// model occasionally calls one instead of writing the summary.
const compactionSummaryPrompt = "Summarize the transcript inside <summary></summary> tags. Include relevant information in the summary such that this conversation will be continued by a new context window without needing to redo work or be reprovided with relevant constraints or context. Be sure to preserve: (1) any difficulties or problems that came up, and how they were handled or resolved; (2) any possibilities, options, or approaches that were raised, tried, or set aside, and why; (3) anything that was asked for, decided, agreed, ruled out, or established as a preference, constraint, or boundary - stated exactly; (4) exactly where things stand now - what has been covered, settled, or completed so far; (5) anything still open, unresolved, promised, or expected to happen next; (6) specific details that would be hard to reconstruct - names, numbers, dates, exact wording, links or references - kept exactly. Be complete on these even at the cost of length; keep everything else concise. Weight the two voices differently: keep what the user said, asked for, shared, or established carefully and close to their own words; your own explanations and reasoning can be condensed much further, to what they concluded or produced - as long as nothing in the six items above is dropped. Do not call any tools while writing this summary; respond with text only."

// minSummarisedShare is the least share of a session's bytes a compaction
// must summarise to be worth a summary call and a cache rewrite.
const minSummarisedShare = 0.30

// summaryTimeout bounds one summary call: a 400k-token read plus a few
// thousand tokens of summary.
const summaryTimeout = 5 * time.Minute

type compactState struct {
	lastContext int64
	warnedAt    time.Time
	startedAt   time.Time // last compaction started; the window runs from here
	skippedAt   time.Time // last "nothing to summarise" log line, to keep it to one per window
	pending     bool
	summary     string
	p0          int    // original messages [0, p0) are summarised
	hash        string // prefixHash of those messages
	swapAt      int    // message count when the swap was first applied; 0 = not yet
	seen        time.Time
}

type compactor struct {
	mu       sync.Mutex
	cfg      config.CompactionConfig
	sessions map[string]*compactState // keyed by session id + "|" + model
	path     string                   // where sessions survive a restart; "" = memory only
	logger   *log.Logger
}

func newCompactor(c config.CompactionConfig, path string, logger *log.Logger) *compactor {
	cp := &compactor{cfg: c.Resolved(), sessions: map[string]*compactState{}, path: path, logger: logger}
	cp.load()
	return cp
}

// savedCompaction is compactState on disk. Without it every deploy threw
// away every session's summary and window: a summary that cost a full read
// of a 500k context was lost one restart before it applied, and the next
// request paid for it again.
type savedCompaction struct {
	LastContext int64     `json:"last_context"`
	WarnedAt    time.Time `json:"warned_at"`
	StartedAt   time.Time `json:"started_at"`
	Pending     bool      `json:"pending,omitempty"`
	Summary     string    `json:"summary,omitempty"`
	P0          int       `json:"p0,omitempty"`
	Hash        string    `json:"hash,omitempty"`
	SwapAt      int       `json:"swap_at,omitempty"`
	Seen        time.Time `json:"seen"`
}

// savedTTL drops sessions not seen for this long when state is saved.
const savedTTL = 48 * time.Hour

func (c *compactor) load() {
	if c.path == "" {
		return
	}
	b, err := os.ReadFile(c.path)
	if err != nil {
		return
	}
	var saved map[string]savedCompaction
	if err := json.Unmarshal(b, &saved); err != nil {
		if c.logger != nil {
			c.logger.Printf("error stage=compaction_load path=%s err=%v (starting with no compaction state)", c.path, err)
		}
		return
	}
	for k, v := range saved {
		st := &compactState{lastContext: v.LastContext, warnedAt: v.WarnedAt, startedAt: v.StartedAt,
			summary: v.Summary, p0: v.P0, hash: v.Hash, swapAt: v.SwapAt, seen: v.Seen}
		// A summary in flight died with the old process. Reopen its window
		// so the next prompt starts another, rather than waiting an hour
		// for a result that will never arrive.
		if v.Pending {
			st.startedAt = time.Time{}
		}
		c.sessions[k] = st
	}
}

// save writes every live session to disk. Called with c.mu held, only on
// transitions (start, ready, failed, applied, dropped), never per request.
func (c *compactor) save() {
	if c.path == "" {
		return
	}
	now := time.Now()
	out := map[string]savedCompaction{}
	for k, st := range c.sessions {
		if !st.seen.IsZero() && now.Sub(st.seen) > savedTTL {
			delete(c.sessions, k)
			continue
		}
		out[k] = savedCompaction{LastContext: st.lastContext, WarnedAt: st.warnedAt, StartedAt: st.startedAt, Pending: st.pending,
			Summary: st.summary, P0: st.p0, Hash: st.hash, SwapAt: st.swapAt, Seen: st.seen}
	}
	b, err := json.Marshal(out)
	if err == nil {
		tmp := c.path + ".tmp"
		if err = os.WriteFile(tmp, b, 0600); err == nil {
			err = os.Rename(tmp, c.path)
		}
	}
	if err != nil && c.logger != nil {
		c.logger.Printf("error stage=compaction_save path=%s err=%v", c.path, err)
	}
}

func (c *compactor) state(key string) *compactState {
	st := c.sessions[key]
	if st == nil {
		st = &compactState{}
		c.sessions[key] = st
	}
	return st
}

// compactInfo rides on the inbound request's context from applyCompaction
// to forward and writeMetric.
type compactInfo struct {
	key          string
	applied      bool
	removedMsgs  int
	removedBytes int64
}

type compactInfoKey struct{}

func compactInfoFrom(ctx context.Context) compactInfo {
	ci, _ := ctx.Value(compactInfoKey{}).(compactInfo)
	return ci
}

// SetCompaction applies c to the running gateway; the admin page's toggle.
func (s *Server) SetCompaction(c config.CompactionConfig) {
	s.compaction.mu.Lock()
	s.compaction.cfg = c.Resolved()
	s.compaction.mu.Unlock()
}

// compactionReady reports whether a summary is waiting to be applied, or
// applied, for any model of session sid.
func (s *Server) compactionReady(sid string) bool {
	s.compaction.mu.Lock()
	defer s.compaction.mu.Unlock()
	for k, st := range s.compaction.sessions {
		if strings.HasPrefix(k, sid+"|") && st.summary != "" {
			return true
		}
	}
	return false
}

// noteSessionContext records the context a primary response reported for
// its session and model: what the next request is judged by.
func (s *Server) noteSessionContext(in *http.Request, tok tokenUsage) {
	ci := compactInfoFrom(in.Context())
	ctxTokens := tok.input + tok.cacheRead + tok.cacheWrite
	if ci.key == "" || ctxTokens <= 0 {
		return
	}
	s.compaction.mu.Lock()
	s.compaction.state(ci.key).lastContext = ctxTokens
	s.compaction.mu.Unlock()
}

// applyCompaction returns the body to send for an inference request and the
// request with compactInfo on its context. It warns, starts a summary in
// the background when a session crosses the threshold, and applies a ready
// summary. Anything it does not understand goes through untouched.
func (s *Server) applyCompaction(in *http.Request, body []byte) ([]byte, *http.Request) {
	s.compaction.mu.Lock()
	cfg := s.compaction.cfg
	s.compaction.mu.Unlock()
	sid := in.Header.Get("x-claude-code-session-id")
	if !cfg.Enabled || sid == "" {
		return body, in
	}
	var top map[string]json.RawMessage
	if json.Unmarshal(body, &top) != nil {
		return body, in
	}
	var msgs []json.RawMessage
	if json.Unmarshal(top["messages"], &msgs) != nil || len(msgs) == 0 {
		return body, in
	}
	key := sid + "|" + requestModel(body)
	ci := compactInfo{key: key}
	now := time.Now()
	window := time.Duration(cfg.WindowMinutes) * time.Minute
	rid := requestIDFrom(in.Context())

	s.compaction.mu.Lock()
	st := s.compaction.state(key)
	st.seen = now
	dirty := false

	// A summary only fits the history it was made from.
	if st.summary != "" && (len(msgs) <= st.p0 || prefixHash(msgs, st.p0) != st.hash) {
		s.logger.Printf("req=%s compaction dropped session=%s: history no longer matches (cleared, compacted or rewound)", rid, key)
		st.summary, st.hash, st.p0, st.swapAt = "", "", 0, 0
		dirty = true
	}

	if st.lastContext >= cfg.WarnAtTokens && (st.warnedAt.IsZero() || now.Sub(st.warnedAt) >= window) {
		st.warnedAt = now
		s.logger.Printf("req=%s warn stage=compaction session=%s context=%dk (warn at %dk, compact at %dk)",
			rid, key, st.lastContext/1000, cfg.WarnAtTokens/1000, cfg.CompactAtTokens/1000)
	}

	bounds := promptBoundaries(msgs)
	fresh := endsInPrompt(msgs)

	// The history as the model currently sees it: rewritten when a swap is
	// in force. A second compaction summarises this view, old summary and
	// all, so its summary covers everything before its boundary.
	view, offset := msgs, 0
	if st.summary != "" && st.swapAt > 0 {
		view, offset = rewriteWithSummary(msgs, st.summary, st.p0, st.swapAt), st.p0
	}

	if st.lastContext >= cfg.CompactAtTokens && !st.pending && (st.startedAt.IsZero() || now.Sub(st.startedAt) >= window) {
		// Only a compaction that actually starts opens the window. A skip
		// (no boundary yet, typically one long prompt) must leave the next
		// prompt free to compact.
		p, cut := compactionBoundary(view, bounds, offset)
		if cut > 0 {
			st.startedAt = now
			st.pending = true
			dirty = true
			prefix := append([]json.RawMessage(nil), view[:cut]...)
			hash := prefixHash(msgs, p)
			s.logger.Printf("req=%s compaction start session=%s context=%dk summarising %d of %d messages", rid, key, st.lastContext/1000, p, len(msgs))
			go s.summarise(in.Clone(context.Background()), top, prefix, key, p, hash)
		} else if st.skippedAt.IsZero() || now.Sub(st.skippedAt) >= window {
			st.skippedAt = now
			s.logger.Printf("req=%s compaction skipped session=%s context=%dk: no prompt boundary leaves at least %.0f%% to summarise",
				rid, key, st.lastContext/1000, minSummarisedShare*100)
		}
	}

	if st.summary != "" && st.swapAt == 0 && !fresh {
		s.logger.Printf("req=%s compaction waiting session=%s: summary ready, request ends in %s, applies at the next plain prompt",
			rid, key, lastMessageShape(msgs))
	}
	if st.summary == "" || (st.swapAt == 0 && !fresh) {
		if dirty {
			s.compaction.save()
		}
		s.compaction.mu.Unlock()
		return body, in.WithContext(context.WithValue(in.Context(), compactInfoKey{}, ci))
	}
	if st.swapAt == 0 {
		st.swapAt = len(msgs)
		s.logger.Printf("req=%s compaction applied session=%s: %d messages replaced by a summary", rid, key, st.p0)
		dirty = true
	}
	out := rewriteWithSummary(msgs, st.summary, st.p0, st.swapAt)
	if dirty {
		s.compaction.save()
	}
	s.compaction.mu.Unlock()

	nb, err := json.Marshal(out)
	if err != nil {
		return body, in
	}
	top["messages"] = nb
	newBody, err := json.Marshal(top)
	if err != nil {
		return body, in
	}
	ci.applied, ci.removedMsgs, ci.removedBytes = true, len(msgs)-len(out), int64(len(body)-len(newBody))
	return newBody, in.WithContext(context.WithValue(in.Context(), compactInfoKey{}, ci))
}

// compactionBoundary picks the latest plain prompt past offset that leaves
// at least minSummarisedShare of the view's bytes before it. It returns the
// boundary in original message indexes and the cut in view indexes, or 0, 0.
func compactionBoundary(view []json.RawMessage, bounds []int, offset int) (p, cut int) {
	total := 0
	for _, m := range view {
		total += len(m)
	}
	for i := len(bounds) - 1; i >= 0; i-- {
		c := bounds[i] - offset
		if c <= 0 {
			break
		}
		before := 0
		for _, m := range view[:c] {
			before += len(m)
		}
		if float64(before) >= minSummarisedShare*float64(total) {
			return bounds[i], c
		}
	}
	return 0, 0
}

// summarise makes one summary request through the primary, with the
// session's own auth, model, system prompt and tools, and stores the
// summary for key when it succeeds.
func (s *Server) summarise(in *http.Request, top map[string]json.RawMessage, prefix []json.RawMessage, key string, p0 int, hash string) {
	summary, err := s.requestSummary(in, top, prefix)
	s.compaction.mu.Lock()
	defer s.compaction.mu.Unlock()
	st := s.compaction.state(key)
	st.pending = false
	defer s.compaction.save()
	if err != nil {
		s.logger.Printf("compaction failed session=%s: %v (next attempt after the window)", key, err)
		return
	}
	st.summary, st.p0, st.hash, st.swapAt = summary, p0, hash, 0
	s.logger.Printf("compaction summary ready session=%s: %d messages summarised into %d characters; applies from the next plain prompt", key, p0, len(summary))
}

func (s *Server) requestSummary(in *http.Request, top map[string]json.RawMessage, prefix []json.RawMessage) (string, error) {
	req := map[string]json.RawMessage{}
	for _, k := range []string{"model", "system", "tools", "thinking", "metadata"} {
		if v, ok := top[k]; ok {
			req[k] = v
		}
	}
	prefix = withCacheBreakpoint(prefix, req)
	instr, _ := json.Marshal(map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": compactionSummaryPrompt}}})
	msgs, _ := json.Marshal(append(prefix, instr))
	req["messages"] = msgs
	req["max_tokens"] = json.RawMessage("16000")
	req["stream"] = json.RawMessage("true")
	body, err := json.Marshal(req)
	if err != nil {
		return "", err
	}

	ctx, cancel := context.WithTimeout(context.Background(), summaryTimeout)
	defer cancel()
	in = in.WithContext(context.WithValue(ctx, requestIDKey, newRequestID()))
	start := time.Now()
	out, model, err := s.primary.Prepare(ctx, in, body)
	if err != nil {
		return "", fmt.Errorf("build summary request: %w", err)
	}
	dest := out.URL.Scheme + "://" + out.URL.Host + out.URL.Path
	resp, err := s.client.Do(out)
	if err != nil {
		s.writeMetric(in, "primary", s.primary.Name(), model, model, 0, start, tokenUsage{}, "", 0, "compaction summary failed: "+err.Error(), dest)
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
		s.writeMetric(in, "primary", s.primary.Name(), model, model, resp.StatusCode, start, tokenUsage{}, "", 0, "compaction summary rejected: "+errorExcerpt(b), dest)
		return "", fmt.Errorf("summary request: status %d: %s", resp.StatusCode, errorExcerpt(b))
	}
	text, stop, tok := readSSEText(resp.Body)
	s.writeMetric(in, "primary", s.primary.Name(), model, model, resp.StatusCode, start, tok, "", 0, "compaction summary", dest)
	summary := summaryFromText(text)
	switch {
	case stop == "tool_use":
		return "", fmt.Errorf("the model called a tool instead of writing the summary")
	case stop == "max_tokens":
		return "", fmt.Errorf("the summary was cut off at max_tokens")
	case summary == "":
		return "", fmt.Errorf("empty summary (stop_reason %q)", stop)
	}
	return summary, nil
}

// withCacheBreakpoint marks the prefix's last cacheable block so the summary
// call reads the session's history from cache instead of paying for it in
// full. Claude Code's own marker sits on the last message of each request,
// which the prefix cuts off, so without this the first live summary read
// 28k of 532k tokens from cache and paid full input price for the rest
// ($2.10 where ~$0.25 was available). The API looks back up to 20 blocks
// from a breakpoint for an existing entry, and the entry Claude Code wrote
// for the turn before the boundary is a few blocks away. Left alone when
// the request already carries the API's maximum of four breakpoints, and a
// thinking block is never marked (the API rejects that).
func withCacheBreakpoint(prefix []json.RawMessage, req map[string]json.RawMessage) []json.RawMessage {
	if len(prefix) == 0 {
		return prefix
	}
	used := 0
	for _, k := range []string{"system", "tools"} {
		used += strings.Count(string(req[k]), `"cache_control"`)
	}
	for _, m := range prefix {
		used += strings.Count(string(m), `"cache_control"`)
	}
	if used >= 4 {
		return prefix
	}
	var msg map[string]any
	last := prefix[len(prefix)-1]
	if json.Unmarshal(last, &msg) != nil {
		return prefix
	}
	blocks := contentBlocks(msg["content"])
	for i := len(blocks) - 1; i >= 0; i-- {
		b, ok := blocks[i].(map[string]any)
		if !ok {
			continue
		}
		switch b["type"] {
		case "thinking", "redacted_thinking":
			continue
		case "text":
			if t, _ := b["text"].(string); t == "" {
				continue
			}
		}
		b["cache_control"] = map[string]any{"type": "ephemeral"}
		msg["content"] = blocks
		nb, err := json.Marshal(msg)
		if err != nil {
			return prefix
		}
		out := append([]json.RawMessage(nil), prefix...)
		out[len(out)-1] = nb
		return out
	}
	return prefix
}

// readSSEText collects a streamed Messages response's text, stop reason and
// usage.
func readSSEText(r io.Reader) (text, stop string, tok tokenUsage) {
	var sb strings.Builder
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 8*1024*1024)
	for sc.Scan() {
		line := sc.Text()
		parseSSEUsage(line, &tok)
		raw := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "data:"))
		if !strings.HasPrefix(strings.TrimSpace(line), "data:") || raw == "" {
			continue
		}
		var ev struct {
			Type  string `json:"type"`
			Delta struct {
				Type       string `json:"type"`
				Text       string `json:"text"`
				StopReason string `json:"stop_reason"`
			} `json:"delta"`
		}
		if json.Unmarshal([]byte(raw), &ev) != nil {
			continue
		}
		switch {
		case ev.Type == "content_block_delta" && ev.Delta.Type == "text_delta":
			sb.WriteString(ev.Delta.Text)
		case ev.Type == "message_delta" && ev.Delta.StopReason != "":
			stop = ev.Delta.StopReason
		}
	}
	return sb.String(), stop, tok
}

// CompactionSession is one tracked session, for the admin page.
type CompactionSession struct {
	Session     string    `json:"session"`
	Model       string    `json:"model"`
	Context     int64     `json:"context"`
	State       string    `json:"state"`
	CompactedAt time.Time `json:"compacted_at,omitempty"`
	Summarised  int       `json:"summarised_messages,omitempty"`
}

// CompactionSessions lists tracked sessions, largest context first.
func (s *Server) CompactionSessions() []CompactionSession {
	s.compaction.mu.Lock()
	defer s.compaction.mu.Unlock()
	cfg := s.compaction.cfg
	var out []CompactionSession
	for k, st := range s.compaction.sessions {
		if st.lastContext == 0 && st.summary == "" && !st.pending {
			continue
		}
		sid, model, _ := strings.Cut(k, "|")
		state := "ok"
		switch {
		case st.pending:
			state = "summarising"
		case st.summary != "" && st.swapAt > 0:
			state = "compacted"
		case st.summary != "":
			state = "summary ready, applies at next prompt"
		case st.lastContext >= cfg.CompactAtTokens && !st.startedAt.IsZero() && time.Since(st.startedAt) < time.Duration(cfg.WindowMinutes)*time.Minute:
			state = "over threshold, next compaction after " + st.startedAt.Add(time.Duration(cfg.WindowMinutes)*time.Minute).Format("15:04")
		case st.lastContext >= cfg.CompactAtTokens:
			state = "over threshold, compacts at the next prompt"
		case st.lastContext >= cfg.WarnAtTokens:
			state = "warning"
		}
		cs := CompactionSession{Session: sid, Model: model, Context: st.lastContext, State: state, Summarised: st.p0}
		if !st.startedAt.IsZero() {
			cs.CompactedAt = st.startedAt
		}
		out = append(out, cs)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Context > out[j].Context })
	return out
}

// lastMessageShape names the last message's role and block types, for the
// log: why a ready summary was not applied to this request.
func lastMessageShape(msgs []json.RawMessage) string {
	if len(msgs) == 0 {
		return "no messages"
	}
	return messageSkeleton([]byte(`{"messages":[` + string(msgs[len(msgs)-1]) + `]}`))
}

// compactionStatePath keeps compaction state beside the overflow state file.
func compactionStatePath(statePath string) string {
	if statePath == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(statePath), "compaction-state.json")
}
