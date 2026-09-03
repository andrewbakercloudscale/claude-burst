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
