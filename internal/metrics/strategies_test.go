package metrics

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// One session grows to 300k, Burst compacts it to 60k at 200k, and it grows
// on. Claude Code alone would never have compacted it; a fixed Compact at
// of 250k would have, one request later than Burst did.
func TestThreeWaysOfCompactingAreCostedOverTheSameRequests(t *testing.T) {
	at := time.Now().Add(-2 * time.Hour)
	n := 0
	ev := func(session, rest string) string {
		n++
		return `{"time":"` + at.Add(time.Duration(n)*time.Minute).Format(time.RFC3339) + `","session_id":"` + session + `","slot":"primary","model":"m","http_status":200,` + rest + `}`
	}
	ctx := func(k int, rest string) string {
		return ev("S", fmt.Sprintf(`"cache_read_tokens":%d%s`, k*1000, rest))
	}
	lines := []string{
		ctx(100, ""), ctx(200, ""),
		ev("S", `"cache_read_tokens":200000,"api_equivalent_usd":0.2,"note":"compaction summary"`),
		ev("S", `"cache_write_tokens":60000,"cache_write_1h_tokens":60000,"compacted_messages":40`), // the swap
		ctx(110, `,"compacted_messages":40`), // 50k more: the twins are at 250k
		ctx(120, `,"compacted_messages":40`),
		ev("T", `"cache_read_tokens":30000`), // another repository, never compacted
	}
	p := filepath.Join(t.TempDir(), "m.jsonl")
	if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// $1/M to read, $11/M to write to the one-hour cache, $10/M of output.
	SetPricer(func(model string, in, out, read, write int64) (float64, bool) {
		return float64(read)/1e6 + float64(write)*6/1e6 + float64(out)*10/1e6, true
	})
	SetLongWritePricer(func(model string, tokens int64) float64 { return float64(tokens) * 5 / 1e6 })
	t.Cleanup(func() { SetPricer(nil); SetLongWritePricer(nil) })

	st, err := CompactionStrategiesSince(p, at, 250_000, func(session string) (string, string) {
		if session == "S" {
			return "big", "/r/big"
		}
		return "small", "/r/small"
	})
	if err != nil {
		t.Fatal(err)
	}
	near := func(what string, got, want float64) {
		t.Helper()
		if math.Abs(got-want) > 1e-9 {
			t.Fatalf("%s: $%.6f, want $%.6f", what, got, want)
		}
	}
	if len(st.Repos) != 2 || st.Repos[0].Repo != "big" || st.Repos[1].Repo != "small" {
		t.Fatalf("repositories %+v, want big (the most context) then small", st.Repos)
	}
	big, small := st.Repos[0], st.Repos[1]
	if big.Requests != 5 || big.Sessions != 1 || big.Path != "/r/big" {
		t.Fatalf("big: %+v", big.StrategyCosts)
	}
	// Claude Code alone: 100k, 200k, 200k, 250k, 260k read, no compaction.
	if big.Default.Tokens != 1_010_000 || big.Default.Compactions != 0 {
		t.Fatalf("default: %+v", big.Default)
	}
	near("default", big.Default.USD, 1.01)
	// Fixed at 250k: 100k, 200k, 200k, then 250k is compacted to the 60k a
	// real compaction left, then 70k. Its compaction reads 250k ($0.25),
	// writes 3.5k of summary ($0.035) and writes 60k to the one-hour cache
	// where it would have been read (60k at $10/M).
	if big.Fixed.Tokens != 630_000 || big.Fixed.Compactions != 1 {
		t.Fatalf("fixed: %+v", big.Fixed)
	}
	near("fixed compaction", big.Fixed.CompactionUSD, 0.25+0.035+0.6)
	near("fixed", big.Fixed.USD, 0.63+0.885)
	// What Burst did: 100k, 200k, 60k, 110k, 120k, the summary it was
	// billed ($0.20) and the same cache write.
	if big.Actual.Tokens != 590_000 || big.Actual.Compactions != 1 {
		t.Fatalf("actual: %+v", big.Actual)
	}
	near("actual", big.Actual.USD, 0.59+0.2+0.6)
	// A session nothing would have compacted costs the same three ways.
	if small.Default.USD != small.Actual.USD || small.Fixed.USD != small.Actual.USD || small.Actual.Tokens != 30_000 {
		t.Fatalf("small: %+v", small.StrategyCosts)
	}
	// The days and the repositories add up to the total.
	var days, repos float64
	for _, d := range st.Daily {
		days += d.Actual.USD
	}
	for _, r := range st.Repos {
		repos += r.Actual.USD
		if len(r.Daily) != len(st.Daily) {
			t.Fatalf("%s has %d days, the window %d", r.Repo, len(r.Daily), len(st.Daily))
		}
	}
	near("days", days, st.Actual.USD)
	near("repositories", repos, st.Actual.USD)
}

// With no Burst summary in force, a twin that has compacted on its own
// keeps following the real context's growth, and a /clear is shared.
func TestATwinKeepsItsOwnCompactionUntilTheUserClears(t *testing.T) {
	at := time.Now().Add(-2 * time.Hour)
	var lines []string
	for i, k := range []int{100, 260, 270, 268, 20, 30} {
		lines = append(lines, fmt.Sprintf(`{"time":"%s","session_id":"S","slot":"primary","model":"m","http_status":200,"cache_read_tokens":%d}`,
			at.Add(time.Duration(i+1)*time.Minute).Format(time.RFC3339), k*1000))
	}
	p := filepath.Join(t.TempDir(), "m.jsonl")
	if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := CompactionStrategiesSince(p, at, 250_000, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Fixed: 100k, 260k compacted to 60k, 70k, 68k (jitter, not a clear),
	// then the /clear to 20k and 30k.
	if st.Fixed.Tokens != 348_000 || st.Fixed.Compactions != 1 {
		t.Fatalf("fixed: %+v", st.Fixed)
	}
	if st.Default.Tokens != 948_000 || st.Actual.Tokens != 948_000 {
		t.Fatalf("default %+v, actual %+v", st.Default, st.Actual)
	}
}
