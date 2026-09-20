package shunt

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/andrewbakercloudscale/claude-burst/internal/rotate"
)

// Event kinds.
const (
	KindRead  = "read"
	KindWrite = "write"
	KindDeny  = "deny" // the guard refused a direct read
)

// Event is one line of shunt.jsonl. Metadata only: never file contents,
// questions, specs or answers.
type Event struct {
	Time         time.Time `json:"time"`
	Kind         string    `json:"kind"`
	OK           bool      `json:"ok"`
	Files        int       `json:"files,omitempty"`
	Calls        int       `json:"calls,omitempty"`
	BytesIn      int64     `json:"bytes_in,omitempty"`  // file bytes delegated (or, for a deny, the size of the refused file)
	BytesOut     int64     `json:"bytes_out,omitempty"` // bytes of answer returned, or of code generated to disk
	InputTokens  int64     `json:"input_tokens,omitempty"`
	OutputTokens int64     `json:"output_tokens,omitempty"`
	Model        string    `json:"model,omitempty"`
	// Destination is the worker endpoint the call went to (scheme, host and
	// path; never a query or a key), so the requests table can show where a
	// shunt physically went the way it does for gateway requests.
	Destination    string  `json:"destination,omitempty"`
	USD            float64 `json:"usd,omitempty"`
	PricingUnknown bool    `json:"pricing_unknown,omitempty"`
	DurationMS     int64   `json:"duration_ms,omitempty"`
	Note           string  `json:"note,omitempty"`
}

const (
	logMaxBytes   = 5 * 1024 * 1024
	logMaxBackups = 3
)

// Append records an event. A failure to log is returned but callers ignore it:
// bookkeeping must never fail the read it describes.
func Append(path string, e Event) error {
	if path == "" {
		return nil
	}
	if e.Time.IsZero() {
		e.Time = time.Now()
	}
	_ = rotate.RotateIfOversized(path, logMaxBytes, logMaxBackups)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
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

// KeptOutTokens estimates the frontier-context tokens this one event avoided:
// for a read, the file text that stayed out less the answer that came back;
// for a write, the code that never had to be emitted. A refused direct read
// kept nothing out on its own -- it only redirected -- and a failure kept
// nothing out at all.
func (e Event) KeptOutTokens() int64 {
	if !e.OK {
		return 0
	}
	switch e.Kind {
	case KindRead:
		kept := e.BytesIn - e.BytesOut
		if kept < 0 {
			kept = 0
		}
		return EstimateTokens(kept)
	case KindWrite:
		return EstimateTokens(e.BytesOut)
	}
	return 0
}

// Recent returns up to limit events, newest first, that keep reports true for
// (nil keeps everything). It reads the live file only, like SummarizeLog.
func Recent(path string, limit int, keep func(Event) bool) ([]Event, error) {
	if limit <= 0 {
		limit = 20
	}
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	ring := make([]Event, 0, limit)
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	for sc.Scan() {
		var e Event
		if json.Unmarshal(sc.Bytes(), &e) != nil || (keep != nil && !keep(e)) {
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
	out := make([]Event, 0, len(ring))
	for i := len(ring) - 1; i >= 0; i-- {
		out = append(out, ring[i])
	}
	return out, nil
}

// Summary totals shunt.jsonl.
type Summary struct {
	Reads, Writes, Denials, Failures int
	BytesDelegated                   int64 // file bytes the worker read instead of the frontier model
	BytesAnswered                    int64 // bytes that came back into context as answers
	BytesGenerated                   int64 // bytes written to disk without passing through context
	WorkerInputTokens                int64
	WorkerOutputTokens               int64
	WorkerUSD                        float64
	Unpriced                         int
}

// KeptOutTokens estimates the frontier-context tokens avoided: file text that
// stayed out (less the answer that came back) plus generated code that never
// had to be emitted. An estimate at ~4 bytes per token, not a measurement.
func (s Summary) KeptOutTokens() int64 {
	kept := s.BytesDelegated - s.BytesAnswered
	if kept < 0 {
		kept = 0
	}
	return EstimateTokens(kept) + EstimateTokens(s.BytesGenerated)
}

// SummarizeLog reads path (the live file only) for events at or after since.
func SummarizeLog(path string, since time.Time) (Summary, error) {
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
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	for sc.Scan() {
		var e Event
		if json.Unmarshal(sc.Bytes(), &e) != nil || e.Time.Before(since) {
			continue
		}
		switch e.Kind {
		case KindDeny:
			s.Denials++
			continue
		case KindRead:
			s.Reads++
		case KindWrite:
			s.Writes++
		default:
			continue
		}
		if !e.OK {
			s.Failures++
			continue
		}
		if e.Kind == KindRead {
			s.BytesDelegated += e.BytesIn
			s.BytesAnswered += e.BytesOut
		} else {
			s.BytesGenerated += e.BytesOut
		}
		s.WorkerInputTokens += e.InputTokens
		s.WorkerOutputTokens += e.OutputTokens
		s.WorkerUSD += e.USD
		if e.PricingUnknown {
			s.Unpriced++
		}
	}
	return s, sc.Err()
}

func (s Summary) String() string {
	out := fmt.Sprintf("reads=%d writes=%d denied_direct_reads=%d failures=%d kept_out_of_context≈%d tokens (estimate) worker_tokens=%d in/%d out worker_cost=$%.4f",
		s.Reads, s.Writes, s.Denials, s.Failures, s.KeptOutTokens(), s.WorkerInputTokens, s.WorkerOutputTokens, s.WorkerUSD)
	if s.Unpriced > 0 {
		out += fmt.Sprintf(" (INCOMPLETE: %d worker call(s) had no pricing entry -- add the worker model to `pricing` in config.json)", s.Unpriced)
	}
	return out
}
