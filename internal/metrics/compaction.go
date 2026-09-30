package metrics

import (
	"sort"
	"time"
)

// What pauseless compaction (router/compact.go) saved, read back from the
// metrics log.
//
// A compacted request does not record what it would have sent without
// Burst, so each session is replayed request by request beside a "without
// Burst" twin: a context that grows by exactly what the real one grows by,
// but never takes Burst's drops. Where the twin would reach Claude Code's own
// auto-compact trigger, it is compacted the way Claude Code would compact it,
// back down to the size of a summary. The saving on each request is the
// twin's context minus the real one, priced at the cache-read rate (every
// resent token of a long session is a cache read). It can be negative: a
// twin that Claude Code has just compacted can be smaller than the real
// session, and that request is counted against Burst.
//
// The net saving then takes off what Burst spent to get there: the summary
// calls, and on the first request after each swap the cache write of the
// new, shorter history (billed as a write where the twin would have read).
// What the twin would have spent on Claude Code's own compactions is not
// credited back, so the net figure errs low.

// claudeCodeTrigger is where the twin is compacted: about 95% of the 1M
// window every model compaction applies to. Claude Code does not publish
// its exact threshold.
const claudeCodeTrigger = 950_000

// CompactionStats is the dashboard's summary of a window.
type CompactionStats struct {
	// Compactions is how many summaries were written, and SummaryUSD what
	// they cost at API-equivalent prices.
	Compactions int     `json:"compactions"`
	SummaryUSD  float64 `json:"summary_usd"`
	// RewriteUSD is the extra cost of writing each shortened history to
	// cache, where the twin would have read it.
	RewriteUSD float64 `json:"rewrite_usd"`
	// CompactedRequests went out with a summary in place of older history;
	// TokensNotResent and SavedUSD are the twin's context minus the real one,
	// summed over them (gross), and NetUSD is SavedUSD less the summaries
	// and the rewrites.
	CompactedRequests int     `json:"compacted_requests"`
	TokensNotResent   int64   `json:"tokens_not_resent"`
	SavedUSD          float64 `json:"saved_usd"`
	NetUSD            float64 `json:"net_usd"`
	// TwinCompactions is how many times the twin reached Claude Code's
	// trigger and was compacted back down.
	TwinCompactions int `json:"twin_compactions"`
	// Largest is the biggest single drop, as before and after context.
	LargestBefore int64 `json:"largest_before"`
	LargestAfter  int64 `json:"largest_after"`
	// Sessions is every compacted session, most recently swapped first.
	Sessions []CompactedSession `json:"sessions"`
}

// CompactedSession is one session over the window.
type CompactedSession struct {
	Session string    `json:"session"`
	Model   string    `json:"model"`
	At      time.Time `json:"at"`
	// Before is the twin's context at the latest swap and After the real
	// one; PerTurnTokens and PerTurnUSD are the difference.
	Before        int64   `json:"before"`
	After         int64   `json:"after"`
	PerTurnTokens int64   `json:"per_turn_tokens"`
	PerTurnUSD    float64 `json:"per_turn_usd"`
	// The rest cover every compacted request of the session in the window.
	Compactions int     `json:"compactions"`
	Requests    int     `json:"requests"`
	SavedTokens int64   `json:"saved_tokens"`
	SavedUSD    float64 `json:"saved_usd"`
	SummaryUSD  float64 `json:"summary_usd"`
	RewriteUSD  float64 `json:"rewrite_usd"`
	NetUSD      float64 `json:"net_usd"`
}

// eventContext is everything a request sent: uncached, cache read and cache
// write.
func eventContext(e Event) int64 { return e.InputTokens + e.CacheReadTokens + e.CacheWriteTokens }

func price(model string, cacheRead, cacheWrite int64) float64 {
	pricerMu.RLock()
	p := pricer
	pricerMu.RUnlock()
	if p == nil {
		return 0
	}
	usd, _ := p(model, 0, 0, cacheRead, cacheWrite)
	return usd
}

// cacheReadUSD prices tokens at model's cache-read rate, keeping the sign.
func cacheReadUSD(model string, tokens int64) float64 {
	if tokens < 0 {
		return -price(model, -tokens, 0)
	}
	return price(model, tokens, 0)
}

// rewriteUSD is what writing tokens to cache costs over reading them.
func rewriteUSD(model string, tokens int64) float64 {
	if tokens <= 0 {
		return 0
	}
	return price(model, 0, tokens) - price(model, tokens, 0)
}

// compactionTracker replays sessions through the log in time order.
type compactionTracker struct {
	sessions map[string]*trackedSession
}

type trackedSession struct {
	lastCtx       int64
	lastCompacted int64 // CompactedMessages of the previous request
	twin          int64 // the context without Burst; 0 until known
	summarySize   int64 // what a compaction brings the context down to
}

