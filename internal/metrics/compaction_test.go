package metrics

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCompactionStats(t *testing.T) {
	lines := []string{
		`{"time":"2026-09-29T20:00:00+02:00","session_id":"S","slot":"primary","model":"m","http_status":200,"input_tokens":2,"cache_read_tokens":530000}`,
		`{"time":"2026-09-29T20:01:00+02:00","session_id":"S","slot":"primary","model":"m","http_status":200,"input_tokens":400,"cache_read_tokens":500000,"api_equivalent_usd":0.2,"note":"compaction summary"}`,
		`{"time":"2026-09-29T20:02:00+02:00","session_id":"S","slot":"primary","model":"m","http_status":200,"input_tokens":2,"cache_read_tokens":535000}`,
		`{"time":"2026-09-29T20:03:00+02:00","session_id":"S","slot":"primary","model":"m","http_status":200,"input_tokens":2,"cache_write_tokens":45000,"compacted_messages":841}`,
		`{"time":"2026-09-29T20:04:00+02:00","session_id":"S","slot":"primary","model":"m","http_status":200,"input_tokens":2,"cache_read_tokens":46000,"compacted_messages":841}`,
		`{"time":"2026-09-29T20:05:00+02:00","session_id":"T","slot":"primary","model":"m","http_status":200,"input_tokens":2,"cache_read_tokens":90000}`,
	}
	p := filepath.Join(t.TempDir(), "m.jsonl")
	if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	SetPricer(func(model string, in, out, cr, cw int64) (float64, bool) { return float64(cr) / 1e6 * 0.2, true })
	t.Cleanup(func() { SetPricer(nil) })
	st, err := CompactionStatsSince(p, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	drop := int64(535002 - 45002)
	if st.Compactions != 1 || st.CompactedRequests != 2 || st.TokensNotResent != 2*drop {
		t.Fatalf("want 1 compaction, 2 compacted requests, %d not resent; got %+v", 2*drop, st)
	}
	if st.LargestBefore != 535002 || st.LargestAfter != 45002 || st.SummaryUSD != 0.2 {
		t.Fatalf("largest drop and summary cost: %+v", st)
	}
	if len(st.Sessions) != 1 {
		t.Fatalf("want one compacted session: %+v", st.Sessions)
	}
	c := st.Sessions[0]
	perTurn := float64(drop) / 1e6 * 0.2
	if c.Before != 535002 || c.After != 45002 || c.PerTurnTokens != drop || c.Requests != 2 ||
		abs(c.PerTurnUSD-perTurn) > 1e-9 || abs(c.SavedUSD-2*perTurn) > 1e-9 {
		t.Fatalf("session row: %+v", c)
	}
}

func abs(f float64) float64 {
	if f < 0 {
		return -f
	}
	return f
}

// A long session: two Burst compactions, and a "without Burst" twin that
// grows until Claude Code would have compacted it on its own. The saving is
// the twin's context minus the real one on every compacted request, and it
// turns negative once the twin has been compacted and the real session is
// still waiting for Burst's next one.
func TestCompactionSavingFollowsAWithoutBurstTwin(t *testing.T) {
	SetPricer(func(model string, in, out, cr, cw int64) (float64, bool) {
		return float64(cr)/1e6*0.2 + float64(cw)/1e6*5, true
	})
	t.Cleanup(func() { SetPricer(nil) })
	ev := func(min int, ctx, cacheWrite, compacted int64, note string) string {
		return `{"time":"2026-09-29T20:` + fmt.Sprintf("%02d", min) + `:00+02:00","session_id":"S","slot":"primary","model":"m","http_status":200,` +
			`"cache_read_tokens":` + fmt.Sprint(ctx-cacheWrite) + `,"cache_write_tokens":` + fmt.Sprint(cacheWrite) +
			`,"compacted_messages":` + fmt.Sprint(compacted) + `,"note":"` + note + `","api_equivalent_usd":0.25}`
	}
	lines := []string{
		ev(0, 400000, 0, 0, ""),                   // twin 400k
		ev(1, 400000, 0, 0, "compaction summary"), // a summary call: $0.25
		ev(2, 50000, 50000, 800, ""),              // swap: twin stays 400k, saved 350k, 50k rewritten
		ev(3, 250000, 0, 800, ""),                 // +200k both: twin 600k, saved 350k
		ev(4, 450000, 0, 800, ""),                 // twin 800k, saved 350k
		ev(5, 450000, 0, 0, "compaction summary"), // second summary: $0.25
		ev(6, 60000, 60000, 990, ""),              // swap: twin 800k, saved 740k (both drops)
		ev(7, 160000, 0, 990, ""),                 // twin 900k, saved 740k
		ev(8, 260000, 0, 990, ""),                 // twin 1000k: Claude Code compacts it to 60k, saved -200k
		ev(9, 300000, 0, 990, ""),                 // twin 100k, saved -200k
	}
	p := filepath.Join(t.TempDir(), "m.jsonl")
	if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := CompactionStatsSince(p, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	wantTokens := int64(350000*3 + 740000*2 - 200000*2)
	if st.TokensNotResent != wantTokens || st.CompactedRequests != 7 || st.TwinCompactions != 1 || st.Compactions != 2 {
		t.Fatalf("want %d tokens over 7 requests, 1 twin compaction, 2 summaries; got %+v", wantTokens, st)
	}
	near := func(a, b float64) bool { return a-b < 1e-9 && b-a < 1e-9 }
	saved := float64(wantTokens) / 1e6 * 0.2
	rewrite := float64(50000+60000) / 1e6 * (5 - 0.2)
	if !near(st.SavedUSD, saved) || !near(st.SummaryUSD, 0.5) || !near(st.RewriteUSD, rewrite) || !near(st.NetUSD, saved-0.5-rewrite) {
		t.Fatalf("money: saved %v summaries %v rewrite %v net %v", st.SavedUSD, st.SummaryUSD, st.RewriteUSD, st.NetUSD)
	}
	c := st.Sessions[0]
	if c.Before != 800000 || c.After != 60000 || c.PerTurnTokens != 740000 || c.Compactions != 2 || c.Requests != 7 || !near(c.NetUSD, st.NetUSD) {
		t.Fatalf("session row: %+v", c)
	}
}
