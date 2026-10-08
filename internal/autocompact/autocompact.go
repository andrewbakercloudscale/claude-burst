// Package autocompact is Intelligent Compaction Mode: a Compact at learned
// for each repository from what its compactions cost and what they saved,
// adjusted once a day.
//
// The trade. A session's context grows g tokens a turn from A, where a
// compaction leaves it, to T, where the next one starts, and every turn
// reads the whole of it from cache at p a token. A compaction at T costs a
// read of T for the summary call plus c0 for the summary's output and the
// cache rewrite. Over one cycle of (T-A)/g turns that is
//
//	cost per turn = (p*T + c0) * g / (T-A)  +  p * (A+T) / 2
//
// the first term cheaper the later you compact, the second the earlier.
// It is lowest at
//
//	T = A + sqrt(2 * g * (A + c0/p))
//
// A failure is a compaction that lost money: it did not save what it cost,
// or its summary was paid for and never used. Those are measured, and the
// cost of a compaction that works is divided by the share that do: with a failure rate f the square root's contents are
// divided by 1-f, which moves T up.
//
// One loss among compactions that paid moves T by that much and no more.
// Two losses in a row are a size that is wrong now: they add a tenth to the
// target, once however long the streak, and a threshold below its target
// then moves up at once. A compaction that pays ends the streak. Until
// 6 Oct 2026 every loss in the window added a tenth: 3 of 35, $1.22 lost
// in all, held one repository 50k to 90k above its cheapest size, which
// cost more on every turn than the losses had.
//
// Late, not cheapest. The money is not all a compaction costs: it is also a
// pause and a summary that keeps less than the history did, and the model
// has no figure for either. So the target is not the cheapest size but the
// latest one whose turn costs no more than slackShare above the cheapest:
// the cost rises slowly past its lowest point, so a little money buys many
// fewer compactions. Until 7 Oct 2026 the target was the cheapest size and
// a busy repository compacted every 80 requests, as often as the delay let
// it. For a few hours that day slackShare was a quarter with a fifth of
// buffer on top: a third more a turn than the cheapest, about $39 in 14
// days on this Mac. At a twentieth and no buffer the turn costs 5% more
// and a compaction comes every 75 requests where the cheapest size has one
// every 47.
//
// Measured, not modelled. The formula above is a model of a repository:
// one growth rate, one size a compaction leaves, and a compaction whenever
// the context reaches T. The log has the repository's real requests, so
// each size from the floor to the ceiling is replayed over them
// (metrics.CompactionStrategiesSince), with the compaction settings' delay
// between a session's compactions, and the target is the latest size whose
// replay cost no more than slackShare above the cheapest one's. On 8 Oct
// 2026 the formula's cheapest size for one repository was 126k and the
// replay's curve was flat from 100k to 200k, and what Burst had done over
// the same 14 days ($622) cost more than one fixed 300k would have ($568)
// and a quarter more than each repository on its replayed best ($464). A
// replayed target is taken at once and whenever it moves, not a tenth a
// day: it is every size tried on the same requests, which is the
// experiment a slow step was waiting for. The buffer is for the formula's
// errors and is not added to it. The formula remains for a repository the
// replay has too little of.
//
// Guards, since the log measures money and not what a summary loses: never
// below the floor, never above the fixed Compact at, a tenth a day at most
// once a threshold is in use (the first one learned goes straight there),
// nothing learned from fewer than MinCompactions, and back to the fixed
// Compact at where most compactions fail or sessions end before one has
// paid for itself. How often a session may compact at all stays the
// compaction settings' delay.
package autocompact

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"sort"
	"time"

	"github.com/andrewbakercloudscale/claude-burst/internal/atomicfile"
	"github.com/andrewbakercloudscale/claude-burst/internal/metrics"
)

