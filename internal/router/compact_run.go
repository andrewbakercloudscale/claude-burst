package router

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"runtime/debug"
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
const compactionSummaryPrompt = "Summarize the transcript inside <summary></summary> tags. Include relevant information in the summary such that this conversation will be continued by a new context window without needing to redo work or be reprovided with relevant constraints or context. Be sure to preserve: (1) any difficulties or problems that came up, and how they were handled or resolved; (2) any possibilities, options, or approaches that were raised, tried, or set aside, and why; (3) anything that was asked for, decided, agreed, ruled out, or established as a preference, constraint, or boundary - stated exactly; (4) exactly where things stand now - what has been covered, settled, or completed so far; (5) anything still open, unresolved, promised, or expected to happen next; (6) specific details that would be hard to reconstruct - names, numbers, dates, exact wording, links or references - kept exactly. Be complete on these even at the cost of length; keep everything else concise. Weight the two voices differently: keep what the user said, asked for, shared, or established carefully and close to their own words; your own explanations and reasoning can be condensed much further, to what they concluded or produced - as long as nothing in the six items above is dropped. Do not call any tools while writing this summary, even if the conversation above was in the middle of using them: tools are unavailable here and any tool call fails. Respond with text only, beginning with <summary>."

// minSummarisedShare is the least share of a session's bytes a compaction
// must summarise to be worth a summary call and a cache rewrite.
const minSummarisedShare = 0.30

// summaryTimeout bounds one summary call: a 400k-token read plus a few
// thousand tokens of summary.
const summaryTimeout = 5 * time.Minute

// retryAfterFailure is how soon a session tries again after a summary fails.
// On 2026-09-30 a failure meant another hour on a 475k history.
var retryAfterFailure = 5 * time.Minute

type compactState struct {
	lastContext int64
	warnedAt    time.Time
	startedAt   time.Time // last compaction started; the window runs from here
	skippedAt   time.Time // last "nothing to summarise" log line, to keep it to one per window
	pending     bool
	summary     string
	p0          int    // original messages [0, p0) are summarised
	hash        string // prefixHash of those messages
	swapAt      int    // message count when the swap was first applied
	// A newer summary waiting for a plain prompt. The current one stays in
	// force until it swaps: dropping it early sent a session's whole
	// uncompacted history (936k, and a 907k cache write) for five minutes.
	next     string
	nextP0   int
	nextHash string
	// Per-message hashes of the summarised prefix, memory only: when a
	// summary is dropped they say which message changed, so the log names
	// the cause instead of guessing.
	marks, nextMarks, pendingMarks []string
	seen                           time.Time
	// Memory only, for the prompt notice hook: lines not yet shown, whether
	// the waiting summary has been announced, and the context before the
	// latest swap until a response reports the context after it.
	notices     []string
	readyShown  bool
	waitShown   bool // told mid-turn that it waits for the next prompt
	swappedFrom int64
	swappedMsgs int
	// A mid-turn swap the API has not yet answered with a success. Memory
	// only: undo holds what was in force before it, to put back on a 400.
	midTurnUnproven bool
	undo            *swapUndo
}

// swapUndo is the summary state from before a mid-turn swap.
type swapUndo struct {
	summary, hash string
	p0, swapAt    int
	marks         []string
}

// maxNotices bounds a session's unshown notices, for a session whose
// prompts never run the hook.
const maxNotices = 5

// notice queues a line for the session's next prompt. Caller holds mu.
func (st *compactState) notice(format string, a ...any) {
	st.notices = append(st.notices, "\u26a1 Claude Burst, pauseless compaction: "+fmt.Sprintf(format, a...))
	if len(st.notices) > maxNotices {
		st.notices = st.notices[len(st.notices)-maxNotices:]
	}
}

