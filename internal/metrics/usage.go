package metrics

import (
	"sort"
	"time"
)

// ---------------------------------------------------------------------------
// Usage: the filterable view behind the dashboard's Usage section.
//
// History (Daily) answers fixed questions over whole days. Usage answers the
// ones a person narrows down to: "the secondary, in this repository, over
// the last hour, only the failures". It streams the same files the same
// way, so memory is bounded by the window's latency samples and one page of
// rows, never by the size of the log.
// ---------------------------------------------------------------------------

// Request results. A request the client abandoned (Esc in Claude Code) is
// "cancelled", not a failure; one recorded with status 0 predates the
// gateway recording what it answered (see Event.HTTPStatus) and is
// "unknown", since it could have been either.
const (
	ResultOK        = "ok"
	ResultError     = "error"
	ResultCancelled = "cancelled"
	ResultUnknown   = "unknown"
)

// ResultOf classifies one event.
func ResultOf(e Event) string {
	switch {
	case e.HTTPStatus == StatusClientClosed:
		return ResultCancelled
	case e.HTTPStatus == 0:
		return ResultUnknown
	case IsError(e):
		return ResultError
	}
	return ResultOK
}

// UsageFilter selects the events a Usage report covers. Empty strings match
// everything. Provider matches a slot ("primary", "secondary") or a route
// name ("anthropic", "openai-compatible", ...).
type UsageFilter struct {
	From, To time.Time
	Model    string
	Provider string
	Repo     string
	Session  string
	Result   string
	// Bucket is the trend bucket width. Daily buckets are local calendar
	// days whatever the width says, so a bar is the day the person at the
	// keyboard remembers.
	Bucket time.Duration
	Daily  bool
	// One page of the newest matching requests.
	Limit, Offset int
}

// UsageTotals is the filtered window in one row.
type UsageTotals struct {
	Requests  int `json:"requests"`
	OK        int `json:"ok"`
	Errors    int `json:"errors"`
	Cancelled int `json:"cancelled"`
	Unknown   int `json:"unknown"`
	// SuccessRate is OK / (OK + Errors), as a fraction; cancelled and
	// unknown requests are in neither. Nil when there is nothing to divide.
	SuccessRate      *float64 `json:"success_rate"`
	InputTokens      int64    `json:"input_tokens"`
	OutputTokens     int64    `json:"output_tokens"`
	CacheReadTokens  int64    `json:"cache_read_tokens"`
	CacheWriteTokens int64    `json:"cache_write_tokens"`
	// CacheHitRate is cache reads / (uncached input + cache reads): how much
	// of the context the model read came from the cache. Nil without input.
	CacheHitRate *float64 `json:"cache_hit_rate"`
	USD          float64  `json:"usd"`
	// Unpriced counts requests with tokens but no price, so USD is a lower
	// bound when it is non-zero.
	Unpriced int `json:"unpriced"`
	// OutputTokensPerSec is total output over total time of the successful
	// requests that produced output. Time is the whole request, first token
	// included, so it reads a little low for short answers.
	OutputTokensPerSec float64 `json:"output_tokens_per_sec"`
	// Latency over successful model requests (ones naming a model): the
	// Remote Control event posts are 300 ms round trips that would drag a
	// generation's median down to nothing.
	LatencyP50MS int64 `json:"latency_p50_ms"`
	LatencyP95MS int64 `json:"latency_p95_ms"`
}

// UsageBucket is one bar of the trend chart.
type UsageBucket struct {
	Start            time.Time `json:"start"`
	Requests         int       `json:"requests"`
	Errors           int       `json:"errors"`
	InputTokens      int64     `json:"input_tokens"`
	OutputTokens     int64     `json:"output_tokens"`
	CacheReadTokens  int64     `json:"cache_read_tokens"`
	CacheWriteTokens int64     `json:"cache_write_tokens"`
	USD              float64   `json:"usd"`
}

// UsageGroup is one row of a breakdown table.
type UsageGroup struct {
	Key      string  `json:"key"`
	Requests int     `json:"requests"`
	Errors   int     `json:"errors"`
	Tokens   int64   `json:"tokens"`
	USD      float64 `json:"usd"`
	Unpriced bool    `json:"unpriced,omitempty"`
}

// UsageRow is one request in the paged list: the event as recorded, plus
// what the report worked out about it.
type UsageRow struct {
	Event
	Repo     string `json:"repo,omitempty"`
	Result   string `json:"result"`
	Provider string `json:"provider"`
}

