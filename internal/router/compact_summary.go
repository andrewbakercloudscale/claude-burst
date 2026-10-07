package router

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"runtime/debug"
	"strings"
	"time"

	"github.com/andrewbakercloudscale/claude-burst/internal/metrics"
	"github.com/andrewbakercloudscale/claude-burst/internal/notice"
)

// compactionSummaryPrompt is Anthropic's recommended client-side compaction
// prompt (claude-api skill, model-migration.md). Its last sentence matters:
// the summary request carries the session's tools, and without it the
// model occasionally calls one instead of writing the summary.
const compactionSummaryPrompt = "Summarize the transcript inside <summary></summary> tags. Include relevant information in the summary such that this conversation will be continued by a new context window without needing to redo work or be reprovided with relevant constraints or context. Be sure to preserve: (1) any difficulties or problems that came up, and how they were handled or resolved; (2) any possibilities, options, or approaches that were raised, tried, or set aside, and why; (3) anything that was asked for, decided, agreed, ruled out, or established as a preference, constraint, or boundary - stated exactly; (4) exactly where things stand now - what has been covered, settled, or completed so far; (5) anything still open, unresolved, promised, or expected to happen next; (6) specific details that would be hard to reconstruct - names, numbers, dates, exact wording, links or references - kept exactly. Be complete on these even at the cost of length; keep everything else concise. Weight the two voices differently: keep what the user said, asked for, shared, or established carefully and close to their own words; your own explanations and reasoning can be condensed much further, to what they concluded or produced - as long as nothing in the six items above is dropped. Do not call any tools while writing this summary, even if the conversation above was in the middle of using them: tools are unavailable here and any tool call fails. Respond with text only, beginning with <summary>."

// summaryTimeout bounds one summary call: a 400k-token read plus a few
// thousand tokens of summary.
const summaryTimeout = 5 * time.Minute

// retryAfterFailure is how soon a session tries again after a summary fails.
// On 2026-09-30 a failure meant another hour on a 475k history.
var retryAfterFailure = 5 * time.Minute

// compactionBoundary picks the latest plain prompt past offset that leaves
// at least minSummarisedShare of the view's bytes before it. It returns the
// boundary in original message indexes and the cut in view indexes, or 0, 0.
func compactionBoundary(view []json.RawMessage, bounds []int, offset int) (p, cut int) {
	total := 0
	for _, m := range view {
		total += len(m)
	}
	for i := len(bounds) - 1; i >= 0; i-- {
		c := bounds[i] - offset
		if c <= 0 {
			break
		}
		before := 0
		for _, m := range view[:c] {
			before += len(m)
		}
		if float64(before) >= minSummarisedShare*float64(total) {
			return bounds[i], c
		}
	}
	return 0, 0
}

// summarise makes one summary request through the primary, with the
// session's own auth, model, system prompt and tools, and stores the
// summary for key when it succeeds.
func (s *Server) summarise(ctx context.Context, in *http.Request, top map[string]json.RawMessage, history []json.RawMessage, cut int, key string, p0 int, hash string, safeP0 int, safeHash string) {
	// Runs on its own goroutine, outside net/http's per-connection recover,
	// and parses model output: a panic here would otherwise exit the gateway
	// and drop every session's in-flight request. Registered first, so it
	// runs after the deferred unlock and Done below.
	defer func() {
		if rec := recover(); rec != nil {
			s.logger.Printf("compaction PANIC session=%s err=%v\n%s", key, rec, debug.Stack())
		}
	}()
	defer s.compaction.running.Done()
	summary, err := s.requestSummary(ctx, in, top, history, cut)
	s.compaction.mu.Lock()
	defer s.compaction.mu.Unlock()
	if cancel := s.compaction.cancels[key]; cancel != nil {
		cancel(nil)
		delete(s.compaction.cancels, key)
	}
	st := s.compaction.state(key)
	st.pending = false
	defer s.compaction.save()
	if context.Cause(ctx) == errSessionCleared {
		// Not a failure, and nobody is left to tell. The window reopens: a
		// cleared session that is resumed can summarise at once.
		st.startedAt = time.Time{}
		s.logger.Printf("compaction cancelled session=%s: the session was cleared while its summary was being written", key)
		return
	}
	if err != nil {
		// A failed attempt must not hold the session on its full history
		// for the whole window: try again after retryAfterFailure.
		if w := time.Duration(s.compaction.cfg.WindowMinutes) * time.Minute; w > retryAfterFailure {
			st.startedAt = time.Now().Add(retryAfterFailure - w)
		}
		s.logger.Printf("compaction failed session=%s: %v (next attempt in %s)", key, err, retryAfterFailure)
		reason := err.Error()
		if len(reason) > 160 {
			reason = reason[:160] + "..."
		}
		notice.Publish(alertCompact, notice.Warn, "Compaction failed",
			fmt.Sprintf("The summary could not be written (%s). The full history keeps going; the next attempt is in %s.", reason, retryAfterFailure))
		st.notice("summary failed (%s); retry in %s", reason, retryAfterFailure)
		return
	}
	st.next, st.nextP0, st.nextHash, st.nextMarks = summary, safeP0, safeHash, st.pendingMarks
	st.nextTightP0, st.nextTightHash = p0, hash
	st.waitShown = false
	s.logger.Printf("compaction summary ready session=%s: %d messages summarised into %d characters; applies from the next request it fits", key, p0, len(summary))
}

