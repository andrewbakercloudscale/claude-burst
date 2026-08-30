package metrics

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"
)

type Event struct {
	Time             time.Time `json:"time"`
	RequestID        string    `json:"request_id,omitempty"`
	SessionID        string    `json:"session_id,omitempty"`
	AgentID          string    `json:"agent_id,omitempty"`
	Slot             string    `json:"slot,omitempty"` // "primary" | "secondary"; empty on events written before this field existed
	Route            string    `json:"route"`
	Model            string    `json:"model,omitempty"`
	HTTPStatus       int       `json:"http_status"`
	DurationMS       int64     `json:"duration_ms"`
	InputTokens      int64     `json:"input_tokens,omitempty"`
	OutputTokens     int64     `json:"output_tokens,omitempty"`
	APIEquivalentUSD float64   `json:"api_equivalent_usd,omitempty"`
	LimitClaim       string    `json:"limit_claim,omitempty"`
	ResetAt          int64     `json:"reset_at,omitempty"`
	Note             string    `json:"note,omitempty"`
}

type Writer struct {
	path string
	mu   sync.Mutex
}

func New(path string) *Writer { return &Writer{path: path} }

func (w *Writer) Write(e Event) error {
	w.mu.Lock()
	defer w.mu.Unlock()
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
	}
	return s, sc.Err()
}

func (s Summary) String() string {
	return fmt.Sprintf("requests=%d primary=%d secondary=%d input_tokens=%d output_tokens=%d api_equivalent_usd=$%.2f",
		s.Requests, s.PrimaryRequests, s.SecondaryRequests, s.InputTokens, s.OutputTokens, s.APIEquivalentUSD)
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
