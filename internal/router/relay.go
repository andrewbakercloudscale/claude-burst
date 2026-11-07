package router

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
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
	input, output, cacheRead, cacheWrite int64
	// cacheWrite1h is the part of cacheWrite that went to the one-hour
	// cache, which costs more than the five-minute one.
	cacheWrite1h                                 int64
	prunedBytes, prunedResults, truncatedResults int64
	repeatedCalls, rerunsAfterStub               int64
	// msgID is the response's message id: what the next request of a
	// message thread names as the one it continues.
	msgID string
}

// relay copies an upstream response to the client as it arrives and reads
// the token usage out of it on the way past. The error it returns means one
// thing: the upstream's body broke before it ended, so what the client has
// is part of a reply. A client that went away is not that, and is only
// logged, as is a clean end of the body.
func (s *Server) relay(rid string, w http.ResponseWriter, resp *http.Response, model string) (tokenUsage, error) {
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
		// io.Copy does not say which side failed, and they mean opposite
		// things, so the upstream side keeps its own error.
		up := &readErrKeeper{r: resp.Body}
		if _, err := io.Copy(w, io.TeeReader(up, capture)); err != nil && up.err == nil {
			// The client (Claude Code) disconnected mid-response. Not a
			// proxy bug, but worth having in the log when someone is
			// debugging a truncated response.
			s.logger.Printf("req=%s relay copy error (client likely disconnected): %q", rid, err)
		}
		if fl != nil {
			fl.Flush()
		}
		if up.err != nil {
			// Content-Length is not passed on (copyResponseHeaders), so
			// without this the client could not tell half a JSON body from
			// a whole one.
			return tokenUsage{}, up.err
		}
		if capture.overflow || !strings.Contains(ct, "json") {
			return tokenUsage{}, nil
		}
		return usageFromMessageJSON(capture.buf.Bytes()), nil
	}

	// Read in pieces of at most sseReadBytes, never a line at a time into
	// one string: a line had no bound, so an upstream that never sent a line
	// break had the gateway hold everything it sent. Each piece goes to the
	// client as it is read, so nothing is held back or cut whatever the
	// line's length; only the copy kept for reading usage out of is bounded.
	br := bufio.NewReaderSize(resp.Body, sseReadBytes)
	var tok tokenUsage
	var line []byte
	tooLong := false
	for {
		piece, err := br.ReadSlice('\n')
		if len(piece) > 0 {
			if _, werr := w.Write(piece); werr != nil {
				s.logger.Printf("req=%s relay SSE write error (client likely disconnected): %q", rid, werr)
				return tok, nil
			}
			if fl != nil {
				fl.Flush()
			}
			switch {
			case tooLong:
			case len(line)+len(piece) > maxSSELineBytes:
				tooLong, line = true, line[:0]
				s.logger.Printf("req=%s warn stage=relay an event-stream line passed %d bytes: relayed in full, but not read for token usage", rid, maxSSELineBytes)
			default:
				line = append(line, piece...)
			}
		}
		if err == bufio.ErrBufferFull {
			continue // the same line goes on
		}
		if len(line) > 0 {
			parseSSEUsage(string(line), &tok)
		}
		line, tooLong = line[:0], false
		if err == io.EOF {
			return tok, nil
		}
		if err != nil {
			return tok, err
		}
	}
}

// sseReadBytes is the most the relay reads of an event stream before passing
// it on. Events are a few hundred bytes; this is room for a large one in one
// piece.
const sseReadBytes = 64 * 1024

// maxSSELineBytes bounds the copy of one event-stream line kept for reading
// its usage. The lines that carry usage (message_start, message_delta) are
// under a kilobyte, and the translator's scanner allows a line the same.
const maxSSELineBytes = 4 * 1024 * 1024

// readErrKeeper remembers the error that ended a read, when it was not a
// plain end of the body.
type readErrKeeper struct {
	r   io.Reader
	err error
}

func (k *readErrKeeper) Read(p []byte) (int, error) {
	n, err := k.r.Read(p)
	if err != nil && err != io.EOF {
		k.err = err
	}
	return n, err
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
	if cc, ok := u["cache_creation"].(map[string]any); ok {
		if n := number(cc["ephemeral_1h_input_tokens"]); n > tok.cacheWrite1h {
			tok.cacheWrite1h = n
		}
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

var errBodyTooLarge = errors.New("decoded request body exceeds the limit")

// gunzipLimited decodes a gzip request body, refusing one that decodes to
// more than limit bytes: the limit on what was read says nothing about what
// it expands to.
func gunzipLimited(body []byte, limit int64) ([]byte, error) {
	zr, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	plain, err := io.ReadAll(io.LimitReader(zr, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(plain)) > limit {
		return nil, errBodyTooLarge
	}
	return plain, nil
}

// PriceTokens is the API-equivalent cost of one request's tokens at the
// configured rates, and whether model has a pricing entry at all. writeMetric
// prices live requests with it; metrics.SetPricer hands it to the readers so
// events recorded before a model was priced are costed from their stored
// token counts instead of staying "unpriced" for the life of the file.
func (s *Server) PriceTokens(model string, input, output, cacheRead, cacheWrite int64) (float64, bool) {
	return s.cfg.PriceTokens(model, input, output, cacheRead, cacheWrite)
}

// LongWriteExtraUSD is what one-hour cache writes cost over PriceTokens.
func (s *Server) LongWriteExtraUSD(model string, tokens int64) float64 {
	return s.cfg.LongWriteExtraUSD(model, tokens)
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
	equiv += s.LongWriteExtraUSD(model, tok.cacheWrite1h)
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
		InputTokens: tok.input, OutputTokens: tok.output, CacheReadTokens: tok.cacheRead, CacheWriteTokens: tok.cacheWrite, CacheWrite1hTokens: tok.cacheWrite1h,
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
