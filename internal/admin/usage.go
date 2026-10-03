package admin

import (
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/andrewbakercloudscale/claude-burst/internal/config"
	"github.com/andrewbakercloudscale/claude-burst/internal/metrics"
)

// The Usage section: metrics.Usage behind a filter bar. Every figure on it
// comes from metrics.jsonl and its rotated backups; the prices are the ones
// the gateway costs requests with, shown so a dollar figure can be checked.

const (
	usageMaxSpan   = 92 * 24 * time.Hour
	usageMaxLimit  = 200
	usageMaxOffset = 10000
)

// usagePrice is one row of the price table: per million tokens, with the
// cache rates as config.ModelPrice.CacheRates resolves them.
type usagePrice struct {
	Model        string  `json:"model"`
	Input        float64 `json:"input"`
	Output       float64 `json:"output"`
	CacheRead    float64 `json:"cache_read"`
	CacheWrite   float64 `json:"cache_write"`
	CacheDerived bool    `json:"cache_derived"`
}

type usageResponse struct {
	metrics.UsageReport
	Range       string       `json:"range"`
	Prices      []usagePrice `json:"prices"`
	PriceSource string       `json:"price_source"`
}

// usageWindow turns the range query into a window and its bucket width;
// every error it returns is the caller's (400).
// Ranges of a week or more use whole local days, starting at midnight.
func usageWindow(q map[string][]string, now time.Time) (metrics.UsageFilter, string, error) {
	get := func(k string) string {
		if v := q[k]; len(v) > 0 {
			return v[0]
		}
		return ""
	}
	var f metrics.UsageFilter
	rng := get("range")
	if rng == "" {
		rng = "24h"
	}
	midnight := func(t time.Time, back int) time.Time {
		return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.Local).AddDate(0, 0, -back)
	}
	f.To = now
	switch rng {
	case "1h":
		f.From, f.Bucket = now.Add(-time.Hour), 5*time.Minute
	case "24h":
		f.From, f.Bucket = now.Add(-24*time.Hour), time.Hour
	case "7d":
		f.From, f.Daily = midnight(now, 6), true
	case "30d":
		f.From, f.Daily = midnight(now, 29), true
	case "custom":
		from, err := parseUsageTime(get("from"))
		if err != nil {
			return f, rng, usageErr("from: " + err.Error())
		}
		to, err := parseUsageTime(get("to"))
		if err != nil {
			return f, rng, usageErr("to: " + err.Error())
		}
		if !from.Before(to) {
			return f, rng, usageErr("from must be before to")
		}
		if to.Sub(from) > usageMaxSpan {
			return f, rng, usageErr("the window can be at most 92 days")
		}
		f.From, f.To = from, to
		switch span := to.Sub(from); {
		case span <= 3*time.Hour:
			f.Bucket = 5 * time.Minute
		case span <= 2*24*time.Hour:
			f.Bucket = time.Hour
		default:
			f.Daily = true
		}
	default:
		return f, rng, usageErr("range must be 1h, 24h, 7d, 30d or custom")
	}
	return f, rng, nil
}

// parseUsageTime accepts RFC 3339, or a local date and time as a
// datetime-local input sends it, or a bare local date.
func parseUsageTime(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, errMissing
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	for _, layout := range []string{"2006-01-02T15:04", "2006-01-02T15:04:05", "2006-01-02"} {
		if t, err := time.ParseInLocation(layout, s, time.Local); err == nil {
			return t, nil
		}
	}
	return time.Time{}, errBadTime
}

type usageErr string

func (e usageErr) Error() string { return string(e) }

const (
	errMissing = usageErr("missing")
	errBadTime = usageErr("not a date: use 2006-01-02, 2006-01-02T15:04 or RFC 3339")
)

func (s *Server) handleUsage(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f, rng, err := usageWindow(q, time.Now())
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	f.Model, f.Provider, f.Repo, f.Session = q.Get("model"), q.Get("provider"), q.Get("repo"), q.Get("session")
	switch f.Result = q.Get("result"); f.Result {
	case "", metrics.ResultOK, metrics.ResultError, metrics.ResultCancelled, metrics.ResultUnknown:
	default:
		http.Error(w, "result must be ok, error, cancelled or unknown", http.StatusBadRequest)
		return
	}
	if f.Limit, err = boundedInt(q.Get("limit"), 50, 1, usageMaxLimit); err != nil {
		http.Error(w, "limit: "+err.Error(), http.StatusBadRequest)
		return
	}
	if f.Offset, err = boundedInt(q.Get("offset"), 0, 0, usageMaxOffset); err != nil {
		http.Error(w, "offset: "+err.Error(), http.StatusBadRequest)
		return
	}

	var repoOf func(string) string
	if s.repos != nil {
		repoOf = func(sid string) string { name, _ := s.repos.resolve(sid); return name }
	}
	rep, err := metrics.Usage(s.metricsPath, f, repoOf)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	out := usageResponse{UsageReport: rep, Range: rng,
		PriceSource: `config.json "pricing", over the built-in prices in internal/config (config.Default); cache rates not set there are derived by ModelPrice.CacheRates`}
	if cfg, err := config.Load(); err == nil {
		for m, p := range cfg.Pricing {
			read, write := p.CacheRates(m)
			out.Prices = append(out.Prices, usagePrice{Model: m, Input: p.InputPerMTok, Output: p.OutputPerMTok,
				CacheRead: read, CacheWrite: write, CacheDerived: p.CacheReadPerMTok == 0 || p.CacheWritePerMTok == 0})
		}
		sort.Slice(out.Prices, func(i, j int) bool { return out.Prices[i].Model < out.Prices[j].Model })
	}
	if out.Prices == nil {
		out.Prices = []usagePrice{}
	}
	writeJSON(w, out)
}

func boundedInt(s string, dflt, lo, hi int) (int, error) {
	if s == "" {
		return dflt, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < lo || n > hi {
		return 0, usageErr("must be a whole number from " + strconv.Itoa(lo) + " to " + strconv.Itoa(hi))
	}
	return n, nil
}
