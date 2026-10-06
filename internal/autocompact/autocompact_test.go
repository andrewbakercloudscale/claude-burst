package autocompact

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/andrewbakercloudscale/claude-burst/internal/metrics"
)

const readPrice = 0.5 / 1_000_000 // USD per cached token

var now = time.Date(2026, 10, 5, 12, 0, 0, 0, time.Local)

// swapped is a compaction of session s that left `after`, replaced
// `before`, cost usd and then ran `turns` turns growing g a turn, ending
// long enough ago to be judged.
func swapped(s string, before, after int64, usd float64, turns int, g int64) metrics.CompactionRun {
	at := now.Add(-48 * time.Hour)
	return metrics.CompactionRun{Session: s, Model: "m", At: at, Last: at.Add(time.Hour), Swapped: true,
		Before: before, Start: after, End: after + int64(turns)*g, Turns: turns, CostUSD: usd}
}

func inputs(runs ...metrics.CompactionRun) Inputs {
	return Inputs{Runs: runs, Now: now,
		Resolve:   func(s string) (string, string) { return "repo-" + s[:1], "/src/repo-" + s[:1] },
		ReadPrice: func(string) float64 { return readPrice }}
}

var bounds = Bounds{Floor: 100_000, Ceiling: 300_000}

func empty() State { return State{Repos: map[string]*Repo{}} }

func TestOptimalIsWhereTheCostPerTurnIsLowest(t *testing.T) {
	const after, growth, extra = 70_000, 2_000, 400_000
	best := Optimal(after, growth, extra, 0)
	at := func(x int64) float64 { return costPerTurn(x, after, growth, readPrice, float64(extra)*readPrice) }
	for _, other := range []int64{best - 20_000, best + 20_000, 100_000, 300_000} {
		if at(other) < at(best) {
			t.Fatalf("%dk costs $%.5f a turn, less than the optimum %dk at $%.5f", other/1000, at(other), best/1000, at(best))
		}
	}
	// Dearer compactions, faster growth and failures all move it later.
	if Optimal(after, growth, 4*extra, 0) <= best || Optimal(after, 4*growth, extra, 0) <= best || Optimal(after, growth, extra, 0.5) <= best {
		t.Fatal("a dearer compaction, faster growth or failures must each raise the optimum")
	}
	if Optimal(after, 0, extra, 0) != 0 {
		t.Fatal("no growth, nothing to optimise")
	}
}

func TestTheFirstLearnedThresholdIsTheTargetAndLaterOnesMoveATenthADay(t *testing.T) {
	runs := []metrics.CompactionRun{
		swapped("a1", 400_000, 70_000, 0.40, 120, 2_000),
		swapped("a2", 400_000, 70_000, 0.40, 120, 2_000),
		swapped("a3", 400_000, 70_000, 0.40, 120, 2_000),
	}
	st := Learn(empty(), inputs(runs...), bounds, true)
	r := st.Repos["/src/repo-a"]
	if r == nil || r.Target <= bounds.Floor || r.Target >= bounds.Ceiling {
		t.Fatalf("target %+v, want one between the floor and the ceiling", r)
	}
	if r.Threshold != r.Target || r.Previous != bounds.Ceiling {
		t.Fatalf("first learned: threshold %d previous %d, want the target %d from the ceiling", r.Threshold, r.Previous, r.Target)
	}
	if r.Target%round != 0 || r.Compactions != 3 || r.AfterTokens != 70_000 || r.GrowthTurn != 2_000 {
		t.Fatalf("inputs %+v", r)
	}
	if got := st.Thresholds()["/src/repo-a"]; got != r.Target {
		t.Fatalf("thresholds %v", st.Thresholds())
	}
	// The same day again changes nothing.
	again := Learn(st, inputs(runs...), bounds, true)
	if again.Repos["/src/repo-a"].Threshold != r.Threshold {
		t.Fatal("stepped twice in one day")
	}
	// Next day the context grows four times as fast: the target jumps, the
	// threshold moves a tenth.
	fast := []metrics.CompactionRun{
		swapped("a1", 400_000, 70_000, 0.40, 120, 8_000),
		swapped("a2", 400_000, 70_000, 0.40, 120, 8_000),
		swapped("a3", 400_000, 70_000, 0.40, 120, 8_000),
	}
	in := inputs(fast...)
	in.Now = now.Add(24 * time.Hour)
	for i := range in.Runs {
		in.Runs[i].At, in.Runs[i].Last = in.Runs[i].At.Add(24*time.Hour), in.Runs[i].Last.Add(24*time.Hour)
	}
	next := Learn(st, in, bounds, true).Repos["/src/repo-a"]
	if next.Target <= r.Target {
		t.Fatalf("faster growth must raise the target: %d then %d", r.Target, next.Target)
	}
	if want := roundTo(r.Threshold + int64(float64(r.Threshold)*stepShare)); next.Threshold != want || next.Previous != r.Threshold {
		t.Fatalf("threshold %d (previous %d), want one tenth up from %d: %d", next.Threshold, next.Previous, r.Threshold, want)
	}
	// Not stepping (the fixed mode) leaves the threshold alone.
	if got := Learn(st, in, bounds, false).Repos["/src/repo-a"].Threshold; got != r.Threshold {
		t.Fatalf("stepped with the mode off: %d", got)
	}
}