// UsageOptions are the values present in the window before filtering, for
// the filter bar's dropdowns.
type UsageOptions struct {
	Models    []string `json:"models"`
	Providers []string `json:"providers"`
	Repos     []string `json:"repos"`
}

// UsageReport is everything the Usage section draws.
type UsageReport struct {
	From          time.Time     `json:"from"`
	To            time.Time     `json:"to"`
	BucketSeconds int64         `json:"bucket_seconds"`
	Daily         bool          `json:"daily"`
	Totals        UsageTotals   `json:"totals"`
	Trend         []UsageBucket `json:"trend"`
	ByModel       []UsageGroup  `json:"by_model"`
	ByProvider    []UsageGroup  `json:"by_provider"`
	ByRepo        []UsageGroup  `json:"by_repo"`
	ByResult      []UsageGroup  `json:"by_result"`
	Recent        []UsageRow    `json:"recent"`
	Offset        int           `json:"offset"`
	Limit         int           `json:"limit"`
	Options       UsageOptions  `json:"options"`
	// Same coverage pair as History: the log rotates, so a window is a
	// request, not a promise.
	Earliest string `json:"earliest,omitempty"`
	Covered  bool   `json:"covered"`
	Files    int    `json:"files"`
}

// providerOf is the route that served the event, falling back to its slot.
func providerOf(e Event) string {
	if e.Route != "" {
		return e.Route
	}
	return slotOf(e)
}

