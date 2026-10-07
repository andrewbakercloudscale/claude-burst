package router

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/andrewbakercloudscale/claude-burst/internal/autocompact"
	"github.com/andrewbakercloudscale/claude-burst/internal/notice"
)

// The runtime half of proxy-side compaction; compact.go has the pure
// history rewrite and the reasoning.

// minSummarisedShare is the least share of a session's bytes a compaction
// must summarise to be worth a summary call and a cache rewrite.
const minSummarisedShare = 0.30

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
	// The cut right after the request the waiting summary was written
	// from: its first kept message is the reply to that request. Preferred
	// over nextP0, the latest plain prompt before it, which is 0 when no
	// prompt left enough to summarise. 0 once this cut is known not to fit.
	nextTightP0   int
	nextTightHash string
	// Per-message hashes of the summarised prefix, memory only: when a
	// summary is dropped they say which message changed, so the log names
	// the cause instead of guessing.
	marks, nextMarks, pendingMarks []string
	seen                           time.Time
	// Memory only, for the prompt notice hook: lines not yet shown, whether
	// the waiting summary has been announced, and the context before the
	// latest swap until a response reports the context after it.
	notices     []string
	waitShown   bool          // told mid-turn that it waits for the next prompt
	parts       []ContextPart // the last request's make-up, scaled to lastContext
	swappedFrom int64
	swappedMsgs int
	// A mid-turn swap the API has not yet answered with a success. Memory
	// only: undo holds what was in force before it, to put back on a 400.
	midTurnUnproven bool
	undo            *swapUndo
	// rawContext is Claude Code's own history in tokens, estimated: what it
	// sends when Burst is not in the path. Memory only, like exposureWarned,
	// the rawContext at which the exposure was last announced.
	rawContext     int64
	exposureWarned int64
	// hand is the summary in force as the mod can hand it to Claude Code
	// (handoff.go), and handOf the summary it was last worked out for, so a
	// cut that cannot be handed over is not tried on every request.
	hand   *Handoff
	handOf string
	// tail: hash, nextHash and nextTightHash leave the first message out
	// (hashOf). Claude Code rewrites the context it puts in the first
	// message, and a summary replaces that message anyway. A state saved
	// before 6 Oct 2026 has hashes of the whole prefix and moves over at
	// the first request they all still fit (useTailHashes).
	tail bool
	// firstRaw is the first message of the latest request, memory only, to
	// say what changed in it; firstLogged keeps that to one line a minute.
	firstRaw    json.RawMessage
	firstLogged time.Time
	msgCount    int // messages in the latest request, memory only
	// thread: the latest request continued a message thread (threadOf).
	thread bool
	// judged is the hand-off (Handoff.Of) whose first answer after the
	// swap has been compared with the limit. Memory only: after a restart
	// the next answer is judged again, and the mod acts once per summary.
	judged string
}

// hashOf identifies messages [0, n) for this state's summaries: without the
// first message once the state is on tail hashes, when there is anything
// after it to tell one history from another.
func (st *compactState) hashOf(msgs []json.RawMessage, n int) string {
	if st.tail && n >= minTailMessages && len(msgs) >= 1 {
		return prefixHash(msgs[1:], n-1)
	}
	return prefixHash(msgs, n)
}

// besideSummary reports whether msgs stops at or before the summary's cut
// and is, up to its last message, the history the summary was made from: a
// request Claude Code makes beside the conversation, or one sent again. It
// has nothing after the cut to put a summary before, and it is not the
// conversation cleared or rewound. On 6 Oct 2026 one came in the second a
// summary of 46 messages was swapped in mid-turn, 46 messages with a last
// one of its own, and the summary was dropped as "cleared, compacted or
// rewound". The thread went on compacted from the swapped request, at 138k,
// and when it expired 39 minutes later its history came back whole, 240k,
// with no summary to send it with. A conversation that really was rewound
// to before the cut is dropped at its next request, which has the new
// prompt where a summarised message was.
func (st *compactState) besideSummary(msgs []json.RawMessage) bool {
	n := len(msgs)
	if st.summary == "" || st.p0 == 0 || n > st.p0 || n-1 > len(st.marks) {
		return false
	}
	from := 0
	if st.tail && n >= minTailMessages {
		from = 1
	}
	if n-1 <= from {
		return false
	}
	for i := from; i < n-1; i++ {
		if prefixHash(msgs[i:i+1], 1) != st.marks[i] {
			return false
		}
	}
	return true
}

