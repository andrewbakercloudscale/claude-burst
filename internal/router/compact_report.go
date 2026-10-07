package router

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/andrewbakercloudscale/claude-burst/internal/autocompact"
)

// MidTurnOff reports whether the API refused a mid-turn swap since the
// compaction settings were last applied.
func (s *Server) MidTurnOff() bool {
	s.compaction.mu.Lock()
	defer s.compaction.mu.Unlock()
	return s.compaction.midTurnOff
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
	with := ci.threadSummary
	if !ci.thread {
		with = ""
		if ci.applied {
			with = st.hash
		}
	}
	tr := threadResponse{key: ci.key, context: ctxTokens, summary: with}
	if with != "" {
		tr.removedMsgs, tr.removedBytes = ci.removedMsgs, ci.removedBytes
	}
	s.compaction.noteThread(tok.msgID, tr)
	st.lastContext = ctxTokens
	if ci.parts != nil {
		st.parts = scaleParts(ci.parts, ctxTokens)
	}
	if ci.sentBytes > 0 {
		st.rawContext = int64(float64(ctxTokens) * float64(ci.rawBytes) / float64(ci.sentBytes))
		s.noteExposure(ci.key, st)
		// The hand-off file says how much Claude Code holds, for a mod that
		// finds the gateway gone. Rewritten when that has grown by a tenth.
		if st.hand != nil && st.rawContext > st.hand.Raw+st.hand.Raw/10 {
			s.compaction.save()
		}
	}
	if ci.midTurn && st.midTurnUnproven {
		st.midTurnUnproven, st.undo = false, nil
		s.logger.Printf("req=%s compaction mid-turn accepted session=%s: the API answered the swapped request normally", requestIDFrom(in.Context()), ci.key)
	}
	// A summary that went in and left the session at its limit or over did
	// not do its job: the turns still carry the old history (a thread that
	// went on from before the swap) or what was kept is that large. Then
	// Claude Code's own history is compacted with the same summary, at
	// once and once per summary, rather than a second summary being paid
	// for an hour later. Judged at the first answer after the swap.
	if h := st.hand; h != nil && st.summary != "" && st.swapAt > 0 && h.Of == handoffOf(st.summary, st.p0) && st.judged != h.Of && (ci.applied || ci.thread && ci.inForce) && ci.limit > 0 {
		st.judged = h.Of
		if ctxTokens >= ci.limit {
			h.Full = true
			s.logger.Printf("req=%s compaction still over session=%s: %dk after the summary went in, limit %dk; /compact-async-full follows now, with the same summary", requestIDFrom(in.Context()), ci.key, ctxTokens/1000, ci.limit/1000)
			st.notice("still %dk after the summary, over the %dk limit: /compact-async-full follows now", ctxTokens/1000, ci.limit/1000)
			s.compaction.save()
		}
	}
	if st.swappedFrom > 0 && ci.applied {
		cut := 0
		if st.swappedFrom > 0 && ctxTokens < st.swappedFrom {
			cut = int(math.Round(100 * float64(st.swappedFrom-ctxTokens) / float64(st.swappedFrom)))
		}
		st.notice("done, %d%% smaller: %dk \u2192 %dk (%d messages summarised)", cut, st.swappedFrom/1000, ctxTokens/1000, st.swappedMsgs)
		st.swappedFrom, st.swappedMsgs = 0, 0
		// Saved, with the new context: the swap itself saved the context
		// from BEFORE it, and a restart that reloaded that figure started a
		// second summary of an already compacted session on the next prompt
		// (2026-10-01: 433k on disk, 82k real, 579 messages re-summarised).
		s.compaction.save()
	}
	s.compaction.mu.Unlock()
}

// Exposure: Burst compacts what it sends, never Claude Code's own history,
// which keeps growing. Whenever Burst drops out of the path (the gateway
// down, the redirect removed, burst-off) the next turn sends all of that,
// uncached: on 2026-10-04 a session Burst kept at 135k sent 994k for $7.75.
// It is what compaction is for, not a fault, so it is information and not
// an alert: until 5 Oct 2026 crossing exposureWarnTokens raised a warning
// on screen, a line under the prompt and a standing problem in the band and
// status line, all for a tool doing its job. The figure is in the usage
// sidebar's session card and the dashboard's sessions table; the log notes
// the crossing once per session.
const exposureWarnTokens = 500_000