func (s *Server) requestSummary(parent context.Context, in *http.Request, top map[string]json.RawMessage, history []json.RawMessage, cut int) (string, error) {
	ctx, cancel := context.WithTimeout(parent, summaryTimeout)
	defer cancel()
	in = in.WithContext(context.WithValue(ctx, requestIDKey, newRequestID()))
	msgs := withSummaryInstruction(history, cut)
	summary, blocks, err := s.summaryCall(ctx, in, top, msgs, metrics.NoteSummary)
	if err != errSummaryCalledTool {
		return summary, err
	}
	// The request ends where the work was: often a prompt or a tool result
	// that asks for the next tool call, and the model sometimes makes it
	// (first live run of the cut after the request, 2026-10-05). Its calls
	// are answered with a refusal and it is asked once more. That request
	// starts with the first one, so it reads it from cache.
	again, ok := afterRefusedTools(msgs, blocks)
	if !ok {
		return "", err
	}
	s.logger.Printf("req=%s compaction summary: the model called a tool instead; the call is refused and it is asked once more", requestIDFrom(in.Context()))
	summary, _, err = s.summaryCall(ctx, in, top, again, metrics.NoteSummaryRetry)
	return summary, err
}

// errSummaryCalledTool: the summary call ended in a tool call.
var errSummaryCalledTool = errors.New("the model called a tool instead of writing the summary")

// summaryCall makes one summary request and returns the summary, or, with
// errSummaryCalledTool, the reply's content blocks.
func (s *Server) summaryCall(ctx context.Context, in *http.Request, top map[string]json.RawMessage, messages []json.RawMessage, note string) (string, []any, error) {
	req := map[string]json.RawMessage{}
	for _, k := range []string{"model", "system", "tools", "thinking", "metadata"} {
		if v, ok := top[k]; ok {
			req[k] = v
		}
	}
	msgs, _ := json.Marshal(messages)
	req["messages"] = msgs
	req["max_tokens"] = json.RawMessage("16000")
	req["stream"] = json.RawMessage("true")
	body, err := json.Marshal(req)
	if err != nil {
		return "", nil, err
	}
	start := time.Now()
	out, model, err := s.primary.Prepare(ctx, in, body)
	if err != nil {
		return "", nil, fmt.Errorf("build summary request: %w", err)
	}
	dest := out.URL.Scheme + "://" + out.URL.Host + out.URL.Path
	resp, err := s.client.Do(out)
	if err != nil {
		s.writeMetric(in, "primary", s.primary.Name(), model, model, http.StatusBadGateway, start, tokenUsage{}, "", 0, "compaction summary failed: "+err.Error(), dest)
		return "", nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
		s.writeMetric(in, "primary", s.primary.Name(), model, model, resp.StatusCode, start, tokenUsage{}, "", 0, "compaction summary rejected: "+errorExcerpt(b), dest)
		return "", nil, fmt.Errorf("summary request: status %d: %s", resp.StatusCode, errorExcerpt(b))
	}
	text, stop, tok, blocks, rerr := readSSEBlocks(resp.Body)
	summary := summaryFromText(text)
	// The note is what the readers of the metrics log judge the call by, so
	// it says what came back: a 200 is not a summary.
	switch {
	case rerr != nil:
		note = metrics.NoteSummaryIncomplete + ": " + rerr.Error()
	case stop == "tool_use":
		note = metrics.NoteSummaryToolCall
	case stop == "max_tokens":
		note = metrics.NoteSummaryIncomplete + ": cut off at max_tokens"
	case summary == "":
		note = metrics.NoteSummaryIncomplete + ": empty"
	}
	s.writeMetric(in, "primary", s.primary.Name(), model, model, resp.StatusCode, start, tok, "", 0, note, dest)
	switch {
	case rerr != nil:
		return "", nil, rerr
	case stop == "tool_use":
		return "", blocks, errSummaryCalledTool
	case stop == "max_tokens":
		return "", nil, fmt.Errorf("the summary was cut off at max_tokens")
	case summary == "":
		return "", nil, fmt.Errorf("empty summary (stop_reason %q)", stop)
	}
	return summary, nil, nil
}