// A message thread. Claude Code 2.1.289 (beta message-threads-2026-08-12)
// sends a turn as the new message alone, with
// thread: {"type":"continue","previous_message_id":"msg_..."}: the API
// holds the history and goes on from the response named. There is nothing
// in such a request to summarise or to replace, and it is not a new
// conversation: until 6 Oct 2026 each one was taken for one, so a session
// had a conversation on record for every tool call, each seen once, and the
// one conversation Burst did know was whatever sent a whole history last,
// which was Claude Code's side requests and not the session's turns.
//
// A thread is compacted where its history is sent whole, and the thread
// then goes on from the response to the request Burst swapped the summary
// into. Claude Code sends it whole when the API answers 404 thread_not_found,
// which the API does by itself when a thread's state has expired (11 times
// in three and a half hours on 6 Oct 2026, each followed within the second
// by the whole conversation, read from cache). So when a thread is over its
// limit, or its summary is ready, Burst gives that answer itself
// (askForHistory): until it did, a session on threads was compacted only
// when a thread happened to expire, and one sat at 347k over a 300k limit
// with its summary written and waiting.

// longHistory is a request too long to be a subagent starting.
const longHistory = 20

// minTailMessages is the shortest prefix identified without its first
// message: a shorter one has too little after it to name a conversation.
const minTailMessages = 3

// useTailHashes moves a state saved with hashes of the whole prefix to tail
// hashes, at a request every one of them still fits, which is what proves
// the new hash names the same history. Caller holds c.mu.
func (st *compactState) useTailHashes(msgs []json.RawMessage) bool {
	if st.tail || st.pending {
		return false
	}
	tailOf := func(n int) string { return (&compactState{tail: true}).hashOf(msgs, n) }
	hash, nextHash, tightHash := "", "", ""
	if st.summary != "" {
		if len(msgs) <= st.p0 || prefixHash(msgs, st.p0) != st.hash {
			return false
		}
		hash = tailOf(st.p0)
	}
	if st.next != "" {
		if st.nextP0 > 0 {
			if len(msgs) <= st.nextP0 || prefixHash(msgs, st.nextP0) != st.nextHash {
				return false
			}
			nextHash = tailOf(st.nextP0)
		}
		if st.nextTightP0 > 0 {
			if len(msgs) < st.nextTightP0 || prefixHash(msgs, st.nextTightP0) != st.nextTightHash {
				return false
			}
			tightHash = tailOf(st.nextTightP0)
		}
	}
	st.tail = true
	if st.summary != "" {
		st.hash = hash
	}
	if st.next != "" {
		st.nextHash, st.nextTightHash = nextHash, tightHash
	}
	return true
}

// fits: msgs is the history this state's summary, waiting summary or
// summary in flight was made from, whatever its first message says now.
func (st *compactState) fits(msgs []json.RawMessage) bool {
	if !st.tail {
		return false
	}
	switch {
	case st.summary != "":
		return st.p0 >= minTailMessages && len(msgs) > st.p0 && st.hashOf(msgs, st.p0) == st.hash
	case st.next != "":
		if tp := st.nextTightP0; tp >= minTailMessages && len(msgs) >= tp && st.hashOf(msgs, tp) == st.nextTightHash {
			return true
		}
		return st.nextP0 >= minTailMessages && len(msgs) > st.nextP0 && st.hashOf(msgs, st.nextP0) == st.nextHash
	case st.pending:
		n := min(len(st.pendingMarks), len(msgs))
		if n < minTailMessages {
			return false
		}
		for i := 1; i < n; i++ {
			if prefixHash(msgs[i:i+1], 1) != st.pendingMarks[i] {
				return false
			}
		}
		return true
	}
	return false
}