const (
	// Window is how far back a repository's compactions are read.
	Window = 14 * 24 * time.Hour
	// MinCompactions is how many a repository needs before anything is
	// learned from them.
	MinCompactions = 3
	// stepShare is the most a threshold moves in one day.
	stepShare = 0.10
	// round is what a threshold is rounded to.
	round = 5_000
	// minGrowthTurns is the shortest run a growth rate is taken from.
	minGrowthTurns = 10
	// settled is how long after its last request a run is taken as over,
	// so its saving can be judged.
	settled = time.Hour
	// maxFailShare caps the failure rate's effect on the target, and
	// backOffShare is the rate from which a repository is left on the
	// fixed Compact at, once backOffAttempts have been seen.
	maxFailShare    = 0.8
	backOffShare    = 0.5
	backOffAttempts = 4
	// lossRaisePercent is added to a repository's Compact at, once, while
	// its latest lossStreak compactions all lost money, at once and not at
	// the daily step: a size that keeps losing money is never kept waiting
	// for tomorrow.
	lossRaisePercent = 10
	lossStreak       = 2
	// slackShare is how much more than the cheapest a turn may cost so that
	// compactions come less often: see the package comment.
	slackShare = 0.05
	// minMeasuredRequests is how many requests a repository needs in the
	// window before a size is taken from their replay.
	minMeasuredRequests = 300
)

// Outcome kinds the gateway records (router), beside the summary calls
// that failed, which the metrics log has.
const (
	// OutcomeUnused: a summary was written and dropped before it applied.
	OutcomeUnused = "unused"
	// OutcomeEnded: a summary in force stopped fitting (/clear, /compact, a
	// rewind). Not a failure: it is counted, and its cost is judged by
	// whether the compaction had paid for itself by then.
	OutcomeEnded = "ended"
)

// Outcome is one line of the gateway's outcome log.
type Outcome struct {
	Time    time.Time `json:"time"`
	Session string    `json:"session"`
	Kind    string    `json:"kind"`
}

// Failures is the compactions that lost money in the window: what one cost
// did not come back. That is the one test of a failure.
type Failures struct {
	// Unpaid: a compaction that had not saved what it cost by the time its
	// run was over. SummaryFailed: a summary call that was paid for and
	// wrote no summary. Unused: a summary paid for and dropped before it
	// applied.
	SummaryFailed int `json:"summary_failed"`
	Unused        int `json:"unused"`
	Unpaid        int `json:"unpaid"`
	// Ended is informational, see OutcomeEnded.
	Ended int `json:"ended"`
	// Attempts is every summary started; Rate is the failures' share of it.
	Attempts int     `json:"attempts"`
	Rate     float64 `json:"rate"`
	// LostUSD is what the failures cost beyond what they saved, where the
	// log has the figure (an unused summary's cost is in the next one's).
	LostUSD float64 `json:"lost_usd"`
	// Streak is how many of the latest compactions lost money, one after
	// another: 0 when the latest one judged paid for itself.
	Streak int `json:"streak"`
}

// Count is the compactions that lost money, of Attempts.
func (f Failures) Count() int { return f.SummaryFailed + f.Unused + f.Unpaid }

// Repo is one repository's learned Compact at and how it got there.
type Repo struct {
	Root string `json:"root"`
	Name string `json:"name"`
	// Threshold is the Compact at in force in the intelligent mode, 0 when
	// nothing is learned yet. Target is where the model says it should be,
	// and Previous what Threshold was before the last daily step.
	Threshold int64 `json:"threshold"`
	Target    int64 `json:"target"`
	Previous  int64 `json:"previous,omitempty"`
	// The inputs, from the window: what a compaction leaves, how fast the
	// context grows, what a compaction costs, and how long a session goes
	// on after one.
	Compactions int     `json:"compactions"`
	AfterTokens int64   `json:"after_tokens"`
	GrowthTurn  int64   `json:"growth_per_turn"`
	CostUSD     float64 `json:"cost_usd"`
	TurnsAfter  int     `json:"turns_after"`
	PaybackTurn int     `json:"payback_turns"`
	// SavedTurnUSD is what a turn costs less at Target than at the fixed
	// Compact at, by the model.
	SavedTurnUSD float64  `json:"saved_per_turn_usd"`
	Failures     Failures `json:"failures"`
	// Measured is set when Target comes from the replay of the
	// repository's own requests and not from the formula.
	Measured  *Measured `json:"measured,omitempty"`
	Reason    string    `json:"reason"`
	LearnedAt time.Time `json:"learned_at"`
	SteppedOn string    `json:"stepped_on,omitempty"` // 2006-01-02, local
}

// Measured is what the replay of a repository's requests said: the
// cheapest size and its cost, what the target's replay cost, and beside
// them what Burst did and what the one fixed Compact at would have.
type Measured struct {
	Requests    int     `json:"requests"`
	CheapestAt  int64   `json:"cheapest_at"`
	CheapestUSD float64 `json:"cheapest_usd"`
	LatestAt    int64   `json:"latest_at"`
	LatestUSD   float64 `json:"latest_usd"`
	ActualUSD   float64 `json:"actual_usd"`
	FixedUSD    float64 `json:"fixed_usd"`
}

