package metrics

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/andrewbakercloudscale/claude-burst/internal/rotate"
)

// metrics.jsonl had no rotation at all before this and grew forever for as
// long as the gateway ran. Not (yet) exposed via config.json -- see the
// same-shaped constants next to claude-burst.log's rotation in
// cmd/claude-burst/main.go for why these are hardcoded rather than
// threaded through Writer's constructor: metrics.New has exactly one real
// caller, and every other reference is a test building a Writer directly,
// so a config-driven signature would only add ceremony no caller needs yet.
const (
	maxBytes   = 10 * 1024 * 1024
	maxBackups = 5
)

type Event struct {
	Time      time.Time `json:"time"`
	RequestID string    `json:"request_id,omitempty"`
	SessionID string    `json:"session_id,omitempty"`
	AgentID   string    `json:"agent_id,omitempty"`
	Slot      string    `json:"slot,omitempty"` // "primary" | "secondary"; empty on events written before this field existed
	Route     string    `json:"route"`
	Model     string    `json:"model,omitempty"`
	// RequestedModel is what Claude Code asked for; Model is what served the
	// request. They differ only when a remapping provider (Bedrock modelMap,
	// openai-compatible fixed/mapped failover) was involved.
	RequestedModel string `json:"requested_model,omitempty"`
	// Destination is the actual outbound URL (scheme+host+path, no query)
	// this request was sent to -- what actually answers "did this go to
	// primary or secondary", independent of the Slot label.
	Destination      string  `json:"destination,omitempty"`
	HTTPStatus       int     `json:"http_status"`
	DurationMS       int64   `json:"duration_ms"`
	InputTokens      int64   `json:"input_tokens,omitempty"`
	OutputTokens     int64   `json:"output_tokens,omitempty"`
	APIEquivalentUSD float64 `json:"api_equivalent_usd,omitempty"`
	LimitClaim       string  `json:"limit_claim,omitempty"`
	ResetAt          int64   `json:"reset_at,omitempty"`
	Note             string  `json:"note,omitempty"`
	// PricingUnknown marks an event whose served Model had no entry in the
	// configured pricing table while it did report tokens. Without it a
	// zero APIEquivalentUSD is indistinguishable from a genuinely free
	// request, so an unpriced secondary reports as $0.00 spend rather than
	// as unknown spend -- reassuring, and wrong. See writeMetric.
	PricingUnknown bool `json:"pricing_unknown,omitempty"`
}

type Writer struct {
	path string
	mu   sync.Mutex
}

func New(path string) *Writer { return &Writer{path: path} }

func (w *Writer) Write(e Event) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := rotate.RotateIfOversized(w.path, maxBytes, maxBackups); err != nil {
		// A rotation failure (e.g. a permissions problem) must not block the
		// metrics write itself -- an oversized-but-growing file is still a
		// far better outcome than silently losing every metrics event from
		// here on, which is what returning early would do.
		fmt.Fprintf(os.Stderr, "claude-burst: metrics rotation failed for %s: %v\n", w.path, err)
	}
	f, err := os.OpenFile(w.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	_, err = f.Write(append(b, '\n'))
	return err
}

type Summary struct {
	Requests          int
	PrimaryRequests   int
	SecondaryRequests int
	InputTokens       int64
	OutputTokens      int64
	APIEquivalentUSD  float64
	// UnpricedRequests counts events that reported tokens but whose served
	// model had no pricing entry, and UnpricedModels names those models with
	// a per-model count. APIEquivalentUSD therefore covers only the priced
	// subset: when UnpricedRequests is non-zero it is a lower bound, not a
	// total, and String() says so rather than presenting it as complete.
	UnpricedRequests int
	UnpricedModels   map[string]int
}

func Summarize(path string, since time.Time) (Summary, error) {
	var s Summary
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return s, nil
	}
	if err != nil {
		return s, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	buf := make([]byte, 64*1024)
	sc.Buffer(buf, 2*1024*1024)
	for sc.Scan() {
		var e Event
		if json.Unmarshal(sc.Bytes(), &e) != nil {
			continue
		}
		if !since.IsZero() && e.Time.Before(since) {
			continue
		}
		s.Requests++
		switch {
		case e.Slot == "primary":
			s.PrimaryRequests++
		case e.Slot == "secondary":
			s.SecondaryRequests++
		case e.Slot == "" && e.Route == "anthropic":
			// Legacy events written before the Slot field existed.
			s.PrimaryRequests++
		case e.Slot == "" && e.Route == "bedrock":
			s.SecondaryRequests++
		}
		s.InputTokens += e.InputTokens
		s.OutputTokens += e.OutputTokens
		s.APIEquivalentUSD += e.APIEquivalentUSD
		if e.PricingUnknown {
			s.UnpricedRequests++
			if s.UnpricedModels == nil {
				s.UnpricedModels = map[string]int{}
			}
			s.UnpricedModels[e.Model]++
		}
	}
	return s, sc.Err()
}

