package router

import (
	"context"
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/andrewbakercloudscale/claude-burst/internal/autocompact"
	"github.com/andrewbakercloudscale/claude-burst/internal/config"
)

// resolve gives the key of the conversation a request belongs to. That is
// its own key, session, model and first message, unless nothing is held
// under it and another conversation of the same session and model holds a
// summary this history fits: then the first message is what changed, and
// the conversation is that one. Claude Code keeps CLAUDE.md, the memory
// index and its other context in the first message and writes it again
// when one of them changes, which until 6 Oct 2026 put the session under a
// key with no summary and sent it whole. Caller holds c.mu.
func (c *compactor) resolve(key string, msgs []json.RawMessage) string {
	own := c.sessions[key]
	if own != nil && (own.summary != "" || own.next != "" || own.pending) {
		return key
	}
	prefix := key[:strings.LastIndex(key, "|")+1]
	best := ""
	for k, d := range c.sessions {
		if k == key || !strings.HasPrefix(k, prefix) || !d.fits(msgs) {
			continue
		}
		if best == "" || d.seen.After(c.sessions[best].seen) {
			best = k
		}
	}
	if best == "" {
		return key
	}
	if own != nil {
		// It held a size and nothing else: a request of this conversation
		// seen before this one was known to be it.
		delete(c.sessions, key)
	}
	return best
}

type compactor struct {
	mu       sync.Mutex
	cfg      config.CompactionConfig
	sessions map[string]*compactState // keyed by session id + "|" + model
	path     string                   // where sessions survive a restart; "" = memory only
	logger   *log.Logger
	running  sync.WaitGroup // summary calls in flight
	// cancels stops a summary call in flight, by session key: /clear ends
	// the conversation it is for, and the rest of it would be paid for
	// and thrown away.
	cancels map[string]context.CancelCauseFunc
	// outcomes is the log of summaries that were dropped, for the learner
	// (internal/autocompact); "" keeps none.
	outcomes string
	// newFirstLogged is when a session and model's latest "new first
	// message" line was logged, to keep it to one a minute.
	newFirstLogged map[string]time.Time
	// threads is which conversation each response belongs to, by its
	// message id, for the request that continues it (threadOf); threadIDs
	// is the order they came in, to forget the oldest.
	threads   map[string]threadResponse
	threadIDs []string
	// asks is the latest request for its history made to each session and
	// model, by session id + "|" + model + "|".
	asks map[string]*threadAsk
	// midTurnOff: the API rejected a mid-turn swap, so none are tried again
	// until the settings are saved again (SetCompaction). One rejection is
	// taken as the API's answer: retrying each turn would cost a wasted
	// round trip every time to learn the same thing.
	midTurnOff bool
}

func newCompactor(c config.CompactionConfig, path string, logger *log.Logger) *compactor {
	cp := &compactor{cfg: c.Resolved(), sessions: map[string]*compactState{}, cancels: map[string]context.CancelCauseFunc{}, path: path, logger: logger}
	if path != "" {
		cp.outcomes = filepath.Join(filepath.Dir(path), "compaction-outcomes.jsonl")
	}
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
	// The cut right after the request the summary was written from.
	NextTightP0   int       `json:"next_tight_p0,omitempty"`
	NextTightHash string    `json:"next_tight_hash,omitempty"`
	Seen          time.Time `json:"seen"`
	// ExposureWarned: the exposure was logged once (noteExposure); kept so a
	// restart does not tell it again.
	ExposureWarned int64 `json:"exposure_warned,omitempty"`
	// The prompt notice's unshown lines and the context before the latest
	// swap. Memory only until 2026-10-01, when a deploy landed between a
	// swap and the next prompt (gateway restarts are routine: six in that
	// hour) and the user saw no notice for it at all.
	Notices     []string `json:"notices,omitempty"`
	SwappedFrom int64    `json:"swapped_from,omitempty"`
	SwappedMsgs int      `json:"swapped_msgs,omitempty"`
	Hand        *Handoff `json:"hand,omitempty"`
	// Tail: the hashes leave the first message out (compactState.tail).
	Tail bool `json:"tail,omitempty"`
	// What a request that carries the whole conversation tells Burst, kept
	// so a restart does not lose it: a session on message threads may not
	// send another for a long time. Parts is the context bar's make-up
	// (on 6 Oct 2026 an upgrade left a panel with no legend), Raw is
	// Claude Code's own history, and the marks name each message a summary
	// covers, to tell a history that changed from a request beside it.
	Parts     []ContextPart `json:"parts,omitempty"`
	Raw       int64         `json:"raw,omitempty"`
	Marks     []string      `json:"marks,omitempty"`
	NextMarks []string      `json:"next_marks,omitempty"`
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
			next: v.Next, nextP0: v.NextP0, nextHash: v.NextHash, nextTightP0: v.NextTightP0, nextTightHash: v.NextTightHash, seen: v.Seen,
			notices: v.Notices, swappedFrom: v.SwappedFrom, swappedMsgs: v.SwappedMsgs, exposureWarned: v.ExposureWarned, hand: v.Hand,
			// With no hash saved there is nothing made the old way.
			tail:  v.Tail || (v.Hash == "" && v.NextHash == "" && v.NextTightHash == ""),
			parts: v.Parts, rawContext: v.Raw, marks: v.Marks, nextMarks: v.NextMarks}
		if st.hand != nil {
			st.handOf = st.hand.Of
		}
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
	// The files are what the mod reads, and the rules for which summary a
	// session offers may have changed with this build.
	c.writeHandoffs()
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
			Next: st.next, NextP0: st.nextP0, NextHash: st.nextHash, NextTightP0: st.nextTightP0, NextTightHash: st.nextTightHash, Seen: st.seen,
			Notices: st.notices, SwappedFrom: st.swappedFrom, SwappedMsgs: st.swappedMsgs, ExposureWarned: st.exposureWarned, Hand: st.hand, Tail: st.tail,
			Parts: st.parts, Raw: st.rawContext, Marks: st.marks, NextMarks: st.nextMarks}
	}
	c.writeHandoffs()
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
		st = &compactState{tail: true}
		c.sessions[key] = st
	}
	return st
}

