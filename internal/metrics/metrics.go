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
	Destination string `json:"destination,omitempty"`
	// HTTPStatus is what the client was answered. A request that never got
	// an upstream answer records the status the gateway sent (502), and one
	// the client abandoned records StatusClientClosed. Until 2026-10-03 both
	// recorded 0, which no error count included.
	HTTPStatus   int   `json:"http_status"`
	DurationMS   int64 `json:"duration_ms"`
	InputTokens  int64 `json:"input_tokens,omitempty"`
	OutputTokens int64 `json:"output_tokens,omitempty"`
	// InputTokens is uncached input only. The cached part of the context is
	// here, and on a long primary session it is nearly all of it: without
	// these, a 150k-token turn recorded as input_tokens=2.
	CacheReadTokens  int64 `json:"cache_read_tokens,omitempty"`
	CacheWriteTokens int64 `json:"cache_write_tokens,omitempty"`
	// CacheWrite1hTokens is the part of CacheWriteTokens written to the
	// one-hour cache. Recorded from 7 Oct 2026; older events have none and
	// stay priced as five-minute writes.
	CacheWrite1hTokens int64 `json:"cache_write_1h_tokens,omitempty"`
	// Secondary context pruning (router/prune.go): bytes of tool output
	// removed from the request, how many old tool results were stubbed, and
	// how many oversized ones were cut down.
	PrunedBytes          int64 `json:"pruned_bytes,omitempty"`
	PrunedToolResults    int64 `json:"pruned_tool_results,omitempty"`
	TruncatedToolResults int64 `json:"truncated_tool_results,omitempty"`
	// The request's latest tool calls: how many repeat an earlier call, and
	// how many repeat one whose output this request stubbed. See prune.go.
	RepeatedCalls   int64 `json:"repeated_calls,omitempty"`
	RerunsAfterStub int64 `json:"reruns_after_stub,omitempty"`
	// PrunedUSD is what the removed input would have cost at the served
	// model's input rate, priced when the request was logged.
	PrunedUSD float64 `json:"pruned_usd,omitempty"`
	// Proxy-side compaction (router/compact.go): how many messages a
	// summary replaced in this request, and the bytes that took out.
	CompactedMessages int64   `json:"compacted_messages,omitempty"`
	CompactedBytes    int64   `json:"compacted_bytes,omitempty"`
	APIEquivalentUSD  float64 `json:"api_equivalent_usd,omitempty"`
	LimitClaim        string  `json:"limit_claim,omitempty"`
	ResetAt           int64   `json:"reset_at,omitempty"`
	Note              string  `json:"note,omitempty"`
	// PricingUnknown marks an event whose served Model had no entry in the
	// configured pricing table while it did report tokens. Without it a
	// zero APIEquivalentUSD is indistinguishable from a genuinely free
	// request, so an unpriced secondary reports as $0.00 spend rather than
	// as unknown spend -- reassuring, and wrong. See writeMetric.
	PricingUnknown bool `json:"pricing_unknown,omitempty"`
	// Repriced is set on read, never written: the event was recorded as
	// PricingUnknown and has since been costed from its stored tokens at
	// the rates configured now. See SetPricer.
	Repriced bool `json:"repriced,omitempty"`
}

// Pricer costs a request's tokens at the currently configured rates and
// says whether model has a pricing entry.
type Pricer func(model string, input, output, cacheRead, cacheWrite int64) (usd float64, priced bool)

var (
	pricerMu  sync.RWMutex
	pricer    Pricer
	longWrite func(model string, tokens int64) float64
)

// SetLongWritePricer installs what one-hour cache writes cost over the
// Pricer's ordinary write rate.
func SetLongWritePricer(f func(model string, tokens int64) float64) {
	pricerMu.Lock()
	longWrite = f
	pricerMu.Unlock()
}

// longWriteUSD is that extra for tokens on model, 0 with no pricer set.
func longWriteUSD(model string, tokens int64) float64 {
	pricerMu.RLock()
	f := longWrite
	pricerMu.RUnlock()
	if f == nil || tokens <= 0 {
		return 0
	}
	return f(model, tokens)
}

// SetPricer installs the pricing used to cost events that were recorded
// before their model was priced. On 2026-09-29 claude-opus-5-5 was priced at
// 11:39, and the 1,689 requests before that kept the dashboard's model row
// reading "unpriced" for every window that reached back to them, hiding the
// priced spend beside them. The stored token counts are exact, so costing
// them now gives the real figure without rewriting the file.
func SetPricer(p Pricer) {
	pricerMu.Lock()
	pricer = p
	pricerMu.Unlock()
}

