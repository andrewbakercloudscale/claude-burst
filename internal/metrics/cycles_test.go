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
		ev(`"input_tokens":2,"cache_write_tokens":60000,"compacted_messages":40`), // the swap
		ctx(70, `,"compacted_messages":40`), ctx(80, `,"compacted_messages":40`),
		ctx(90, ""), // the summary no longer fits: a new run
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
	if len(runs) != 3 {
		t.Fatalf("runs %+v, want 3", runs)
	}
	first, swap, last := runs[0], runs[1], runs[2]
	if first.Swapped || first.Start != 100_002 || first.End != 250_002 || first.Turns != 4 {
		t.Fatalf("the run before the swap: %+v", first)
	}
	// The swap replaced 250k with 60k, for the summary ($0.20) and writing
	// 60k to cache where it would have been read (60k * $5.75/M).
	if !swap.Swapped || swap.Before != 250_002 || swap.Start != 60_002 || swap.End != 80_002 || swap.Turns != 3 {
		t.Fatalf("the swapped run: %+v", swap)
	}
	if want := 0.2 + 60_000*5.75/1e6; swap.CostUSD < want-1e-9 || swap.CostUSD > want+1e-9 {
		t.Fatalf("the swap cost $%.4f, want $%.4f", swap.CostUSD, want)
	}
	if last.Swapped || last.Start != 90_002 || last.Turns != 1 {
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