type compactor struct {
	mu       sync.Mutex
	cfg      config.CompactionConfig
	sessions map[string]*compactState // keyed by session id + "|" + model
	path     string                   // where sessions survive a restart; "" = memory only
	logger   *log.Logger
	running  sync.WaitGroup // summary calls in flight
	// midTurnOff: the API rejected a mid-turn swap, so none are tried again
	// until the settings are saved again (SetCompaction). One rejection is
	// taken as the API's answer: retrying each turn would cost a wasted
	// round trip every time to learn the same thing.
	midTurnOff bool
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
	Next        string    `json:"next,omitempty"`
	NextP0      int       `json:"next_p0,omitempty"`
	NextHash    string    `json:"next_hash,omitempty"`
	Seen        time.Time `json:"seen"`
	// The prompt notice's unshown lines and the context before the latest
	// swap. Memory only until 2026-10-01, when a deploy landed between a
	// swap and the next prompt (gateway restarts are routine: six in that
	// hour) and the user saw no notice for it at all.
	Notices     []string `json:"notices,omitempty"`
	SwappedFrom int64    `json:"swapped_from,omitempty"`
	SwappedMsgs int      `json:"swapped_msgs,omitempty"`
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
			summary: v.Summary, p0: v.P0, hash: v.Hash, swapAt: v.SwapAt,
			next: v.Next, nextP0: v.NextP0, nextHash: v.NextHash, seen: v.Seen,
			notices: v.Notices, swappedFrom: v.SwappedFrom, swappedMsgs: v.SwappedMsgs}
		// Saved before summaries waited as next: an unapplied summary.
		if st.summary != "" && st.swapAt == 0 && st.next == "" {
			st.next, st.nextP0, st.nextHash = st.summary, st.p0, st.hash
			st.summary, st.p0, st.hash = "", 0, ""
		}
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
// transitions (start, ready, failed, applied, dropped, the first answer
// after a swap, notices shown), never per request.
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
			Summary: st.summary, P0: st.p0, Hash: st.hash, SwapAt: st.swapAt,
			Next: st.next, NextP0: st.nextP0, NextHash: st.nextHash, Seen: st.seen,
			Notices: st.notices, SwappedFrom: st.swappedFrom, SwappedMsgs: st.swappedMsgs}
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
	// midTurn: this request carries a mid-turn swap the API has not yet
	// accepted. original is what Claude Code sent, to rebuild the request
	// without the swap if the API rejects it.
	midTurn  bool
	original []byte
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
	s.compaction.midTurnOff = false
	s.compaction.mu.Unlock()
}

// MidTurnOff reports whether the API refused a mid-turn swap since the
// compaction settings were last applied.
func (s *Server) MidTurnOff() bool {
	s.compaction.mu.Lock()
	defer s.compaction.mu.Unlock()
	return s.compaction.midTurnOff
}

// rejectMidTurn undoes the unproven mid-turn swap the request in carried,
// after the API answered it with a 400: the summary goes back to waiting
// for the next plain prompt, whatever was in force before is restored, and
// no more mid-turn swaps are tried. It reports whether there was one.
func (s *Server) rejectMidTurn(in *http.Request, reason string) bool {
	ci := compactInfoFrom(in.Context())
	if !ci.midTurn {
		return false
	}
	s.compaction.mu.Lock()
	defer s.compaction.mu.Unlock()
	st := s.compaction.sessions[ci.key]
	s.compaction.midTurnOff = true
	if st == nil || !st.midTurnUnproven || st.undo == nil {
		return false
	}
	st.next, st.nextP0, st.nextHash, st.nextMarks = st.summary, st.p0, st.hash, st.marks
	u := st.undo
	st.summary, st.hash, st.p0, st.swapAt, st.marks = u.summary, u.hash, u.p0, u.swapAt, u.marks
	st.undo, st.midTurnUnproven = nil, false
	st.swappedFrom, st.swappedMsgs = 0, 0
	s.logger.Printf("req=%s compaction mid-turn REJECTED session=%s: the API answered 400 (%s); swap undone, request resent without it, the summary waits for the next plain prompt, and mid-turn swaps are off until the compaction settings are saved again",
		requestIDFrom(in.Context()), ci.key, reason)
	st.notice("the API refused the summary mid-turn, so this turn carries on unchanged; the summary swaps in with your next prompt instead")
	s.compaction.save()
	return true
}