// decodeEvent unmarshals one metrics line and reprices it if it was recorded
// unpriced and its model has a price now.
func decodeEvent(b []byte, e *Event) bool {
	if json.Unmarshal(b, e) != nil {
		return false
	}
	if e.PricingUnknown {
		pricerMu.RLock()
		p := pricer
		pricerMu.RUnlock()
		if p != nil {
			if usd, ok := p(e.Model, e.InputTokens, e.OutputTokens, e.CacheReadTokens, e.CacheWriteTokens); ok {
				e.APIEquivalentUSD, e.PricingUnknown, e.Repriced = usd+longWriteUSD(e.Model, e.CacheWrite1hTokens), false, true
			}
		}
	}
	return true
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
	CacheReadTokens   int64
	CacheWriteTokens  int64
	PrunedBytes       int64
	APIEquivalentUSD  float64
	// UnpricedRequests counts events that reported tokens but whose served
	// model had no pricing entry, and UnpricedModels names those models with
	// a per-model count. APIEquivalentUSD therefore covers only the priced
	// subset: when UnpricedRequests is non-zero it is a lower bound, not a
	// total, and String() says so rather than presenting it as complete.
	UnpricedRequests int
	UnpricedModels   map[string]int
}

// Summarize totals every event since since, across the rotated files too:
// "today" read from the current file alone loses whatever came before a
// rotation, and the daily-spend alert with it.
func Summarize(path string, since time.Time) (Summary, error) {
	var s Summary
	for _, p := range historyFiles(path, since) {
		if err := scanEvents(p, func(e Event) {
			if !since.IsZero() && e.Time.Before(since) {
				return
			}
			s.add(e)
		}); err != nil {
			return s, err
		}
	}
	return s, nil
}

func (s *Summary) add(e Event) {
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
	s.CacheReadTokens += e.CacheReadTokens
	s.CacheWriteTokens += e.CacheWriteTokens
	s.PrunedBytes += e.PrunedBytes
	s.APIEquivalentUSD += e.APIEquivalentUSD
	if e.PricingUnknown {
		s.UnpricedRequests++
		if s.UnpricedModels == nil {
			s.UnpricedModels = map[string]int{}
		}
		s.UnpricedModels[e.Model]++
	}
}

