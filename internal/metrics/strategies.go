package metrics

import (
	"sort"
	"time"
)

// What three ways of compacting would have cost over the same sessions, day
// by day and repository by repository:
//
//   - Default: Claude Code alone, which compacts only when the context
//     nears the model's window (claudeCodeTrigger).
//   - Fixed: one Compact at for every repository (the configured size).
//   - Actual: what Burst did, whichever mode was in force at the time. With
//     Intelligent Compaction Mode on, this is the learned size for each
//     repository.
//
// Actual is read from the log. The other two are twins replayed beside it:
// a context that grows by exactly what the real one grew by and is compacted
// where that strategy would compact it. Every line is costed the same way,
// so the lines can be compared: each request resends its context at the
// cache-read price, and each compaction pays for its summary and for
// writing the shorter history to the cache. A real compaction is charged
// what it was billed; a twin's is modelled (see twinCompactionUSD). Output
// and new input are the same on all three and are left out.

const (
	// assumedAfter is what a twin's compaction leaves when the session has
	// no real compaction to copy the size from: about what Burst's
	// summaries leave on this log (58k to 62k).
	assumedAfter = 60_000
	// assumedSummaryOutput is the tokens a twin's summary writes: the
	// median of Burst's own is about 3.5k.
	assumedSummaryOutput = 3_500
	// sharedDrop is how far a context must fall, with no Burst summary in
	// force, to be a /clear, a /compact or a rewind: something the user did
	// and every strategy shares, rather than a few tokens of jitter.
	sharedDrop = 0.5
)

// StrategyCost is one strategy's cost over some requests.
type StrategyCost struct {
	// Tokens is the context sent, summed over the requests.
	Tokens        int64   `json:"tokens"`
	ContextUSD    float64 `json:"context_usd"`
	Compactions   int     `json:"compactions"`
	CompactionUSD float64 `json:"compaction_usd"`
	// USD is ContextUSD plus CompactionUSD.
	USD float64 `json:"usd"`
}

func (c *StrategyCost) add(o StrategyCost) {
	c.Tokens += o.Tokens
	c.ContextUSD += o.ContextUSD
	c.Compactions += o.Compactions
	c.CompactionUSD += o.CompactionUSD
	c.USD = c.ContextUSD + c.CompactionUSD
}

// StrategyCosts is the three strategies over the same requests.
type StrategyCosts struct {
	Requests int          `json:"requests"`
	Default  StrategyCost `json:"default"`
	Fixed    StrategyCost `json:"fixed"`
	Actual   StrategyCost `json:"actual"`
}

func (c *StrategyCosts) add(o StrategyCosts) {
	c.Requests += o.Requests
	c.Default.add(o.Default)
	c.Fixed.add(o.Fixed)
	c.Actual.add(o.Actual)
}

// StrategyDay is one local day.
type StrategyDay struct {
	Date string `json:"date"` // 2006-01-02, local
	StrategyCosts
}

// StrategyRepo is one repository's sessions.
type StrategyRepo struct {
	Repo     string `json:"repo"`
	Path     string `json:"path,omitempty"`
	Sessions int    `json:"sessions"`
	StrategyCosts
	Daily []StrategyDay `json:"daily"`
}

// Strategies is the dashboard's comparison over a window.
type Strategies struct {
	// FixedAt and DefaultAt are where the two twins compact.
	FixedAt   int64 `json:"fixed_at"`
	DefaultAt int64 `json:"default_at"`
	StrategyCosts
	Daily []StrategyDay  `json:"daily"`
	Repos []StrategyRepo `json:"repos"`
}

// twin is a context under a strategy the session did not run.
type twin struct {
	at  int64 // where it compacts
	ctx int64 // 0 until known
	// own is set once the twin has compacted where the real session did
	// not: from then on it follows the real context's growth, not its size.
	own bool
}

type strategySession struct {
	lastCtx       int64
	lastCompacted int64
	after         int64 // what a real compaction left, 0 when none yet
	def, fixed    twin
}

// twinCompactionUSD models a compaction of before tokens down to after: the
// summary reads the whole context from the cache and writes its text, then
// the shorter history is written to the one-hour cache where it would have
// been read.
func twinCompactionUSD(model string, before, after int64) float64 {
	usd := cacheReadUSD(model, before) + rewriteUSD(model, after, after)
	pricerMu.RLock()
	p := pricer
	pricerMu.RUnlock()
	if p != nil {
		out, _ := p(model, 0, assumedSummaryOutput, 0, 0)
		usd += out
	}
	return usd
}

