package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/andrewbakercloudscale/claude-burst/internal/metrics"
)

func getUsage(t *testing.T, s *Server, query string) *httptest.ResponseRecorder {
	t.Helper()
	req := localRequest(http.MethodGet, "http://127.0.0.1/api/usage?"+query, nil)
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	return rr
}

func TestUsageRefusesBadQueries(t *testing.T) {
	s := newTestServer(t)
	cases := map[string]string{
		"range=2w":     "range must be",
		"range=custom": "from: missing",
		"range=custom&from=yesterday&to=2026-10-03":  "from: not a date",
		"range=custom&from=2026-10-03&to=2026-10-02": "from must be before to",
		"range=custom&from=2026-01-01&to=2026-10-01": "at most 92 days",
		"result=maybe": "result must be",
		"limit=0":      "limit:",
		"limit=500":    "limit:",
		"offset=-1":    "offset:",
	}
	for q, want := range cases {
		rr := getUsage(t, s, q)
		if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), want) {
			t.Errorf("%s: %d %q, want 400 containing %q", q, rr.Code, rr.Body.String(), want)
		}
	}
}

func TestUsageAnswersAWindow(t *testing.T) {
	s := newTestServer(t)
	w := metrics.New(s.metricsPath)
	now := time.Now()
	for _, e := range []metrics.Event{
		{Time: now.Add(-30 * time.Minute), Slot: "primary", Route: "anthropic", Model: "claude-opus-5-5", HTTPStatus: 200, DurationMS: 1000, InputTokens: 5, OutputTokens: 50, CacheReadTokens: 95, APIEquivalentUSD: 0.5},
		{Time: now.Add(-20 * time.Minute), Slot: "primary", Route: "anthropic", Model: "claude-opus-5-5", HTTPStatus: 499, DurationMS: 100},
	} {
		if err := w.Write(e); err != nil {
			t.Fatal(err)
		}
	}
	rr := getUsage(t, s, "range=1h")
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rr.Code, rr.Body.String())
	}
	var got usageResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Range != "1h" || got.BucketSeconds != 300 || got.Daily {
		t.Fatalf("window = %s, %ds, daily %v", got.Range, got.BucketSeconds, got.Daily)
	}
	if got.Totals.Requests != 2 || got.Totals.Cancelled != 1 || got.Totals.SuccessRate == nil || *got.Totals.SuccessRate != 1 {
		t.Fatalf("totals = %+v", got.Totals)
	}
	if len(got.Recent) != 2 || got.Recent[0].Result != metrics.ResultCancelled {
		t.Fatalf("recent = %+v", got.Recent)
	}
	// No config.json in this HOME: the built-in prices, with their source.
	if len(got.Prices) == 0 || !strings.Contains(got.PriceSource, "pricing") {
		t.Fatalf("prices = %d, source %q", len(got.Prices), got.PriceSource)
	}

	rr = getUsage(t, s, "range=7d")
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if !got.Daily || len(got.Trend) != 7 || got.Trend[0].Start.Hour() != 0 {
		t.Fatalf("7d: daily %v, %d buckets", got.Daily, len(got.Trend))
	}
}