// noteExposure logs a compacted session's exposure, once. Called with
// s.compaction.mu held.
func (s *Server) noteExposure(key string, st *compactState) {
	if st.rawContext < exposureWarnTokens || st.summary == "" || st.exposureWarned > 0 {
		return
	}
	st.exposureWarned = st.rawContext
	s.logger.Printf("compaction exposure session=%s: Claude Code holds %dk, Burst sends %dk", key, st.rawContext/1000, st.lastContext/1000)
}

// PromptNotices returns, and forgets, the lines to show under the prompt
// session sid is sending now: what compaction did since its last prompt.
// A summary that swaps in with this prompt gets no line of its own: the
// "done" line follows within a request, and the two read as one repeated.
// Nothing when compaction or the notices are off.
//
// midTurn is the same hook after a tool call, inside a turn. A summary
// cannot swap in there (it would cut the turn's own tool calls), so it says
// once that the summary waits for the turn to finish and the next prompt:
// a long turn otherwise looked like compaction had not fired at all.
func (s *Server) PromptNotices(sid string, midTurn bool) []string {
	return append(s.takeAutomaskNotices(sid), s.promptNotices(sid, midTurn)...)
}

func (s *Server) promptNotices(sid string, midTurn bool) []string {
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
				out = append(out, fmt.Sprintf("\u26a1 Burst compaction: summary ready (%d messages, %dk), swaps in next prompt", st.nextP0, st.lastContext/1000))
			}
			continue
		}
	}
	return out
}

