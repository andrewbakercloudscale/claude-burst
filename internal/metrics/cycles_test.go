package metrics

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// One session: it grows, a summary swaps in, it grows again, with a
// subagent and a side call in between that are not the conversation.
func TestCompactionRunsFollowTheMainConversation(t *testing.T) {
	at := time.Now().Add(-3 * time.Hour)
	n := 0
	ev := func(rest string) string {
		n++
		return `{"time":"` + at.Add(time.Duration(n)*time.Minute).Format(time.RFC3339) + `","session_id":"S","slot":"primary","model":"m","http_status":200,` + rest + `}`
	}
	ctx := func(k int, rest string) string {
		return ev(fmt.Sprintf(`"input_tokens":2,"cache_read_tokens":%d%s`, k*1000, rest))
	}
	lines := []string{
		ctx(100, ""), ctx(150, ""), ctx(200, ""),
		ev(`"input_tokens":2,"cache_read_tokens":8000,"agent_id":"sub1"`), // a subagent
		ctx(20, ""),  // a side call: far under the conversation
		ctx(250, ""), // the conversation again
		ev(`"input_tokens":400,"cache_read_tokens":250000,"api_equivalent_usd":0.2,"note":"compaction summary"`),
		ev(`"input_tokens":400,"cache_read_tokens":250000,"api_equivalent_usd":0.1,"note":"compaction summary incomplete: the stream broke off"`),
		`{"time":"` + at.Add(9*time.Minute).Format(time.RFC3339) + `","session_id":"S","slot":"primary","model":"m","http_status":502,"note":"compaction summary failed: dial"}`,
		ev(`"input_tokens":2,"cache_write_tokens":60000,"cache_write_1h_tokens":60000,"compacted_messages":40`), // the swap
		ctx(70, `,"compacted_messages":40`), ctx(80, `,"compacted_messages":40`),
		// The summary no longer fits: a new run, with what it replaced back
		// and all of it written to the one-hour cache again.
		ev(`"input_tokens":2,"cache_write_tokens":270000,"cache_write_1h_tokens":270000`),
	}
	p := filepath.Join(t.TempDir(), "m.jsonl")
	if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	SetPricer(func(model string, in, out, read, write int64) (float64, bool) {
		return float64(read)*0.5/1e6 + float64(write)*6.25/1e6, true
	})
	SetLongWritePricer(func(model string, tokens int64) float64 { return float64(tokens) * 3.75 / 1e6 })
	t.Cleanup(func() { SetPricer(nil); SetLongWritePricer(nil) })
	runs, failed, err := CompactionRunsSince(p, at)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 3 {
		t.Fatalf("runs %+v, want 3", runs)
	}
	first, swap, last := runs[0], runs[1], runs[2]
	if first.Swapped || first.Start != 100_002 || first.End != 250_002 || first.Turns != 4 {
		t.Fatalf("the run before the swap: %+v", first)
	}
	// The swap replaced 250k with 60k, for the summary ($0.20) and writing
	// 60k to the one-hour cache where it would have been read (60k at
	// $5.75/M, and $3.75/M more for the hour).
	if !swap.Swapped || swap.Before != 250_002 || swap.Start != 60_002 || swap.End != 80_002 || swap.Turns != 3 {
		t.Fatalf("the swapped run: %+v", swap)
	}
	if want := 0.2 + 60_000*9.5/1e6; swap.CostUSD < want-1e-9 || swap.CostUSD > want+1e-9 {
		t.Fatalf("the swap cost $%.4f, want $%.4f", swap.CostUSD, want)
	}
	// Going back to the full history wrote 270k that had been read.
	if want := 270_000 * 9.5 / 1e6; swap.BackUSD < want-1e-9 || swap.BackUSD > want+1e-9 || first.BackUSD != 0 {
		t.Fatalf("going back cost $%.4f, want $%.4f", swap.BackUSD, want)
	}
	if last.Swapped || last.Start != 270_002 || last.Turns != 1 {
		t.Fatalf("the run after the summary was dropped: %+v", last)
	}
	// The summary cut short was paid for; the one that never connected is
	// listed with nothing to its name.
	if len(failed) != 2 || failed[0].USD != 0.1 || failed[1].USD != 0 {
		t.Fatalf("failed summaries %+v", failed)
	}
	if got := CacheReadPrice("m"); got < 0.5/1e6-1e-15 || got > 0.5/1e6+1e-15 {
		t.Fatalf("cache read price %v", got)
	}
}