// measuredSizes is the cheapest replayed size inside the bounds and the
// latest one that cost no more than slackShare above it. ok is false when
// the replay has too little to say: too few requests, or a cheapest size
// that compacted fewer than MinCompactions times.
func measuredSizes(m metrics.StrategyRepo, b Bounds) (cheapest, latest metrics.StrategySize, ok bool) {
	if m.Requests < minMeasuredRequests {
		return cheapest, latest, false
	}
	found := false
	for _, z := range m.Sizes {
		if z.At < b.Floor || z.At > b.Ceiling {
			continue
		}
		if !found || z.USD <= cheapest.USD {
			cheapest, found = z, true
		}
	}
	if !found || cheapest.Compactions < MinCompactions || cheapest.USD <= 0 {
		return cheapest, latest, false
	}
	latest = cheapest
	for _, z := range m.Sizes {
		if z.At > latest.At && z.At <= b.Ceiling && z.USD <= cheapest.USD*(1+slackShare) {
			latest = z
		}
	}
	return cheapest, latest, true
}

// State is the learner's file.
type State struct {
	Repos map[string]*Repo `json:"repos"`
}

// Load reads the state, empty when there is none or it cannot be read.
func Load(path string) State {
	st := State{Repos: map[string]*Repo{}}
	b, err := os.ReadFile(path)
	if err != nil {
		return st
	}
	_ = json.Unmarshal(b, &st)
	if st.Repos == nil {
		st.Repos = map[string]*Repo{}
	}
	return st
}

// Save writes the state.
func Save(path string, st State) error {
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	return atomicfile.Write(path, b, 0o600)
}

// Reseat forgets every threshold in use, so the next Learn that steps takes
// each repository straight to its target: for when the user has changed
// what the targets are worked out from.
func (st State) Reseat() {
	for _, r := range st.Repos {
		r.Threshold, r.SteppedOn = 0, ""
	}
}

// Thresholds is each repository's Compact at in force, by root.
func (st State) Thresholds() map[string]int64 {
	out := map[string]int64{}
	for root, r := range st.Repos {
		if r.Threshold > 0 {
			out[root] = r.Threshold
		}
	}
	return out
}