// Usage reads the metrics log and its backups for f's window. repoOf names a
// session's repository; nil leaves repositories out.
func Usage(path string, f UsageFilter, repoOf func(session string) string) (UsageReport, error) {
	if f.Limit <= 0 {
		f.Limit = 50
	}
	if f.Bucket <= 0 {
		f.Bucket = time.Hour
	}
	r := UsageReport{From: f.From, To: f.To, Daily: f.Daily, Offset: f.Offset, Limit: f.Limit,
		BucketSeconds: int64(f.Bucket / time.Second)}

	// Every bucket in the window, empty ones included: a gap is information.
	index := map[int64]int{}
	for t := bucketStart(f.From, f); !t.After(f.To); t = nextBucket(t, f) {
		index[t.Unix()] = len(r.Trend)
		r.Trend = append(r.Trend, UsageBucket{Start: t})
	}

	repoCache := map[string]string{}
	repoFor := func(sid string) string {
		if repoOf == nil || sid == "" {
			return ""
		}
		name, ok := repoCache[sid]
		if !ok {
			name = repoOf(sid)
			repoCache[sid] = name
		}
		return name
	}

	groups := map[string]map[string]*UsageGroup{"model": {}, "provider": {}, "repo": {}, "result": {}}
	addGroup := func(kind, key string, e Event, res string) {
		g := groups[kind][key]
		if g == nil {
			g = &UsageGroup{Key: key}
			groups[kind][key] = g
		}
		g.Requests++
		if res == ResultError {
			g.Errors++
		}
		g.Tokens += e.InputTokens + e.OutputTokens + e.CacheReadTokens + e.CacheWriteTokens
		g.USD += e.APIEquivalentUSD
		g.Unpriced = g.Unpriced || e.PricingUnknown
	}
	opts := map[string]map[string]struct{}{"model": {}, "provider": {}, "repo": {}}

	keep := f.Offset + f.Limit
	ring := make([]UsageRow, 0, keep)
	var latencies []int64
	var outTok, outMS int64
	var earliest time.Time
	t := &r.Totals

	for _, file := range historyFiles(path, f.From) {
		r.Files++
		err := scanEvents(file, func(e Event) {
			if earliest.IsZero() || e.Time.Before(earliest) {
				earliest = e.Time
			}
			if e.Time.Before(f.From) || e.Time.After(f.To) {
				return
			}
			prov, repo, res := providerOf(e), repoFor(e.SessionID), ResultOf(e)
			if e.Model != "" {
				opts["model"][e.Model] = struct{}{}
			}
			if prov != "" {
				opts["provider"][prov] = struct{}{}
			}
			if repo != "" {
				opts["repo"][repo] = struct{}{}
			}
			if f.Model != "" && e.Model != f.Model ||
				f.Provider != "" && f.Provider != prov && f.Provider != slotOf(e) ||
				f.Repo != "" && repo != f.Repo ||
				f.Session != "" && e.SessionID != f.Session ||
				f.Result != "" && res != f.Result {
				return
			}

			t.Requests++
			switch res {
			case ResultOK:
				t.OK++
				if e.Model != "" && e.DurationMS > 0 {
					latencies = append(latencies, e.DurationMS)
				}
				if e.OutputTokens > 0 && e.DurationMS > 0 {
					outTok += e.OutputTokens
					outMS += e.DurationMS
				}
			case ResultError:
				t.Errors++
			case ResultCancelled:
				t.Cancelled++
			default:
				t.Unknown++
			}
			t.InputTokens += e.InputTokens
			t.OutputTokens += e.OutputTokens
			t.CacheReadTokens += e.CacheReadTokens
			t.CacheWriteTokens += e.CacheWriteTokens
			t.USD += e.APIEquivalentUSD
			if e.PricingUnknown {
				t.Unpriced++
			}

			if i, ok := index[bucketStart(e.Time, f).Unix()]; ok {
				b := &r.Trend[i]
				b.Requests++
				if res == ResultError {
					b.Errors++
				}
				b.InputTokens += e.InputTokens
				b.OutputTokens += e.OutputTokens
				b.CacheReadTokens += e.CacheReadTokens
				b.CacheWriteTokens += e.CacheWriteTokens
				b.USD += e.APIEquivalentUSD
			}

			model := e.Model
			if model == "" {
				model = "(no model)"
			}
			addGroup("model", model, e, res)
			addGroup("provider", prov, e, res)
			if repo == "" {
				repo = "(no session)"
				if e.SessionID != "" {
					repo = "(unknown)"
				}
			}
			addGroup("repo", repo, e, res)
			addGroup("result", res, e, res)

			// The newest offset+limit rows: older ones fall out of the ring.
			row := UsageRow{Event: e, Repo: repoFor(e.SessionID), Result: res, Provider: prov}
			if len(ring) < keep {
				ring = append(ring, row)
			} else if keep > 0 {
				copy(ring, ring[1:])
				ring[keep-1] = row
			}
		})
		if err != nil {
			return r, err
		}
	}

	if d := t.OK + t.Errors; d > 0 {
		v := float64(t.OK) / float64(d)
		t.SuccessRate = &v
	}
	if d := t.InputTokens + t.CacheReadTokens; d > 0 {
		v := float64(t.CacheReadTokens) / float64(d)
		t.CacheHitRate = &v
	}
	if outMS > 0 {
		t.OutputTokensPerSec = float64(outTok) / (float64(outMS) / 1000)
	}
	t.LatencyP50MS = percentile(latencies, 0.50)
	t.LatencyP95MS = percentile(latencies, 0.95)

	// Newest first, then the requested page.
	for i := len(ring) - 1 - f.Offset; i >= 0; i-- {
		r.Recent = append(r.Recent, ring[i])
	}
	if r.Recent == nil {
		r.Recent = []UsageRow{}
	}

	sorted := func(kind string) []UsageGroup {
		out := make([]UsageGroup, 0, len(groups[kind]))
		for _, g := range groups[kind] {
			out = append(out, *g)
		}
		sort.Slice(out, func(i, j int) bool {
			if out[i].Requests != out[j].Requests {
				return out[i].Requests > out[j].Requests
			}
			return out[i].Key < out[j].Key
		})
		return out
	}
	r.ByModel, r.ByProvider, r.ByRepo, r.ByResult = sorted("model"), sorted("provider"), sorted("repo"), sorted("result")

	list := func(kind string) []string {
		out := make([]string, 0, len(opts[kind]))
		for k := range opts[kind] {
			out = append(out, k)
		}
		sort.Strings(out)
		return out
	}
	r.Options = UsageOptions{Models: list("model"), Providers: list("provider"), Repos: list("repo")}

	if !earliest.IsZero() {
		r.Earliest = earliest.Format(time.RFC3339)
		r.Covered = !earliest.After(f.From)
	}
	return r, nil
}

// bucketStart is the start of the bucket t falls in, in local time.
func bucketStart(t time.Time, f UsageFilter) time.Time {
	t = t.Local()
	if f.Daily {
		return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.Local)
	}
	// Whole minutes from local midnight, so an hour bucket starts on the
	// hour even in a zone with a half-hour offset.
	mid := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.Local)
	w := f.Bucket
	return mid.Add(t.Sub(mid) / w * w)
}

func nextBucket(t time.Time, f UsageFilter) time.Time {
	if f.Daily {
		return t.AddDate(0, 0, 1)
	}
	return bucketStart(t.Add(f.Bucket), f)
}