// Three requests in a row far under the conversation are the conversation
// itself, cut back by a rewind: a new run, not side calls for ever.
func TestAConversationThatShrinksStartsANewRun(t *testing.T) {
	at := time.Now().Add(-time.Hour)
	var lines []string
	for i, k := range []int{200, 210, 30, 31, 32, 33} {
		lines = append(lines, fmt.Sprintf(`{"time":"%s","session_id":"S","slot":"primary","model":"m","http_status":200,"cache_read_tokens":%d}`,
			at.Add(time.Duration(i)*time.Minute).Format(time.RFC3339), k*1000))
	}
	p := filepath.Join(t.TempDir(), "m.jsonl")
	if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runs, _, err := CompactionRunsSince(p, at.Add(-time.Minute))
	if err != nil || len(runs) != 2 || runs[0].End != 210_000 || runs[1].Start != 32_000 || runs[1].End != 33_000 {
		t.Fatalf("runs %+v err %v", runs, err)
	}
}

// Until 0.20.7 a request that continues a message thread was logged without
// its summary's count. Such a log must not read as the summary dropped at
// the second request of every compaction: on 6 Oct 2026 that was "sessions
// go on 1 turns after a compaction" for a session 30 turns into one, and
// the repository's learned Compact at went back to the fixed one.
func TestARunLoggedWithoutItsThreadsSummaryGoesOn(t *testing.T) {
	at := time.Now().Add(-3 * time.Hour)
	n := 0
	ctx := func(k int, rest string) string {
		n++
		return fmt.Sprintf(`{"time":%q,"session_id":"S","slot":"primary","model":"m","http_status":200,"input_tokens":2,"cache_read_tokens":%d%s}`,
			at.Add(time.Duration(n)*time.Minute).Format(time.RFC3339), k*1000, rest)
	}
	lines := []string{
		ctx(120, ""), ctx(148, ""),
		ctx(50, `,"compacted_messages":45`),                  // the swap
		ctx(56, ""), ctx(68, ""), ctx(100, ""), ctx(138, ""), // the thread, with no count
		ctx(20, ""),  // a side call
		ctx(240, ""), // the thread expired with its summary gone: the history whole
		ctx(241, ""),
		ctx(47, `,"compacted_messages":120`), // the next swap
		ctx(48, `,"compacted_messages":120`), // a thread logged with its count
	}
	p := filepath.Join(t.TempDir(), "m.jsonl")
	if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runs, _, err := CompactionRunsSince(p, at)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 4 {
		t.Fatalf("runs %+v, want 4", runs)
	}
	first, whole, second := runs[1], runs[2], runs[3]
	if !first.Swapped || first.Before != 148_002 || first.Start != 50_002 || first.End != 138_002 || first.Turns != 5 {
		t.Fatalf("the first compaction is one run of 5 turns: %+v", first)
	}
	if whole.Swapped || whole.Start != 240_002 || whole.Turns != 2 {
		t.Fatalf("the history sent whole is a run of its own: %+v", whole)
	}
	if !second.Swapped || second.Before != 241_002 || second.Turns != 2 {
		t.Fatalf("the second compaction: %+v", second)
	}
}