// compactionEffect is what one event means for compaction.
type compactionEffect struct {
	summaryUSD float64
	isSummary  bool
	compacted  bool  // the request carried a Burst summary
	saved      int64 // twin minus real, may be negative
	savedUSD   float64
	rewriteUSD float64
	twinReset  bool
	swapBefore int64 // set on the request a new summary first applies to
	swapAfter  int64
	sessionKey string
}

func newCompactionTracker() *compactionTracker {
	return &compactionTracker{sessions: map[string]*trackedSession{}}
}

func (t *compactionTracker) session(key string) *trackedSession {
	s := t.sessions[key]
	if s == nil {
		s = &trackedSession{}
		t.sessions[key] = s
	}
	return s
}

func (t *compactionTracker) observe(e Event) compactionEffect {
	var fx compactionEffect
	if e.Slot != "primary" || !ok(e) || e.SessionID == "" {
		return fx
	}
	key := e.SessionID + "|" + e.Model
	fx.sessionKey = key
	s := t.session(key)
	if e.Note == "compaction summary" {
		fx.isSummary, fx.summaryUSD = true, e.APIEquivalentUSD
		return fx
	}
	ctx := eventContext(e)
	if ctx == 0 {
		return fx
	}
	swap := e.CompactedMessages > 0 && e.CompactedMessages != s.lastCompacted
	switch {
	case e.CompactedMessages == 0:
		// No Burst summary in force (never compacted, cleared, rewound, or
		// a summary that stopped fitting): both worlds send the same.
		s.twin = ctx
	case swap:
		// Burst's drop: the twin does not take it.
		if s.twin == 0 {
			s.twin = max(s.lastCtx, ctx)
		}
		s.summarySize = ctx
		fx.swapBefore, fx.swapAfter = s.twin, ctx
		fx.rewriteUSD = rewriteUSD(e.Model, e.CacheWriteTokens)
	default:
		// Ordinary growth, which the twin shares.
		if s.twin == 0 {
			s.twin = ctx
		} else {
			s.twin += ctx - s.lastCtx
		}
		if s.twin >= claudeCodeTrigger {
			// Claude Code would compact here on its own.
			s.twin = s.summarySize
			fx.twinReset = true
		}
	}
	if e.CompactedMessages > 0 {
		fx.compacted = true
		fx.saved = s.twin - ctx
		fx.savedUSD = cacheReadUSD(e.Model, fx.saved)
	}
	s.lastCtx, s.lastCompacted = ctx, e.CompactedMessages
	return fx
}

func CompactionStatsSince(path string, since time.Time) (CompactionStats, error) {
	var st CompactionStats
	t := newCompactionTracker()
	bySession := map[string]*CompactedSession{}
	pendingSummary := map[string]float64{} // summary cost awaiting its swap
	for _, f := range historyFiles(path, since) {
		err := scanEvents(f, func(e Event) {
			fx := t.observe(e)
			if e.Time.Before(since) || fx.sessionKey == "" {
				return
			}
			if fx.isSummary {
				st.Compactions++
				st.SummaryUSD += fx.summaryUSD
				if c := bySession[fx.sessionKey]; c != nil {
					c.SummaryUSD += fx.summaryUSD
				} else {
					pendingSummary[fx.sessionKey] += fx.summaryUSD
				}
				return
			}
			if !fx.compacted {
				return
			}
			c := bySession[fx.sessionKey]
			if c == nil {
				c = &CompactedSession{Session: e.SessionID, Model: e.Model}
				bySession[fx.sessionKey] = c
				c.SummaryUSD += pendingSummary[fx.sessionKey]
				delete(pendingSummary, fx.sessionKey)
			}
			if fx.swapAfter > 0 {
				c.Compactions++
				c.At, c.Before, c.After = e.Time, fx.swapBefore, fx.swapAfter
				c.PerTurnTokens = fx.swapBefore - fx.swapAfter
				c.PerTurnUSD = cacheReadUSD(e.Model, c.PerTurnTokens)
				if c.PerTurnTokens > st.LargestBefore-st.LargestAfter {
					st.LargestBefore, st.LargestAfter = fx.swapBefore, fx.swapAfter
				}
			}
			if fx.twinReset {
				st.TwinCompactions++
			}
			c.Requests++
			c.SavedTokens += fx.saved
			c.SavedUSD += fx.savedUSD
			c.RewriteUSD += fx.rewriteUSD
			st.CompactedRequests++
			st.TokensNotResent += fx.saved
			st.SavedUSD += fx.savedUSD
			st.RewriteUSD += fx.rewriteUSD
		})
		if err != nil {
			return st, err
		}
	}
	st.NetUSD = st.SavedUSD - st.SummaryUSD - st.RewriteUSD
	for _, c := range bySession {
		c.NetUSD = c.SavedUSD - c.SummaryUSD - c.RewriteUSD
		st.Sessions = append(st.Sessions, *c)
	}
	sort.Slice(st.Sessions, func(i, j int) bool { return st.Sessions[i].At.After(st.Sessions[j].At) })
	return st, nil
}