func (s Summary) String() string {
	out := fmt.Sprintf("requests=%d primary=%d secondary=%d input_tokens=%d cache_read_tokens=%d cache_write_tokens=%d output_tokens=%d api_equivalent_usd=$%.2f",
		s.Requests, s.PrimaryRequests, s.SecondaryRequests, s.InputTokens, s.CacheReadTokens, s.CacheWriteTokens, s.OutputTokens, s.APIEquivalentUSD)
	if s.PrunedBytes > 0 {
		out += fmt.Sprintf(" pruned_bytes=%d", s.PrunedBytes)
	}
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
		if !decodeEvent(sc.Bytes(), &e) {
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
	Errors int `json:"errors"`
	// Offline and Cancelled are what Errors leaves out: requests refused
	// while this Mac had no network, and ones the client gave up on.
	Offline          int     `json:"offline"`
	Cancelled        int     `json:"cancelled"`
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

	// The Saved view: what the secondary was sent (cached input included:
	// it was sent, only billed less) and what pruning removed from it, so a
	// bar's full height is what would have been sent without pruning.
	SecondarySentTokens int64   `json:"secondary_sent_tokens"`
	PrunedTokens        int64   `json:"pruned_tokens"`
	PrunedUSD           float64 `json:"pruned_usd"`
	// Pauseless compaction's side of the Saved view: the "without Burst"
	// twin's context minus the real one over compacted requests, priced at
	// the cache-read rate, and what the summaries and the cache rewrites
	// after each swap cost. See compaction.go.
	CompactedTokens      int64   `json:"compacted_tokens"`
	CompactedUSD         float64 `json:"compacted_usd"`
	CompactionSummaryUSD float64 `json:"compaction_summary_usd"`
	CompactionRewriteUSD float64 `json:"compaction_rewrite_usd"`
}

// SessionUse is one session's requests and spend over the window.
type SessionUse struct {
	Requests int
	USD      float64
	Unpriced bool
}

// RepoUse is one repository's share of the window.
type RepoUse struct {
	Repo     string  `json:"repo"`
	Path     string  `json:"path,omitempty"`
	Sessions int     `json:"sessions"`
	Requests int     `json:"requests"`
	USD      float64 `json:"usd"`
	Unpriced bool    `json:"unpriced,omitempty"`
	// SavedUSD is what Pauseless Compaction saved the repository's sessions
	// over the same window, net of summaries and cache rewrites (it can be
	// negative), and Compacted whether any of them was compacted at all.
	SavedUSD  float64 `json:"saved_usd,omitempty"`
	Compacted bool    `json:"compacted,omitempty"`
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
	// Repos is spend per repository, filled in by the admin server, which
	// can map a session to the directory it ran in; SessionUse is the
	// per-session spend it works from.
	Repos      []RepoUse             `json:"repos"`
	SessionUse map[string]SessionUse `json:"-"`

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

	ct := newCompactionTracker()
	for _, f := range historyFiles(path, start) {
		h.Files++
		err := scanEvents(f, func(e Event) {
			if earliest.IsZero() || e.Time.Before(earliest) {
				earliest = e.Time
			}
			// Before the window check: a swap just before midnight sets the
			// saving the next day's requests are credited with.
			fx := ct.observe(e)
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
			d.CompactedTokens += fx.saved
			d.CompactedUSD += fx.savedUSD
			d.CompactionSummaryUSD += fx.summaryUSD
			d.CompactionRewriteUSD += fx.rewriteUSD
			d.Requests++
			switch slotOf(e) {
			case "primary":
				d.PrimaryRequests++
				d.PrimaryTokens += allTokens(e)
				d.PrimaryUSD += e.APIEquivalentUSD
			case "secondary":
				d.SecondaryRequests++
				d.SecondaryTokens += allTokens(e)
				d.SecondaryUSD += e.APIEquivalentUSD
				d.SecondarySentTokens += e.InputTokens + e.CacheReadTokens + e.CacheWriteTokens
				d.PrunedTokens += e.PrunedBytes / BytesPerToken
				d.PrunedUSD += e.PrunedUSD
			}
			switch {
			case IsError(e):
				d.Errors++
			case IsOffline(e):
				d.Offline++
			case e.HTTPStatus == StatusClientClosed:
				d.Cancelled++
			}
			d.InputTokens += e.InputTokens
			d.OutputTokens += e.OutputTokens
			d.APIEquivalentUSD += e.APIEquivalentUSD
			if e.SessionID != "" {
				if h.SessionUse == nil {
					h.SessionUse = map[string]SessionUse{}
				}
				su := h.SessionUse[e.SessionID]
				su.Requests++
				su.USD += e.APIEquivalentUSD
				su.Unpriced = su.Unpriced || e.PricingUnknown
				h.SessionUse[e.SessionID] = su
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
				u.Tokens += allTokens(e)
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

// Scan calls fn with every event at or after start, oldest first, across
// the rotated files too. It returns the oldest event read, before start or
// not, which is what says whether the window is covered, and how many files
// were read.
func Scan(path string, start time.Time, fn func(Event)) (earliest time.Time, files int, err error) {
	for _, f := range historyFiles(path, start) {
		files++
		err := scanEvents(f, func(e Event) {
			if earliest.IsZero() || e.Time.Before(earliest) {
				earliest = e.Time
			}
			if !e.Time.Before(start) {
				fn(e)
			}
		})
		if err != nil {
			return earliest, files, err
		}
	}
	return earliest, files, nil
}

// Percentile is the nearest-rank percentile the dashboard's latencies use.
// It sorts v in place.
func Percentile(v []int64, p float64) int64 { return percentile(v, p) }

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
		if !decodeEvent(sc.Bytes(), &e) {
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

// allTokens is every token a request was billed for, cached ones too: on a
// cached prompt input and output alone are a sixtieth of what was read.
func allTokens(e Event) int64 {
	return e.InputTokens + e.OutputTokens + e.CacheReadTokens + e.CacheWriteTokens
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
	s.CacheReadTokens += e.CacheReadTokens
	s.CacheWriteTokens += e.CacheWriteTokens
	s.PrunedBytes += e.PrunedBytes
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

// StatusClientClosed records a request the client abandoned (nginx's 499):
// pressing Esc in Claude Code is not a gateway error.
const StatusClientClosed = 499

// offlineNote opens the note of a request the gateway answered itself with
// a 502 because this Mac had no network (the router writes it).
// The three notes the router writes for it share this opening: unavailable
// (no DNS), slow, and not passing traffic (a captive portal, a phone out of
// data). The last two were counted as real failures until 7 Oct 2026.
const offlineNote = "local network "

// IsOffline is a request refused here because the Mac was offline. It says
// nothing about Burst or the provider: with the lid shut or between
// networks, Claude Code's heartbeats, event streams and telemetry retry
// every few seconds, and on 2026-10-06 those were 1,851 of the day's 1,937
// "errors" and kept the dashboard's error rate amber for a fortnight.
func IsOffline(e Event) bool {
	return e.HTTPStatus == 502 && strings.HasPrefix(e.Note, offlineNote)
}

// IsError is a request that failed for a reason other than the client
// leaving or this Mac being offline.
func IsError(e Event) bool {
	return e.HTTPStatus >= 400 && e.HTTPStatus != StatusClientClosed && !IsOffline(e)
}