// A summary written at the second asking, after the model called a tool at
// the first, is one compaction that worked: both calls are its cost and
// neither is a failure. A summary cut off at max_tokens was paid for and
// wrote nothing: a failure.
func TestASummaryWrittenAtTheSecondAskingIsNotAFailure(t *testing.T) {
	at := time.Now().Add(-3 * time.Hour)
	n := 0
	ev := func(rest string) string {
		n++
		return `{"time":"` + at.Add(time.Duration(n)*time.Minute).Format(time.RFC3339) + `","session_id":"S","slot":"primary","model":"m","http_status":200,` + rest + `}`
	}
	lines := []string{
		ev(`"input_tokens":2,"cache_read_tokens":200000`), ev(`"input_tokens":2,"cache_read_tokens":250000`),
		ev(`"input_tokens":400,"cache_read_tokens":250000,"api_equivalent_usd":0.15,"note":"` + NoteSummaryToolCall + `"`),
		ev(`"input_tokens":400,"cache_read_tokens":250000,"api_equivalent_usd":0.05,"note":"` + NoteSummaryRetry + `"`),
		ev(`"input_tokens":2,"cache_write_tokens":60000,"compacted_messages":40`),
		ev(`"input_tokens":2,"cache_read_tokens":70000,"compacted_messages":40`),
		ev(`"input_tokens":400,"cache_read_tokens":70000,"api_equivalent_usd":0.3,"note":"` + NoteSummaryIncomplete + `: cut off at max_tokens"`),
	}
	p := filepath.Join(t.TempDir(), "m.jsonl")
	if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	SetPricer(func(model string, in, out, read, write int64) (float64, bool) {
		return float64(read)*0.5/1e6 + float64(write)*6.25/1e6, true
	})
	t.Cleanup(func() { SetPricer(nil) })
	runs, failed, err := CompactionRunsSince(p, at)
	if err != nil {
		t.Fatal(err)
	}
	if len(failed) != 1 || failed[0].USD != 0.3 {
		t.Fatalf("failures %+v: want the cut-off summary alone", failed)
	}
	var swap *CompactionRun
	for i := range runs {
		if runs[i].Swapped {
			swap = &runs[i]
		}
	}
	if want := 0.15 + 0.05 + 60_000*5.75/1e6; swap == nil || swap.CostUSD < want-1e-9 || swap.CostUSD > want+1e-9 {
		t.Fatalf("the swapped run %+v: want both summary calls in its cost, %v", swap, want)
	}
	for note, want := range map[string][2]bool{
		NoteSummary: {true, true}, NoteSummaryRetry: {true, true}, NoteSummaryToolCall: {false, true},
		NoteSummaryIncomplete + ": empty": {false, true}, "compaction summary failed: dial": {false, false},
		"compaction summary rejected: overloaded": {false, false}, "": {false, false},
	} {
		if SummaryWritten(note) != want[0] || SummaryPaid(note) != want[1] {
			t.Errorf("%q: written %v paid %v", note, SummaryWritten(note), SummaryPaid(note))
		}
	}
}

// A summary that swaps in an hour or more after the session's last request
// lands on a cold cache: that request would have written the whole history
// anyway, so the write is not the compaction's cost.
func TestASwapOnAColdCacheCostsItsSummaryAlone(t *testing.T) {
	at := time.Now().Add(-6 * time.Hour)
	ev := func(min int, rest string) string {
		return `{"time":"` + at.Add(time.Duration(min)*time.Minute).Format(time.RFC3339) + `","session_id":"S","slot":"primary","model":"m","http_status":200,` + rest + `}`
	}
	lines := []string{
		ev(1, `"input_tokens":2,"cache_read_tokens":150000`),
		ev(2, `"input_tokens":2,"cache_read_tokens":160000`),
		ev(52, `"input_tokens":400,"cache_read_tokens":160000,"api_equivalent_usd":0.1,"note":"compaction summary"`),
		ev(200, `"input_tokens":2,"cache_write_tokens":60000,"cache_write_1h_tokens":60000,"compacted_messages":40`),
		ev(201, `"input_tokens":2,"cache_read_tokens":61000,"compacted_messages":40`),
	}
	p := filepath.Join(t.TempDir(), "m.jsonl")
	if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	SetPricer(func(model string, in, out, read, write int64) (float64, bool) {
		return float64(read)*0.5/1e6 + float64(write)*6.25/1e6, true
	})
	t.Cleanup(func() { SetPricer(nil) })
	runs, _, err := CompactionRunsSince(p, at)
	if err != nil || len(runs) != 2 {
		t.Fatalf("runs %+v, %v", runs, err)
	}
	if swap := runs[1]; !swap.Swapped || !swap.Cold || swap.CostUSD != 0.1 || swap.Before != 160_002 {
		t.Fatalf("the cold swap: %+v", swap)
	}
}