// afterRefusedTools returns msgs followed by the reply that called tools
// and a refusal of each call that asks for the summary again. False when
// the reply has no tool call to refuse.
func afterRefusedTools(msgs []json.RawMessage, blocks []any) ([]json.RawMessage, bool) {
	var results []any
	for _, b := range blocks {
		if bm, _ := b.(map[string]any); bm["type"] == "tool_use" {
			results = append(results, map[string]any{"type": "tool_result", "tool_use_id": bm["id"], "is_error": true,
				"content": "Not run: tools are unavailable while the summary is written."})
		}
	}
	if len(results) == 0 {
		return nil, false
	}
	results = append(results, map[string]any{"type": "text", "text": "No tool was run and none can be. Write the summary now, text only, beginning with <summary>. " + compactionSummaryPrompt})
	a, err1 := json.Marshal(map[string]any{"role": "assistant", "content": blocks})
	u, err2 := json.Marshal(map[string]any{"role": "user", "content": results})
	if err1 != nil || err2 != nil {
		return nil, false
	}
	return append(append([]json.RawMessage(nil), msgs...), a, u), true
}

// errSessionCleared is why ClearSession stops a summary call.
var errSessionCleared = errors.New("session cleared")

// ClearSession is told that session sid ran /clear: its conversation is
// gone, so a summary still being written for it is stopped. It was seen on
// 3 Dec 2026 in another client of the same idea: a summary started at 407k,
// /clear 14 seconds later, and the summary finished 12 seconds after that
// for a conversation nobody had. The read of the history is paid for the
// moment the call starts; stopping saves the writing. It returns how many
// calls it stopped.
//
// Only /clear: a session that exits is often resumed, and its summary is
// then wanted. A summary already written is left too: it costs nothing to
// keep, and is dropped by the history check if it never fits again.
func (s *Server) ClearSession(sid string) int {
	if sid == "" {
		return 0
	}
	s.compaction.mu.Lock()
	defer s.compaction.mu.Unlock()
	n := 0
	for key, cancel := range s.compaction.cancels {
		if strings.HasPrefix(key, sid+"|") {
			cancel(errSessionCleared)
			n++
		}
	}
	return n
}

// withSummaryInstruction returns the request's own history with the summary
// instruction appended after its last block, so the summary call reads the
// whole history from the cache entry Claude Code wrote a moment earlier.
// Cutting the history at the boundary instead cost full price twice live:
// the first run read 28k of 532k from cache ($2.10), and a breakpoint at the
// cut found no entry within the API's 20-block lookback and wrote 577k to
// cache at 1.25x ($2.96). The messages from the boundary on stay in the
// request, so the instruction names where the summary must stop.
func withSummaryInstruction(history []json.RawMessage, cut int) []json.RawMessage {
	// The whole request is summarised: the work carries on from the reply
	// to its last message, which must not be written here.
	text := "Stop here and do not answer or continue the conversation. Everything above is being replaced by your summary, and the work then carries on from the reply to the last message above, which is kept. " +
		"So cover the conversation right up to and including that last message: what it asks for or reports, and exactly what was in progress. " +
		compactionSummaryPrompt
	if ex := promptExcerpt(history[min(cut, len(history)-1)]); cut < len(history) && ex != "" {
		text = "Stop reading here and do not answer or continue the conversation. " +
			"Only the part of this conversation BEFORE the user message that begins \"" + ex +
			"\" is being replaced; that message and everything after it is kept word for word, so leave it out of the summary. " +
			compactionSummaryPrompt
	}
	instr := map[string]any{"type": "text", "text": text}
	out := append([]json.RawMessage(nil), history...)
	role := "user"
	if n := len(out); n > 0 {
		var msg map[string]any
		if json.Unmarshal(out[n-1], &msg) == nil {
			switch msg["role"] {
			case "user":
				msg["content"] = append(contentBlocks(msg["content"]), instr)
				if b, err := json.Marshal(msg); err == nil {
					out[n-1] = b
					return out
				}
			case "system":
				// Claude Code often ends a turn with a mid-conversation
				// system message, which the API accepts only last or before
				// an assistant turn: a user message after it is a 400 (the
				// first live run of this code, 2026-09-29 22:26). Another
				// system message keeps it last and leaves the history, and
				// its cache entry, untouched.
				role = "system"
			}
		}
	}
	b, _ := json.Marshal(map[string]any{"role": role, "content": []any{instr}})
	return append(out, b)
}

