package metrics

import (
	"bufio"
	"encoding/json"
	"os"
	"strings"
	"time"
)

// Efficiency is what the admin page's "Context & cache" section shows: how
// much of each route's context is served from cache, and whether pruning of
// overflow requests is happening and is safe. Every figure is from
// metrics.jsonl, so it covers only what the gateway itself recorded.
type Efficiency struct {
	Primary   RouteEfficiency `json:"primary"`
	Secondary RouteEfficiency `json:"secondary"`

	// Pruning of secondary requests. PrunedOK/PrunedFailed and
	// UnprunedOK/UnprunedFailed split the secondary's inference requests by
	// whether anything was removed, so a prune that confuses the model shows
	// up as a failure rate that differs between the two.
	PrunedRequests   int       `json:"pruned_requests"`
	PrunedOK         int       `json:"pruned_ok"`
	PrunedFailed     int       `json:"pruned_failed"`
	UnprunedOK       int       `json:"unpruned_ok"`
	UnprunedFailed   int       `json:"unpruned_failed"`
	PrunedBytes      int64     `json:"pruned_bytes"`
	StubbedResults   int64     `json:"stubbed_results"`
	TruncatedResults int64     `json:"truncated_results"`
	LastPrunedAt     time.Time `json:"last_pruned_at,omitempty"`
}

type RouteEfficiency struct {
	Requests         int     `json:"requests"`
	InputTokens      int64   `json:"input_tokens"`
	CacheReadTokens  int64   `json:"cache_read_tokens"`
	CacheWriteTokens int64   `json:"cache_write_tokens"`
	OutputTokens     int64   `json:"output_tokens"`
	USD              float64 `json:"usd"`
}

// CacheHitRate is the share of the context that was read from cache: the
// number that says whether caching is working at all on a route.
func (r RouteEfficiency) CacheHitRate() float64 {
	total := r.InputTokens + r.CacheReadTokens + r.CacheWriteTokens
	if total == 0 {
		return 0
	}
	return float64(r.CacheReadTokens) / float64(total)
}

// isInference is a request that asked a model for something: it named a
// model, as control-plane calls (heartbeats, event logging) do not.
func isInference(e Event) bool {
	return e.Model != "" || e.RequestedModel != ""
}

// ok counts 2xx as success. Status 0 is a transport failure or a request
// that never got a response, which is a failure too.
func ok(e Event) bool { return e.HTTPStatus >= 200 && e.HTTPStatus < 300 }

func EfficiencySince(path string, since time.Time) (Efficiency, error) {
	var eff Efficiency
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return eff, nil
	}
	if err != nil {
		return eff, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 2*1024*1024)
	for sc.Scan() {
		var e Event
		if json.Unmarshal(sc.Bytes(), &e) != nil || e.Time.Before(since) || !isInference(e) {
			continue
		}
		// Client cancellations are the caller going away, not the provider
		// failing; counting them would blame pruning for closed terminals.
		if strings.HasPrefix(e.Note, "client cancelled") {
			continue
		}
		r := &eff.Primary
		if e.Slot == "secondary" {
			r = &eff.Secondary
		}
		r.Requests++
		r.InputTokens += e.InputTokens
		r.CacheReadTokens += e.CacheReadTokens
		r.CacheWriteTokens += e.CacheWriteTokens
		r.OutputTokens += e.OutputTokens
		r.USD += e.APIEquivalentUSD
		if e.Slot != "secondary" {
			continue
		}
		wasPruned := e.PrunedBytes > 0
		switch {
		case wasPruned && ok(e):
			eff.PrunedOK++
		case wasPruned:
			eff.PrunedFailed++
		case ok(e):
			eff.UnprunedOK++
		default:
			eff.UnprunedFailed++
		}
		if wasPruned {
			eff.PrunedRequests++
			eff.PrunedBytes += e.PrunedBytes
			eff.StubbedResults += e.PrunedToolResults
			eff.TruncatedResults += e.TruncatedToolResults
			if e.Time.After(eff.LastPrunedAt) {
				eff.LastPrunedAt = e.Time
			}
		}
	}
	return eff, sc.Err()
}
