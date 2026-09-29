package metrics

import (
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
