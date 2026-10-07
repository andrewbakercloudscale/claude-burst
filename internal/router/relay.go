package router

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/andrewbakercloudscale/claude-burst/internal/logline"
	"github.com/andrewbakercloudscale/claude-burst/internal/metrics"
)

func copyResponseHeaders(dst http.Header, src http.Header) {
	for k, vals := range src {
		lk := strings.ToLower(k)
		if lk == "content-length" || lk == "connection" || lk == "content-encoding" {
			continue
		}
		for _, v := range vals {
			dst.Add(k, v)
		}
	}
}

// tokenUsage follows Anthropic's semantics on every route: input is the
// UNCACHED input only, and cacheRead/cacheWrite are counted separately.
// Anthropic reports it this way natively; the OpenAI-compatible translation
// splits cached_tokens out of prompt_tokens to match. Recording input alone
// is what made a 150k-token primary turn show up as "input_tokens": 2.
//
// prunedBytes/prunedResults/truncatedResults are not usage; they ride along
// so the secondary's context pruning (prune.go) reaches the metrics row
// without widening writeMetric's signature for every caller.
type tokenUsage struct {
	input, output, cacheRead, cacheWrite         int64
	prunedBytes, prunedResults, truncatedResults int64
	repeatedCalls, rerunsAfterStub               int64
	// msgID is the response's message id: what the next request of a
	// message thread names as the one it continues.
	msgID string
}

func (s *Server) relay(rid string, w http.ResponseWriter, resp *http.Response, model string) tokenUsage {
	defer resp.Body.Close()
	copyResponseHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	fl, _ := w.(http.Flusher)

	ct := strings.ToLower(resp.Header.Get("content-type"))
	if !strings.Contains(ct, "text/event-stream") {
		// A stream:false Messages response carries its usage in the JSON
		// body. Tee a bounded copy while relaying, so the client still gets
		// bytes as they arrive and a billed request is not recorded as $0.
		capture := &cappedBuffer{max: maxUsageCaptureBytes}
		if _, err := io.Copy(w, io.TeeReader(resp.Body, capture)); err != nil {
			// Almost always means the client (Claude Code) disconnected
			// mid-response. Not a proxy bug, but worth having in the log
			// when someone is debugging a truncated response.
			s.logger.Printf("req=%s relay copy error (client likely disconnected): %v", rid, err)
		}
		if fl != nil {
			fl.Flush()
		}
		if capture.overflow || !strings.Contains(ct, "json") {
			return tokenUsage{}
		}
		return usageFromMessageJSON(capture.buf.Bytes())
	}

	br := bufio.NewReader(resp.Body)
	var tok tokenUsage
	for {
		line, err := br.ReadString('\n')
		if len(line) > 0 {
			if _, werr := io.WriteString(w, line); werr != nil {
				s.logger.Printf("req=%s relay SSE write error (client likely disconnected): %v", rid, werr)
				break
			}
			if fl != nil {
				fl.Flush()
			}
			parseSSEUsage(line, &tok)
		}
		if err != nil {
			if err != io.EOF {
				s.logger.Printf("req=%s relay SSE read error: %v", rid, err)
			}
			break
		}
	}
	return tok
}

// maxUsageCaptureBytes bounds how much of a non-streaming body relay keeps
// for usage extraction. Message responses are far smaller; anything larger
// is relayed in full but not parsed.
const maxUsageCaptureBytes = 4 * 1024 * 1024

// cappedBuffer records up to max bytes and then stops recording, without
// ever returning an error, so it can never break the relay it is teed from.
type cappedBuffer struct {
	buf      bytes.Buffer
	max      int
	overflow bool
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	if !c.overflow {
		if c.buf.Len()+len(p) > c.max {
			c.overflow = true
			c.buf.Reset()
		} else {
			c.buf.Write(p)
		}
	}
	return len(p), nil
}

// usageFromMessageJSON reads the top-level usage block of a non-streaming
// Messages response. Only "usage" is read: count_tokens replies carry a
// top-level input_tokens that is not a billed request and must stay zero.
func usageFromMessageJSON(body []byte) tokenUsage {
	var v struct {
		ID    string         `json:"id"`
		Usage map[string]any `json:"usage"`
	}
	if json.Unmarshal(body, &v) != nil || v.Usage == nil {
		return tokenUsage{}
	}
	tok := tokenUsage{input: number(v.Usage["input_tokens"]), output: number(v.Usage["output_tokens"]), msgID: v.ID}
	readCacheUsage(v.Usage, &tok)
	return tok
}

func parseSSEUsage(line string, tok *tokenUsage) {
	line = strings.TrimSpace(line)
	if !strings.HasPrefix(line, "data:") {
		return
	}
	raw := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
	if raw == "" || raw == "[DONE]" {
		return
	}
	var v map[string]any
	if json.Unmarshal([]byte(raw), &v) != nil {
		return
	}
	// message_start: {message:{usage:{input_tokens:...}}}
	if m, ok := v["message"].(map[string]any); ok {
		if id, _ := m["id"].(string); id != "" {
			tok.msgID = id
		}
		if u, ok := m["usage"].(map[string]any); ok {
			if n := number(u["input_tokens"]); n > tok.input {
				tok.input = n
			}
			if n := number(u["output_tokens"]); n > tok.output {
				tok.output = n
			}
			readCacheUsage(u, tok)
		}
	}
	// message_delta: {usage:{output_tokens:...}}
	if u, ok := v["usage"].(map[string]any); ok {
		if n := number(u["input_tokens"]); n > tok.input {
			tok.input = n
		}
		if n := number(u["output_tokens"]); n > tok.output {
			tok.output = n
		}
		readCacheUsage(u, tok)
	}
}