func TestBoundsAndEvidence(t *testing.T) {
	// Tiny summaries and slow growth: the optimum is under the floor.
	low := []metrics.CompactionRun{
		swapped("a1", 200_000, 20_000, 0.11, 400, 100),
		swapped("a2", 200_000, 20_000, 0.11, 400, 100),
		swapped("a3", 200_000, 20_000, 0.11, 400, 100),
	}
	r := Learn(empty(), inputs(low...), bounds, true).Repos["/src/repo-a"]
	if r.Target != bounds.Floor || !strings.Contains(r.Reason, "held at 100k") {
		t.Fatalf("target %d reason %q, want the floor", r.Target, r.Reason)
	}
	// Dear compactions and fast growth: over the ceiling.
	high := []metrics.CompactionRun{
		swapped("b1", 900_000, 150_000, 6, 400, 20_000),
		swapped("b2", 900_000, 150_000, 6, 400, 20_000),
		swapped("b3", 900_000, 150_000, 6, 400, 20_000),
	}
	r = Learn(empty(), inputs(high...), bounds, true).Repos["/src/repo-b"]
	if r.Target != bounds.Ceiling || !strings.Contains(r.Reason, "fixed Compact at of 300k") {
		t.Fatalf("target %d reason %q, want the ceiling", r.Target, r.Reason)
	}
	// Two compactions are not enough to learn from.
	r = Learn(empty(), inputs(low[:2]...), bounds, true).Repos["/src/repo-a"]
	if r.Target != 0 || r.Threshold != 0 || !strings.Contains(r.Reason, "2 of the 3") {
		t.Fatalf("too little evidence: %+v", r)
	}
	// A repository that was never compacted is not listed at all.
	plain := metrics.CompactionRun{Session: "c1", Model: "m", At: now, Last: now, Start: 10_000, End: 90_000, Turns: 40}
	if st := Learn(empty(), inputs(plain), bounds, true); len(st.Repos) != 0 {
		t.Fatalf("listed %v", st.Repos)
	}
	// Bounds moved by the user apply at once, without a step.
	st := State{Repos: map[string]*Repo{"/x": {Root: "/x", Threshold: 120_000, Target: 120_000, SteppedOn: now.Format("2006-01-02")}}}
	if got := Learn(st, inputs(), Bounds{Floor: 150_000, Ceiling: 300_000}, true).Repos["/x"].Threshold; got != 150_000 {
		t.Fatalf("a raised floor left the threshold at %d", got)
	}
}

// The buffer sits on top of the cheapest size, and a reseat takes a
// threshold already in use straight to the new target the same day.
func TestTheBufferRaisesTheTargetAndAReseatAppliesItAtOnce(t *testing.T) {
	runs := []metrics.CompactionRun{
		swapped("a1", 400_000, 70_000, 0.40, 120, 2_000),
		swapped("a2", 400_000, 70_000, 0.40, 120, 2_000),
		swapped("a3", 400_000, 70_000, 0.40, 120, 2_000),
	}
	plain := Learn(empty(), inputs(runs...), bounds, true)
	base := plain.Repos["/src/repo-a"].Target
	b := bounds
	b.BufferPercent = 20
	// The same day, already stepped: the threshold stays where it was.
	same := Learn(plain, inputs(runs...), b, true).Repos["/src/repo-a"]
	if same.Target <= base || same.Threshold != base {
		t.Fatalf("target %d (was %d), threshold %d", same.Target, base, same.Threshold)
	}
	if d := same.Target - base*120/100; d < -round || d > round {
		t.Fatalf("target %d, want about a fifth over %d", same.Target, base)
	}
	if !strings.Contains(same.Reason, "plus the 20% buffer") {
		t.Fatalf("reason %q", same.Reason)
	}
	plain.Reseat()
	if got := Learn(plain, inputs(runs...), b, true).Repos["/src/repo-a"]; got.Threshold != got.Target || got.Target != same.Target {
		t.Fatalf("after a reseat: threshold %d target %d", got.Threshold, got.Target)
	}
}