// DropSummary takes session sid's summary out of force, and any waiting
// one with it: Claude Code still holds the whole history, so its next
// request goes as sent, in full. It is the way back from a summary that
// lost something. The window starts now, so the next summary is not
// started by that same request. It returns how many summaries it dropped.
func (s *Server) DropSummary(sid string) int {
	if sid == "" {
		return 0
	}
	s.compaction.mu.Lock()
	defer s.compaction.mu.Unlock()
	n := 0
	for key, st := range s.compaction.sessions {
		if !strings.HasPrefix(key, sid+"|") || (st.summary == "" && st.next == "") {
			continue
		}
		n++
		st.summary, st.hash, st.p0, st.swapAt, st.marks = "", "", 0, 0, nil
		st.next, st.nextHash, st.nextP0, st.nextMarks = "", "", 0, nil
		st.nextTightP0, st.nextTightHash = 0, ""
		st.undo, st.midTurnUnproven = nil, false
		st.swappedFrom, st.swappedMsgs = 0, 0
		st.startedAt = time.Now()
		st.notice("summary dropped on request; the full history is sent again")
		s.compaction.outcome(key, autocompact.OutcomeEnded)
		s.logger.Printf("compaction dropped session=%s: asked for from the dashboard; the full history goes again and the window starts now", key)
	}
	if n > 0 {
		s.compaction.save()
	}
	return n
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
		st.notice("test line from the dashboard")
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
	// Seen is the conversation's latest request, and Thread says that one
	// continued a message thread: it carried the new message alone, and
	// the history is the API's to hold.
	Seen   time.Time `json:"seen"`
	Thread bool      `json:"thread,omitempty"`
	// Repo is the session's repository, and CompactAt the limit that applies
	// to it: 0 when compaction is off for that repository. Override says
	// the limit is the repository's own, not the default.
	Repo      string `json:"repo,omitempty"`
	RepoRoot  string `json:"repo_root,omitempty"`
	CompactAt int64  `json:"compact_at"`
	Override  bool   `json:"override,omitempty"`
	// Learned: CompactAt is the repository's learned one (intelligent mode).
	Learned bool `json:"learned,omitempty"`
	// DelayMinutes is the least time between this session's compactions,
	// and BufferPercent what a learned CompactAt has on top of the cheapest
	// size.
	DelayMinutes  int `json:"delay_minutes,omitempty"`
	BufferPercent int `json:"buffer_percent,omitempty"`
	// Parts is what the context is made of, for the band's context bar.
	Parts []ContextPart `json:"parts,omitempty"`
	// Raw is Claude Code's own history, estimated, when it is larger than
	// Context: what goes if Burst drops out. RawUSD prices it uncached.
	Raw    int64   `json:"raw,omitempty"`
	RawUSD float64 `json:"raw_usd,omitempty"`
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
		case st.thread && st.lastContext >= cfg.CompactAtTokens && s.compaction.asks[sid+"|"+model+"|"] != nil && s.compaction.asks[sid+"|"+model+"|"].deaf:
			state = "over threshold, compacts when Claude Code next sends its whole history"
		case st.lastContext >= cfg.CompactAtTokens && !st.startedAt.IsZero() && time.Since(st.startedAt) < time.Duration(cfg.WindowMinutes)*time.Minute:
			state = "over threshold, next compaction after " + st.startedAt.Add(time.Duration(cfg.WindowMinutes)*time.Minute).Format("15:04")
		case st.lastContext >= cfg.CompactAtTokens && cfg.MidTurn && !s.compaction.midTurnOff:
			state = "over threshold, compacts at the next request"
		case st.lastContext >= cfg.CompactAtTokens:
			state = "over threshold, compacts at the next prompt"
		case st.lastContext >= cfg.WarnAtTokens:
			state = "warning"
		}
		cs := CompactionSession{Session: sid, Model: model, Context: st.lastContext, State: state, Summarised: st.p0, Seen: st.seen, Thread: st.thread,
			Repo: repos[sid].name, RepoRoot: repos[sid].root, CompactAt: cfg.CompactAtTokens, Override: override != nil && !override.Learned, Learned: override != nil && override.Learned, Parts: st.parts, DelayMinutes: cfg.WindowMinutes}
		if cs.Learned {
			cs.BufferPercent = *cfg.BufferPercent
		}
		if override != nil && override.Off {
			cs.CompactAt = 0
		}
		if st.rawContext > st.lastContext*11/10 {
			cs.Raw = st.rawContext
			cs.RawUSD, _ = s.PriceTokens(model, 0, 0, 0, st.rawContext)
		}
		if !st.startedAt.IsZero() {
			cs.CompactedAt = st.startedAt
		}
		out = append(out, cs)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Context > out[j].Context })
	return out
}

// mainConversationWindow is how long before a session's latest request a
// conversation holding a summary still counts as the one in use.
const mainConversationWindow = 30 * time.Minute

// MainConversation picks the row that speaks for session sid from rows,
// which are largest first: its main conversation and not a subagent's. The
// largest is that, among those in use lately, unless one of them is a
// message thread or holds a summary: a thread is what the session is
// sending turn by turn, and a compacted conversation is the small one. On
// 6 Oct 2026 the panel read "343k, over threshold, compacts at the next
// request" for half an hour over a session that was sending 95k: the
// largest row was a conversation last heard of an hour before.
func MainConversation(rows []CompactionSession, sid string) *CompactionSession {
	var newest time.Time
	for i := range rows {
		if rows[i].Session == sid && rows[i].Seen.After(newest) {
			newest = rows[i].Seen
		}
	}
	var largest, live, thread, compacted *CompactionSession
	for i := range rows {
		r := &rows[i]
		if r.Session != sid {
			continue
		}
		if largest == nil {
			largest = r
		}
		if newest.Sub(r.Seen) >= mainConversationWindow {
			continue
		}
		if live == nil {
			live = r
		}
		if thread == nil && r.Thread {
			thread = r
		}
		if compacted == nil && (r.State == "compacted" || r.State == "summarising" || strings.HasPrefix(r.State, "summary ready")) {
			compacted = r
		}
	}
	for _, r := range []*CompactionSession{thread, compacted, live} {
		if r != nil {
			return r
		}
	}
	return largest
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