// readCacheUsage takes the larger of what is already recorded and what this
// usage block says, for the same reason parseSSEUsage does: message_start
// and message_delta can both carry the counts, and a later block restating
// a smaller (or absent, so zero) figure must not overwrite a real one.
func readCacheUsage(u map[string]any, tok *tokenUsage) {
	if n := number(u["cache_read_input_tokens"]); n > tok.cacheRead {
		tok.cacheRead = n
	}
	if n := number(u["cache_creation_input_tokens"]); n > tok.cacheWrite {
		tok.cacheWrite = n
	}
}

func number(v any) int64 {
	switch n := v.(type) {
	case float64:
		return int64(n)
	case json.Number:
		i, _ := n.Int64()
		return i
	case string:
		i, _ := strconv.ParseInt(n, 10, 64)
		return i
	}
	return 0
}

func requestModel(body []byte) string {
	var v struct {
		Model string `json:"model"`
	}
	if json.Unmarshal(body, &v) == nil {
		return v.Model
	}
	return ""
}

// PriceTokens is the API-equivalent cost of one request's tokens at the
// configured rates, and whether model has a pricing entry at all. writeMetric
// prices live requests with it; metrics.SetPricer hands it to the readers so
// events recorded before a model was priced are costed from their stored
// token counts instead of staying "unpriced" for the life of the file.
func (s *Server) PriceTokens(model string, input, output, cacheRead, cacheWrite int64) (float64, bool) {
	return s.cfg.PriceTokens(model, input, output, cacheRead, cacheWrite)
}

func (s *Server) writeMetric(in *http.Request, slot, route, model, requestedModel string, status int, start time.Time, tok tokenUsage, claim string, reset int64, note, destination string) {
	// As the text log: a Go network error quotes the whole URL, and the
	// query of one (the name a DNS lookup asked for, a device id) is not
	// metadata.
	note = logline.StripQueries(note)

	// Two-value lookup, not a bare index. A missing key yields the zero
	// ModelPrice, so indexing alone silently prices an unknown model at
	// $0/Mtok -- which is exactly what happened once the served model
	// started being recorded correctly: the secondary's real model id is
	// not in the default pricing table, so every overflow request recorded
	// api_equivalent_usd=0 and `stats` reported no secondary spend at all.
	// A zero that means "not priced" must not look like a zero that means
	// "free".
	price, priced := s.cfg.Pricing[model]
	equiv, _ := s.PriceTokens(model, tok.input, tok.output, tok.cacheRead, tok.cacheWrite)
	// Only tokens make a missing price a problem. Events with no token
	// counts (failover notes, upstream errors, control-plane passthrough)
	// legitimately cost nothing and must not be flagged.
	unpriced := !priced && (tok.input > 0 || tok.output > 0 || tok.cacheRead > 0 || tok.cacheWrite > 0)
	if unpriced {
		if _, seen := s.warnedUnpriced.LoadOrStore(model, true); !seen {
			s.logger.Printf("warn stage=pricing model=%q no pricing entry; cost for this model is not being counted -- add it to `pricing` in config.json", model)
		}
	}
	rid := requestIDFrom(in.Context())
	s.alertOutcome(slot, status)
	traceHop(in.Context(), TraceHop{Slot: slot, Route: route, Model: model, RequestedModel: requestedModel,
		Status: status, DurationMS: time.Since(start).Milliseconds(), Note: note, Destination: destination})
	err := s.metrics.Write(metrics.Event{
		Time: time.Now(), RequestID: rid, SessionID: in.Header.Get("x-claude-code-session-id"), AgentID: in.Header.Get("x-claude-code-agent-id"),
		Slot: slot, Route: route, Model: model, RequestedModel: requestedModel, HTTPStatus: status, DurationMS: time.Since(start).Milliseconds(),
		InputTokens: tok.input, OutputTokens: tok.output, CacheReadTokens: tok.cacheRead, CacheWriteTokens: tok.cacheWrite,
		PrunedBytes: tok.prunedBytes, PrunedToolResults: tok.prunedResults, TruncatedToolResults: tok.truncatedResults,
		RepeatedCalls: tok.repeatedCalls, RerunsAfterStub: tok.rerunsAfterStub,
		PrunedUSD:         float64(tok.prunedBytes/metrics.BytesPerToken) / 1_000_000 * price.InputPerMTok,
		CompactedMessages: int64(compactInfoFrom(in.Context()).removedMsgs), CompactedBytes: compactInfoFrom(in.Context()).removedBytes,
		APIEquivalentUSD: equiv, LimitClaim: claim, ResetAt: reset, Note: note, Destination: destination,
		PricingUnknown: unpriced,
	})
	if err != nil {
		// The request has already been served to Claude Code by this point;
		// a metrics-write failure (e.g. disk full, permissions) must never
		// take the gateway down. Log it loudly so it's diagnosable, and move on.
		s.logger.Printf("req=%s error stage=write_metric err=%v", rid, err)
	}
}