func (s Summary) String() string {
	out := fmt.Sprintf("requests=%d primary=%d secondary=%d input_tokens=%d output_tokens=%d api_equivalent_usd=$%.2f",
		s.Requests, s.PrimaryRequests, s.SecondaryRequests, s.InputTokens, s.OutputTokens, s.APIEquivalentUSD)
	if s.UnpricedRequests == 0 {
		return out
	}
	// Say the total is incomplete rather than letting a confident-looking
	// dollar figure stand for spend it does not actually include, and name
	// the models so the gap is one config edit away from closed.
	models := make([]string, 0, len(s.UnpricedModels))
	for m := range s.UnpricedModels {
		models = append(models, m)
	}
	sort.Strings(models)
	parts := make([]string, 0, len(models))
	for _, m := range models {
		parts = append(parts, fmt.Sprintf("%s x%d", m, s.UnpricedModels[m]))
	}
	return out + fmt.Sprintf(" (INCOMPLETE: %d request(s) had no pricing entry, so their cost is missing from the total above -- add `pricing` entries in config.json for: %s)",
		s.UnpricedRequests, strings.Join(parts, ", "))
}

// Recent returns up to limit of the most recent events, newest first.
//
// It reads the whole file rather than seeking from the end: the log is
// line-delimited JSON of unbounded line length, so a tail-seek would have to
// guess where a record starts. Growth is bounded in practice (one line per
// request) and this keeps the reader obviously correct.
func Recent(path string, limit int) ([]Event, error) {
	if limit <= 0 {
		limit = 50
	}
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()

	// Ring buffer: holds at most `limit` events regardless of file size.
	ring := make([]Event, 0, limit)
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 2*1024*1024)
	for sc.Scan() {
		var e Event
		if json.Unmarshal(sc.Bytes(), &e) != nil {
			continue
		}
		if len(ring) < limit {
			ring = append(ring, e)
		} else {
			copy(ring, ring[1:])
			ring[limit-1] = e
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	// Newest first.
	out := make([]Event, 0, len(ring))
	for i := len(ring) - 1; i >= 0; i-- {
		out = append(out, ring[i])
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// History: the daily view the dashboard's activity chart is drawn from.
//
// Summarize answers "how much, in total, since T". That is the right shape
// for the CLI and for the two count cards, and the wrong shape for a chart,
// which needs the same numbers bucketed per day and split by slot. Doing
// that in the browser would mean shipping every event to the page just to
// throw almost all of them away.
// ---------------------------------------------------------------------------

// Day is one calendar day of activity in THIS MACHINE'S local zone, not UTC.
// The bar labelled "14 Sep" has to mean the day the person at the keyboard
// remembers, or the chart quietly attributes a late-evening session to the
// following day for anyone east of Greenwich.
type Day struct {
	Date              string `json:"date"` // YYYY-MM-DD, local
	Requests          int    `json:"requests"`
	PrimaryRequests   int    `json:"primary_requests"`
	SecondaryRequests int    `json:"secondary_requests"`
	// Errors counts responses with a 4xx/5xx status. Kept per-day rather
	// than as one window figure because the question a chart answers is
	// "when did this start", and a single percentage cannot.
	Errors           int     `json:"errors"`
	InputTokens      int64   `json:"input_tokens"`
	OutputTokens     int64   `json:"output_tokens"`
	APIEquivalentUSD float64 `json:"api_equivalent_usd"`
	Sessions         int     `json:"sessions"`
	// The same day split by slot, for tokens and spend as well as request
	// count. Without these the chart could stack request counts and then
	// silently stop stacking when switched to tokens -- two bars that look
	// like the same chart and are not, which is worse than no split at all.
	PrimaryTokens   int64   `json:"primary_tokens"`
	SecondaryTokens int64   `json:"secondary_tokens"`
	PrimaryUSD      float64 `json:"primary_usd"`
	SecondaryUSD    float64 `json:"secondary_usd"`
}

// ModelUse is one served model's share of the window. This is the answer to
// "what is the spend actually on", which neither the slot split nor the
// total can give: primary and secondary each serve several models at prices
// that differ by an order of magnitude.
type ModelUse struct {
	Model    string  `json:"model"`
	Slot     string  `json:"slot,omitempty"`
	Requests int     `json:"requests"`
	Tokens   int64   `json:"tokens"`
	USD      float64 `json:"usd"`
	// Unpriced marks a model that reported tokens with no pricing entry, so
	// its USD is not "cheap", it is unknown. Same distinction Summary draws,
	// carried per model so the page can mark the specific row.
	Unpriced bool `json:"unpriced"`
}

// History is one window of activity, in every cut the dashboard draws.
type History struct {
	Days     []Day      `json:"days"`
	Window   Summary    `json:"window"`
	Sessions int        `json:"sessions"`
	Models   []ModelUse `json:"models"`

	// LatencyP50MS and LatencyP95MS are over the window's successful
	// (2xx) requests only. Mixing in failures would average an instant
	// connection-refused into the same number as a real generation and
	// make the gateway look faster the more broken it is.
	LatencyP50MS int64 `json:"latency_p50_ms"`
	LatencyP95MS int64 `json:"latency_p95_ms"`

	// Earliest is the oldest event actually read, and Covered says whether
	// that is at or before the start of the requested window.
	//
	// This pair is the point of the whole struct. metrics.jsonl rotates at
	// 10MB, so "last 30 days" is a request, never a promise: on a busy
	// machine the files may only reach back four. A chart that silently
	// drew empty bars for the days it has no data about would report a
	// quiet fortnight that never happened -- the exact shape of failure
	// this project keeps finding in its own gates. The page states its
	// coverage instead.
	Earliest string `json:"earliest,omitempty"`
	Covered  bool   `json:"covered"`
	// Files is how many metrics files (the live one plus rotated backups)
	// were actually read, so the coverage claim above is checkable rather
	// than asserted.
	Files int `json:"files"`
}

// Daily reads the metrics log and its rotated backups and buckets the last
// `days` local days.
//
// It reads the backups, not just the live file, because Summarize does not
// -- which is why the "All time" card has always under-reported after the
// first rotation. Backups whose last write predates the window are skipped
// without opening them: events are appended in time order, so a file's
// mtime is its newest event.
func Daily(path string, days int) (History, error) {
	if days <= 0 {
		days = 14
	}
	now := time.Now()
	// Local midnight, `days-1` days back: the window is whole days, so that
	// the first and last bars are not fractions of one.
	start := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location()).
		AddDate(0, 0, -(days - 1))

	h := History{Days: make([]Day, 0, days)}
	index := map[string]int{}
	daySessions := map[string]map[string]struct{}{}
	for d := start; !d.After(now); d = d.AddDate(0, 0, 1) {
		key := d.Format("2006-01-02")
		index[key] = len(h.Days)
		h.Days = append(h.Days, Day{Date: key})
		daySessions[key] = map[string]struct{}{}
	}

	sessions := map[string]struct{}{}
	models := map[string]*ModelUse{}
	var latencies []int64
	var earliest time.Time

	for _, f := range historyFiles(path, start) {
		h.Files++
		err := scanEvents(f, func(e Event) {
			if earliest.IsZero() || e.Time.Before(earliest) {
				earliest = e.Time
			}
			if e.Time.Before(start) {
				return
			}
			i, ok := index[e.Time.Local().Format("2006-01-02")]
			if !ok {
				// A timestamp in the future, or one the day loop above did
				// not produce. Counted in the window totals but not placed
				// on a bar it has no home on.
				return
			}
			d := &h.Days[i]
			d.Requests++
			switch slotOf(e) {
			case "primary":
				d.PrimaryRequests++
				d.PrimaryTokens += e.InputTokens + e.OutputTokens
				d.PrimaryUSD += e.APIEquivalentUSD
			case "secondary":
				d.SecondaryRequests++
				d.SecondaryTokens += e.InputTokens + e.OutputTokens
				d.SecondaryUSD += e.APIEquivalentUSD
			}
			if e.HTTPStatus >= 400 {
				d.Errors++
			}
			d.InputTokens += e.InputTokens
			d.OutputTokens += e.OutputTokens
			d.APIEquivalentUSD += e.APIEquivalentUSD
			if e.SessionID != "" {
				sessions[e.SessionID] = struct{}{}
				daySessions[d.Date][e.SessionID] = struct{}{}
			}
			if e.HTTPStatus >= 200 && e.HTTPStatus < 300 && e.DurationMS > 0 {
				latencies = append(latencies, e.DurationMS)
			}
			if m := e.Model; m != "" {
				// Keyed by SLOT AND MODEL, not model alone. claude-opus-5 is
				// served by both slots -- by the subscription on primary and
				// by Bedrock on secondary -- and merging them produced one
				// row labelled "primary" carrying the secondary's metered
				// spend inside it. On 2026-08-31 that was $32 of Bedrock
				// filed under the subscription, in the one view whose whole
				// job is to say where the money went.
				key := slotOf(e) + "\x00" + m
				u := models[key]
				if u == nil {
					u = &ModelUse{Model: m, Slot: slotOf(e)}
					models[key] = u
				}
				u.Requests++
				u.Tokens += e.InputTokens + e.OutputTokens
				u.USD += e.APIEquivalentUSD
				u.Unpriced = u.Unpriced || e.PricingUnknown
			}
			addToSummary(&h.Window, e)
		})
		if err != nil {
			return h, err
		}
	}

	for i := range h.Days {
		h.Days[i].Sessions = len(daySessions[h.Days[i].Date])
	}
	h.Sessions = len(sessions)
	h.Models = make([]ModelUse, 0, len(models))
	for _, u := range models {
		h.Models = append(h.Models, *u)
	}
	sort.Slice(h.Models, func(i, j int) bool {
		if h.Models[i].Requests != h.Models[j].Requests {
			return h.Models[i].Requests > h.Models[j].Requests
		}
		return h.Models[i].Model < h.Models[j].Model
	})
	h.LatencyP50MS = percentile(latencies, 0.50)
	h.LatencyP95MS = percentile(latencies, 0.95)
	if !earliest.IsZero() {
		h.Earliest = earliest.Format(time.RFC3339)
		// Compared by DAY, not by instant. The question Covered answers is
		// "is there a bar on this chart for a day I have no data about" --
		// and the oldest event landing at 11am on the first day still means
		// that day has data. Comparing instants would report every short
		// window as incomplete, which trains the reader to ignore the one
		// warning that matters on a long one.
		h.Covered = earliest.Local().Format("2006-01-02") <= start.Format("2006-01-02")
	}
	return h, nil
}

// historyFiles returns the metrics files to read, oldest first: the rotated
// backups path.N ... path.1 and then path itself. A backup last written
// before the window starts cannot contain an event inside it, so it is
// skipped -- which is what keeps a 14-day chart from parsing 40MB of JSON
// that is all older than its first bar.
func historyFiles(path string, start time.Time) []string {
	var files []string
	for i := maxBackups; i >= 1; i-- {
		p := fmt.Sprintf("%s.%d", path, i)
		st, err := os.Stat(p)
		if err != nil || st.ModTime().Before(start) {
			continue
		}
		files = append(files, p)
	}
	return append(files, path)
}

func scanEvents(path string, fn func(Event)) error {
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 2*1024*1024)
	for sc.Scan() {
		var e Event
		if json.Unmarshal(sc.Bytes(), &e) != nil {
			continue
		}
		fn(e)
	}
	return sc.Err()
}

// slotOf resolves an event's slot, including the legacy events written
// before the field existed. Summarize has the same switch inline; this is
// the shared version, so the chart and the totals can never disagree about
// which requests were primary.
func slotOf(e Event) string {
	if e.Slot != "" {
		return e.Slot
	}
	switch e.Route {
	case "anthropic":
		return "primary"
	case "bedrock":
		return "secondary"
	}
	return ""
}

func addToSummary(s *Summary, e Event) {
	s.Requests++
	switch slotOf(e) {
	case "primary":
		s.PrimaryRequests++
	case "secondary":
		s.SecondaryRequests++
	}
	s.InputTokens += e.InputTokens
	s.OutputTokens += e.OutputTokens
	s.APIEquivalentUSD += e.APIEquivalentUSD
	if e.PricingUnknown {
		s.UnpricedRequests++
		if s.UnpricedModels == nil {
			s.UnpricedModels = map[string]int{}
		}
		s.UnpricedModels[e.Model]++
	}
}

// percentile sorts in place and returns the nearest-rank value, or 0 for an
// empty set. Nearest-rank rather than interpolated: these are millisecond
// observations of real requests, and an interpolated p95 is a number no
// request actually took.
func percentile(v []int64, p float64) int64 {
	if len(v) == 0 {
		return 0
	}
	sort.Slice(v, func(i, j int) bool { return v[i] < v[j] })
	i := int(float64(len(v))*p+0.5) - 1
	if i < 0 {
		i = 0
	}
	if i >= len(v) {
		i = len(v) - 1
	}
	return v[i]
}