// Sorted is the repositories, most compactions first.
func (st State) Sorted() []Repo {
	out := make([]Repo, 0, len(st.Repos))
	for _, r := range st.Repos {
		out = append(out, *r)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Compactions != out[j].Compactions {
			return out[i].Compactions > out[j].Compactions
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// Bounds are the limits a learned Compact at stays inside.
type Bounds struct {
	Floor   int64 // never compact below this
	Ceiling int64 // the fixed Compact at: never above it
	// Start is what a repository compacts at before anything is learned
	// for it, the middle of the range; 0 is the Ceiling.
	Start int64
	// BufferPercent is added to the cheapest size: room for what the money
	// does not measure.
	BufferPercent int
}

// Optimal is the Compact at with the lowest cost per turn: see the package
// comment. after is A, growth g, extraTokens c0/p (what a compaction costs
// beyond reading the history, in cache-read tokens) and failRate f.
func Optimal(after, growth, extraTokens int64, failRate float64) int64 {
	if growth <= 0 {
		return 0
	}
	f := math.Min(math.Max(failRate, 0), maxFailShare)
	return after + int64(math.Sqrt(2*float64(growth)*float64(after+extraTokens)/(1-f)))
}

// costPerTurn is the model's cost of a turn in USD at Compact at t.
func costPerTurn(t, after, growth int64, readPrice, extraUSD float64) float64 {
	if t <= after || growth <= 0 {
		return 0
	}
	return (readPrice*float64(t)+extraUSD)*float64(growth)/float64(t-after) + readPrice*float64(after+t)/2
}

// Latest is the largest Compact at, in steps of round from cheapest, whose
// turn costs no more than slackShare above a turn at cheapest. The cost
// only rises past the cheapest size, so the first step over the limit ends
// it.
func Latest(cheapest, after, growth int64, readPrice, extraUSD float64) int64 {
	// With no price for a token read the cost only falls as t grows, and
	// the walk below would never end.
	if readPrice <= 0 {
		return cheapest
	}
	limit := costPerTurn(cheapest, after, growth, readPrice, extraUSD) * (1 + slackShare)
	t := cheapest
	for limit > 0 && costPerTurn(t+round, after, growth, readPrice, extraUSD) <= limit {
		t += round
	}
	return t
}

func roundTo(n int64) int64 { return (n + round/2) / round * round }

func median(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	sort.Float64s(v)
	if len(v)%2 == 1 {
		return v[len(v)/2]
	}
	return (v[len(v)/2-1] + v[len(v)/2]) / 2
}

// Inputs is what Learn reads: the window's runs and failed summary calls
// from the metrics log, the gateway's outcome log, which repository a
// session is in, and what a cached token costs on a model.
type Inputs struct {
	Runs      []metrics.CompactionRun
	Failed    []metrics.SummaryFailure
	Outcomes  []Outcome
	Resolve   func(session string) (name, root string)
	ReadPrice func(model string) float64
	Now       time.Time
	// Measured is the replay of each repository's requests over the window
	// at every size, by root: see the package comment. nil leaves every
	// repository to the formula.
	Measured map[string]metrics.StrategyRepo
}

// Learn works out each repository's target from in and, when step is true,
// moves each Threshold one day's step towards it. Repositories already in
// st keep their Threshold when the window has nothing new to say. It
// returns the new state; st is not changed.
func Learn(st State, in Inputs, b Bounds, step bool) State {
	type acc struct {
		name              string
		swapped           []metrics.CompactionRun
		growth, after, c0 []float64
		turnsAfter        []float64
		price             float64
		latest            time.Time
		fails             Failures
		costs             []float64
		judged            []judgedAt
	}
	by := map[string]*acc{}
	get := func(session string) *acc {
		name, root := in.Resolve(session)
		if root == "" {
			return nil
		}
		a := by[root]
		if a == nil {
			a = &acc{name: name}
			by[root] = a
		}
		return a
	}
	for _, r := range in.Runs {
		a := get(r.Session)
		if a == nil {
			continue
		}
		p := in.ReadPrice(r.Model)
		if !r.Last.Before(a.latest) && p > 0 {
			a.latest, a.price = r.Last, p
		}
		if r.Turns >= minGrowthTurns && r.End > r.Start {
			a.growth = append(a.growth, float64(r.End-r.Start)/float64(r.Turns))
		}
		if !r.Swapped {
			continue
		}
		a.swapped = append(a.swapped, r)
		a.after = append(a.after, float64(r.Start))
		// One that swapped in on a cold cache cost its summary and no
		// rewrite: not what a compaction in a session at work costs, which
		// is what a Compact at is worked out from.
		if !r.Cold {
			a.costs = append(a.costs, r.CostUSD)
			if p > 0 {
				a.c0 = append(a.c0, math.Max(0, r.CostUSD-p*float64(r.Before)))
			}
		}
		if in.Now.Sub(r.Last) >= settled {
			a.turnsAfter = append(a.turnsAfter, float64(r.Turns))
			// Each of its turns read Before-Start fewer tokens, less what
			// it cost to write the whole history again if the
			// conversation went back to it.
			saved := p*float64(r.Before-r.Start)*float64(r.Turns) - r.BackUSD
			if saved < r.CostUSD {
				a.fails.Unpaid++
				a.fails.LostUSD += r.CostUSD - saved
			}
			a.judged = append(a.judged, judgedAt{r.At, saved < r.CostUSD})
		}
	}
	for _, f := range in.Failed {
		// One that failed before it was billed lost nothing.
		if a := get(f.Session); a != nil && f.USD > 0 {
			a.fails.SummaryFailed++
			a.fails.LostUSD += f.USD
			a.judged = append(a.judged, judgedAt{f.At, true})
		}
	}
	for _, o := range in.Outcomes {
		a := get(o.Session)
		if a == nil {
			continue
		}
		switch o.Kind {
		case OutcomeUnused:
			a.fails.Unused++
			a.judged = append(a.judged, judgedAt{o.Time, true})
		case OutcomeEnded:
			a.fails.Ended++
		}
	}

	out := State{Repos: map[string]*Repo{}}
	for root, old := range st.Repos {
		c := *old
		out.Repos[root] = &c
	}
	today := in.Now.Local().Format("2006-01-02")
	for root, a := range by {
		r := out.Repos[root]
		cheapest, latest, measured := measuredSizes(in.Measured[root], b)
		// A repository that has never had a summary started, and too few
		// requests to replay, has nothing to show: it is on the starting
		// size like any folder not listed.
		if r == nil && !measured && len(a.swapped) == 0 && a.fails.SummaryFailed+a.fails.Unused == 0 {
			continue
		}
		if r == nil {
			r = &Repo{Root: root}
			out.Repos[root] = r
		}
		r.Name, r.LearnedAt = a.name, in.Now
		n := len(a.swapped)
		// A summary that was never used never swapped in: it is an attempt
		// beside the ones that did.
		a.fails.Attempts = n + a.fails.SummaryFailed + a.fails.Unused
		if a.fails.Attempts > 0 {
			a.fails.Rate = math.Min(1, float64(a.fails.Count())/float64(a.fails.Attempts))
		}
		a.fails.Streak = streak(a.judged)
		r.Failures, r.Compactions = a.fails, n
		r.AfterTokens = int64(median(a.after))
		r.GrowthTurn = int64(median(a.growth))
		r.CostUSD = median(a.costs)
		r.TurnsAfter = int(median(a.turnsAfter))
		r.PaybackTurn, r.SavedTurnUSD, r.Measured = 0, 0, nil

		extraUSD := median(a.c0)
		// Room to work in: half as much again as a compaction leaves.
		low := max(b.Floor, r.AfterTokens*3/2)
		// best is the size before the bounds are applied to it.
		var best int64
		switch {
		case measured:
			m := in.Measured[root]
			best = latest.At
			r.Measured = &Measured{Requests: m.Requests, CheapestAt: cheapest.At, CheapestUSD: cheapest.USD,
				LatestAt: latest.At, LatestUSD: latest.USD, ActualUSD: m.Actual.USD, FixedUSD: m.Fixed.USD}
			r.Reason = fmt.Sprintf("replayed over the %d requests of the last %d days, which cost $%.2f as Burst ran them: cheapest at %dk ($%.2f)", m.Requests, int(Window.Hours()/24), m.Actual.USD, cheapest.At/1000, cheapest.USD)
			if latest.At > cheapest.At {
				r.Reason += fmt.Sprintf(", as late as costs no more than %d%% more: %dk ($%.2f)", int(slackShare*100), latest.At/1000, latest.USD)
			}
		case n < MinCompactions:
			r.Target = 0
			r.Reason = fmt.Sprintf("%d of the %d compactions needed in the last %d days: on the starting size, the middle of the range, until then", n, MinCompactions, int(Window.Hours()/24))
			continue
		case r.GrowthTurn <= 0 || a.price <= 0:
			r.Target = 0
			r.Reason = "no run long enough to measure how fast the context grows: on the starting size, the middle of the range"
			continue
		default:
			best = Optimal(r.AfterTokens, r.GrowthTurn, int64(extraUSD/a.price), a.fails.Rate)
			cheapest := best
			best = Latest(best, r.AfterTokens, r.GrowthTurn, a.price, extraUSD)
			late := best
			best = best * int64(100+max(b.BufferPercent, 0)) / 100
			r.Reason = fmt.Sprintf("a compaction leaves %dk and costs $%.2f, the context grows %.1fk a turn: cheapest at %dk", r.AfterTokens/1000, r.CostUSD, float64(r.GrowthTurn)/1000, cheapest/1000)
			if late > cheapest {
				r.Reason += fmt.Sprintf(", as late as costs no more than %d%% more a turn: %dk", int(slackShare*100), late/1000)
			}
			if b.BufferPercent > 0 {
				r.Reason += fmt.Sprintf(", plus the %d%% buffer: %dk", b.BufferPercent, best/1000)
			}
		}
		target := min(max(best, low), b.Ceiling)
		if gap := float64(target - r.AfterTokens); gap > 0 && a.price > 0 && n > 0 {
			r.PaybackTurn = int(math.Ceil((a.price*float64(target) + extraUSD) / (a.price * gap)))
		}
		if c := a.fails.Streak; c >= lossStreak {
			raised := min(max(best, low)*int64(100+lossRaisePercent)/100, b.Ceiling)
			r.Reason += fmt.Sprintf(", plus %d%% because the last %d compactions lost money: %dk", lossRaisePercent, c, raised/1000)
			target = raised
		}
		switch {
		case a.fails.Attempts >= backOffAttempts && a.fails.Rate > backOffShare:
			target = b.Ceiling
			r.Reason = fmt.Sprintf("%d of %d compactions lost money: back to the fixed Compact at", a.fails.Count(), a.fails.Attempts)
		case len(a.turnsAfter) >= MinCompactions && r.TurnsAfter < 2*r.PaybackTurn:
			target = b.Ceiling
			r.Reason = fmt.Sprintf("sessions go on %d turns after a compaction and one needs %d to pay for itself: back to the fixed Compact at", r.TurnsAfter, r.PaybackTurn)
		case best < low:
			r.Reason += fmt.Sprintf(", held at %dk (the floor, or room above what a compaction leaves)", low/1000)
		case best > b.Ceiling:
			r.Reason += fmt.Sprintf(", held at the fixed Compact at of %dk", b.Ceiling/1000)
		}
		r.Target = roundTo(target)
		r.SavedTurnUSD = costPerTurn(b.Ceiling, r.AfterTokens, r.GrowthTurn, a.price, extraUSD) - costPerTurn(r.Target, r.AfterTokens, r.GrowthTurn, a.price, extraUSD)
	}
	for _, r := range out.Repos {
		// Bounds the user has since moved apply at once, step or no step.
		if r.Threshold > 0 {
			r.Threshold = min(max(r.Threshold, b.Floor), b.Ceiling)
		}
		// The latest compactions lost money: up to the target now, whatever
		// the day.
		if r.Failures.Streak >= lossStreak && r.Threshold > 0 && r.Target > r.Threshold {
			r.Previous, r.Threshold = r.Threshold, min(max(r.Target, b.Floor), b.Ceiling)
		}
		// A size replayed over the repository's own requests: taken at
		// once and whenever it moves, see the package comment.
		if step && r.Measured != nil && r.Target > 0 {
			if next := min(max(r.Target, b.Floor), b.Ceiling); next != r.Threshold {
				from := r.Threshold
				if from <= 0 {
					from = b.Start
				}
				if from <= 0 {
					from = b.Ceiling
				}
				r.Previous, r.Threshold = from, next
			}
			r.SteppedOn = today
			continue
		}
		if !step || r.SteppedOn == today {
			continue
		}
		r.SteppedOn = today
		if r.Target <= 0 {
			// Nothing to learn from any more: the starting size.
			r.Previous, r.Threshold = r.Threshold, 0
			continue
		}
		cur := r.Threshold
		if cur <= 0 {
			// Learned for the first time: straight to the target. The daily
			// step is for adjusting a threshold already in use.
			start := b.Start
			if start <= 0 {
				start = b.Ceiling
			}
			r.Previous, r.Threshold = start, r.Target
			continue
		}
		limit := int64(float64(cur) * stepShare)
		next := cur + min(max(r.Target-cur, -limit), limit)
		if d := r.Target - next; d > -round && d < round {
			next = r.Target
		}
		r.Previous, r.Threshold = cur, min(max(roundTo(next), b.Floor), b.Ceiling)
	}
	return out
}

// judgedAt is one compaction whose money is known: when, and whether it
// lost any.
type judgedAt struct {
	at   time.Time
	lost bool
}

// streak is how many of the latest judged compactions lost money, one
// after another.
func streak(j []judgedAt) int {
	sort.SliceStable(j, func(a, b int) bool { return j[a].at.Before(j[b].at) })
	n := 0
	for i := len(j) - 1; i >= 0 && j[i].lost; i-- {
		n++
	}
	return n
}

// ReadOutcomes reads the gateway's outcome log from since on. A missing
// file is no outcomes.
func ReadOutcomes(path string, since time.Time) []Outcome {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var out []Outcome
	start := 0
	for i := 0; i <= len(b); i++ {
		if i < len(b) && b[i] != '\n' {
			continue
		}
		var o Outcome
		if json.Unmarshal(b[start:i], &o) == nil && !o.Time.Before(since) && o.Session != "" {
			out = append(out, o)
		}
		start = i + 1
	}
	return out
}

// AppendOutcome adds one line to the outcome log. An empty path is no log.
func AppendOutcome(path string, o Outcome) error {
	if path == "" {
		return nil
	}
	b, err := json.Marshal(o)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append(b, '\n'))
	return err
}
