package metrics

import (
	"sort"
	"strings"
	"time"
)

// What a repository's compactions cost and bought, read back from the
// metrics log for internal/autocompact, which learns each repository's
// Compact at from it.
//
// A session's main conversation is cut into runs. A run starts where a
// summary first applies (Swapped, with the context it replaced in Before
// and what the summary and the cache rewrite cost in CostUSD) or where the
// session, or a history no summary fits any more, starts. It ends at the
// next of those. Start and End are the context at each end and Turns the
// requests between, so (End-Start)/Turns is how fast the run grew.
//
// Subagents run under their parent's session id with far smaller contexts:
// they carry an agent id and are left out. So are one-shot side calls on
// the same model, recognised by a context under two fifths of the
// conversation's; three in a row are taken for the conversation itself
// having shrunk (a rewind), which starts a new run.

// CompactionRun is one run of one session's main conversation.
type CompactionRun struct {
	Session string
	Model   string
	At      time.Time // the run's first request
	Last    time.Time // its latest
	Swapped bool      // it starts with a summary swapping in
	Before  int64     // the context the summary replaced; Swapped only
	Start   int64
	End     int64
	Turns   int
	CostUSD float64 // summary call and cache rewrite; Swapped only
}

// SummaryFailure is a summary call that wrote no summary.
type SummaryFailure struct {
	Session string
	At      time.Time
	USD     float64 // what it cost all the same
	Note    string
}

type runState struct {
	run        *CompactionRun
	compacted  int64 // CompactedMessages of the previous request
	pendingUSD float64
	lows       int // requests in a row far under the conversation's context
}

// sideShare is the share of the conversation's context under which a
// request is taken for a side call, and sideRun how many in a row are taken
// for the conversation instead.
const (
	sideFloor = 50_000
	sideRun   = 3
)

// CompactionRunsSince returns every run with a request at or after since,
// oldest first, and the summary calls that failed.
func CompactionRunsSince(path string, since time.Time) ([]CompactionRun, []SummaryFailure, error) {
	var runs []CompactionRun
	var failures []SummaryFailure
	states := map[string]*runState{}
	order := []string{}
	closeRun := func(st *runState) {
		if st.run != nil && st.run.Turns > 0 {
			runs = append(runs, *st.run)
		}
		st.run = nil
	}
	for _, f := range historyFiles(path, since) {
		err := scanEvents(f, func(e Event) {
			if e.Time.Before(since) || e.Slot != "primary" || e.SessionID == "" || e.AgentID != "" {
				return
			}
			key := e.SessionID + "|" + e.Model
			st := states[key]
			if st == nil {
				st = &runState{}
				states[key] = st
				order = append(order, key)
			}
			if strings.HasPrefix(e.Note, "compaction summary") {
				if e.Note == "compaction summary" && ok(e) {
					st.pendingUSD += e.APIEquivalentUSD
				} else {
					failures = append(failures, SummaryFailure{Session: e.SessionID, At: e.Time, USD: e.APIEquivalentUSD, Note: e.Note})
				}
				return
			}
			ctx := eventContext(e)
			if !ok(e) || ctx == 0 {
				return
			}
			swap := e.CompactedMessages > 0 && e.CompactedMessages != st.compacted
			dropped := e.CompactedMessages == 0 && st.compacted > 0
			start := func(swapped bool) {
				var before int64
				if st.run != nil {
					before = st.run.End
				}
				closeRun(st)
				st.run = &CompactionRun{Session: e.SessionID, Model: e.Model, At: e.Time, Last: e.Time, Swapped: swapped, Start: ctx, End: ctx, Turns: 1}
				if swapped {
					st.run.Before = max(before, ctx)
					st.run.CostUSD = st.pendingUSD + rewriteUSD(e.Model, e.CacheWriteTokens)
					st.pendingUSD = 0
				}
				st.lows = 0
			}
			switch {
			case st.run == nil, swap, dropped:
				start(swap)
			case st.run.End >= sideFloor && ctx*5 < st.run.End*2:
				st.lows++
				if st.lows >= sideRun {
					start(false)
				}
			default:
				st.lows = 0
				st.run.End, st.run.Last = ctx, e.Time
				st.run.Turns++
			}
			st.compacted = e.CompactedMessages
		})
		if err != nil {
			return nil, nil, err
		}
	}
	for _, key := range order {
		closeRun(states[key])
	}
	sort.SliceStable(runs, func(i, j int) bool { return runs[i].At.Before(runs[j].At) })
	return runs, failures, nil
}

// CacheReadPrice is what one token read from cache costs on model, in USD:
// the price every resent token of a long session pays. 0 when unpriced.
func CacheReadPrice(model string) float64 {
	return price(model, 1_000_000, 0) / 1_000_000
}