// step moves the twin by one real request and costs it.
func (t *twin) step(s *strategySession, e Event, ctx int64, swap bool) StrategyCost {
	var c StrategyCost
	delta := ctx - s.lastCtx
	switch {
	case t.ctx == 0:
		t.ctx = max(s.lastCtx, ctx)
	case swap:
		// Burst's drop: the twin does not take it.
	case e.CompactedMessages == 0 && s.lastCompacted > 0:
		// Burst's summary went out of force and the real session is back at
		// its full size, which is where a twin that never compacted on its
		// own already is.
		if !t.own {
			t.ctx = ctx
		}
	case e.CompactedMessages == 0 && s.lastCtx > 0 && float64(ctx) < float64(s.lastCtx)*sharedDrop:
		// The user cleared, compacted or rewound: every strategy shares it.
		t.ctx, t.own = ctx, false
	case e.CompactedMessages == 0 && !t.own:
		t.ctx = ctx
	default:
		t.ctx = max(t.ctx+delta, 1)
	}
	if t.ctx >= t.at {
		after := s.after
		if after <= 0 || after >= t.at {
			after = min(assumedAfter, t.at/2)
		}
		c.Compactions = 1
		c.CompactionUSD = twinCompactionUSD(e.Model, t.ctx, after)
		t.ctx, t.own = after, true
	}
	c.Tokens = t.ctx
	c.ContextUSD = cacheReadUSD(e.Model, t.ctx)
	c.USD = c.ContextUSD + c.CompactionUSD
	return c
}

// CompactionStrategiesSince replays the log since `since` under the three
// strategies. fixedAt is the one Compact at the Fixed twin uses. repoOf
// names a session's repository and its root; nil files every session under
// one row.
func CompactionStrategiesSince(path string, since time.Time, fixedAt int64, repoOf func(session string) (name, root string)) (Strategies, error) {
	st := Strategies{FixedAt: fixedAt, DefaultAt: claudeCodeTrigger}
	if fixedAt <= 0 || fixedAt > claudeCodeTrigger {
		st.FixedAt = claudeCodeTrigger
	}
	now := time.Now()
	from := since
	if limit := now.AddDate(0, 0, -92); from.Before(limit) {
		from = limit
	}
	dayIndex := map[string]int{}
	var dates []string
	for d := from.Local(); ; d = d.AddDate(0, 0, 1) {
		key := d.Format("2006-01-02")
		if _, ok := dayIndex[key]; !ok {
			dayIndex[key] = len(dates)
			dates = append(dates, key)
		}
		if key >= now.Local().Format("2006-01-02") {
			break
		}
	}
	newDays := func() []StrategyDay {
		days := make([]StrategyDay, len(dates))
		for i, d := range dates {
			days[i].Date = d
		}
		return days
	}
	st.Daily = newDays()

	sessions := map[string]*strategySession{}
	repos := map[string]*StrategyRepo{}
	repoBySession := map[string]*StrategyRepo{}
	repoFor := func(session string) *StrategyRepo {
		if r := repoBySession[session]; r != nil {
			return r
		}
		name, root := "all sessions", ""
		if repoOf != nil {
			name, root = repoOf(session)
		}
		r := repos[name]
		if r == nil {
			r = &StrategyRepo{Repo: name, Path: root, Daily: newDays()}
			repos[name] = r
		}
		r.Sessions++
		repoBySession[session] = r
		return r
	}
	record := func(e Event, c StrategyCosts) {
		i, ok := dayIndex[e.Time.Local().Format("2006-01-02")]
		if e.Time.Before(since) || !ok {
			return
		}
		r := repoFor(e.SessionID)
		st.add(c)
		st.Daily[i].add(c)
		r.add(c)
		r.Daily[i].add(c)
	}

	for _, f := range historyFiles(path, since) {
		err := scanEvents(f, func(e Event) {
			if e.Slot != "primary" || !ok(e) || e.SessionID == "" {
				return
			}
			key := e.SessionID + "|" + e.Model
			s := sessions[key]
			if s == nil {
				s = &strategySession{def: twin{at: claudeCodeTrigger}, fixed: twin{at: st.FixedAt}}
				sessions[key] = s
			}
			if SummaryPaid(e.Note) {
				// Paid whether or not it wrote a summary.
				var c StrategyCosts
				c.Actual.CompactionUSD = e.APIEquivalentUSD
				if SummaryWritten(e.Note) {
					c.Actual.Compactions = 1
				}
				c.Actual.USD = c.Actual.CompactionUSD
				record(e, c)
				return
			}
			ctx := eventContext(e)
			if ctx == 0 {
				return
			}
			swap := e.CompactedMessages > 0 && e.CompactedMessages != s.lastCompacted
			c := StrategyCosts{Requests: 1}
			c.Default = s.def.step(s, e, ctx, swap)
			c.Fixed = s.fixed.step(s, e, ctx, swap)
			c.Actual.Tokens = ctx
			c.Actual.ContextUSD = cacheReadUSD(e.Model, ctx)
			if swap {
				s.after = ctx
				c.Actual.CompactionUSD = rewriteUSD(e.Model, e.CacheWriteTokens, e.CacheWrite1hTokens)
			}
			c.Actual.USD = c.Actual.ContextUSD + c.Actual.CompactionUSD
			s.lastCtx, s.lastCompacted = ctx, e.CompactedMessages
			record(e, c)
		})
		if err != nil {
			return st, err
		}
	}
	for _, r := range repos {
		st.Repos = append(st.Repos, *r)
	}
	// The repository with the most context to pay for first.
	sort.Slice(st.Repos, func(i, j int) bool {
		a, b := st.Repos[i].Default.USD, st.Repos[j].Default.USD
		if a != b {
			return a > b
		}
		return st.Repos[i].Repo < st.Repos[j].Repo
	})
	return st, nil
}