// A failure is a compaction that lost money, and that alone.
func TestFailuresAreCompactionsThatLostMoney(t *testing.T) {
	// Each saves (400k-70k) * $0.5/M = $0.165 a turn against $0.40.
	paid := swapped("a1", 400_000, 70_000, 0.40, 120, 2_000)  // saved $19.80
	lost := swapped("a2", 400_000, 70_000, 0.40, 2, 2_000)    // saved $0.33
	running := swapped("a3", 400_000, 70_000, 0.40, 1, 2_000) // still going: not judged
	running.Last = now.Add(-time.Minute)
	in := inputs(paid, paid, lost, running)
	in.Failed = []metrics.SummaryFailure{
		{Session: "a4", At: now, USD: 0.20, Note: "compaction summary incomplete: the stream broke off"},
		{Session: "a5", At: now, USD: 0, Note: "compaction summary failed: dial"}, // never billed: nothing lost
	}
	in.Outcomes = []Outcome{{Time: now, Session: "a6", Kind: OutcomeUnused}, {Time: now, Session: "a7", Kind: OutcomeEnded}}
	f := Learn(empty(), in, bounds, true).Repos["/src/repo-a"].Failures
	if f.Unpaid != 1 || f.SummaryFailed != 1 || f.Unused != 1 || f.Ended != 1 {
		t.Fatalf("failures %+v", f)
	}
	// Four swapped, one summary paid for and failed, one never used.
	if f.Attempts != 6 || f.Count() != 3 || f.Rate != 0.5 {
		t.Fatalf("%d of %d, rate %v", f.Count(), f.Attempts, f.Rate)
	}
	// $0.40 less the $0.33 it saved, and the $0.20 failed call.
	if d := f.LostUSD - (0.40 - 0.33 + 0.20); d > 1e-9 || d < -1e-9 {
		t.Fatalf("lost $%.4f", f.LostUSD)
	}
}

func TestMostCompactionsLosingMoneyGoesBackToTheFixedCompactAt(t *testing.T) {
	lost := swapped("a1", 400_000, 70_000, 0.40, 2, 2_000)
	grow := metrics.CompactionRun{Session: "a9", Model: "m", At: now.Add(-72 * time.Hour), Last: now.Add(-71 * time.Hour), Start: 20_000, End: 220_000, Turns: 100}
	r := Learn(empty(), inputs(lost, lost, lost, lost, grow), bounds, true).Repos["/src/repo-a"]
	if r.Failures.Unpaid != 4 || r.Target != bounds.Ceiling || !strings.Contains(r.Reason, "4 of 4 compactions lost money") {
		t.Fatalf("%+v", r)
	}
	// Failures short of that still raise the target.
	ok := swapped("b1", 400_000, 70_000, 0.40, 120, 2_000)
	clean := Learn(empty(), inputs(ok, ok, ok, ok), bounds, true).Repos["/src/repo-b"]
	in := inputs(ok, ok, ok, ok)
	in.Outcomes = []Outcome{{Time: now, Session: "b7", Kind: OutcomeUnused}, {Time: now, Session: "b8", Kind: OutcomeUnused}}
	some := Learn(empty(), in, bounds, true).Repos["/src/repo-b"]
	if some.Target <= clean.Target {
		t.Fatalf("two unused summaries of six must raise the target: %d then %d", clean.Target, some.Target)
	}
}

func TestSessionsThatEndBeforeACompactionPaysGoBackToTheFixedCompactAt(t *testing.T) {
	// Each run paid, but only just: far short of twice the payback.
	short := swapped("a1", 150_000, 70_000, 0.40, 12, 2_000)
	grow := metrics.CompactionRun{Session: "a9", Model: "m", At: now.Add(-72 * time.Hour), Last: now.Add(-71 * time.Hour), Start: 20_000, End: 220_000, Turns: 100}
	r := Learn(empty(), inputs(short, short, short, grow), bounds, true).Repos["/src/repo-a"]
	if r.Target != bounds.Ceiling || !strings.Contains(r.Reason, "sessions go on 12 turns") {
		t.Fatalf("%+v", r)
	}
}

func TestStateAndOutcomesSurviveTheFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "learned.json")
	if st := Load(p); st.Repos == nil || len(st.Repos) != 0 {
		t.Fatalf("no file: %+v", st)
	}
	want := State{Repos: map[string]*Repo{"/x": {Root: "/x", Name: "x", Threshold: 150_000, Target: 140_000, Reason: "r"}}}
	if err := Save(p, want); err != nil {
		t.Fatal(err)
	}
	if got := Load(p).Repos["/x"]; got == nil || got.Threshold != 150_000 || got.Reason != "r" {
		t.Fatalf("read back %+v", got)
	}
	log := filepath.Join(dir, "outcomes.jsonl")
	if AppendOutcome("", Outcome{Session: "s"}) != nil || len(ReadOutcomes("", now)) != 0 {
		t.Fatal("no path must be no log")
	}
	for _, o := range []Outcome{{Time: now.Add(-time.Hour), Session: "old", Kind: OutcomeUnused}, {Time: now.Add(time.Hour), Session: "new", Kind: OutcomeEnded}} {
		if err := AppendOutcome(log, o); err != nil {
			t.Fatal(err)
		}
	}
	got := ReadOutcomes(log, now)
	if len(got) != 1 || got[0].Session != "new" || got[0].Kind != OutcomeEnded {
		t.Fatalf("outcomes since now: %+v", got)
	}
}

// One loss among compactions that paid is a failure rate, not an alarm: the
// threshold stays where the day's step left it. Two in a row are a size
// that is wrong now: a tenth more, once, at once. A compaction that pays
// ends the streak.
func TestOnlyAStreakOfLossesRaisesTheThresholdAtOnce(t *testing.T) {
	at := func(r metrics.CompactionRun, hoursAgo int) metrics.CompactionRun {
		r.At = now.Add(-time.Duration(hoursAgo) * time.Hour)
		r.Last = r.At.Add(time.Hour)
		return r
	}
	paid := swapped("a1", 400_000, 70_000, 0.40, 120, 2_000)
	lost := swapped("a4", 400_000, 70_000, 0.40, 2, 2_000) // 2 turns: it did not save what it cost
	good := []metrics.CompactionRun{at(paid, 90), at(paid, 80), at(paid, 70)}
	wide := Bounds{Floor: 100_000, Ceiling: 600_000}
	st := Learn(empty(), inputs(good...), wide, true)
	before := st.Repos["/src/repo-a"].Threshold
	if before <= 0 || before >= wide.Ceiling {
		t.Fatalf("want a learned threshold below the ceiling, got %d", before)
	}

	one := append(append([]metrics.CompactionRun(nil), good...), at(lost, 60))
	r := Learn(st, inputs(one...), wide, false).Repos["/src/repo-a"]
	if r.Failures.Unpaid != 1 || r.Failures.Streak != 1 {
		t.Fatalf("want 1 loss, a streak of 1, got %+v", r.Failures)
	}
	if r.Threshold != before || strings.Contains(r.Reason, "lost money") {
		t.Fatalf("one loss must not move the threshold at once: %d then %d (%s)", before, r.Threshold, r.Reason)
	}
	// The failure rate alone moves the target, and by far less than a tenth.
	if clean := st.Repos["/src/repo-a"].Target; r.Target < clean || r.Target >= clean*110/100 {
		t.Fatalf("one loss of four: target %d then %d", clean, r.Target)
	}

	two := append(append([]metrics.CompactionRun(nil), one...), at(lost, 50))
	r = Learn(st, inputs(two...), wide, false).Repos["/src/repo-a"]
	if r.Failures.Streak != 2 || r.Threshold <= before || r.Threshold != r.Target || r.Previous != before {
		t.Fatalf("two in a row: want the threshold up at once from %d, got threshold %d target %d previous %d (%+v)", before, r.Threshold, r.Target, r.Previous, r.Failures)
	}
	if !strings.Contains(r.Reason, "plus 10% because the last 2 compactions lost money") {
		t.Fatalf("the reason must say so: %s", r.Reason)
	}
	twoTarget := r.Target

	// A third loss adds no second tenth: only what the failure rate says.
	three := append(append([]metrics.CompactionRun(nil), two...), at(lost, 40))
	r = Learn(st, inputs(three...), wide, false).Repos["/src/repo-a"]
	if r.Failures.Streak != 3 || r.Target >= twoTarget*110/100 {
		t.Fatalf("a longer streak must not add a tenth each time: %d then %d", twoTarget, r.Target)
	}

	// A compaction that pays ends the streak: no tenth, nothing at once.
	ended := append(append([]metrics.CompactionRun(nil), two...), at(paid, 40))
	r = Learn(st, inputs(ended...), wide, false).Repos["/src/repo-a"]
	if r.Failures.Streak != 0 || r.Failures.Unpaid != 2 || r.Threshold != before || strings.Contains(r.Reason, "lost money") {
		t.Fatalf("after one that paid: %+v threshold %d (%s)", r.Failures, r.Threshold, r.Reason)
	}
}
