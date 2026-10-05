package metrics

import (
	"sort"
	"strings"
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
	// TwinTokens and TwinUSD are what the same compacted requests would
	// have sent without Burst: the base for "what share of the context was
	// compacted". TokensNotResent / TwinTokens is that share.
	TwinTokens int64   `json:"twin_tokens"`
	TwinUSD    float64 `json:"twin_usd"`
	// TwinCompactions is how many times the twin reached Claude Code's
	// trigger and was compacted back down.
	TwinCompactions int `json:"twin_compactions"`
	// Largest is the biggest single drop, as before and after context.
	LargestBefore int64 `json:"largest_before"`
	LargestAfter  int64 `json:"largest_after"`
	// Sessions is every compacted session, most recently swapped first.
	Sessions []CompactedSession `json:"sessions"`
	// Daily is the same money by local day, every day of the window
	// included, oldest first: the savings chart. Its days sum to the totals.
	Daily []CompactionDay `json:"daily"`
}

// CompactionDay is one local day of CompactionStats.
type CompactionDay struct {
	Date        string  `json:"date"` // 2006-01-02, local
	Compactions int     `json:"compactions"`
	Requests    int     `json:"requests"`
	SavedUSD    float64 `json:"saved_usd"`
	SummaryUSD  float64 `json:"summary_usd"`
	RewriteUSD  float64 `json:"rewrite_usd"`
	NetUSD      float64 `json:"net_usd"`
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
	unfinished bool  // a summary call that was paid for and wrote no summary
	compacted  bool  // the request carried a Burst summary
	saved      int64 // twin minus real, may be negative
	savedUSD   float64
	twin       int64 // what the request would have sent without Burst
	twinUSD    float64
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
	// A summary cut short (the stream broke, or the session was cleared)
	// was still paid for: its cost counts, the summary does not.
	if e.Note == "compaction summary" || strings.HasPrefix(e.Note, "compaction summary incomplete") {
		fx.isSummary, fx.summaryUSD = true, e.APIEquivalentUSD
		fx.unfinished = e.Note != "compaction summary"
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
		fx.twin = s.twin
		fx.twinUSD = cacheReadUSD(e.Model, s.twin)
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
	dayIndex := map[string]int{}
	// Daily covers the window, but never more than 92 days: a zero since
	// (every event) would otherwise list every day since year 1.
	from := since
	if limit := time.Now().AddDate(0, 0, -92); from.Before(limit) {
		from = limit
	}
	for d := from.Local(); !d.After(time.Now()); d = d.AddDate(0, 0, 1) {
		key := d.Format("2006-01-02")
		if _, ok := dayIndex[key]; !ok {
			dayIndex[key] = len(st.Daily)
			st.Daily = append(st.Daily, CompactionDay{Date: key})
		}
	}
	if today := time.Now().Local().Format("2006-01-02"); len(st.Daily) == 0 || st.Daily[len(st.Daily)-1].Date != today {
		dayIndex[today] = len(st.Daily)
		st.Daily = append(st.Daily, CompactionDay{Date: today})
	}
	dayOf := func(t time.Time) *CompactionDay {
		if i, ok := dayIndex[t.Local().Format("2006-01-02")]; ok {
			return &st.Daily[i]
		}
		return nil
	}
	for _, f := range historyFiles(path, since) {
		err := scanEvents(f, func(e Event) {
			fx := t.observe(e)
			if e.Time.Before(since) || fx.sessionKey == "" {
				return
			}
			day := dayOf(e.Time)
			if fx.isSummary {
				if !fx.unfinished {
					st.Compactions++
				}
				st.SummaryUSD += fx.summaryUSD
				if day != nil {
					if !fx.unfinished {
						day.Compactions++
					}
					day.SummaryUSD += fx.summaryUSD
				}
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
			if day != nil {
				day.Requests++
				day.SavedUSD += fx.savedUSD
				day.RewriteUSD += fx.rewriteUSD
			}
			st.CompactedRequests++
			st.TokensNotResent += fx.saved
			st.SavedUSD += fx.savedUSD
			st.TwinTokens += fx.twin
			st.TwinUSD += fx.twinUSD
			st.RewriteUSD += fx.rewriteUSD
		})
		if err != nil {
			return st, err
		}
	}
	st.NetUSD = st.SavedUSD - st.SummaryUSD - st.RewriteUSD
	for i := range st.Daily {
		d := &st.Daily[i]
		d.NetUSD = d.SavedUSD - d.SummaryUSD - d.RewriteUSD
	}
	for _, c := range bySession {
		c.NetUSD = c.SavedUSD - c.SummaryUSD - c.RewriteUSD
		st.Sessions = append(st.Sessions, *c)
	}
	sort.Slice(st.Sessions, func(i, j int) bool { return st.Sessions[i].At.After(st.Sessions[j].At) })
	return st, nil
}
