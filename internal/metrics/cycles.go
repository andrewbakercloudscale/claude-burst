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
// A summary's run is the requests sent with it, which is not every request
// between two summaries, and not only those logged with its count. A
// session on message threads sends a turn as the new message alone: until
// 0.20.7 such a request was logged with no count, and one whose thread
// began before the summary still carries the whole history. Claude Code's
// side requests send the conversation whole and take the summary first. So
// a request with no count that stays at the summary's size is its run going
// on, one that has risen by half of what the summary took out is the
// conversation at its full size (a run of its own, while the summary's
// waits), and the summary's count seen again is its run going on. Read by
// the count alone, every compaction of a thread ended at its second
// request: on 6 Oct 2026 that was "sessions go on 1 turns after a
// compaction" for a session 30 turns into one, and its repository's learned
// Compact at went back to the fixed one.
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
	run *CompactionRun
	// sw is the run of the summary in force, and count its
	// CompactedMessages. It is run itself, or it waits while run is the
	// conversation at its full size.
	sw         *CompactionRun
	count      int64
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
		if st.sw != nil && st.sw != st.run && st.sw.Turns > 0 {
			runs = append(runs, *st.sw)
		}
		if st.run != nil && st.run.Turns > 0 {
			runs = append(runs, *st.run)
		}
		st.run, st.sw, st.count = nil, nil, 0
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
			count := e.CompactedMessages
			newRun := func() {
				st.run = &CompactionRun{Session: e.SessionID, Model: e.Model, At: e.Time, Last: e.Time, Start: ctx, End: ctx, Turns: 1}
				st.lows = 0
			}
			start := func(swapped bool) {
				var before int64
				if st.run != nil {
					before = st.run.End
				}
				closeRun(st)
				newRun()
				if swapped {
					st.run.Swapped, st.sw, st.count = true, st.run, count
					st.run.Before = max(before, ctx)
					st.run.CostUSD = st.pendingUSD + rewriteUSD(e.Model, e.CacheWriteTokens)
					st.pendingUSD = 0
				}
			}
			goOn := func() {
				st.lows = 0
				st.run.End, st.run.Last = ctx, e.Time
				st.run.Turns++
			}
			// How far the summary's run is from the context it replaced: a
			// request that has risen by half of that carries the history
			// the summary took out.
			whole := func() bool {
				return st.sw.Before > st.sw.Start && (ctx-st.sw.End)*2 >= st.sw.Before-st.sw.Start
			}
			side := func() bool { return st.run.End >= sideFloor && ctx*5 < st.run.End*2 }
			switch {
			case st.run == nil:
				start(count > 0)
			case count > 0 && count != st.count:
				start(true)
			case count > 0 && st.run != st.sw:
				// The summary again, after the conversation at its full
				// size: its run goes on.
				runs = append(runs, *st.run)
				st.run = st.sw
				goOn()
			case count == 0 && st.sw != nil && st.run == st.sw && whole():
				// The summary's run waits: it goes on if the summary comes
				// back, and ends at the next one otherwise.
				newRun()
			case count == 0 && st.sw != nil && st.run != st.sw && !whole() && ctx*5 >= st.sw.End*2:
				// Back at the summary's size, though nothing says so.
				runs = append(runs, *st.run)
				st.run = st.sw
				goOn()
			case side():
				st.lows++
				if st.lows >= sideRun {
					start(false)
				}
			default:
				goOn()
			}
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