// adopt gives a session seen for the first time the summary another session
// holds for the same conversation: a fork (claude --resume --fork-session,
// which is how the handover writer opens a finished session) has a new
// session id and Claude Code's whole history. Without this the fork's first
// request goes whole: on 2026-10-05 a session Burst held at 158k was forked
// to write its handover and sent 1,320k, which the API refused; Claude Code
// then summarised 747k of it itself and sent the other 629k, $7 and two
// minutes for a note. A summary only fits the history it was made from, so
// the donor's is taken only where its hash matches this request, and with
// its swapAt, so the request is the one the donor's cache already holds.
// Returns the donor's key, or "". Caller holds c.mu.
func (c *compactor) adopt(st *compactState, key string, msgs []json.RawMessage) string {
	if st.summary != "" || st.next != "" || st.pending {
		return ""
	}
	_, rest, _ := strings.Cut(key, "|")
	_, conv, _ := strings.Cut(rest, "|")
	var from string
	var donor *compactState
	for k, d := range c.sessions {
		if k == key || !strings.HasSuffix(k, "|"+conv) || d.summary == "" || d.swapAt == 0 || d.midTurnUnproven || len(msgs) <= d.p0 {
			continue
		}
		if donor != nil && d.p0 <= donor.p0 {
			continue
		}
		if d.hashOf(msgs, d.p0) == d.hash {
			from, donor = k, d
		}
	}
	if donor == nil {
		return ""
	}
	st.summary, st.p0, st.hash, st.swapAt = donor.summary, donor.p0, donor.hash, min(donor.swapAt, len(msgs))
	st.tail = donor.tail
	st.marks = donor.marks
	st.startedAt = donor.startedAt
	return from
}

// SaveCompaction writes the compaction state as it stands, for a gateway
// about to exit: it is otherwise saved on transitions only, so each
// session's size and make-up on disk are those of its last transition.
func (s *Server) SaveCompaction() {
	s.compaction.mu.Lock()
	s.compaction.save()
	s.compaction.mu.Unlock()
}

// SetCompaction applies c to the running gateway; the admin page's toggle.
func (s *Server) SetCompaction(c config.CompactionConfig) {
	s.compaction.mu.Lock()
	learned := s.compaction.cfg.Learned
	s.compaction.cfg = c.Resolved()
	s.compaction.cfg.Learned = learned
	s.compaction.midTurnOff = false
	s.compaction.mu.Unlock()
}

// SetLearnedCompaction gives the running gateway each repository's learned
// Compact at, by root (internal/autocompact). They apply only in the
// intelligent mode, and never over a repository's own override.
func (s *Server) SetLearnedCompaction(byRoot map[string]int64) {
	s.compaction.mu.Lock()
	s.compaction.cfg.Learned = byRoot
	s.compaction.mu.Unlock()
}

// CompactionOutcomesPath is where dropped summaries are logged, "" when
// nowhere.
func (s *Server) CompactionOutcomesPath() string { return s.compaction.outcomes }

// outcome logs a summary that was dropped. Caller holds mu; the write is a
// short append.
func (c *compactor) outcome(key, kind string) {
	sid, _, _ := strings.Cut(key, "|")
	if err := autocompact.AppendOutcome(c.outcomes, autocompact.Outcome{Time: time.Now(), Session: sid, Kind: kind}); err != nil {
		c.logger.Printf("compaction outcome not logged: %v", err)
	}
}

// compactionStatePath keeps compaction state beside the overflow state file.
func compactionStatePath(statePath string) string {
	if statePath == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(statePath), "compaction-state.json")
}