// compactionReady reports whether a summary is waiting to be applied, or
// applied, for any model of session sid.
func (s *Server) compactionReady(sid string) bool {
	s.compaction.mu.Lock()
	defer s.compaction.mu.Unlock()
	for k, st := range s.compaction.sessions {
		if strings.HasPrefix(k, sid+"|") && (st.summary != "" || st.next != "") {
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
	st := s.compaction.state(ci.key)
	st.lastContext = ctxTokens
	if ci.midTurn && st.midTurnUnproven {
		st.midTurnUnproven, st.undo = false, nil
		s.logger.Printf("req=%s compaction mid-turn accepted session=%s: the API answered the swapped request normally", requestIDFrom(in.Context()), ci.key)
	}
	if st.swappedFrom > 0 && ci.applied {
		cut := 0
		if st.swappedFrom > 0 && ctxTokens < st.swappedFrom {
			cut = int(math.Round(100 * float64(st.swappedFrom-ctxTokens) / float64(st.swappedFrom)))
		}
		st.notice("done. Context down %d%%, %dk \u2192 %dk: %d earlier messages now go as a summary", cut, st.swappedFrom/1000, ctxTokens/1000, st.swappedMsgs)
		st.swappedFrom, st.swappedMsgs = 0, 0
		// Saved, with the new context: the swap itself saved the context
		// from BEFORE it, and a restart that reloaded that figure started a
		// second summary of an already compacted session on the next prompt
		// (2026-10-01: 433k on disk, 82k real, 579 messages re-summarised).
		s.compaction.save()
	}
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
	// A session id alone does not name one conversation: subagents run
	// under their parent's session id and model, and on 2026-09-30 two of
	// them, a 2-message and a 53-message history, were taken for the main
	// conversation being cleared, dropping its summary twice in two minutes.
	// The first message tells them apart.
	key := sid + "|" + requestModel(body) + "|" + conversationID(msgs[0])
	if isSideRequest(msgs) {
		return s.applySideRequest(in, top, body, msgs, key)
	}
	// The session's repository may have its own Compact at, or none. Looked
	// up outside the lock: the first lookup reads the transcript.
	_, root := s.repos.Resolve(sid)
	cfg, override := cfg.ForRepo(root)
	limit := fmt.Sprintf("compact at %dk", cfg.CompactAtTokens/1000)
	if override != nil {
		limit += " for " + filepath.Base(root)
	}
	ci := compactInfo{key: key}
	now := time.Now()
	window := time.Duration(cfg.WindowMinutes) * time.Minute
	rid := requestIDFrom(in.Context())

	s.compaction.mu.Lock()
	st := s.compaction.state(key)
	st.seen = now
	dirty := false

	// A summary only fits the history it was made from.
	// A dropped summary also reopens the window: the session is back to its
	// full history, and on 2026-09-30 the window kept it there, at 450k and
	// growing, for the rest of the hour after a drop.
	if st.summary != "" && (len(msgs) <= st.p0 || prefixHash(msgs, st.p0) != st.hash) {
		s.logger.Printf("req=%s compaction dropped session=%s: history no longer matches (cleared, compacted or rewound; %s); window reopened", rid, key, divergence(msgs, st.p0, st.marks))
		st.summary, st.hash, st.p0, st.swapAt, st.marks = "", "", 0, 0, nil
		st.startedAt = time.Time{}
		st.notice("the summary no longer fits (history cleared, compacted or rewound), so the full history goes again; a new summary can start at once")
		dirty = true
	}
	if st.next != "" && (len(msgs) <= st.nextP0 || prefixHash(msgs, st.nextP0) != st.nextHash) {
		s.logger.Printf("req=%s compaction dropped session=%s: history no longer matches the waiting summary (%s); window reopened", rid, key, divergence(msgs, st.nextP0, st.nextMarks))
		st.next, st.nextHash, st.nextP0, st.nextMarks = "", "", 0, nil
		st.startedAt = time.Time{}
		st.notice("the waiting summary no longer fits (history cleared, compacted or rewound) and was dropped; a new one can start at once")
		dirty = true
	}

	if st.lastContext >= cfg.WarnAtTokens && (st.warnedAt.IsZero() || now.Sub(st.warnedAt) >= window) {
		st.warnedAt = now
		s.logger.Printf("req=%s warn stage=compaction session=%s context=%dk (warn at %dk, %s)",
			rid, key, st.lastContext/1000, cfg.WarnAtTokens/1000, limit)
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

	// /compact-async asks for a summary now, whatever the context size and
	// the window: the user chose the moment.
	forced := fresh && RequestsCompaction(msgs[len(msgs)-1])
	if forced && st.pending {
		st.notice("/compact-async: a summary is already being written; it swaps in at a later prompt")
	} else if forced && st.next != "" {
		st.notice("/compact-async: a summary is already ready; it swaps in with your next prompt")
	}
	auto := st.lastContext >= cfg.CompactAtTokens && (st.startedAt.IsZero() || now.Sub(st.startedAt) >= window)
	if (auto || forced) && !st.pending && st.next == "" {
		// Only a compaction that actually starts opens the window. A skip
		// (no boundary yet, typically one long prompt) must leave the next
		// prompt free to compact.
		p, cut := compactionBoundary(view, bounds, offset)
		if cut > 0 {
			st.startedAt = now
			st.pending = true
			dirty = true
			history := append([]json.RawMessage(nil), view...)
			hash := prefixHash(msgs, p)
			st.pendingMarks = messageMarks(msgs, p)
			why := ""
			if forced {
				why = " (requested with /compact-async)"
			}
			s.logger.Printf("req=%s compaction start session=%s context=%dk (%s) summarising %d of %d messages%s", rid, key, st.lastContext/1000, limit, p, len(msgs), why)
			if forced {
				st.notice("/compact-async: %d earlier messages (context %dk) are being summarised in the background. Keep working: it swaps in with your next prompt once ready, with no pause", p, st.lastContext/1000)
			} else {
				st.notice("context is %dk, so %d earlier messages are being summarised in the background. Keep working: it swaps in at a later prompt, with no pause", st.lastContext/1000, p)
			}
			s.compaction.running.Add(1)
			// Its own copy of top: this same request may still be rewritten
			// below (the current summary stays in force), which writes
			// top["messages"] while the summary goroutine reads top.
			own := make(map[string]json.RawMessage, len(top))
			for k, v := range top {
				own[k] = v
			}
			go s.summarise(in.Clone(context.Background()), own, history, cut, key, p, hash)
		} else if forced {
			s.logger.Printf("req=%s compaction skipped session=%s context=%dk: /compact-async with too little before this prompt to summarise", rid, key, st.lastContext/1000)
			st.notice("/compact-async: nothing to compact yet; there is too little conversation before this prompt to summarise")
		} else if st.skippedAt.IsZero() || now.Sub(st.skippedAt) >= window {
			st.skippedAt = now
			s.logger.Printf("req=%s compaction skipped session=%s context=%dk: no prompt boundary leaves at least %.0f%% to summarise",
				rid, key, st.lastContext/1000, minSummarisedShare*100)
		}
	}

	if st.next != "" && !fresh {
		kept := ""
		if st.summary != "" {
			kept = " (the previous summary stays in force)"
		}
		s.logger.Printf("req=%s compaction waiting session=%s: summary ready, request ends in %s, applies at the next plain prompt%s",
			rid, key, lastMessageShape(msgs), kept)
	}
	// Mid-turn: swap at the prompt that started the running turn. Its own
	// messages keep their thinking (they are past swapAt); everything before
	// it is summarised or loses thinking, exactly as at a plain prompt.
	turnStart := 0
	if len(bounds) > 0 {
		turnStart = bounds[len(bounds)-1]
	}
	midTurn := !fresh && st.next != "" && cfg.MidTurn && !s.compaction.midTurnOff &&
		turnStart > 0 && turnStart >= st.nextP0
	if st.next != "" && (fresh || midTurn) {
		if midTurn {
			st.undo = &swapUndo{summary: st.summary, hash: st.hash, p0: st.p0, swapAt: st.swapAt, marks: st.marks}
			st.midTurnUnproven = true
		} else {
			st.undo, st.midTurnUnproven = nil, false
		}
		swapAt := len(msgs)
		if midTurn {
			swapAt = turnStart
		}
		st.summary, st.p0, st.hash, st.swapAt = st.next, st.nextP0, st.nextHash, swapAt
		st.marks, st.nextMarks = st.nextMarks, nil
		st.next, st.nextP0, st.nextHash = "", 0, ""
		where := ""
		if midTurn {
			where = fmt.Sprintf(" mid-turn (the running turn, from message %d, kept as it was)", turnStart)
		}
		s.logger.Printf("req=%s compaction applied session=%s: %d messages replaced by a summary%s", rid, key, st.p0, where)
		st.swappedFrom, st.swappedMsgs, st.readyShown, st.waitShown = st.lastContext, st.p0, false, false
		// The pre-swap context no longer describes anything this session
		// sends. Unknown until the response reports it, so nothing (a restart
		// reloading it included) can start a summary on a stale 433k.
		st.lastContext = 0
		dirty = true
	}
	if st.midTurnUnproven {
		ci.midTurn, ci.original = true, body
	}
	if st.summary == "" || st.swapAt == 0 {
		if dirty {
			s.compaction.save()
		}
		s.compaction.mu.Unlock()
		return body, in.WithContext(context.WithValue(in.Context(), compactInfoKey{}, ci))
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

// sideRequestMarkers identify requests Claude Code makes on its own, beside
// the conversation, with the conversation's history: the away recap is sent
// minutes after a turn ends, when nobody is there. Taken as a plain prompt,
// on 2026-10-01 it swapped a summary in with no hook to show it (the notice
// waited 80 minutes and a restart lost it), and later dropped a waiting
// summary because it alters the last message.
var sideRequestMarkers = [][]byte{
	[]byte("The user stepped away and is coming back. Recap in"),
}

// isSideRequest reports whether the last message is one of Claude Code's
// own side requests rather than a prompt the user sent.
func isSideRequest(msgs []json.RawMessage) bool {
	last := msgs[len(msgs)-1]
	for _, m := range sideRequestMarkers {
		if bytes.Contains(last, m) {
			return true
		}
	}
	return false
}

// applySideRequest sends a side request with the summary already in force,
// when it still fits, and changes nothing: no swap, no drop, no start, and
// no context noted (its compactInfo has no key), because the conversation
// did not move.
func (s *Server) applySideRequest(in *http.Request, top map[string]json.RawMessage, body []byte, msgs []json.RawMessage, key string) ([]byte, *http.Request) {
	s.compaction.mu.Lock()
	st := s.compaction.sessions[key]
	var summary string
	var p0, swapAt int
	if st != nil && st.summary != "" && st.swapAt > 0 && len(msgs) > st.p0 && prefixHash(msgs, st.p0) == st.hash {
		summary, p0, swapAt = st.summary, st.p0, st.swapAt
	}
	s.compaction.mu.Unlock()
	if summary == "" {
		return body, in
	}
	out := rewriteWithSummary(msgs, summary, p0, swapAt)
	nb, err := json.Marshal(out)
	if err != nil {
		return body, in
	}
	top["messages"] = nb
	newBody, err := json.Marshal(top)
	if err != nil {
		return body, in
	}
	ci := compactInfo{applied: true, removedMsgs: len(msgs) - len(out), removedBytes: int64(len(body) - len(newBody))}
	return newBody, in.WithContext(context.WithValue(in.Context(), compactInfoKey{}, ci))
}

// CompactAsyncMarker is in the prompt the /compact-async command sends; a
// plain prompt whose last message carries it asks for a summary now.
const CompactAsyncMarker = "claude-burst:compact-async"

// RequestsCompaction reports whether a message is a /compact-async prompt.
func RequestsCompaction(msg json.RawMessage) bool {
	return bytes.Contains(msg, []byte(CompactAsyncMarker))
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
func (s *Server) summarise(in *http.Request, top map[string]json.RawMessage, history []json.RawMessage, cut int, key string, p0 int, hash string) {
	// Runs on its own goroutine, outside net/http's per-connection recover,
	// and parses model output: a panic here would otherwise exit the gateway
	// and drop every session's in-flight request. Registered first, so it
	// runs after the deferred unlock and Done below.
	defer func() {
		if rec := recover(); rec != nil {
			s.logger.Printf("compaction PANIC session=%s err=%v\n%s", key, rec, debug.Stack())
		}
	}()
	defer s.compaction.running.Done()
	summary, err := s.requestSummary(in, top, history, cut)
	s.compaction.mu.Lock()
	defer s.compaction.mu.Unlock()
	st := s.compaction.state(key)
	st.pending = false
	defer s.compaction.save()
	if err != nil {
		// A failed attempt must not hold the session on its full history
		// for the whole window: try again after retryAfterFailure.
		if w := time.Duration(s.compaction.cfg.WindowMinutes) * time.Minute; w > retryAfterFailure {
			st.startedAt = time.Now().Add(retryAfterFailure - w)
		}
		s.logger.Printf("compaction failed session=%s: %v (next attempt in %s)", key, err, retryAfterFailure)
		reason := err.Error()
		if len(reason) > 160 {
			reason = reason[:160] + "..."
		}
		st.notice("the summary failed (%s); the next attempt is in %s", reason, retryAfterFailure)
		return
	}
	st.next, st.nextP0, st.nextHash, st.nextMarks = summary, p0, hash, st.pendingMarks
	st.readyShown, st.waitShown = false, false
	s.logger.Printf("compaction summary ready session=%s: %d messages summarised into %d characters; applies from the next plain prompt", key, p0, len(summary))
}

func (s *Server) requestSummary(in *http.Request, top map[string]json.RawMessage, history []json.RawMessage, cut int) (string, error) {
	req := map[string]json.RawMessage{}
	for _, k := range []string{"model", "system", "tools", "thinking", "metadata"} {
		if v, ok := top[k]; ok {
			req[k] = v
		}
	}
	msgs, _ := json.Marshal(withSummaryInstruction(history, cut))
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

// withSummaryInstruction returns the request's own history with the summary
// instruction appended after its last block, so the summary call reads the
// whole history from the cache entry Claude Code wrote a moment earlier.
// Cutting the history at the boundary instead cost full price twice live:
// the first run read 28k of 532k from cache ($2.10), and a breakpoint at the
// cut found no entry within the API's 20-block lookback and wrote 577k to
// cache at 1.25x ($2.96). The messages from the boundary on stay in the
// request, so the instruction names where the summary must stop.
func withSummaryInstruction(history []json.RawMessage, cut int) []json.RawMessage {
	text := compactionSummaryPrompt
	if ex := promptExcerpt(history[min(cut, len(history)-1)]); cut < len(history) && ex != "" {
		text = "Stop reading here and do not answer or continue the conversation. " +
			"Only the part of this conversation BEFORE the user message that begins \"" + ex +
			"\" is being replaced; that message and everything after it is kept word for word, so leave it out of the summary. " +
			compactionSummaryPrompt
	}
	instr := map[string]any{"type": "text", "text": text}
	out := append([]json.RawMessage(nil), history...)
	role := "user"
	if n := len(out); n > 0 {
		var msg map[string]any
		if json.Unmarshal(out[n-1], &msg) == nil {
			switch msg["role"] {
			case "user":
				msg["content"] = append(contentBlocks(msg["content"]), instr)
				if b, err := json.Marshal(msg); err == nil {
					out[n-1] = b
					return out
				}
			case "system":
				// Claude Code often ends a turn with a mid-conversation
				// system message, which the API accepts only last or before
				// an assistant turn: a user message after it is a 400 (the
				// first live run of this code, 2026-09-29 22:26). Another
				// system message keeps it last and leaves the history, and
				// its cache entry, untouched.
				role = "system"
			}
		}
	}
	b, _ := json.Marshal(map[string]any{"role": role, "content": []any{instr}})
	return append(out, b)
}

// promptExcerpt returns the start of a message's first text that is not a
// <system-reminder>, for naming it in the summary instruction.
func promptExcerpt(m json.RawMessage) string {
	var msg map[string]any
	if json.Unmarshal(m, &msg) != nil {
		return ""
	}
	for _, b := range contentBlocks(msg["content"]) {
		bm, _ := b.(map[string]any)
		t, _ := bm["text"].(string)
		t = strings.TrimSpace(t)
		if bm["type"] != "text" || t == "" || strings.HasPrefix(t, "<system-reminder>") {
			continue
		}
		if r := []rune(t); len(r) > 120 {
			t = string(r[:120])
		}
		return t
	}
	return ""
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

// PromptNotices returns, and forgets, the lines to show under the prompt
// session sid is sending now: what compaction did since its last prompt,
// and a waiting summary, which swaps in with this very prompt. Nothing when
// compaction or the notices are off.
//
// midTurn is the same hook after a tool call, inside a turn. A summary
// cannot swap in there (it would cut the turn's own tool calls), so it says
// once that the summary waits for the turn to finish and the next prompt:
// a long turn otherwise looked like compaction had not fired at all.
func (s *Server) PromptNotices(sid string, midTurn bool) []string {
	s.compaction.mu.Lock()
	defer s.compaction.mu.Unlock()
	cfg := s.compaction.cfg
	if sid == "" || !cfg.Enabled || cfg.NoPromptNotice {
		// Compaction lines obey these; failover ones do not, because
		// spending money is the one thing that must never be silent.
		return s.takeFailoverNotices()
	}
	keys := make([]string, 0, 2)
	for k := range s.compaction.sessions {
		if strings.HasPrefix(k, sid+"|") {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	out := s.takeFailoverNotices()
	shown := false
	defer func() {
		// Notices are saved now, so a shown one must be saved as shown or a
		// restart would show it again.
		if shown {
			s.compaction.save()
		}
	}()
	for _, k := range keys {
		st := s.compaction.sessions[k]
		shown = shown || len(st.notices) > 0
		out = append(out, st.notices...)
		st.notices = nil
		if st.next != "" && midTurn && !(cfg.MidTurn && !s.compaction.midTurnOff) {
			if !st.waitShown {
				st.waitShown = true
				out = append(out, fmt.Sprintf("\u26a1 Claude Burst, pauseless compaction: the summary is ready (%d earlier messages, context %dk now). It swaps in when this turn finishes and you send your next prompt; nothing to do meanwhile", st.nextP0, st.lastContext/1000))
			}
			continue
		}
		if st.next != "" && !st.readyShown {
			st.readyShown = true
			out = append(out, fmt.Sprintf("\u26a1 Claude Burst, pauseless compaction: the summary is ready and swaps in with this message (%d earlier messages, context %dk now)", st.nextP0, st.lastContext/1000))
		}
	}
	return out
}

// QueueTestNotice queues a test line for session sid, or for every tracked
// session when sid is "", and returns how many sessions got one.
func (s *Server) QueueTestNotice(sid string) int {
	s.compaction.mu.Lock()
	defer s.compaction.mu.Unlock()
	seen := map[string]bool{}
	for k, st := range s.compaction.sessions {
		id, _, _ := strings.Cut(k, "|")
		if (sid != "" && id != sid) || seen[id] {
			continue
		}
		seen[id] = true
		st.notice("test line from the dashboard. Real ones appear here when a summary starts, swaps in, or fails")
	}
	return len(seen)
}

// CompactionSession is one tracked session, for the admin page.
type CompactionSession struct {
	Session     string    `json:"session"`
	Model       string    `json:"model"`
	Context     int64     `json:"context"`
	State       string    `json:"state"`
	CompactedAt time.Time `json:"compacted_at,omitempty"`
	Summarised  int       `json:"summarised_messages,omitempty"`
	// Repo is the session's repository, and CompactAt the limit that applies
	// to it: 0 when compaction is off for that repository. Override says
	// the limit is the repository's own, not the default.
	Repo      string `json:"repo,omitempty"`
	RepoRoot  string `json:"repo_root,omitempty"`
	CompactAt int64  `json:"compact_at"`
	Override  bool   `json:"override,omitempty"`
}

// CompactionSessions lists tracked sessions, largest context first.
func (s *Server) CompactionSessions() []CompactionSession {
	// Repositories first, outside the lock: a first lookup reads a transcript.
	s.compaction.mu.Lock()
	sids := map[string]bool{}
	for k := range s.compaction.sessions {
		sid, _, _ := strings.Cut(k, "|")
		sids[sid] = true
	}
	s.compaction.mu.Unlock()
	type where struct{ name, root string }
	repos := make(map[string]where, len(sids))
	for sid := range sids {
		name, root := s.repos.Resolve(sid)
		repos[sid] = where{name, root}
	}

	s.compaction.mu.Lock()
	defer s.compaction.mu.Unlock()
	var out []CompactionSession
	for k, st := range s.compaction.sessions {
		if st.lastContext == 0 && st.summary == "" && !st.pending {
			continue
		}
		sid, model, _ := strings.Cut(k, "|")
		model, _, _ = strings.Cut(model, "|")
		cfg, override := s.compaction.cfg.ForRepo(repos[sid].root)
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
		case st.lastContext >= cfg.CompactAtTokens && cfg.MidTurn && !s.compaction.midTurnOff:
			state = "over threshold, compacts at the next request"
		case st.lastContext >= cfg.CompactAtTokens:
			state = "over threshold, compacts at the next prompt"
		case st.lastContext >= cfg.WarnAtTokens:
			state = "warning"
		}
		cs := CompactionSession{Session: sid, Model: model, Context: st.lastContext, State: state, Summarised: st.p0,
			Repo: repos[sid].name, RepoRoot: repos[sid].root, CompactAt: cfg.CompactAtTokens, Override: override != nil}
		if override != nil && override.Off {
			cs.CompactAt = 0
		}
		if !st.startedAt.IsZero() {
			cs.CompactedAt = st.startedAt
		}
		out = append(out, cs)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Context > out[j].Context })
	return out
}

// conversationID is a short hash of a conversation's first message, which
// stays the same for its whole life and differs between a session and the
// subagents it starts.
func conversationID(first json.RawMessage) string {
	return prefixHash([]json.RawMessage{first}, 1)[:12]
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

// messageMarks is prefixHash per message, for divergence.
func messageMarks(msgs []json.RawMessage, n int) []string {
	out := make([]string, 0, n)
	for i := 0; i < n && i < len(msgs); i++ {
		out = append(out, prefixHash(msgs[i:i+1], 1))
	}
	return out
}

// divergence says where a history stopped matching its summary: the length,
// or the first changed message with its role and block types (never its
// text).
func divergence(msgs []json.RawMessage, p0 int, marks []string) string {
	if len(msgs) <= p0 {
		return fmt.Sprintf("history now %d messages, summary covers %d", len(msgs), p0)
	}
	if len(marks) == 0 {
		return "which message changed is unknown (summary made before a restart)"
	}
	for i := 0; i < len(marks) && i < len(msgs); i++ {
		if prefixHash(msgs[i:i+1], 1) != marks[i] {
			return fmt.Sprintf("message %d of %d changed, now %s", i, p0, lastMessageShape(msgs[i:i+1]))
		}
	}
	return "no single message differs"
}
