package metrics

import (
	"sort"
	"time"
)

// What pauseless compaction (router/compact.go) saved, read back from the
// metrics log. A compacted request does not record what it would have sent
// without the summary, so each one is credited with the drop its session
// saw when the summary was swapped in: the context of the last request
// before it, minus the context of the first request after. The session keeps
// growing afterwards on both sides of that line, so the drop is what every
// later compacted request did not carry.
//
// Drops accumulate: a second compaction starts from a context the first had
// already shrunk, so without either the session would carry both drops. The
// credit is capped so that a request's context plus its saving never passes
// maxContext, because Claude Code compacts on its own near the end of its
// window and a session never really grows past it.

// maxContext is the context window of every model compaction applies to
// (it only starts at hundreds of thousands of tokens, past Haiku's 200k).
const maxContext = 1_000_000

// CompactionStats is the dashboard's summary of a window.
type CompactionStats struct {
	// Compactions is how many summaries were written, and SummaryUSD what
	// they cost at API-equivalent prices.
	Compactions int     `json:"compactions"`
	SummaryUSD  float64 `json:"summary_usd"`
	// CompactedRequests went out with a summary in place of older history;
	// TokensNotResent and SavedUSD are what they did not carry, priced at
	// the cache-read rate those tokens would have been billed at.
	CompactedRequests int     `json:"compacted_requests"`
	TokensNotResent   int64   `json:"tokens_not_resent"`
	SavedUSD          float64 `json:"saved_usd"`
	// Largest is the biggest single drop, as before and after context.
	LargestBefore int64 `json:"largest_before"`
	LargestAfter  int64 `json:"largest_after"`
	// Sessions is each compacted session's latest compaction, newest first.
	Sessions []CompactedSession `json:"sessions"`
}

// CompactedSession is one session's most recent compaction.
type CompactedSession struct {
	Session string    `json:"session"`
	Model   string    `json:"model"`
	At      time.Time `json:"at"`
	// Before is the context the session would have had without any of its
	// summaries at the latest swap (capped at maxContext); After is what it
	// actually sent.
	Before int64 `json:"before"`
	After  int64 `json:"after"`
	// PerTurnTokens and PerTurnUSD are what each later request saves.
	PerTurnTokens int64   `json:"per_turn_tokens"`
	PerTurnUSD    float64 `json:"per_turn_usd"`
	// Requests and SavedTokens/SavedUSD cover every request since.
	Requests    int     `json:"requests"`
	SavedTokens int64   `json:"saved_tokens"`
	SavedUSD    float64 `json:"saved_usd"`
}

// eventContext is everything a request sent: uncached, cache read and cache
// write.
func eventContext(e Event) int64 { return e.InputTokens + e.CacheReadTokens + e.CacheWriteTokens }

// cacheReadUSD prices tokens at model's cache-read rate; 0 when unpriced.
func cacheReadUSD(model string, tokens int64) float64 {
	pricerMu.RLock()
	p := pricer
	pricerMu.RUnlock()
	if p == nil || tokens <= 0 {
		return 0
	}
	usd, _ := p(model, 0, 0, tokens, 0)
	return usd
}

// compactionTracker follows sessions through the log in time order.
type compactionTracker struct {
	sessions map[string]*trackedSession
}

type trackedSession struct {
	lastCtx       int64
	lastCompacted int64 // CompactedMessages of the previous request
	cur           *CompactedSession
}

// compactionEffect is what one event means for compaction: a summary's cost,
// or a compacted request's saving (and whether it is the swap itself).
type compactionEffect struct {
	summary    bool
	summaryUSD float64
	saved      int64
	savedUSD   float64
	swap       *CompactedSession
}

func newCompactionTracker() *compactionTracker {
	return &compactionTracker{sessions: map[string]*trackedSession{}}
}

func (t *compactionTracker) observe(e Event) compactionEffect {
	var fx compactionEffect
	if e.Slot != "primary" || !ok(e) || e.SessionID == "" {
		return fx
	}
	if e.Note == "compaction summary" {
		fx.summary, fx.summaryUSD = true, e.APIEquivalentUSD
		return fx
	}
	ctx := eventContext(e)
	if ctx == 0 {
		return fx
	}
	key := e.SessionID + "|" + e.Model
	s := t.sessions[key]
	if s == nil {
		s = &trackedSession{}
		t.sessions[key] = s
	}
	if e.CompactedMessages > 0 {
		// A new summary took effect: measure its drop once, on top of what
		// the session's earlier summaries were already saving.
		if e.CompactedMessages != s.lastCompacted && s.lastCtx > ctx {
			perTurn := s.lastCtx - ctx
			if s.cur != nil {
				perTurn += s.cur.PerTurnTokens
			}
			perTurn = min(perTurn, maxContext-ctx)
			s.cur = &CompactedSession{
				Session: e.SessionID, Model: e.Model, At: e.Time,
				Before: ctx + perTurn, After: ctx,
				PerTurnTokens: perTurn, PerTurnUSD: cacheReadUSD(e.Model, perTurn),
			}
			fx.swap = s.cur
		}
		if s.cur != nil {
			if saved := min(s.cur.PerTurnTokens, maxContext-ctx); saved > 0 {
				fx.saved, fx.savedUSD = saved, cacheReadUSD(e.Model, saved)
			}
		}
	} else {
		s.cur = nil
	}
	s.lastCtx, s.lastCompacted = ctx, e.CompactedMessages
	return fx
}

func CompactionStatsSince(path string, since time.Time) (CompactionStats, error) {
	var st CompactionStats
	t := newCompactionTracker()
	latest := map[string]*CompactedSession{}
	for _, f := range historyFiles(path, since) {
		err := scanEvents(f, func(e Event) {
			fx := t.observe(e)
			if e.Time.Before(since) {
				return
			}
			switch {
			case fx.summary:
				st.Compactions++
				st.SummaryUSD += fx.summaryUSD
				return
			case fx.saved == 0:
				return
			}
			if fx.swap != nil {
				latest[e.SessionID+"|"+e.Model] = fx.swap
				if fx.swap.PerTurnTokens > st.LargestBefore-st.LargestAfter {
					st.LargestBefore, st.LargestAfter = fx.swap.Before, fx.swap.After
				}
			}
			st.CompactedRequests++
			st.TokensNotResent += fx.saved
			st.SavedUSD += fx.savedUSD
			if c := t.sessions[e.SessionID+"|"+e.Model].cur; c != nil {
				c.Requests++
				c.SavedTokens += fx.saved
				c.SavedUSD += fx.savedUSD
			}
		})
		if err != nil {
			return st, err
		}
	}
	for _, c := range latest {
		st.Sessions = append(st.Sessions, *c)
	}
	sort.Slice(st.Sessions, func(i, j int) bool { return st.Sessions[i].At.After(st.Sessions[j].At) })
	return st, nil
}