// firstMessageChange says what differs between two first messages: which
// block, its kind and size, and for a block Claude Code writes itself (a
// system reminder) the first line that changed. Never what the user typed.
func firstMessageChange(was, now json.RawMessage) string {
	blocks := func(m json.RawMessage) []map[string]any {
		var msg struct {
			Content json.RawMessage `json:"content"`
		}
		_ = json.Unmarshal(m, &msg)
		var list []map[string]any
		if json.Unmarshal(msg.Content, &list) == nil {
			return list
		}
		var text string
		if json.Unmarshal(msg.Content, &text) == nil {
			return []map[string]any{{"type": "text", "text": text}}
		}
		return nil
	}
	a, b := blocks(was), blocks(now)
	var out []string
	if len(a) != len(b) {
		out = append(out, fmt.Sprintf("%d blocks, was %d", len(b), len(a)))
	}
	// A line is named by its length and a short hash, never quoted: the
	// log holds metadata only, and a reminder carries CLAUDE.md and paths.
	// The hash still tells one change from another across requests.
	clip := func(l string) string {
		sum := sha256.Sum256([]byte(l))
		return fmt.Sprintf("%d characters (hash %x)", len([]rune(l)), sum[:4])
	}
	for i := 0; i < len(a) && i < len(b) && len(out) < 4; i++ {
		ja, _ := json.Marshal(hashForm(a[i]))
		jb, _ := json.Marshal(hashForm(b[i]))
		if bytes.Equal(ja, jb) {
			continue
		}
		kind, _ := b[i]["type"].(string)
		ta, _ := a[i]["text"].(string)
		tb, _ := b[i]["text"].(string)
		if kind != "text" || ta == "" || tb == "" {
			out = append(out, fmt.Sprintf("block %d (%s) changed", i, kind))
			continue
		}
		if !strings.HasPrefix(strings.TrimSpace(ta), "<system-reminder>") || !strings.HasPrefix(strings.TrimSpace(tb), "<system-reminder>") {
			out = append(out, fmt.Sprintf("block %d (text, %d characters, was %d) changed", i, len(tb), len(ta)))
			continue
		}
		la, lb := strings.Split(ta, "\n"), strings.Split(tb, "\n")
		line := 0
		for line < len(la) && line < len(lb) && la[line] == lb[line] {
			line++
		}
		oldLine, newLine := "(nothing: the block ends)", "(nothing: the block ends)"
		if line < len(la) {
			oldLine = clip(la[line])
		}
		if line < len(lb) {
			newLine = clip(lb[line])
		}
		out = append(out, fmt.Sprintf("block %d (system reminder, %d characters, was %d) from line %d: was %s, now %s", i, len(tb), len(ta), line+1, oldLine, newLine))
	}
	if len(out) == 0 {
		return "nothing a reader would see"
	}
	return strings.Join(out, "; ")
}

// swapUndo is the summary state from before a mid-turn swap.
type swapUndo struct {
	summary, hash string
	p0, swapAt    int
	marks         []string
	// The waiting summary's two cuts as they were before the swap.
	nextP0, nextTightP0     int
	nextHash, nextTightHash string
	nextMarks               []string
}

// maxNotices bounds a session's unshown notices, for a session whose
// prompts never run the hook.
const maxNotices = 5

// notice queues a line for the session's next prompt. Caller holds mu.
func (st *compactState) notice(format string, a ...any) {
	st.notices = append(st.notices, "\u26a1 Burst compaction: "+fmt.Sprintf(format, a...))
	if len(st.notices) > maxNotices {
		st.notices = st.notices[len(st.notices)-maxNotices:]
	}
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
	// parts: the request's make-up in bytes, for the band's context bar.
	parts []int64
	// rawBytes is what Claude Code sent, sentBytes what went upstream: their
	// ratio scales the reported context to Claude Code's own history.
	rawBytes, sentBytes int64
	// thread: this request continues a thread, and threadSummary is the
	// summary that thread's history was sent with (threadResponse).
	thread        bool
	threadSummary string
	// limit is the session's Compact at, and inForce says a thread request
	// came with a summary already in force for its conversation.
	limit   int64
	inForce bool
	// replay: this request continues a thread whose conversation has a
	// compaction to make, and this is why. It is answered with a request
	// for the history (askForHistory) and not sent.
	replay string
}