// promptExcerpt returns the start of a message's first text that is not a
// <system-reminder>, for naming it in the summary instruction.
func promptExcerpt(m json.RawMessage) string {
	var msg map[string]any
	if json.Unmarshal(m, &msg) != nil {
		return ""
	}
	for _, b := range contentBlocks(msg["content"]) {
		bm, _ := b.(map[string]any)
		t, _ := bm["text"].(string)
		t = strings.TrimSpace(t)
		if bm["type"] != "text" || t == "" || strings.HasPrefix(t, "<system-reminder>") {
			continue
		}
		if r := []rune(t); len(r) > 120 {
			t = string(r[:120])
		}
		return t
	}
	return ""
}

// readSSEText collects a streamed Messages response's text, stop reason and
// usage.
// readSSEText also returns an error unless the stream ended properly with
// message_stop: an error event or a broken connection mid-summary leaves
// text that reads like a summary and is only the first part of one.
func readSSEText(r io.Reader) (text, stop string, tok tokenUsage, err error) {
	text, stop, tok, _, err = readSSEBlocks(r)
	return
}

// readSSEBlocks is readSSEText that also rebuilds the reply's content
// blocks (text, thinking with its signature, tool calls), so the reply can
// be sent back as an assistant message.
func readSSEBlocks(r io.Reader) (text, stop string, tok tokenUsage, blocks []any, err error) {
	var sb strings.Builder
	done := false
	byIndex := map[int]map[string]any{}
	var order []int
	partial := map[int]*strings.Builder{}
	finish := func() []any {
		out := make([]any, 0, len(order))
		for _, i := range order {
			b := byIndex[i]
			if b["type"] == "tool_use" {
				var input any = map[string]any{}
				if p := partial[i]; p != nil && p.Len() > 0 {
					_ = json.Unmarshal([]byte(p.String()), &input)
				}
				b["input"] = input
			}
			out = append(out, b)
		}
		return out
	}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 8*1024*1024)
	for sc.Scan() {
		line := sc.Text()
		parseSSEUsage(line, &tok)
		raw := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "data:"))
		if !strings.HasPrefix(strings.TrimSpace(line), "data:") || raw == "" {
			continue
		}
		var ev struct {
			Type  string `json:"type"`
			Index int    `json:"index"`
			Error struct {
				Type    string `json:"type"`
				Message string `json:"message"`
			} `json:"error"`
			Block map[string]any `json:"content_block"`
			Delta struct {
				Type       string `json:"type"`
				Text       string `json:"text"`
				Thinking   string `json:"thinking"`
				Signature  string `json:"signature"`
				Partial    string `json:"partial_json"`
				StopReason string `json:"stop_reason"`
			} `json:"delta"`
		}
		if json.Unmarshal([]byte(raw), &ev) != nil {
			continue
		}
		switch {
		case ev.Type == "content_block_start" && ev.Block != nil:
			if _, seen := byIndex[ev.Index]; !seen {
				order = append(order, ev.Index)
			}
			byIndex[ev.Index] = ev.Block
		case ev.Type == "content_block_delta":
			b := byIndex[ev.Index]
			if b == nil {
				b = map[string]any{"type": "text", "text": ""}
				byIndex[ev.Index] = b
				order = append(order, ev.Index)
			}
			switch ev.Delta.Type {
			case "text_delta":
				sb.WriteString(ev.Delta.Text)
				t, _ := b["text"].(string)
				b["text"] = t + ev.Delta.Text
			case "thinking_delta":
				t, _ := b["thinking"].(string)
				b["thinking"] = t + ev.Delta.Thinking
			case "signature_delta":
				t, _ := b["signature"].(string)
				b["signature"] = t + ev.Delta.Signature
			case "input_json_delta":
				if partial[ev.Index] == nil {
					partial[ev.Index] = &strings.Builder{}
				}
				partial[ev.Index].WriteString(ev.Delta.Partial)
			}
		case ev.Type == "message_delta" && ev.Delta.StopReason != "":
			stop = ev.Delta.StopReason
		case ev.Type == "message_stop":
			done = true
		case ev.Type == "error":
			return sb.String(), stop, tok, nil, fmt.Errorf("the stream ended with an error: %s %s", ev.Error.Type, ev.Error.Message)
		}
	}
	if err := sc.Err(); err != nil {
		return sb.String(), stop, tok, nil, fmt.Errorf("the stream broke off: %w", err)
	}
	if !done {
		return sb.String(), stop, tok, nil, errors.New("the stream ended before the summary was finished")
	}
	return sb.String(), stop, tok, finish(), nil
}