type compactInfoKey struct{}

func compactInfoFrom(ctx context.Context) compactInfo {
	ci, _ := ctx.Value(compactInfoKey{}).(compactInfo)
	return ci
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
	if st == nil || !st.midTurnUnproven || st.undo == nil {
		// No unproven swap of this session's to blame: a 400 of the
		// request's own (an image too large, a prompt too long) must not
		// turn mid-turn swaps off for every session.
		return false
	}
	s.compaction.midTurnOff = true
	u := st.undo
	st.next, st.nextP0, st.nextHash, st.nextMarks = st.summary, u.nextP0, u.nextHash, u.nextMarks
	st.nextTightP0, st.nextTightHash = u.nextTightP0, u.nextTightHash
	st.summary, st.hash, st.p0, st.swapAt, st.marks = u.summary, u.hash, u.p0, u.swapAt, u.marks
	st.undo, st.midTurnUnproven = nil, false
	st.swappedFrom, st.swappedMsgs = 0, 0
	s.logger.Printf("req=%s compaction mid-turn REJECTED session=%s: the API answered 400 (%s); swap undone, request resent without it, the summary waits for the next plain prompt, and mid-turn swaps are off until the compaction settings are saved again",
		requestIDFrom(in.Context()), ci.key, reason)
	st.notice("summary refused mid-turn; swaps in next prompt")
	s.compaction.save()
	return true
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
	prefix := sid + "|" + requestModel(body) + "|"
	key := prefix + conversationID(msgs[0])
	if prev := threadContinues(top); prev != "" {
		_, root := s.repos.Resolve(sid)
		cfg, _ := cfg.ForRepo(root)
		return s.applyThreadRequest(in, body, msgs, cfg, prefix, prev)
	}
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
		if override.Learned {
			limit += ", learned"
		}
	}
	ci := compactInfo{key: key, limit: cfg.CompactAtTokens}
	now := time.Now()
	window := time.Duration(cfg.WindowMinutes) * time.Minute
	rid := requestIDFrom(in.Context())

	s.compaction.mu.Lock()
	asSent := key
	key = s.compaction.resolve(key, msgs)
	if d := s.compaction.sessions[key]; d != nil && d.besideSummary(msgs) {
		s.logger.Printf("req=%s compaction kept session=%s: %d messages, the history the summary of %d was made from as far as it goes and a last message of its own (%s), so a request beside the conversation and not the conversation cleared or rewound; sent as it came, and nothing noted from it",
			rid, key, len(msgs), d.p0, lastMessageShape(msgs))
		s.compaction.mu.Unlock()
		return body, in
	}
	ci.key = key
	st := s.compaction.state(key)
	first := st.seen.IsZero()
	st.seen = now
	st.thread = false
	// The history Burst asked for, under a conversation nobody has measured:
	// it is the thread's, so it holds what the thread held. Once, and only
	// for a history long enough to be the conversation and not a subagent
	// starting in the same minute.
	if ask := s.compaction.asks[prefix]; ask != nil && ask.context > 0 && len(msgs) >= longHistory {
		if st.lastContext == 0 && st.summary == "" && now.Sub(ask.at) < time.Minute {
			st.lastContext = ask.context
		}
		ask.context = 0
	}
	// The thread known only by its last response is this conversation now.
	// Left on record, on 6 Oct 2026 it kept the panel at "422k, over
	// threshold" over a session whose history had come back and gone at 116k.
	if ask := s.compaction.asks[prefix]; ask != nil && ask.orphan != "" && ask.orphan != key && len(msgs) >= longHistory && now.Sub(ask.at) < time.Minute {
		delete(s.compaction.sessions, ask.orphan)
		ask.orphan = ""
	}
	dirty := st.useTailHashes(msgs)
	quietStart := false
	if key != asSent && now.Sub(st.firstLogged) >= time.Minute {
		st.firstLogged = now
		what := "what changed is unknown (nothing to compare it with since the restart)"
		if st.firstRaw != nil {
			what = firstMessageChange(st.firstRaw, msgs[0])
		}
		s.logger.Printf("req=%s compaction first message changed session=%s: this request came as %s and its history is the one the summary was made from, so the summary stays; %s", rid, key, asSent[strings.LastIndex(asSent, "|")+1:], what)
	}
	if first && key == asSent && len(msgs) >= longHistory {
		// A long history nobody has seen, in a session that holds a
		// summary: say why the summary is not this history's.
		prefix := key[:strings.LastIndex(key, "|")+1]
		for k, d := range s.compaction.sessions {
			if k == key || !strings.HasPrefix(k, prefix) || d.summary == "" || now.Sub(d.firstLogged) < time.Minute {
				continue
			}
			d.firstLogged = now
			why := divergence(msgs, d.p0, d.marks)
			if !d.tail {
				why = "that summary was saved when a first message could not change, and is tried again when its own comes back"
			}
			s.logger.Printf("req=%s compaction not shared session=%s: %d messages under a first message not seen before, and the summary held as %s does not fit them (%s)", rid, key, len(msgs), k[strings.LastIndex(k, "|")+1:], why)
		}
	}
	if first && key == asSent && len(msgs) >= longHistory {
		// A long history nobody has seen, though this session and model
		// sent another a moment ago: the line says how they differ.
		prefix := key[:strings.LastIndex(key, "|")+1]
		var near *compactState
		nearKey := ""
		for k, d := range s.compaction.sessions {
			if k != key && strings.HasPrefix(k, prefix) && d.firstRaw != nil && now.Sub(d.seen) < 2*time.Minute && (near == nil || d.seen.After(near.seen)) {
				near, nearKey = d, k
			}
		}
		if near != nil && now.Sub(s.compaction.newFirstLogged[prefix]) >= time.Minute {
			if s.compaction.newFirstLogged == nil {
				s.compaction.newFirstLogged = map[string]time.Time{}
			}
			// One entry a session for as long as the gateway runs, unless
			// the old ones go: they only hold back a repeat for a minute.
			if len(s.compaction.newFirstLogged) > 500 {
				for k, at := range s.compaction.newFirstLogged {
					if now.Sub(at) >= time.Minute {
						delete(s.compaction.newFirstLogged, k)
					}
				}
			}
			s.compaction.newFirstLogged[prefix] = now
			s.logger.Printf("req=%s compaction new first message session=%s: %d messages, %d bytes, the first %d bytes of %s, the last %s; %s was seen %s ago with %d messages; first message: %s", rid, key, len(msgs), len(body), len(msgs[0]), lastMessageShape(msgs[:1]), lastMessageShape(msgs[len(msgs)-1:]), nearKey[strings.LastIndex(nearKey, "|")+1:], now.Sub(near.seen).Round(time.Second), near.msgCount, firstMessageChange(near.firstRaw, msgs[0]))
		}
	}
	st.msgCount = len(msgs)
	if st.firstRaw == nil || !bytes.Equal(st.firstRaw, msgs[0]) {
		st.firstRaw = append(json.RawMessage(nil), msgs[0]...)
	}
	if s.compaction.supersede(key, msgs[0]) {
		dirty = true
	}
	if first {
		if from := s.compaction.adopt(st, key, msgs); from != "" {
			s.logger.Printf("req=%s compaction adopted session=%s: the same conversation under another session id (%s), whose summary of %d messages fits this history", rid, key, from, st.p0)
			dirty = true
		}
	}

	// A summary only fits the history it was made from.
	// A dropped summary also reopens the window: the session is back to its
	// full history, and on 2026-09-30 the window kept it there, at 450k and
	// growing, for the rest of the hour after a drop.
	if st.summary != "" && (len(msgs) <= st.p0 || st.hashOf(msgs, st.p0) != st.hash) {
		s.logger.Printf("req=%s compaction dropped session=%s: history no longer matches (cleared, compacted or rewound; %s); window reopened", rid, key, divergence(msgs, st.p0, st.marks))
		st.summary, st.hash, st.p0, st.swapAt, st.marks = "", "", 0, 0, nil
		st.startedAt = time.Time{}
		st.notice("summary dropped (history changed); full history sent")
		s.compaction.outcome(key, autocompact.OutcomeEnded)
		notice.Publish(alertCompact, notice.Warn, "Compaction summary dropped",
			"The history was cleared, compacted or rewound, so the summary no longer fits. The full history goes again; a new summary can start at once.")
		dirty = true
	}
	// The waiting summary's cut: right after the request it was written
	// from when the history still starts with exactly that request, else
	// the latest plain prompt before it.
	nextCut, nextCutHash := 0, ""
	if st.next != "" {
		if tp := st.nextTightP0; tp > 0 && len(msgs) > tp {
			if h := st.hashOf(msgs, tp); h == st.nextTightHash && messageRole(msgs[tp]) == "assistant" {
				nextCut, nextCutHash = tp, h
			} else {
				s.logger.Printf("req=%s compaction cut moved back session=%s: the request the summary was written from is no longer the start of the history (%s, then %s); cutting at the latest prompt before it instead",
					rid, key, divergence(msgs, tp, st.nextMarks), lastMessageShape(msgs[tp:tp+1]))
				st.nextTightP0, st.nextTightHash = 0, ""
				dirty = true
			}
		}
		if nextCut == 0 && st.nextP0 > 0 && len(msgs) > st.nextP0 && st.hashOf(msgs, st.nextP0) == st.nextHash {
			nextCut, nextCutHash = st.nextP0, st.nextHash
		}
	}
	// A request no longer than the one the summary was written from, and
	// the same as far as it goes, is that request sent again: wait.
	resent := st.next != "" && nextCut == 0 && st.nextTightP0 > 0 && len(msgs) == st.nextTightP0 && st.hashOf(msgs, len(msgs)) == st.nextTightHash
	if st.next != "" && nextCut == 0 && !resent {
		at := st.nextP0
		if st.nextTightP0 > 0 {
			at = st.nextTightP0
		}
		s.logger.Printf("req=%s compaction dropped session=%s: history no longer matches the waiting summary (%s); window reopened", rid, key, divergence(msgs, at, st.nextMarks))
		st.next, st.nextHash, st.nextP0, st.nextMarks = "", "", 0, nil
		st.nextTightP0, st.nextTightHash = 0, ""
		st.startedAt = time.Time{}
		s.compaction.outcome(key, autocompact.OutcomeUnused)
		// Log only: a summary that never swapped in changed nothing the
		// model sees, and a notice for it, then one for the summary that
		// replaces it, was most of the noise on 2026-10-03.
		quietStart = true
		dirty = true
	}

	bounds := promptBoundaries(msgs)
	fresh := endsInPrompt(msgs)

	// The history as the model currently sees it: rewritten when a swap is
	// in force. A second compaction summarises this view, old summary and
	// all, so its summary covers everything before its boundary.
	view, offset := msgs, 0
	if st.summary != "" && st.swapAt > 0 {
		view = rewriteWithSummary(msgs, st.summary, st.p0, st.swapAt)
		// The view has a message of its own for the summary when the cut
		// is at a reply, so view index plus offset is the original index.
		offset = len(msgs) - len(view)
	}

	// A one-shot request carries a whole conversation in a single prompt
	// and is never continued: Claude Code's side calls (auto mode's
	// classifier, a recap) on another model, with the session's id. There
	// is nothing to summarise and no session to warn about: on 2026-10-04
	// one at 410k said "compaction soon" four times and never compacted.
	// The one seen live on 2026-10-04 was 2 user messages and no reply
	// ("2 messages, 2 prompts"), so a history no turn has answered yet
	// counts as one prompt too.
	oneShot := len(bounds) <= 1 || !hasAssistant(msgs)
	if !oneShot && st.lastContext >= cfg.WarnAtTokens && (st.warnedAt.IsZero() || now.Sub(st.warnedAt) >= window) {
		st.warnedAt = now
		s.logger.Printf("req=%s warn stage=compaction session=%s context=%dk (warn at %dk, %s)",
			rid, key, st.lastContext/1000, cfg.WarnAtTokens/1000, limit)
		alertContextNear(sid, root, st.lastContext, cfg.CompactAtTokens, true)
	}

	// /compact-async asks for a summary now, whatever the context size and
	// the window: the user chose the moment.
	forced := fresh && RequestsCompaction(msgs[len(msgs)-1])
	if forced && st.pending {
		st.notice("/compact-async: already summarising")
	} else if forced && st.next != "" {
		st.notice("/compact-async: summary ready, swaps in next prompt")
	}
	auto := !oneShot && st.lastContext >= cfg.CompactAtTokens && (st.startedAt.IsZero() || now.Sub(st.startedAt) >= window)
	if (auto || forced) && !st.pending && st.next == "" {
		// Only a compaction that actually starts opens the window. A skip
		// (no boundary yet, typically one long prompt) must leave the next
		// prompt free to compact.
		// The summary is written from this whole request, and its cut is
		// right after it: the reply to this request is the first message
		// kept. The latest plain prompt that leaves enough before it is the
		// cut to fall back to, when there is one.
		safeP, _ := compactionBoundary(view, bounds, offset)
		p, cut := len(msgs), len(view)
		if !hasAssistant(msgs) {
			cut = 0
		}
		if cut > 0 {
			st.startedAt = now
			st.pending = true
			dirty = true
			history := append([]json.RawMessage(nil), view...)
			hash := st.hashOf(msgs, p)
			safeHash := ""
			if safeP > 0 {
				safeHash = st.hashOf(msgs, safeP)
			}
			st.pendingMarks = messageMarks(msgs, p)
			why := ""
			if forced {
				why = " (requested with /compact-async)"
			}
			s.logger.Printf("req=%s compaction start session=%s context=%dk (%s) summarising %d of %d messages%s", rid, key, st.lastContext/1000, limit, p, len(msgs), why)
			if forced {
				st.notice("/compact-async: summarising %d messages (%dk) in the background", p, st.lastContext/1000)
			} else if !quietStart {
				st.notice("%dk context: summarising %d messages in the background", st.lastContext/1000, p)
			}
			s.compaction.running.Add(1)
			// Its own copy of top: this same request may still be rewritten
			// below (the current summary stays in force), which writes
			// top["messages"] while the summary goroutine reads top.
			own := make(map[string]json.RawMessage, len(top))
			for k, v := range top {
				own[k] = v
			}
			ctx, cancel := context.WithCancelCause(context.Background())
			s.compaction.cancels[key] = cancel
			go s.summarise(ctx, in.Clone(context.Background()), own, history, cut, key, p, hash, safeP, safeHash)
		} else if forced {
			s.logger.Printf("req=%s compaction skipped session=%s context=%dk: /compact-async with too little before this prompt to summarise", rid, key, st.lastContext/1000)
			st.notice("/compact-async: too little to summarise yet")
		} else if st.skippedAt.IsZero() || now.Sub(st.skippedAt) >= window {
			st.skippedAt = now
			s.logger.Printf("req=%s compaction skipped session=%s context=%dk: no turn has been answered yet (%d messages, %d prompts)",
				rid, key, st.lastContext/1000, len(msgs), len(bounds))
		}
	}

	if st.next != "" && nextCut > 0 && !fresh {
		kept := ""
		if st.summary != "" {
			kept = " (the previous summary stays in force)"
		}
		s.logger.Printf("req=%s compaction waiting session=%s: summary ready, request ends in %s, applies at the next plain prompt%s",
			rid, key, lastMessageShape(msgs), kept)
	}
	// Mid-turn: the running turn's own messages keep their thinking (they
	// are past swapAt); everything kept from before it loses thinking,
	// exactly as at a plain prompt. When the cut is inside the running turn
	// everything kept is that turn's, so nothing loses it.
	turnStart := 0
	if len(bounds) > 0 {
		turnStart = bounds[len(bounds)-1]
	}
	midTurn := !fresh && nextCut > 0 && cfg.MidTurn && !s.compaction.midTurnOff &&
		turnStart > 0
	if nextCut > 0 && (fresh || midTurn) {
		if midTurn {
			st.undo = &swapUndo{summary: st.summary, hash: st.hash, p0: st.p0, swapAt: st.swapAt, marks: st.marks,
				nextP0: st.nextP0, nextHash: st.nextHash, nextTightP0: st.nextTightP0, nextTightHash: st.nextTightHash, nextMarks: st.nextMarks}
			st.midTurnUnproven = true
		} else {
			st.undo, st.midTurnUnproven = nil, false
		}
		swapAt := len(msgs)
		if midTurn {
			swapAt = max(turnStart, nextCut)
		}
		st.summary, st.p0, st.hash, st.swapAt = st.next, nextCut, nextCutHash, swapAt
		st.marks, st.nextMarks = st.nextMarks, nil
		if len(st.marks) > nextCut {
			st.marks = st.marks[:nextCut]
		}
		st.next, st.nextP0, st.nextHash = "", 0, ""
		st.nextTightP0, st.nextTightHash = 0, ""
		where := ""
		switch {
		case midTurn && turnStart < nextCut:
			where = fmt.Sprintf(" mid-turn (the running turn is kept from message %d, the reply to the request the summary was written from)", nextCut)
		case midTurn:
			where = fmt.Sprintf(" mid-turn (the running turn, from message %d, kept as it was)", turnStart)
		}
		s.logger.Printf("req=%s compaction applied session=%s: %d messages replaced by a summary%s", rid, key, st.p0, where)
		st.swappedFrom, st.swappedMsgs, st.waitShown = st.lastContext, st.p0, false
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
		ci.parts = contextBytes(top, msgs)
		ci.rawBytes, ci.sentBytes = int64(len(body)), int64(len(body))
		return body, in.WithContext(context.WithValue(in.Context(), compactInfoKey{}, ci))
	}
	out := rewriteWithSummary(msgs, st.summary, st.p0, st.swapAt)
	if of := handoffOf(st.summary, st.p0); st.handOf != of {
		st.hand, st.handOf = buildHandoff(sid, msgs, st.summary, st.p0, st.rawContext, now), of
		dirty = true
	}
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
	ci.parts = contextBytes(top, out)
	ci.rawBytes, ci.sentBytes = int64(len(body)), int64(len(newBody))
	return newBody, in.WithContext(context.WithValue(in.Context(), compactInfoKey{}, ci))
}

// CompactAsyncMarker is in the prompt the /compact-async command sends; a
// plain prompt whose last message carries it asks for a summary now.
const CompactAsyncMarker = "claude-burst:compact-async"

// RequestsCompaction reports whether a message is a /compact-async prompt.
func RequestsCompaction(msg json.RawMessage) bool {
	return bytes.Contains(msg, []byte(CompactAsyncMarker))
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
	// The first message last: it changes without the history changing, so
	// a later one that differs is the news.
	for n := 1; n <= len(marks) && n <= len(msgs); n++ {
		i := n % min(len(marks), len(msgs))
		if prefixHash(msgs[i:i+1], 1) != marks[i] {
			return fmt.Sprintf("message %d of %d changed, now %s", i, p0, lastMessageShape(msgs[i:i+1]))
		}
	}
	return "no single message differs"
}

// hasAssistant: some turn in the history has been answered.
func hasAssistant(msgs []json.RawMessage) bool {
	for _, m := range msgs {
		var msg struct {
			Role string `json:"role"`
		}
		if json.Unmarshal(m, &msg) == nil && msg.Role == "assistant" {
			return true
		}
	}
	return false
}
