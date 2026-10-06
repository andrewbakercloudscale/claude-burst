package router

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/andrewbakercloudscale/claude-burst/internal/autocompact"
	"github.com/andrewbakercloudscale/claude-burst/internal/config"
	"github.com/andrewbakercloudscale/claude-burst/internal/metrics"
)

func msgs(t *testing.T, raw string) []json.RawMessage {
	t.Helper()
	var out []json.RawMessage
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// A session as Claude Code sends it: CLAUDE.md and other context ride in
// <system-reminder> blocks of the FIRST user message, then prompts and tool
// rounds alternate.
const session = `[
 {"role":"user","content":[{"type":"text","text":"<system-reminder>CLAUDE.md: never push without asking</system-reminder>"},{"type":"text","text":"first task"}]},
 {"role":"assistant","content":[{"type":"thinking","thinking":"","signature":"s1"},{"type":"tool_use","id":"t1","name":"Read","input":{}}]},
 {"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"old file"}]},
 {"role":"assistant","content":[{"type":"text","text":"done with first"}]},
 {"role":"user","content":[{"type":"text","text":"second task"}]},
 {"role":"assistant","content":[{"type":"thinking","thinking":"","signature":"s2"},{"type":"tool_use","id":"t2","name":"Bash","input":{}}]},
 {"role":"user","content":[{"type":"tool_result","tool_use_id":"t2","content":"output"}]},
 {"role":"assistant","content":[{"type":"thinking","thinking":"","signature":"s3"},{"type":"text","text":"done with second"}]},
 {"role":"user","content":[{"type":"text","text":"third task"}]}
]`

// Boundaries are plain user prompts only: a user message carrying a
// tool_result answers the assistant turn before it, and cutting there would
// leave a result whose call was summarised away.
func TestPromptBoundaries(t *testing.T) {
	got := promptBoundaries(msgs(t, session))
	want := []int{0, 4, 8}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

func TestRewriteReplacesThePrefixWithTheSummary(t *testing.T) {
	m := msgs(t, session)
	// Summary covers messages 0..3 (p0 = 4, the "second task" prompt); the
	// swap is first applied on the request ending at message 8.
	out := rewriteWithSummary(m, "SUMMARY OF FIRST TASK", 4, 9)
	if len(out) != 5 {
		t.Fatalf("want 5 messages (4..8), got %d", len(out))
	}
	first := string(out[0])
	for _, want := range []string{"SUMMARY OF FIRST TASK", "never push without asking", "second task"} {
		if !strings.Contains(first, want) {
			t.Fatalf("first message lacks %q:\n%s", want, first)
		}
	}
	if strings.Contains(first, "first task") || strings.Contains(strings.Join(rawStrings(out), ""), "old file") {
		t.Fatal("summarised messages must not be replayed")
	}
	// Thinking blocks in retained turns were produced with the old history
	// in place; the API rejects (or drops) them after the swap.
	for i, r := range out {
		if strings.Contains(string(r), `"thinking"`) {
			t.Fatalf("message %d still carries a thinking block from before the swap: %s", i, r)
		}
	}
	if !strings.Contains(string(out[1]), `"tool_use"`) || !strings.Contains(string(out[3]), "done with second") {
		t.Fatal("retained turns keep their text and tool calls")
	}
}

// Turns added AFTER the swap were produced against the compacted history,
// so their thinking blocks are valid and must be kept.
func TestRewriteKeepsThinkingProducedAfterTheSwap(t *testing.T) {
	m := msgs(t, session)
	out := rewriteWithSummary(m, "S", 4, 7) // swapped when the request ended at message 6
	if strings.Contains(string(out[1]), `"thinking"`) {
		t.Fatal("message 5 predates the swap: its thinking must be stripped")
	}
	if !strings.Contains(string(out[3]), `"signature":"s3"`) {
		t.Fatalf("message 7 came after the swap: its thinking must be kept, got %s", out[3])
	}
}

func TestPrefixHashDetectsAChangedHistory(t *testing.T) {
	a := msgs(t, session)
	b := msgs(t, strings.Replace(session, "first task", "FIRST TASK", 1))
	if prefixHash(a, 4) != prefixHash(msgs(t, session), 4) {
		t.Fatal("the same history must hash the same")
	}
	if prefixHash(a, 4) == prefixHash(b, 4) {
		t.Fatal("an edited history (/clear, rewind, Claude Code's own compaction) must not match")
	}
}

func TestPrefixHashIgnoresWhatClaudeCodeRewrites(t *testing.T) {
	sent := []json.RawMessage{
		json.RawMessage(`{"role":"user","content":"go"}`),
		json.RawMessage(`{"role":"assistant","content":[{"type":"thinking","thinking":"hm","signature":"s"},{"type":"text","text":"done","cache_control":{"type":"ephemeral"}}]}`),
	}
	later := []json.RawMessage{
		sent[0],
		json.RawMessage(`{"role":"assistant","content":[{"type":"text","text":"done"}]}`),
	}
	if prefixHash(sent, 2) != prefixHash(later, 2) {
		t.Fatal("a moved cache_control or cleared thinking must not drop a summary")
	}
	edited := []json.RawMessage{sent[0], json.RawMessage(`{"role":"assistant","content":[{"type":"text","text":"DONE"}]}`)}
	if prefixHash(later, 2) == prefixHash(edited, 2) {
		t.Fatal("changed text must still count as a changed history")
	}
}

func TestSummaryFromText(t *testing.T) {
	if got := summaryFromText("preamble <summary>\nthe gist\n</summary> trailer"); got != "the gist" {
		t.Fatalf("got %q", got)
	}
	if got := summaryFromText("no tags, just text"); got != "no tags, just text" {
		t.Fatalf("untagged text is the summary: got %q", got)
	}
}

func rawStrings(r []json.RawMessage) []string {
	out := make([]string, len(r))
	for i := range r {
		out[i] = string(r[i])
	}
	return out
}

// fakeAnthropic answers /v1/messages with SSE. Ordinary turns report a
// large cached context; a summary request (recognised by the summarisation
// prompt) answers with a summary. It records every body it was sent.
type fakeAnthropic struct {
	mu        sync.Mutex
	bodies    []string
	summaries int
	context   int64
	// rejectMidTurn answers 400 to a summarised request that still carries
	// the running turn's thinking (signature s3): an API that refuses a
	// mid-turn swap.
	rejectMidTurn bool
	// toolFirst answers the first summary request with a tool call.
	toolFirst bool
}

func (f *fakeAnthropic) handler(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	f.bodies = append(f.bodies, string(b))
	isSummary := strings.Contains(string(b), "Summarize the transcript inside")
	if isSummary {
		f.summaries++
	}
	ctx := f.context
	n := f.summaries
	reject := f.rejectMidTurn && !isSummary && strings.Contains(string(b), "THE GIST") && strings.Contains(string(b), `"signature":"s3"`)
	f.mu.Unlock()
	if reject {
		w.Header().Set("content-type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"type":"error","error":{"type":"invalid_request_error","message":"messages.1.content.0: invalid thinking signature"}}`)
		return
	}
	if f.toolFirst && isSummary && !strings.Contains(string(b), "Not run: tools are unavailable") {
		w.Header().Set("content-type", "text/event-stream")
		fmt.Fprint(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":5,\"cache_read_input_tokens\":1000,\"output_tokens\":1}}}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"thinking\",\"thinking\":\"\",\"signature\":\"\"}}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"signature_delta\",\"signature\":\"sigX\"}}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"content_block_start\",\"index\":1,\"content_block\":{\"type\":\"tool_use\",\"id\":\"tu9\",\"name\":\"Bash\",\"input\":{}}}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"content_block_delta\",\"index\":1,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"command\\\":\"}}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"content_block_delta\",\"index\":1,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"\\\"ls\\\"}\"}}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\"},\"usage\":{\"output_tokens\":20}}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"message_stop\"}\n\n")
		return
	}
	w.Header().Set("content-type", "text/event-stream")
	text := "ok"
	if isSummary {
		text = fmt.Sprintf("<summary>THE GIST OF THE FIRST TASK #%d</summary>", n)
		ctx = 1000
	}
	fmt.Fprintf(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":5,\"cache_read_input_tokens\":%d,\"output_tokens\":1}}}\n\n", ctx)
	fmt.Fprintf(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":%q}}\n\n", text)
	fmt.Fprint(w, "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":20}}\n\n")
	fmt.Fprint(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
}

func (f *fakeAnthropic) last() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := len(f.bodies) - 1; i >= 0; i-- {
		if !strings.Contains(f.bodies[i], "Summarize the transcript inside") {
			return f.bodies[i]
		}
	}
	return ""
}

func (f *fakeAnthropic) summaryCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.summaries
}

func compactServer(t *testing.T, f *fakeAnthropic, c config.CompactionConfig) *Server {
	t.Helper()
	up := httptest.NewServer(http.HandlerFunc(f.handler))
	t.Cleanup(up.Close)
	cfg := config.Default()
	cfg.AnthropicBaseURL = up.URL
	cfg.PrimaryCompaction = c
	dir := t.TempDir()
	s, err := New(cfg, filepath.Join(dir, "state.json"), filepath.Join(dir, "metrics.jsonl"), log.New(testLogWriter{t}, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	// A summary still saving state when the test ends would race the
	// TempDir cleanup ("directory not empty"). Cleanups run last-in first.
	t.Cleanup(s.compaction.running.Wait)
	return s
}

// testLogWriter sends the gateway's log to t.Log, so a failing compaction
// test shows what the gateway decided.
type testLogWriter struct{ t *testing.T }

func (w testLogWriter) Write(p []byte) (int, error) {
	w.t.Log(strings.TrimSpace(string(p)))
	return len(p), nil
}

// send posts a turn for session sid whose history is the first n messages
// of `session` (n=9 is the whole thing, ending in a plain prompt).
func send(t *testing.T, s *Server, sid string, history []json.RawMessage) {
	t.Helper()
	b, _ := json.Marshal(map[string]any{"model": "claude-opus-5-5", "stream": true, "max_tokens": 100,
		"system": "sys", "tools": []any{}, "messages": history})
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/v1/messages", bytes.NewReader(b))
	req.Header.Set("x-claude-code-session-id", sid)
	req.Header.Set("authorization", "Bearer oauth")
	s.ServeHTTP(httptest.NewRecorder(), req)
}

// waitFor polls for something a turn starts. To check that a turn started
// nothing, call compaction.running.Wait() instead: a summary is counted there
// on the request path, before send returns, so Wait has seen every summary
// that turn started.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatal("timed out waiting")
}

func TestCompactionEndToEnd(t *testing.T) {
	f := &fakeAnthropic{context: 450_000}
	s := compactServer(t, f, config.CompactionConfig{Enabled: true, CompactAtTokens: 400_000, WarnAtPercent: 75, WindowMinutes: 60})
	all := msgs(t, session)

	// Turn 1 (history up to the "second task" prompt): the response reports
	// 450k of context. Nothing is rewritten yet.
	send(t, s, "S", all[:5])
	if strings.Contains(f.last(), "THE GIST") {
		t.Fatal("nothing may be rewritten before a summary exists")
	}
	// Turn 2: over the threshold, so a summary of everything before the
	// latest plain prompt starts in the background. This turn goes as is.
	send(t, s, "S", all[:7])
	waitFor(t, func() bool { return f.summaryCount() == 1 })
	if !strings.Contains(f.last(), "first task") {
		t.Fatal("the triggering turn itself must go unmodified")
	}
	// Turn 3 ends in a plain prompt: the swap applies.
	waitFor(t, func() bool { return s.compactionReady("S") })
	send(t, s, "S", all[:9])
	got := f.last()
	if !strings.Contains(got, "THE GIST OF THE FIRST TASK") || !strings.Contains(got, "never push without asking") {
		t.Fatalf("turn 3 must carry the summary and the CLAUDE.md reminder:\n%s", got)
	}
	if strings.Contains(got, "old file") || strings.Contains(got, `"signature":"s2"`) {
		t.Fatalf("summarised messages and pre-swap thinking must be gone:\n%s", got)
	}
	// Still over the threshold, but inside the window: no second summary.
	send(t, s, "S", all[:9])
	s.compaction.running.Wait()
	if n := f.summaryCount(); n != 1 {
		t.Fatalf("one compaction per window, got %d summaries", n)
	}
	// The user ran /clear (history no longer matches): pass through untouched.
	send(t, s, "S", msgs(t, `[{"role":"user","content":"a fresh start"}]`))
	if strings.Contains(f.last(), "THE GIST") {
		t.Fatal("a changed history must drop the swap")
	}
}

// A fork of a session (claude --resume --fork-session, the handover writer)
// has a new session id and the same history. Its first request must go with
// the summary the first session holds, not whole.
func TestAForkTakesTheSummaryOfTheSessionItWasForkedFrom(t *testing.T) {
	f := &fakeAnthropic{context: 450_000}
	s := compactServer(t, f, config.CompactionConfig{Enabled: true, CompactAtTokens: 400_000, WarnAtPercent: 75, WindowMinutes: 60})
	all := msgs(t, session)
	send(t, s, "S", all[:5])
	send(t, s, "S", all[:7])
	waitFor(t, func() bool { return f.summaryCount() == 1 })
	waitFor(t, func() bool { return s.compactionReady("S") })
	send(t, s, "S", all[:9])
	parent := f.last()
	if !strings.Contains(parent, "THE GIST OF THE FIRST TASK") {
		t.Fatalf("the first session must be compacted:\n%s", parent)
	}

	send(t, s, "FORK", all[:9])
	if got := f.last(); got != parent {
		t.Fatalf("the fork's first request must be the one the first session sent, summary and all:\n%s\nwant:\n%s", got, parent)
	}
	s.compaction.running.Wait()
	if n := f.summaryCount(); n != 1 {
		t.Fatalf("the fork must not pay for a summary of its own, got %d summaries", n)
	}

	// Another conversation under a new session id takes nothing.
	send(t, s, "OTHER", msgs(t, `[{"role":"user","content":"a fresh start"},{"role":"assistant","content":"ok"},{"role":"user","content":"go on"}]`))
	if strings.Contains(f.last(), "THE GIST") {
		t.Fatal("a different conversation must not be given the summary")
	}
	// A fork whose history has parted from the summarised one takes nothing.
	changed := append([]json.RawMessage(nil), all[:9]...)
	changed[1] = json.RawMessage(`{"role":"assistant","content":"something else was said"}`)
	send(t, s, "PARTED", changed)
	if strings.Contains(f.last(), "THE GIST") {
		t.Fatal("a history the summary was not made from must go as it is")
	}
	// A summary dropped on request stays dropped: it is not taken again.
	if s.DropSummary("FORK") != 1 {
		t.Fatal("the fork holds a summary to drop")
	}
	send(t, s, "FORK", all[:9])
	if strings.Contains(f.last(), "THE GIST") {
		t.Fatal("a dropped summary must not come back from the first session")
	}
}

func TestCompactionIsOffByDefault(t *testing.T) {
	if config.Default().PrimaryCompaction.Enabled {
		t.Fatal("experimental: must be off unless switched on")
	}
	f := &fakeAnthropic{context: 900_000}
	s := compactServer(t, f, config.Default().PrimaryCompaction)
	all := msgs(t, session)
	send(t, s, "S", all[:5])
	send(t, s, "S", all[:7])
	s.compaction.running.Wait()
	if f.summaryCount() != 0 {
		t.Fatal("no summary requests while disabled")
	}
}

// A summary the model did not finish (a tool call, max_tokens, nothing)
// must never replace real history.
func TestCompactionFailedSummaryNeverSwaps(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		w.Header().Set("content-type", "text/event-stream")
		fmt.Fprint(w, "data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":5,\"cache_read_input_tokens\":450000}}}\n\n")
		if strings.Contains(string(b), "Summarize the transcript inside") {
			fmt.Fprint(w, "data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\"}}\n\n")
			return
		}
		fmt.Fprint(w, "data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"}}\n\n")
	}))
	defer up.Close()
	cfg := config.Default()
	cfg.AnthropicBaseURL = up.URL
	cfg.PrimaryCompaction = config.CompactionConfig{Enabled: true}
	dir := t.TempDir()
	s, err := New(cfg, filepath.Join(dir, "state.json"), filepath.Join(dir, "metrics.jsonl"), log.New(testLogWriter{t}, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	all := msgs(t, session)
	send(t, s, "S", all[:5])
	send(t, s, "S", all[:7])
	waitFor(t, func() bool {
		s.compaction.mu.Lock()
		defer s.compaction.mu.Unlock()
		st := stateFor(s, "S")
		return st != nil && !st.startedAt.IsZero() && !st.pending
	})
	if s.compactionReady("S") {
		t.Fatal("a summary that ended in a tool call must not be used")
	}
	// A failure retries after retryAfterFailure, not after the whole window.
	s.compaction.mu.Lock()
	next := stateFor(s, "S").startedAt.Add(time.Duration(s.compaction.cfg.WindowMinutes) * time.Minute)
	s.compaction.mu.Unlock()
	if d := time.Until(next); d > retryAfterFailure+time.Second || d < retryAfterFailure-time.Minute {
		t.Fatalf("next attempt in %s, want about %s", d, retryAfterFailure)
	}
}

func TestCompactionRecordsWhatTheSwapRemoved(t *testing.T) {
	f := &fakeAnthropic{context: 450_000}
	up := httptest.NewServer(http.HandlerFunc(f.handler))
	defer up.Close()
	cfg := config.Default()
	cfg.AnthropicBaseURL = up.URL
	cfg.PrimaryCompaction = config.CompactionConfig{Enabled: true}
	dir := t.TempDir()
	metricsPath := filepath.Join(dir, "metrics.jsonl")
	s, err := New(cfg, filepath.Join(dir, "state.json"), metricsPath, log.New(testLogWriter{t}, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	all := msgs(t, session)
	send(t, s, "S", all[:5])
	send(t, s, "S", all[:7])
	waitFor(t, func() bool { return s.compactionReady("S") })
	send(t, s, "S", all[:9])
	b, _ := os.ReadFile(metricsPath)
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	var e metrics.Event
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &e); err != nil {
		t.Fatal(err)
	}
	if e.CompactedMessages != 6 || e.CompactedBytes <= 0 {
		t.Fatalf("the compacted request's metrics row must say what the swap removed: %+v", e)
	}
	var sawSummary bool
	for _, l := range lines {
		sawSummary = sawSummary || strings.Contains(l, `"note":"compaction summary"`)
	}
	if !sawSummary {
		t.Fatal("the summary call must have its own metrics row")
	}
}

// A session that is one long prompt has no boundary to cut at, so the
// first crossing is skipped. That skip must not start the window: the
// user's next prompt creates a boundary, and compaction should happen
// then, not an hour later. (Found by the live test on 2026-09-29.)
func TestCompactionSkipDoesNotConsumeTheWindow(t *testing.T) {
	f := &fakeAnthropic{context: 450_000}
	s := compactServer(t, f, config.CompactionConfig{Enabled: true})
	one := msgs(t, `[
 {"role":"user","content":"one long task"},
 {"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"Read","input":{}}]},
 {"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"`+strings.Repeat("x", 5000)+`"}]}]`)
	send(t, s, "S", one[:1])
	send(t, s, "S", one) // over threshold, no boundary: skipped
	s.compaction.running.Wait()
	if f.summaryCount() != 0 {
		t.Fatal("nothing to summarise yet")
	}
	next := append(append([]json.RawMessage{}, one...),
		json.RawMessage(`{"role":"assistant","content":[{"type":"text","text":"done"}]}`),
		json.RawMessage(`{"role":"user","content":"second prompt"}`))
	send(t, s, "S", next)
	waitFor(t, func() bool { return f.summaryCount() == 1 })
}

// Claude Code appends a mid-conversation system message after the user's
// prompt, so a fresh turn often ENDS in system[text]. That is still a
// plain-prompt turn and the swap must apply to it. (Found by the live test:
// a ready summary sat unapplied on "request ends in system[text]".)
func TestEndsInPromptIgnoresTrailingSystemMessages(t *testing.T) {
	m := msgs(t, `[
 {"role":"user","content":"task"},
 {"role":"assistant","content":[{"type":"text","text":"ok"}]},
 {"role":"user","content":[{"type":"text","text":"next prompt"}]},
 {"role":"system","content":[{"type":"text","text":"reminder"}]}]`)
	if !endsInPrompt(m) {
		t.Fatal("a prompt followed only by system messages is a fresh turn")
	}
	tool := msgs(t, `[
 {"role":"user","content":"task"},
 {"role":"assistant","content":[{"type":"tool_use","id":"t","name":"Read","input":{}}]},
 {"role":"user","content":[{"type":"tool_result","tool_use_id":"t","content":"x"}]},
 {"role":"system","content":[{"type":"text","text":"reminder"}]}]`)
	if endsInPrompt(tool) {
		t.Fatal("a tool round followed by a system message is still mid tool round")
	}
}

func (f *fakeAnthropic) summaryBody() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, b := range f.bodies {
		if strings.Contains(b, "Summarize the transcript inside") {
			return b
		}
	}
	return ""
}

// The summary call must send the history exactly as Claude Code last sent
// it, or it pays full input price (or a 1.25x cache write) for a context
// that is already in cache: live runs cost $2.10 and $2.96 that way.
func TestCompactionSummaryReusesTheCachedHistory(t *testing.T) {
	f := &fakeAnthropic{context: 450_000}
	s := compactServer(t, f, config.CompactionConfig{Enabled: true, CompactAtTokens: 400_000, WarnAtPercent: 75, WindowMinutes: 60})
	all := msgs(t, session)
	send(t, s, "S", all[:5])
	send(t, s, "S", all[:7])
	waitFor(t, func() bool { return f.summaryCount() == 1 })

	var req struct {
		Messages []json.RawMessage `json:"messages"`
	}
	if err := json.Unmarshal([]byte(f.summaryBody()), &req); err != nil {
		t.Fatal(err)
	}
	if len(req.Messages) != 7 {
		t.Fatalf("want the 7 messages of the triggering request, got %d:\n%s", len(req.Messages), f.summaryBody())
	}
	for i := 0; i < 6; i++ {
		if !jsonEqual(t, req.Messages[i], all[i]) {
			t.Fatalf("message %d changed:\n got %s\nwant %s", i, req.Messages[i], all[i])
		}
	}
	last := string(req.Messages[6])
	if !strings.Contains(last, "Summarize the transcript inside") || !strings.Contains(last, "right up to and including that last message") {
		t.Fatalf("last message must end with the bounded instruction: %s", last)
	}
	if strings.Count(f.summaryBody(), "cache_control") != strings.Count(string(mustJSON(t, all[:7])), "cache_control") {
		t.Fatalf("the summary call must add no breakpoints of its own")
	}
}

func TestSummaryInstructionAfterAnAssistantTurn(t *testing.T) {
	h := msgs(t, `[{"role":"user","content":"do it"},{"role":"assistant","content":[{"type":"text","text":"done"}]}]`)
	out := withSummaryInstruction(h, 0)
	if len(out) != 3 || !strings.Contains(string(out[2]), `"role":"user"`) || !strings.Contains(string(out[2]), "do it") {
		t.Fatalf("want a new user message naming the boundary: %s", out)
	}
}

// A history ending in a mid-conversation system message must stay valid:
// a user message after it is rejected by the API.
func TestSummaryInstructionAfterATrailingSystemMessage(t *testing.T) {
	h := msgs(t, `[{"role":"user","content":"do it"},{"role":"assistant","content":[{"type":"text","text":"done"}]},
 {"role":"user","content":"next"},{"role":"system","content":[{"type":"text","text":"reminder"}]}]`)
	out := withSummaryInstruction(h, 2)
	if len(out) != 5 || !strings.Contains(string(out[4]), `"role":"system"`) || !strings.Contains(string(out[4]), "Summarize the transcript") {
		t.Fatalf("want the instruction as a final system message: %s", out)
	}
	for i := 0; i < 4; i++ {
		if string(out[i]) != string(h[i]) {
			t.Fatalf("message %d changed", i)
		}
	}
}

func jsonEqual(t *testing.T, a, b json.RawMessage) bool {
	t.Helper()
	var x, y any
	if json.Unmarshal(a, &x) != nil || json.Unmarshal(b, &y) != nil {
		t.Fatal("bad json")
	}
	return string(mustJSON(t, x)) == string(mustJSON(t, y))
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// A deploy must not throw away a summary that cost a full context read.
func TestCompactionStateSurvivesARestart(t *testing.T) {
	f := &fakeAnthropic{context: 450_000}
	up := httptest.NewServer(http.HandlerFunc(f.handler))
	t.Cleanup(up.Close)
	cfg := config.Default()
	cfg.AnthropicBaseURL = up.URL
	cfg.PrimaryCompaction = config.CompactionConfig{Enabled: true, CompactAtTokens: 400_000, WarnAtPercent: 75, WindowMinutes: 60}
	dir := t.TempDir()
	start := func() *Server {
		s, err := New(cfg, filepath.Join(dir, "state.json"), filepath.Join(dir, "metrics.jsonl"), log.New(testLogWriter{t}, "", 0))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(s.compaction.running.Wait)
		return s
	}
	all := msgs(t, session)
	s := start()
	send(t, s, "S", all[:5])
	send(t, s, "S", all[:7])
	waitFor(t, func() bool { return s.compactionReady("S") })

	s2 := start()
	if !s2.compactionReady("S") {
		t.Fatal("the ready summary was lost across a restart")
	}
	send(t, s2, "S", all[:9])
	if !strings.Contains(f.last(), "THE GIST OF THE FIRST TASK") {
		t.Fatalf("the restarted gateway must apply the saved summary:\n%s", f.last())
	}
	s2.compaction.running.Wait()
	if n := f.summaryCount(); n != 1 {
		t.Fatalf("the window must survive a restart too; got %d summaries", n)
	}
}

// A summary in flight when the gateway stopped will never arrive: the
// restarted gateway must be free to start another at once.
func TestCompactionPendingAtRestartReopensTheWindow(t *testing.T) {
	path := filepath.Join(t.TempDir(), "compaction-state.json")
	os.WriteFile(path, []byte(`{"S|m":{"last_context":450000,"started_at":"`+time.Now().Format(time.RFC3339)+`","pending":true,"seen":"`+time.Now().Format(time.RFC3339)+`"}}`), 0600)
	c := newCompactor(config.CompactionConfig{}, path, nil)
	st := c.sessions["S|m"]
	if st == nil || !st.startedAt.IsZero() || st.pending || st.lastContext != 450000 {
		t.Fatalf("got %+v", st)
	}
}

// A second summary that becomes ready in the middle of a tool loop waits for
// the next plain prompt, and until then the first one stays in force. On
// 2026-09-30 the first was dropped as soon as the second was ready, and for
// five minutes a live session sent its whole uncompacted history: 936k
// tokens a turn, starting with a 907k cache write.
func TestSecondSummaryWaitsWithoutDroppingTheFirst(t *testing.T) {
	f := &fakeAnthropic{context: 450_000}
	s := compactServer(t, f, config.CompactionConfig{Enabled: true, CompactAtTokens: 400_000, WarnAtPercent: 75, WindowMinutes: 60})
	all := msgs(t, strings.TrimSuffix(session, "]")+`,
 {"role":"assistant","content":[{"type":"tool_use","id":"t3","name":"Read","input":{}}]},
 {"role":"user","content":[{"type":"tool_result","tool_use_id":"t3","content":"third output"}]},
 {"role":"assistant","content":[{"type":"text","text":"done with third"}]},
 {"role":"user","content":[{"type":"text","text":"fourth task"}]},
 {"role":"assistant","content":[{"type":"tool_use","id":"t4","name":"Bash","input":{}}]},
 {"role":"user","content":[{"type":"tool_result","tool_use_id":"t4","content":"fourth output"}]},
 {"role":"assistant","content":[{"type":"tool_use","id":"t5","name":"Bash","input":{}}]},
 {"role":"user","content":[{"type":"tool_result","tool_use_id":"t5","content":"fifth output"}]},
 {"role":"assistant","content":[{"type":"text","text":"done with fourth"}]},
 {"role":"user","content":[{"type":"text","text":"fifth task"}]}
]`)
	send(t, s, "S", all[:5])
	send(t, s, "S", all[:7])
	waitFor(t, func() bool { return s.compactionReady("S") })
	send(t, s, "S", all[:9])
	if !strings.Contains(f.last(), "TASK #1") {
		t.Fatal("the first summary must apply at the third task")
	}

	// Reopen the window: the next prompt starts a second summary.
	s.compaction.mu.Lock()
	for _, st := range s.compaction.sessions {
		st.startedAt = time.Time{}
	}
	s.compaction.mu.Unlock()
	send(t, s, "S", all[:13])
	waitFor(t, func() bool {
		s.compaction.mu.Lock()
		defer s.compaction.mu.Unlock()
		for _, st := range s.compaction.sessions {
			if st.next != "" {
				return true
			}
		}
		return false
	})

	// Mid tool loop: the second waits, and the first is still applied.
	for _, n := range []int{15, 17} {
		send(t, s, "S", all[:n])
		got := f.last()
		if !strings.Contains(got, "TASK #1") || strings.Contains(got, "old file") {
			t.Fatalf("turn of %d messages went out without the first summary:\n%s", n, got)
		}
	}
	// The next plain prompt swaps to the second.
	send(t, s, "S", all[:19])
	if got := f.last(); !strings.Contains(got, "TASK #2") || strings.Contains(got, "TASK #1") {
		t.Fatalf("the plain prompt must carry the second summary alone:\n%s", got)
	}
}

// When Claude Code changes an early message, the summary no longer fits and
// is dropped. The log must say which message changed, and the window must
// reopen: on 2026-09-30 a drop left the session on its full history for the
// rest of the hour.
func TestDroppedSummaryNamesTheChangeAndReopensTheWindow(t *testing.T) {
	f := &fakeAnthropic{context: 450_000}
	var logBuf strings.Builder
	s := compactServer(t, f, config.CompactionConfig{Enabled: true, CompactAtTokens: 400_000, WarnAtPercent: 75, WindowMinutes: 60})
	s.logger.SetOutput(io.MultiWriter(testLogWriter{t}, &logBuf))
	all := msgs(t, session)
	send(t, s, "S", all[:5])
	send(t, s, "S", all[:7])
	waitFor(t, func() bool { return s.compactionReady("S") })
	send(t, s, "S", all[:9])
	if !strings.Contains(f.last(), "THE GIST") {
		t.Fatal("the summary must apply first")
	}

	changed := append([]json.RawMessage(nil), all[:9]...)
	changed[2] = json.RawMessage(`{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"[cleared]"}]}`)
	n := f.summaryCount()
	send(t, s, "S", changed)
	if strings.Contains(f.last(), "THE GIST") {
		t.Fatal("a changed history must go without the summary")
	}
	if !strings.Contains(logBuf.String(), "message 2 of 7 changed") {
		t.Fatalf("the log must name the changed message:\n%s", logBuf.String())
	}
	// Still over the threshold: a new summary starts on that same request,
	// not an hour later.
	waitFor(t, func() bool { return f.summaryCount() > n })
}

func TestPromptNoticesFollowACompaction(t *testing.T) {
	f := &fakeAnthropic{context: 450_000}
	s := compactServer(t, f, config.CompactionConfig{Enabled: true})
	all := msgs(t, session)
	send(t, s, "S", all[:5])
	send(t, s, "S", all[:7])
	waitFor(t, func() bool { return s.compactionReady("S") })

	got := strings.Join(s.PromptNotices("S", false), "\n")
	if !strings.Contains(got, "450k context: summarising 7 messages") {
		t.Fatalf("want the start line, got:\n%s", got)
	}
	// Ready gets no line of its own: "done" follows on the next request.
	if strings.Contains(got, "summary is ready") {
		t.Fatalf("the ready line repeats the done line, got:\n%s", got)
	}
	if again := s.PromptNotices("S", false); len(again) != 0 {
		t.Fatalf("each line is shown once, got %q", again)
	}
	if other := s.PromptNotices("T", false); len(other) != 0 {
		t.Fatalf("another session sees nothing, got %q", other)
	}

	f.mu.Lock()
	f.context = 60_000
	f.mu.Unlock()
	send(t, s, "S", all[:9])
	got = strings.Join(s.PromptNotices("S", false), "\n")
	if !strings.Contains(got, "Burst compaction: done, 87% smaller: 450k → 60k (7 messages summarised)") {
		t.Fatalf("want the result of the swap, got:\n%s", got)
	}
	// Claude Code still holds the whole history: the session shows it, larger
	// than what Burst sends, for the band and the dashboard.
	if cs := s.CompactionSessions(); len(cs) == 0 || cs[0].Raw <= cs[0].Context*11/10 {
		t.Fatalf("want Raw above the 60k sent, got %+v", cs)
	}

	// Turned off: nothing, and nothing queued is shown later either.
	s.SetCompaction(config.CompactionConfig{Enabled: true, NoPromptNotice: true})
	send(t, s, "S", msgs(t, `[{"role":"user","content":"a fresh start"}]`))
	if got := s.PromptNotices("S", false); got != nil {
		t.Fatalf("notices off, got %q", got)
	}
}

// Inside a long turn the summary cannot swap in. The hook after a tool call
// says so once, and the next prompt adds nothing until the swap is done.
func TestPromptNoticesMidTurnSayItWaitsForTheNextPrompt(t *testing.T) {
	f := &fakeAnthropic{context: 450_000}
	s := compactServer(t, f, config.CompactionConfig{Enabled: true})
	all := msgs(t, session)
	send(t, s, "S", all[:5])
	send(t, s, "S", all[:7])
	waitFor(t, func() bool { return s.compactionReady("S") })

	got := strings.Join(s.PromptNotices("S", true), "\n")
	if !strings.Contains(got, "summarising") || !strings.Contains(got, "summary ready (") {
		t.Fatalf("mid-turn: want the start and the waiting line, got:\n%s", got)
	}
	if strings.Contains(got, "swaps in with this message") {
		t.Fatalf("mid-turn must not claim this message swaps it in:\n%s", got)
	}
	if again := s.PromptNotices("S", true); len(again) != 0 {
		t.Fatalf("the waiting line is shown once per summary, got %q", again)
	}
	if got := s.PromptNotices("S", false); len(got) != 0 {
		t.Fatalf("the next prompt waits for the done line, got %q", got)
	}

	// After the swap nothing is waiting, so mid-turn says only what happened.
	f.mu.Lock()
	f.context = 60_000
	f.mu.Unlock()
	send(t, s, "S", all[:9])
	got = strings.Join(s.PromptNotices("S", true), "\n")
	if !strings.Contains(got, "done, ") || strings.Contains(got, "swaps in") {
		t.Fatalf("after the swap, want only the done line, got:\n%s", got)
	}
}

// stateFor is the compaction state of session sid's main conversation.
// Caller holds mu.
func stateFor(s *Server, sid string) *compactState {
	for k, st := range s.compaction.sessions {
		if strings.HasPrefix(k, sid+"|claude-opus-5-5|") {
			return st
		}
	}
	return nil
}

// A subagent runs under its parent's session id and model with a short
// history of its own. It must not drop the parent's summary.
func TestSubagentDoesNotDropTheParentsSummary(t *testing.T) {
	f := &fakeAnthropic{context: 450_000}
	s := compactServer(t, f, config.CompactionConfig{Enabled: true})
	all := msgs(t, session)
	send(t, s, "S", all[:5])
	send(t, s, "S", all[:7])
	waitFor(t, func() bool { return s.compactionReady("S") })
	send(t, s, "S", all[:9])
	if !strings.Contains(f.last(), "THE GIST") {
		t.Fatal("the summary must apply first")
	}
	send(t, s, "S", msgs(t, `[{"role":"user","content":"review the code, read only"}]`))
	if strings.Contains(f.last(), "THE GIST") {
		t.Fatal("the subagent's own request must go untouched")
	}
	send(t, s, "S", all[:9])
	if !strings.Contains(f.last(), "THE GIST") {
		t.Fatal("the parent must still carry its summary after a subagent request")
	}
}

// /compact-async: a prompt carrying the marker starts a summary at once,
// well below Compact at and inside the window, and it swaps in with the
// next plain prompt.
func TestCompactAsyncStartsASummaryNow(t *testing.T) {
	f := &fakeAnthropic{context: 50_000}
	s := compactServer(t, f, config.CompactionConfig{Enabled: true, CompactAtTokens: 400_000, WarnAtPercent: 75, WindowMinutes: 60})
	all := msgs(t, session)
	send(t, s, "S", all[:5])
	send(t, s, "S", all[:7])
	if f.summaryCount() != 0 {
		t.Fatal("50k is far below Compact at: no summary without the command")
	}
	cmd := json.RawMessage(`{"role":"user","content":[{"type":"text","text":"(` + CompactAsyncMarker + `) reply with one line"}]}`)
	withCmd := append(append([]json.RawMessage(nil), all[:8]...), cmd)
	send(t, s, "S", withCmd)
	waitFor(t, func() bool { return s.compactionReady("S") })
	if got := strings.Join(s.PromptNotices("S", false), "\n"); !strings.Contains(got, "/compact-async: summarising 9 messages") {
		t.Fatalf("want the /compact-async start line, got:\n%s", got)
	}

	// Asking again while one is ready starts nothing new and says so.
	n := f.summaryCount()
	send(t, s, "S", withCmd)
	if f.summaryCount() != n {
		t.Fatal("a second /compact-async must not start another summary")
	}
	if got := strings.Join(s.PromptNotices("S", false), "\n"); !strings.Contains(got, "summary ready, swaps in next prompt") {
		t.Fatalf("want already ready, got:\n%s", got)
	}

	// The next plain prompt carries the summary.
	next := append(append([]json.RawMessage(nil), withCmd...),
		json.RawMessage(`{"role":"assistant","content":[{"type":"text","text":"Pauseless compaction started"}]}`),
		json.RawMessage(`{"role":"user","content":[{"type":"text","text":"carry on"}]}`))
	send(t, s, "S", next)
	if !strings.Contains(f.last(), "THE GIST") {
		t.Fatalf("the next prompt must carry the summary:\n%s", f.last())
	}
}

// The marker counts only in a plain prompt: a tool result that happens to
// contain it (a file that mentions the command) starts nothing.
func TestCompactAsyncMarkerInAToolResultIsIgnored(t *testing.T) {
	f := &fakeAnthropic{context: 50_000}
	s := compactServer(t, f, config.CompactionConfig{Enabled: true, CompactAtTokens: 400_000, WarnAtPercent: 75, WindowMinutes: 60})
	all := msgs(t, session)
	send(t, s, "S", all[:5])
	h := append(append([]json.RawMessage(nil), all[:6]...),
		json.RawMessage(`{"role":"user","content":[{"type":"tool_result","tool_use_id":"t2","content":"grep hit: `+CompactAsyncMarker+`"}]}`))
	send(t, s, "S", h)
	s.compaction.running.Wait()
	if f.summaryCount() != 0 {
		t.Fatal("a tool result mentioning the marker must not start a summary")
	}
}

// sendCode is send, returning the status Claude Code would see.
func sendCode(t *testing.T, s *Server, sid string, history []json.RawMessage) int {
	t.Helper()
	b, _ := json.Marshal(map[string]any{"model": "claude-opus-5-5", "stream": true, "max_tokens": 100,
		"system": "sys", "tools": []any{}, "messages": history})
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/v1/messages", bytes.NewReader(b))
	req.Header.Set("x-claude-code-session-id", sid)
	req.Header.Set("authorization", "Bearer oauth")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	return rec.Code
}

// readyMidTurn brings session S to a ready summary of messages [0, 7) while
// the turn started by the "second task" prompt (message 4) is still running.
func readyMidTurn(t *testing.T, f *fakeAnthropic, midTurn bool) (*Server, []json.RawMessage) {
	t.Helper()
	s := compactServer(t, f, config.CompactionConfig{Enabled: true, MidTurn: midTurn})
	all := msgs(t, session)
	send(t, s, "S", all[:5])
	send(t, s, "S", all[:7])
	waitFor(t, func() bool { return s.compactionReady("S") })
	return s, all
}

// Off (the default), a ready summary still waits for the next plain prompt.
func TestMidTurnSwapIsOffByDefault(t *testing.T) {
	f := &fakeAnthropic{context: 450_000}
	s, all := readyMidTurn(t, f, false)
	send(t, s, "S", all[:8])
	if strings.Contains(f.last(), "THE GIST") {
		t.Fatal("with mid_turn off, a request inside a turn must not swap")
	}
}

// On, the next request swaps even inside a turn. The summary was written
// from the request of 7 messages, so the running turn is kept from the
// reply to it (message 7), thinking and all; the prompt that started the
// turn is carried word for word beside the summary.
func TestMidTurnSwapKeepsTheRunningTurn(t *testing.T) {
	f := &fakeAnthropic{context: 450_000}
	s, all := readyMidTurn(t, f, true)
	if code := sendCode(t, s, "S", all[:8]); code != http.StatusOK {
		t.Fatalf("status %d", code)
	}
	got := f.last()
	if !strings.Contains(got, "THE GIST OF THE FIRST TASK") {
		t.Fatalf("mid-turn request must carry the summary:\n%s", got)
	}
	if strings.Contains(got, "old file") || strings.Contains(got, `"signature":"s1"`) || strings.Contains(got, `"signature":"s2"`) || strings.Contains(got, `"content":"output"`) {
		t.Fatalf("everything the summary was written from must be gone:\n%s", got)
	}
	for _, want := range []string{"latest-user-message\\u003e\\nsecond task", `"signature":"s3"`, "done with second"} {
		if !strings.Contains(got, want) {
			t.Fatalf("the running turn must go as sent, missing %s:\n%s", want, got)
		}
	}
	s.compaction.mu.Lock()
	st := stateFor(s, "S")
	proven, off := !st.midTurnUnproven, s.compaction.midTurnOff
	s.compaction.mu.Unlock()
	if !proven || off {
		t.Fatalf("a normal answer proves the swap: proven=%v off=%v", proven, off)
	}
	// The next prompt keeps the swap; the turn that was running keeps its thinking.
	send(t, s, "S", all[:9])
	if got := f.last(); !strings.Contains(got, "THE GIST") || !strings.Contains(got, `"signature":"s3"`) {
		t.Fatalf("after the turn, the swap stays and so does that turn's thinking:\n%s", got)
	}
}

// THE RISK, contained: if the API refuses the swapped request, Claude Code
// never sees the 400. The request is resent as it would have been without
// the swap, the summary goes back to waiting for the next plain prompt, and
// mid-turn swaps stop until the settings are saved again.
func TestMidTurnSwapRejectedIsUndoneAndResent(t *testing.T) {
	f := &fakeAnthropic{context: 450_000, rejectMidTurn: true}
	s, all := readyMidTurn(t, f, true)
	if code := sendCode(t, s, "S", all[:8]); code != http.StatusOK {
		t.Fatalf("Claude Code must not see the rejection, got status %d", code)
	}
	if got := f.last(); strings.Contains(got, "THE GIST") || !strings.Contains(got, "old file") {
		t.Fatalf("the resend must be the request without the swap:\n%s", got)
	}
	s.compaction.mu.Lock()
	st := stateFor(s, "S")
	waiting, off, notes := st.next != "" && st.summary == "", s.compaction.midTurnOff, strings.Join(st.notices, "\n")
	s.compaction.mu.Unlock()
	if !waiting || !off {
		t.Fatalf("summary must wait again and mid-turn go off: waiting=%v off=%v", waiting, off)
	}
	if !strings.Contains(notes, "summary refused mid-turn") {
		t.Fatalf("the user is told, got:\n%s", notes)
	}
	// Later requests in the turn are not swapped again.
	send(t, s, "S", all[:8])
	if strings.Contains(f.last(), "THE GIST") {
		t.Fatal("after a rejection no more mid-turn swaps")
	}
	// The next plain prompt swaps the ordinary way: thinking before it stripped.
	if code := sendCode(t, s, "S", all[:9]); code != http.StatusOK {
		t.Fatalf("status %d", code)
	}
	if got := f.last(); !strings.Contains(got, "THE GIST") || strings.Contains(got, `"signature":"s3"`) {
		t.Fatalf("the next prompt swaps as before mid-turn existed:\n%s", got)
	}
	// Saving the settings again re-arms it.
	s.SetCompaction(config.CompactionConfig{Enabled: true, MidTurn: true})
	s.compaction.mu.Lock()
	off = s.compaction.midTurnOff
	s.compaction.mu.Unlock()
	if off {
		t.Fatal("saving the settings must re-arm mid-turn swaps")
	}
}

// awayRecap is Claude Code's away recap as it arrives: the conversation's
// history with a recap request appended, sent minutes after a turn ended.
func awayRecap(t *testing.T, history []json.RawMessage) []json.RawMessage {
	t.Helper()
	recap := msgs(t, `[{"role":"user","content":[{"type":"text","text":"The user stepped away and is coming back. Recap in under 40 words, 1-2 plain sentences, no markdown."}]}]`)
	return append(append([]json.RawMessage(nil), history...), recap...)
}

// On 2026-10-01 the away recap was taken for a plain prompt: it swapped the
// summary in while nobody was there, so the notice saying so waited 80
// minutes for a prompt and was lost to a restart first.
func TestAwayRecapDoesNotSwapOrDrop(t *testing.T) {
	f := &fakeAnthropic{context: 450_000}
	s := compactServer(t, f, config.CompactionConfig{Enabled: true})
	all := msgs(t, session)
	send(t, s, "S", all[:5])
	send(t, s, "S", all[:7])
	waitFor(t, func() bool { return s.compactionReady("S") })
	s.PromptNotices("S", false)

	send(t, s, "S", awayRecap(t, all[:8]))
	if st := stateFor(s, "S"); st.next == "" || st.summary != "" {
		t.Fatalf("the recap must leave the summary waiting, not swap it: next=%q summary=%q", st.next, st.summary)
	}
	if strings.Contains(f.last(), "THE GIST") {
		t.Fatal("the recap must go as Claude Code sent it while nothing is swapped in")
	}
	// It also alters the conversation's own messages; that must not drop
	// the waiting summary either.
	altered := awayRecap(t, all[:8])
	altered[3] = json.RawMessage(`{"role":"assistant","content":[{"type":"text","text":"trimmed"}]}`)
	send(t, s, "S", altered)
	if st := stateFor(s, "S"); st.next == "" {
		t.Fatal("an altered recap dropped the waiting summary")
	}
	if got := s.PromptNotices("S", false); len(got) != 0 && strings.Contains(strings.Join(got, ""), "done") {
		t.Fatalf("no swap happened, so no result line: %q", got)
	}

	send(t, s, "S", all[:9])
	if !strings.Contains(f.last(), "THE GIST") {
		t.Fatal("the next real prompt swaps it in")
	}
	// After the swap a recap carries the summary in force, unchanged.
	send(t, s, "S", awayRecap(t, all[:9]))
	if !strings.Contains(f.last(), "THE GIST") || !strings.Contains(f.last(), "stepped away") {
		t.Fatal("a recap after the swap must carry the summary in force")
	}
}

// A restart between the swap and the next prompt lost the notice, and
// reloaded the context from BEFORE the swap, so the next prompt started a
// second summary of an already compacted session (433k saved, 82k real).
func TestSwapNoticeAndContextSurviveARestart(t *testing.T) {
	f := &fakeAnthropic{context: 450_000}
	up := httptest.NewServer(http.HandlerFunc(f.handler))
	t.Cleanup(up.Close)
	cfg := config.Default()
	cfg.AnthropicBaseURL = up.URL
	cfg.PrimaryCompaction = config.CompactionConfig{Enabled: true, CompactAtTokens: 400_000, WarnAtPercent: 75, WindowMinutes: 60}
	dir := t.TempDir()
	start := func() *Server {
		s, err := New(cfg, filepath.Join(dir, "state.json"), filepath.Join(dir, "metrics.jsonl"), log.New(testLogWriter{t}, "", 0))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(s.compaction.running.Wait)
		return s
	}
	all := msgs(t, session)
	s := start()
	send(t, s, "S", all[:5])
	send(t, s, "S", all[:7])
	waitFor(t, func() bool { return s.compactionReady("S") })
	s.PromptNotices("S", false)
	f.mu.Lock()
	f.context = 60_000
	f.mu.Unlock()
	send(t, s, "S", all[:9])

	s2 := start()
	if got := strings.Join(s2.PromptNotices("S", false), "\n"); !strings.Contains(got, "done, 87% smaller") {
		t.Fatalf("the swap's result line must survive a restart, got:\n%s", got)
	}
	if again := start().PromptNotices("S", false); len(again) != 0 {
		t.Fatalf("a shown line is saved as shown, got %q after another restart", again)
	}
	st := stateFor(s2, "S")
	if st.lastContext < 60_000 || st.lastContext > 61_000 {
		t.Fatalf("the saved context must be the one after the swap, got %d", st.lastContext)
	}
	st.startedAt = time.Now().Add(-2 * time.Hour) // the window has passed, as on the day
	n := f.summaryCount()
	more := append(append([]json.RawMessage(nil), all...), msgs(t, `[{"role":"assistant","content":[{"type":"text","text":"done with third"}]},{"role":"user","content":[{"type":"text","text":"fourth task"}]}]`)...)
	send(t, s2, "S", more)
	s2.compaction.running.Wait()
	if f.summaryCount() != n {
		t.Fatal("a compacted session at 60k must not start another summary")
	}
}

// Only a stream that ended with message_stop yields a summary: an error event
// or a cut-off stream leaves text that reads like one and is only its start.
func TestReadSSETextNeedsACompletedStream(t *testing.T) {
	delta := `data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"Only the first half"}}` + "\n\n"
	for name, tc := range map[string]struct {
		body string
		ok   bool
	}{
		"complete":    {delta + `data: {"type":"message_delta","delta":{"stop_reason":"end_turn"}}` + "\n\n" + `data: {"type":"message_stop"}` + "\n\n", true},
		"error event": {delta + `data: {"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}` + "\n\n", false},
		"cut off":     {delta, false},
	} {
		text, _, _, err := readSSEText(strings.NewReader(tc.body))
		if (err == nil) != tc.ok || text != "Only the first half" {
			t.Errorf("%s: text %q err %v", name, text, err)
		}
	}
}

// /clear ends the conversation a summary is being written for: the call is
// stopped, nothing is kept or announced, and the window is open again.
func TestClearSessionStopsASummaryInFlight(t *testing.T) {
	started := make(chan struct{})
	gone := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		w.Header().Set("content-type", "text/event-stream")
		fmt.Fprint(w, "data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":5,\"cache_read_input_tokens\":450000}}}\n\n")
		if !strings.Contains(string(b), "Summarize the transcript inside") {
			fmt.Fprint(w, "data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"}}\n\n")
			fmt.Fprint(w, "data: {\"type\":\"message_stop\"}\n\n")
			return
		}
		w.(http.Flusher).Flush()
		close(started)
		// The summary is still being written until the gateway hangs up.
		select {
		case <-r.Context().Done():
			close(gone)
		case <-time.After(10 * time.Second):
		}
	}))
	defer up.Close()
	cfg := config.Default()
	cfg.AnthropicBaseURL = up.URL
	cfg.PrimaryCompaction = config.CompactionConfig{Enabled: true}
	dir := t.TempDir()
	metricsPath := filepath.Join(dir, "metrics.jsonl")
	s, err := New(cfg, filepath.Join(dir, "state.json"), metricsPath, log.New(testLogWriter{t}, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	all := msgs(t, session)
	send(t, s, "S", all[:5])
	send(t, s, "S", all[:7])
	<-started
	s.PromptNotices("S", false)

	if n := s.ClearSession("T"); n != 0 {
		t.Fatalf("another session's /clear stopped %d summaries", n)
	}
	if n := s.ClearSession("S"); n != 1 {
		t.Fatalf("stopped %d summaries, want 1", n)
	}
	select {
	case <-gone:
	case <-time.After(5 * time.Second):
		t.Fatal("the summary call was not hung up")
	}
	s.compaction.running.Wait()

	s.compaction.mu.Lock()
	st := stateFor(s, "S")
	pending, next, opened, left := st.pending, st.next, st.startedAt.IsZero(), len(s.compaction.cancels)
	s.compaction.mu.Unlock()
	if pending || next != "" || !opened || left != 0 {
		t.Fatalf("pending=%v next=%q window open=%v cancels left=%d", pending, next, opened, left)
	}
	// Not a failure: nobody is told anything.
	if got := s.PromptNotices("S", false); len(got) != 0 {
		t.Fatalf("a cleared session was told %q", got)
	}
	// What the call cost before it was stopped is still on the old session.
	b, _ := os.ReadFile(metricsPath)
	found := false
	for _, l := range strings.Split(string(b), "\n") {
		if strings.Contains(l, `"note":"compaction summary incomplete`) {
			found = strings.Contains(l, `"session_id":"S"`) && strings.Contains(l, `"cache_read_tokens":450000`)
		}
	}
	if !found {
		t.Fatalf("the stopped call's cost is not recorded against session S:\n%s", b)
	}
}

// A learned Compact at reaches the running gateway, survives the settings
// being saved again, and a summary dropped before it applied is logged for
// the learner as a compaction that bought nothing.
func TestLearnedCompactAtAndTheOutcomeLog(t *testing.T) {
	s := compactServer(t, &fakeAnthropic{context: 450_000}, config.CompactionConfig{Enabled: true, Mode: config.CompactionIntelligent})
	s.SetLearnedCompaction(map[string]int64{"/src/x": 150_000})
	s.SetCompaction(config.CompactionConfig{Enabled: true, Mode: config.CompactionIntelligent, CompactAtTokens: 400_000})
	s.compaction.mu.Lock()
	got, o := s.compaction.cfg.ForRepo("/src/x")
	s.compaction.mu.Unlock()
	if got.CompactAtTokens != 150_000 || o == nil || !o.Learned {
		t.Fatalf("after saving the settings again: %d %+v", got.CompactAtTokens, o)
	}

	log := s.CompactionOutcomesPath()
	if filepath.Base(log) != "compaction-outcomes.jsonl" || filepath.Dir(log) != filepath.Dir(s.compaction.path) {
		t.Fatalf("outcome log at %q, want it beside the compaction state", log)
	}
	all := msgs(t, session)
	send(t, s, "S", all[:5])
	send(t, s, "S", all[:7])
	waitFor(t, func() bool { return s.compactionReady("S") })
	// The history is cut back before the summary applied: /clear or a rewind.
	send(t, s, "S", all[:1])
	out := autocompact.ReadOutcomes(log, time.Time{})
	if len(out) != 1 || out[0].Session != "S" || out[0].Kind != autocompact.OutcomeUnused {
		t.Fatalf("outcomes %+v, want one unused summary for S", out)
	}
}

// The cut is right after the request the summary was written from: the
// reply to it is the first message kept, nothing of the turn before it is
// re-sent, and the prompt that started the turn rides beside the summary.
// Until 5 Oct 2026 the tail started at the latest plain prompt, so a long
// turn was kept whole: a session compacted at 175k was back at 191k
// twenty-five minutes later, inside the 30 minutes that held off the next.
func TestCutIsRightAfterTheRequestTheSummaryWasWrittenFrom(t *testing.T) {
	f := &fakeAnthropic{context: 450_000}
	s := compactServer(t, f, config.CompactionConfig{Enabled: true})
	all := msgs(t, session)
	send(t, s, "S", all[:5])
	send(t, s, "S", all[:7])
	waitFor(t, func() bool { return s.compactionReady("S") })
	send(t, s, "S", all[:9])
	var req struct {
		Messages []json.RawMessage `json:"messages"`
	}
	if err := json.Unmarshal([]byte(f.last()), &req); err != nil {
		t.Fatal(err)
	}
	if len(req.Messages) != 3 {
		t.Fatalf("want the summary, the reply and the new prompt, got %d messages:\n%s", len(req.Messages), f.last())
	}
	lead := string(req.Messages[0])
	for _, want := range []string{`"role":"user"`, "THE GIST", "never push without asking", "latest-user-message\\u003e\\nsecond task\\n\\u003c/latest-user-message"} {
		if !strings.Contains(lead, want) {
			t.Fatalf("the summary message lacks %s:\n%s", want, lead)
		}
	}
	if strings.Count(lead, "second task") != 1 {
		t.Fatalf("the latest prompt is carried once, without Claude Code's reminders:\n%s", lead)
	}
	if !strings.Contains(string(req.Messages[1]), "done with second") || strings.Contains(string(req.Messages[1]), "thinking") {
		t.Fatalf("the reply is kept, without thinking from before the swap:\n%s", req.Messages[1])
	}
	if !jsonEqual(t, req.Messages[2], all[8]) {
		t.Fatalf("the new prompt must go as sent:\n%s", req.Messages[2])
	}
	// A later request of the same turn keeps the same start, so the cache
	// entry the swap wrote is read again.
	more := append(append([]json.RawMessage(nil), all...),
		json.RawMessage(`{"role":"assistant","content":[{"type":"tool_use","id":"t9","name":"Read","input":{}}]}`),
		json.RawMessage(`{"role":"user","content":[{"type":"tool_result","tool_use_id":"t9","content":"more"}]}`))
	send(t, s, "S", more)
	var again struct {
		Messages []json.RawMessage `json:"messages"`
	}
	if err := json.Unmarshal([]byte(f.last()), &again); err != nil {
		t.Fatal(err)
	}
	if len(again.Messages) != 5 || !jsonEqual(t, again.Messages[0], req.Messages[0]) || !jsonEqual(t, again.Messages[1], req.Messages[1]) {
		t.Fatalf("the start of the request must not change between requests:\n%s", f.last())
	}
}

// When the newest message of the request the summary was written from has
// changed by the next request, the cut right after it cannot be trusted:
// the cut moves back to the latest plain prompt before it, which still
// matches. The summary overlaps what is kept and nothing is lost.
func TestCutMovesBackToThePromptWhenTheNewestMessageChanged(t *testing.T) {
	f := &fakeAnthropic{context: 450_000}
	var logBuf strings.Builder
	s := compactServer(t, f, config.CompactionConfig{Enabled: true})
	s.logger.SetOutput(io.MultiWriter(testLogWriter{t}, &logBuf))
	all := msgs(t, session)
	send(t, s, "S", all[:5])
	send(t, s, "S", all[:7])
	waitFor(t, func() bool { return s.compactionReady("S") })
	changed := append([]json.RawMessage(nil), all[:9]...)
	changed[6] = json.RawMessage(`{"role":"user","content":[{"type":"tool_result","tool_use_id":"t2","content":"output, rewritten"}]}`)
	send(t, s, "S", changed)
	got := f.last()
	if !strings.Contains(got, "THE GIST") || !strings.Contains(got, "output, rewritten") || strings.Contains(got, "old file") {
		t.Fatalf("want the summary with the tail kept from the second task's prompt:\n%s", got)
	}
	if strings.Contains(got, "latest-user-message") {
		t.Fatalf("the prompt itself is kept, so it is not carried a second time:\n%s", got)
	}
	if !strings.Contains(logBuf.String(), "compaction cut moved back") || !strings.Contains(logBuf.String(), "message 6 of 7 changed") {
		t.Fatalf("the log must say the cut moved and which message changed:\n%s", logBuf.String())
	}
}

// The request the summary was written from, sent again (a retry), is not a
// changed history: the summary keeps waiting and applies afterwards.
func TestSummaryWaitsThroughAResentRequest(t *testing.T) {
	f := &fakeAnthropic{context: 450_000}
	h := msgs(t, `[
 {"role":"user","content":"hi"},
 {"role":"assistant","content":[{"type":"text","text":"hello"}]},
 {"role":"user","content":"big task"},
 {"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"Read","input":{}}]},
 {"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"`+strings.Repeat("x", 20000)+`"}]},
 {"role":"assistant","content":[{"type":"text","text":"read it"}]},
 {"role":"user","content":"next"}]`)
	s := compactServer(t, f, config.CompactionConfig{Enabled: true})
	send(t, s, "S", h[:3])
	// No plain prompt leaves 30% before it: only the cut after the request exists.
	send(t, s, "S", h[:5])
	waitFor(t, func() bool { return s.compactionReady("S") })
	send(t, s, "S", h[:5])
	if !s.compactionReady("S") || strings.Contains(f.last(), "THE GIST") {
		t.Fatal("a resent request must neither drop the summary nor carry it")
	}
	send(t, s, "S", h)
	if got := f.last(); !strings.Contains(got, "THE GIST") || strings.Contains(got, "xxxx") || !strings.Contains(got, "latest-user-message\\u003e\\nbig task") {
		t.Fatalf("the summary must replace the long turn and carry its prompt:\n%s", got)
	}
}

func TestLatestPromptText(t *testing.T) {
	h := msgs(t, `[
 {"role":"user","content":[{"type":"text","text":"<system-reminder>ctx</system-reminder>"},{"type":"text","text":"do the thing"},{"type":"text","text":"and this"}]},
 {"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"Read","input":{}}]},
 {"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"x"}]}]`)
	if got := latestPromptText(h); got != "do the thing\n\nand this" {
		t.Fatalf("got %q", got)
	}
	long := msgs(t, `[{"role":"user","content":"`+strings.Repeat("a", 9000)+`END"}]`)
	got := latestPromptText(long)
	if len(got) > latestPromptMax+100 || !strings.HasSuffix(got, "END") || !strings.Contains(got, "middle left out") {
		t.Fatalf("a long prompt keeps its start and end: %d bytes", len(got))
	}
	if got := latestPromptText(h[1:]); got != "" {
		t.Fatalf("no plain prompt, got %q", got)
	}
}

// The request the summary is written from ends where the work was, so the
// model sometimes makes the next tool call instead (first live run,
// 2026-10-05: "the model called a tool instead of writing the summary",
// and five more minutes on the full history). The call is refused and the
// model asked once more, on top of the first request so it reads from cache.
func TestSummaryThatCallsAToolIsAskedAgain(t *testing.T) {
	f := &fakeAnthropic{context: 450_000, toolFirst: true}
	s := compactServer(t, f, config.CompactionConfig{Enabled: true})
	all := msgs(t, session)
	send(t, s, "S", all[:5])
	send(t, s, "S", all[:7])
	waitFor(t, func() bool { return s.compactionReady("S") })
	if n := f.summaryCount(); n != 2 {
		t.Fatalf("want the summary asked for twice, got %d", n)
	}
	var req struct {
		Messages []json.RawMessage `json:"messages"`
	}
	f.mu.Lock()
	second := ""
	for _, b := range f.bodies {
		if strings.Contains(b, "Not run: tools are unavailable") {
			second = b
		}
	}
	f.mu.Unlock()
	if err := json.Unmarshal([]byte(second), &req); err != nil {
		t.Fatal(err)
	}
	if len(req.Messages) != 9 {
		t.Fatalf("want the first request plus the refused call, got %d messages", len(req.Messages))
	}
	call, refusal := string(req.Messages[7]), string(req.Messages[8])
	for _, want := range []string{`"signature":"sigX"`, `"id":"tu9"`, `"command":"ls"`} {
		if !strings.Contains(call, want) {
			t.Fatalf("the model's reply must go back as it came, missing %s: %s", want, call)
		}
	}
	if !strings.Contains(refusal, `"tool_use_id":"tu9"`) || !strings.Contains(refusal, `"is_error":true`) {
		t.Fatalf("the call must be refused: %s", refusal)
	}
	send(t, s, "S", all[:9])
	if !strings.Contains(f.last(), "THE GIST") {
		t.Fatal("the second answer is the summary in force")
	}
}

// The way back from a poor summary: Claude Code still holds everything, so
// dropping the summary sends the request as it was built, and no new
// summary starts on that request.
func TestDropSummarySendsTheFullHistoryAgain(t *testing.T) {
	f := &fakeAnthropic{context: 450_000}
	s := compactServer(t, f, config.CompactionConfig{Enabled: true})
	all := msgs(t, session)
	send(t, s, "S", all[:5])
	send(t, s, "S", all[:7])
	waitFor(t, func() bool { return s.compactionReady("S") })
	send(t, s, "S", all[:9])
	if !strings.Contains(f.last(), "THE GIST") {
		t.Fatal("the summary must be in force first")
	}
	if n := s.DropSummary("S"); n != 1 {
		t.Fatalf("want 1 summary dropped, got %d", n)
	}
	send(t, s, "S", all[:9])
	s.compaction.running.Wait()
	if got := f.last(); strings.Contains(got, "THE GIST") || !strings.Contains(got, "old file") {
		t.Fatalf("the full history must go again:\n%s", got)
	}
	if n := f.summaryCount(); n != 1 {
		t.Fatalf("no new summary inside the delay, got %d", n)
	}
	if s.DropSummary("S") != 0 || s.DropSummary("") != 0 {
		t.Fatal("nothing left to drop")
	}
}

// Claude Code writes the first message again when CLAUDE.md or the memory
// index changes. The summary is of the history, and the first message is
// the part it replaces.
func TestASummarySurvivesAFirstMessageThatChanges(t *testing.T) {
	f := &fakeAnthropic{context: 450_000}
	s := compactServer(t, f, config.CompactionConfig{Enabled: true, CompactAtTokens: 400_000, WarnAtPercent: 75, WindowMinutes: 60})
	var logged bytes.Buffer
	s.logger = log.New(io.MultiWriter(&logged, testLogWriter{t}), "", 0)
	all := msgs(t, session)
	send(t, s, "S", all[:5])
	send(t, s, "S", all[:7])
	waitFor(t, func() bool { return f.summaryCount() == 1 })
	waitFor(t, func() bool { return s.compactionReady("S") })
	send(t, s, "S", all[:9])
	if !strings.Contains(f.last(), "THE GIST OF THE FIRST TASK") {
		t.Fatalf("the session must be compacted:\n%s", f.last())
	}

	// Claude Code rewrites the context it keeps in the first message.
	changed := append([]json.RawMessage(nil), all[:9]...)
	changed[0] = json.RawMessage(`{"role":"user","content":[{"type":"text","text":"<system-reminder>CLAUDE.md: never push without asking\nand one more rule</system-reminder>"},{"type":"text","text":"first task"}]}`)
	send(t, s, "S", changed)
	if got := f.last(); !strings.Contains(got, "THE GIST OF THE FIRST TASK") || strings.Contains(got, "old file") {
		t.Fatalf("a changed first message must not send the history whole:\n%s", got)
	}
	if l := logged.String(); !strings.Contains(l, "compaction first message changed") || !strings.Contains(l, "block 0 (system reminder") || !strings.Contains(l, "from line 1") {
		t.Fatalf("the log must say what changed in the first message:\n%s", l)
	}
	if strings.Contains(logged.String(), "compaction dropped") {
		t.Fatalf("nothing was dropped:\n%s", logged.String())
	}
	// One conversation, not two, and no second summary paid for.
	n := 0
	for _, cs := range s.CompactionSessions() {
		if cs.Session == "S" {
			n++
		}
	}
	s.compaction.running.Wait()
	if n != 1 || f.summaryCount() != 1 {
		t.Fatalf("want one conversation and one summary, got %d and %d", n, f.summaryCount())
	}

	// A history that parted from the summarised one is still not it.
	parted := append([]json.RawMessage(nil), changed...)
	parted[1] = json.RawMessage(`{"role":"assistant","content":"something else was said"}`)
	send(t, s, "S", parted)
	if strings.Contains(f.last(), "THE GIST") {
		t.Fatal("a history the summary was not made from must go as it is")
	}
	// And the summary is still there for the history it was made from.
	send(t, s, "S", all[:9])
	if !strings.Contains(f.last(), "THE GIST OF THE FIRST TASK") {
		t.Fatalf("the summary must still be in force:\n%s", f.last())
	}
}

// A summary saved before first messages could change has a hash of the
// whole prefix. It still applies, and moves to the new hash at the first
// request it fits.
func TestASummarySavedTheOldWayMovesToTailHashes(t *testing.T) {
	all := msgs(t, session)
	st := &compactState{summary: "x", p0: 4, swapAt: 5, hash: prefixHash(all, 4)}
	changed := append([]json.RawMessage(nil), all...)
	changed[0] = json.RawMessage(`{"role":"user","content":"another first message"}`)
	if st.fits(changed) || st.useTailHashes(changed) || st.tail {
		t.Fatal("an old hash proves nothing about a history with another first message")
	}
	if !st.useTailHashes(all) || !st.tail || st.hash == prefixHash(all, 4) {
		t.Fatal("the request it fits must move it to tail hashes")
	}
	if !st.fits(changed) || !st.fits(all) {
		t.Fatal("once moved, the first message no longer matters")
	}
	changed[2] = json.RawMessage(`{"role":"user","content":"a different result"}`)
	if st.fits(changed) {
		t.Fatal("a later message that changed is another history")
	}
	// A summary of fewer than three messages keeps its first message.
	short := &compactState{tail: true, summary: "x", p0: 2}
	if short.hashOf(all, 2) != prefixHash(all, 2) || short.fits(all) {
		t.Fatal("two messages are too few to name a conversation without the first")
	}
}

// threadAPI answers /v1/messages the way the API does for message threads:
// each response has an id for the next request to continue, and reports the
// context set in ctx. A summary request is answered with a summary.
type threadAPI struct {
	mu     sync.Mutex
	n      int
	ctx    int64
	bodies []string
}

func (a *threadAPI) handler(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	// A summary takes no number: the turns are msg_01, msg_02, ... in the
	// order the test sends them, whenever the summary is written.
	summary := strings.Contains(string(b), "Summarize the transcript inside")
	a.mu.Lock()
	if !summary {
		a.n++
	}
	id, ctx := fmt.Sprintf("msg_%02d", a.n), a.ctx
	a.bodies = append(a.bodies, string(b))
	a.mu.Unlock()
	text := "ok"
	if summary {
		id, text, ctx = "msg_summary", "<summary>THE GIST OF THE THREAD</summary>", 1000
	}
	w.Header().Set("content-type", "text/event-stream")
	fmt.Fprintf(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":%q,\"usage\":{\"input_tokens\":5,\"cache_read_input_tokens\":%d}}}\n\n", id, ctx)
	fmt.Fprintf(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":%q}}\n\n", text)
	fmt.Fprint(w, "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":20}}\n\n")
	fmt.Fprint(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
}

// turns is how many requests that were not summaries reached the API, and
// the last of them.
func (a *threadAPI) turns() (int, string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	n, last := 0, ""
	for _, b := range a.bodies {
		if !strings.Contains(b, "Summarize the transcript inside") {
			n, last = n+1, b
		}
	}
	return n, last
}

func (a *threadAPI) summaries() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	n := 0
	for _, b := range a.bodies {
		if strings.Contains(b, "Summarize the transcript inside") {
			n++
		}
	}
	return n
}

// holds sets what the next responses report as the context.
func (a *threadAPI) holds(ctx int64) {
	a.mu.Lock()
	a.ctx = ctx
	a.mu.Unlock()
}

func threadServer(t *testing.T, a *threadAPI, c config.CompactionConfig) *Server {
	t.Helper()
	up := httptest.NewServer(http.HandlerFunc(a.handler))
	t.Cleanup(up.Close)
	cfg := config.Default()
	cfg.AnthropicBaseURL = up.URL
	cfg.PrimaryCompaction = c
	dir := t.TempDir()
	s, err := New(cfg, filepath.Join(dir, "state.json"), filepath.Join(dir, "metrics.jsonl"), log.New(testLogWriter{t}, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.compaction.running.Wait)
	return s
}

// continueThread sends what Claude Code sends for a turn on a thread: the
// new message alone, continuing the response prev.
func continueThread(t *testing.T, s *Server, sid, prev string, message any) *httptest.ResponseRecorder {
	t.Helper()
	b, _ := json.Marshal(map[string]any{"model": "claude-opus-5-5", "stream": true, "max_tokens": 100, "system": "sys",
		"thread": map[string]any{"type": "continue", "previous_message_id": prev}, "messages": []any{message}})
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/v1/messages", bytes.NewReader(b))
	req.Header.Set("x-claude-code-session-id", sid)
	req.Header.Set("authorization", "Bearer oauth")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	return rec
}

func toolResult(text string) any {
	return map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": "t9", "content": text}}}
}

func rowsOf(s *Server, sid string) []CompactionSession {
	var out []CompactionSession
	for _, cs := range s.CompactionSessions() {
		if cs.Session == sid {
			out = append(out, cs)
		}
	}
	return out
}

// askedForHistory is whether rec is the API's own answer for a thread whose
// state is gone, which Claude Code answers with the conversation whole.
func askedForHistory(rec *httptest.ResponseRecorder) bool {
	var v struct {
		Type  string `json:"type"`
		Error struct {
			Type    string `json:"type"`
			Details struct {
				Code string `json:"error_code"`
			} `json:"details"`
		} `json:"error"`
	}
	return rec.Code == http.StatusNotFound && json.Unmarshal(rec.Body.Bytes(), &v) == nil &&
		v.Type == "error" && v.Error.Type == "not_found_error" && v.Error.Details.Code == "thread_not_found"
}

// Claude Code 2.1.289 sends a turn as the new message alone, continuing the
// response before it. On 6 Oct 2026 every such request was a conversation
// of its own: 29 on record for one session, and its real size nowhere.
func TestARequestThatContinuesAThreadIsItsConversation(t *testing.T) {
	a := &threadAPI{ctx: 90_000}
	s := threadServer(t, a, config.CompactionConfig{Enabled: true, CompactAtTokens: 200_000, WarnAtPercent: 75, WindowMinutes: 60})

	send(t, s, "S", msgs(t, session)[:5]) // msg_01, 90k
	a.holds(120_000)
	continueThread(t, s, "S", "msg_01", toolResult("one")) // msg_02, 120k
	a.holds(140_000)
	continueThread(t, s, "S", "msg_02", toolResult("two")) // msg_03, 140k
	continueThread(t, s, "S", "msg_03", toolResult("three"))
	got := rowsOf(s, "S")
	if len(got) != 1 || got[0].Context != 140_005 || !got[0].Thread || got[0].State != "ok" {
		t.Fatalf("want one conversation, a thread at 140k, got %+v", got)
	}
	n, last := a.turns()
	if n != 4 || !strings.Contains(last, `"previous_message_id":"msg_03"`) || !strings.Contains(last, "three") {
		t.Fatalf("a thread request under the limit must go as it came, got %d turns, the last:\n%s", n, last)
	}
	if a.summaries() != 0 {
		t.Fatal("a summary was started from a request that holds no history")
	}

	// A response from before a restart: a conversation of its own, once.
	continueThread(t, s, "S", "msg_from_before_the_restart", toolResult("four")) // msg_05
	continueThread(t, s, "S", "msg_05", toolResult("five"))
	if got := rowsOf(s, "S"); len(got) != 2 {
		t.Fatalf("want the thread nobody knows as one more conversation, got %d: %+v", len(got), got)
	}
	if main := MainConversation(s.CompactionSessions(), "S"); main == nil || !main.Thread {
		t.Fatalf("the thread speaks for the session, got %+v", main)
	}
}

// A request that continues a thread holds nothing to summarise or replace,
// so until 6 Oct 2026 a session on threads was compacted only when a thread
// happened to expire: one sat at 347k over a 300k limit, its summary written
// and waiting. Burst now asks for the history the way the API does.
func TestAThreadOverItsLimitIsAskedForItsHistoryAndCompacted(t *testing.T) {
	a := &threadAPI{ctx: 90_000}
	s := threadServer(t, a, config.CompactionConfig{Enabled: true, CompactAtTokens: 200_000, WarnAtPercent: 75, WindowMinutes: 60})
	all := msgs(t, session)

	send(t, s, "S", all[:5]) // msg_01: the conversation, whole, at 90k
	a.holds(210_000)
	if rec := continueThread(t, s, "S", "msg_01", all[6]); rec.Code != http.StatusOK { // msg_02: over the limit
		t.Fatalf("a thread under its limit goes to the API, got %d", rec.Code)
	}
	if got := rowsOf(s, "S"); len(got) != 1 || !strings.HasPrefix(got[0].State, "over threshold, compacts at the next") {
		t.Fatalf("a thread over its limit is compacted like any session, got %+v", got)
	}

	// Over the limit: the next request is refused as the API refuses a
	// thread that has expired, and nothing is sent.
	before, _ := a.turns()
	rec := continueThread(t, s, "S", "msg_02", toolResult("more"))
	if !askedForHistory(rec) {
		t.Fatalf("want 404 thread_not_found, got %d %s", rec.Code, rec.Body)
	}
	if rec.Header().Get("content-type") != "application/json" || !strings.Contains(rec.Body.String(), `Replay the full conversation with `+"`"+`thread: {\"type\": \"create\"}`+"`") {
		t.Fatalf("the answer must be the API's own, word for word: %s", rec.Body)
	}
	if after, _ := a.turns(); after != before {
		t.Fatal("a request Burst refuses must not reach the API")
	}

	// Claude Code sends the conversation whole: the summary starts from it.
	send(t, s, "S", all[:7]) // msg_03
	waitFor(t, func() bool { return a.summaries() == 1 })
	s.compaction.running.Wait()

	// Mid-turn swaps are off: the summary waits for a prompt, and a tool
	// result goes to the API as it came.
	if rec := continueThread(t, s, "S", "msg_03", toolResult("still working")); rec.Code != http.StatusOK { // msg_04
		t.Fatalf("a summary that waits for a prompt must not stop a tool result, got %d %s", rec.Code, rec.Body)
	}
	// The next prompt: asked again, and the history that comes back is
	// sent with the summary in place of what it covers. (A summary takes
	// longer to write than the gap between two requests for the history.)
	s.compaction.mu.Lock()
	s.compaction.asks["S|claude-opus-5-5|"].at = time.Now().Add(-askToSwapGap)
	s.compaction.mu.Unlock()
	if rec := continueThread(t, s, "S", "msg_04", all[8]); !askedForHistory(rec) {
		t.Fatalf("a ready summary must ask for the history at the next prompt, got %d %s", rec.Code, rec.Body)
	}
	a.holds(60_000)
	send(t, s, "S", all) // msg_05
	_, last := a.turns()
	if !strings.Contains(last, "THE GIST OF THE THREAD") || strings.Contains(last, "old file") || !strings.Contains(last, "third task") {
		t.Fatalf("the history must go with the summary swapped in:\n%s", last)
	}
	if got := rowsOf(s, "S"); len(got) != 1 || got[0].State != "compacted" || got[0].Context != 60_005 {
		t.Fatalf("want the session compacted at 60k, got %+v", got)
	}

	// The thread goes on from the compacted response, untouched.
	if rec := continueThread(t, s, "S", "msg_05", toolResult("after")); rec.Code != http.StatusOK {
		t.Fatalf("a compacted thread under its limit goes to the API, got %d %s", rec.Code, rec.Body)
	}
	if a.summaries() != 1 {
		t.Fatalf("one compaction, one summary: got %d", a.summaries())
	}
}

// Claude Code's side requests send the conversation whole, and one of them
// can take the swap before the thread is asked: on 6 Oct 2026 a summary went
// into those alone, and the thread carried on from its old response at 408k
// under a panel that read "115k, compacted".
func TestAThreadThatBeganBeforeItsSummaryIsAskedForItsHistory(t *testing.T) {
	a := &threadAPI{ctx: 210_000}
	s := threadServer(t, a, config.CompactionConfig{Enabled: true, CompactAtTokens: 200_000, WarnAtPercent: 75, WindowMinutes: 60})
	all := msgs(t, session)
	past := func() {
		s.compaction.mu.Lock()
		s.compaction.asks["S|claude-opus-5-5|"].at = time.Now().Add(-askToSwapGap)
		s.compaction.mu.Unlock()
	}
	send(t, s, "S", all[:5]) // msg_01, 210k
	if !askedForHistory(continueThread(t, s, "S", "msg_01", all[6])) {
		t.Fatal("want the history asked for")
	}
	send(t, s, "S", all[:7]) // msg_02: the thread's history, and the summary starts
	waitFor(t, func() bool { return a.summaries() == 1 })
	s.compaction.running.Wait()
	// A request that is not the thread's sends the history whole, ending
	// in a prompt: the summary swaps into it, and it reports its own size.
	a.holds(60_000)
	send(t, s, "S", all) // msg_03
	if got := rowsOf(s, "S"); len(got) != 1 || got[0].State != "compacted" || got[0].Context != 60_005 || got[0].Thread {
		t.Fatalf("want the other request compacted at 60k, got %+v", got)
	}
	// The thread goes on from msg_02, which holds everything: its size is
	// its own again, and it is asked for its history.
	past()
	if rec := continueThread(t, s, "S", "msg_02", toolResult("still the old thread")); !askedForHistory(rec) {
		t.Fatalf("a thread that began before the summary must be asked for its history, got %d %s", rec.Code, rec.Body)
	}
	send(t, s, "S", all) // msg_04: the thread's history, sent with the summary
	if _, last := a.turns(); !strings.Contains(last, "THE GIST OF THE THREAD") || strings.Contains(last, "old file") {
		t.Fatalf("the thread's history must go with the summary:\n%s", last)
	}
	// From there the thread holds the summary, and is left alone.
	past()
	a.holds(70_000)
	if rec := continueThread(t, s, "S", "msg_04", toolResult("the new thread")); rec.Code != http.StatusOK {
		t.Fatalf("a thread sent with the summary goes to the API, got %d %s", rec.Code, rec.Body)
	}
	if got := rowsOf(s, "S"); len(got) != 1 || got[0].Context != 70_005 || !got[0].Thread || got[0].State != "compacted" {
		t.Fatalf("want the thread compacted at 70k, got %+v", got)
	}
	if a.summaries() != 1 {
		t.Fatalf("one compaction, one summary: got %d", a.summaries())
	}
}

// With mid-turn swaps on, a ready summary is swapped in at the next request
// of any kind, so that is when the history is asked for.
func TestAThreadIsAskedForItsHistoryMidTurnWhereThatIsOn(t *testing.T) {
	a := &threadAPI{ctx: 210_000}
	s := threadServer(t, a, config.CompactionConfig{Enabled: true, CompactAtTokens: 200_000, WarnAtPercent: 75, WindowMinutes: 60, MidTurn: true})
	all := msgs(t, session)
	send(t, s, "S", all[:5])                                           // msg_01, 210k
	if !askedForHistory(continueThread(t, s, "S", "msg_01", all[6])) { // over the limit
		t.Fatal("want the history asked for")
	}
	send(t, s, "S", all[:7]) // msg_02: the summary starts
	waitFor(t, func() bool { return a.summaries() == 1 })
	s.compaction.running.Wait()
	s.compaction.mu.Lock()
	s.compaction.asks["S|claude-opus-5-5|"].at = time.Now().Add(-time.Minute) // past the gap between two
	s.compaction.mu.Unlock()
	if rec := continueThread(t, s, "S", "msg_02", toolResult("mid-turn")); !askedForHistory(rec) {
		t.Fatalf("mid-turn swaps are on: a tool result must ask for the history, got %d %s", rec.Code, rec.Body)
	}
}

// The history is not asked for on every tool call: once, then not again
// until the gap has passed, however far over the limit the thread is.
func TestAThreadIsNotAskedForItsHistoryOverAndOver(t *testing.T) {
	a := &threadAPI{ctx: 250_000}
	s := threadServer(t, a, config.CompactionConfig{Enabled: true, CompactAtTokens: 200_000, WarnAtPercent: 75, WindowMinutes: 60})
	// A thread from before a restart: Burst knows it only by its response.
	continueThread(t, s, "S", "msg_old", toolResult("one")) // msg_01, 250k
	if !askedForHistory(continueThread(t, s, "S", "msg_01", toolResult("two"))) {
		t.Fatal("want the history asked for")
	}
	// Claude Code does not answer with its history (a first message alone
	// is no history to compact): the thread goes on, and is left alone.
	for i, prev := range []string{"msg_01x", "msg_02", "msg_03"} {
		if rec := continueThread(t, s, "S", prev, toolResult("more")); rec.Code != http.StatusOK {
			t.Fatalf("request %d inside the gap must go to the API, got %d %s", i, rec.Code, rec.Body)
		}
	}
	s.compaction.mu.Lock()
	s.compaction.asks["S|claude-opus-5-5|"].at = time.Now().Add(-askToStartGap)
	s.compaction.mu.Unlock()
	if !askedForHistory(continueThread(t, s, "S", "msg_04", toolResult("later"))) {
		t.Fatal("past the gap, a thread still over its limit is asked again")
	}
}

// A client that sends the refused request again, and not its history, is
// not asked a second time: it would never get an answer.
func TestAClientThatDoesNotReplayIsLeftAlone(t *testing.T) {
	a := &threadAPI{ctx: 250_000}
	s := threadServer(t, a, config.CompactionConfig{Enabled: true, CompactAtTokens: 200_000, WarnAtPercent: 75, WindowMinutes: 60})
	send(t, s, "S", msgs(t, session)[:5]) // msg_01, 250k
	if !askedForHistory(continueThread(t, s, "S", "msg_01", toolResult("one"))) {
		t.Fatal("want the history asked for")
	}
	if rec := continueThread(t, s, "S", "msg_01", toolResult("one")); rec.Code != http.StatusOK { // msg_02
		t.Fatalf("the same request again must go to the API, got %d %s", rec.Code, rec.Body)
	}
	s.compaction.mu.Lock()
	s.compaction.asks["S|claude-opus-5-5|"].at = time.Now().Add(-time.Hour)
	s.compaction.mu.Unlock()
	if rec := continueThread(t, s, "S", "msg_02", toolResult("two")); rec.Code != http.StatusOK {
		t.Fatalf("a client that does not replay is not asked again, got %d %s", rec.Code, rec.Body)
	}
	if got := rowsOf(s, "S"); len(got) != 1 || !strings.Contains(got[0].State, "when Claude Code next sends its whole history") {
		t.Fatalf("the state must say why it is not compacted, got %+v", got)
	}
}

// After a restart a thread is known only by its last response, and the
// history that comes back arrives under a conversation nobody has measured:
// it holds what the thread held, so the summary starts from it at once.
func TestTheHistoryAskedForAfterARestartIsCompactedAtOnce(t *testing.T) {
	a := &threadAPI{ctx: 250_000}
	s := threadServer(t, a, config.CompactionConfig{Enabled: true, CompactAtTokens: 200_000, WarnAtPercent: 75, WindowMinutes: 60})
	continueThread(t, s, "S", "msg_old", toolResult("one")) // msg_01, 250k
	if !askedForHistory(continueThread(t, s, "S", "msg_01", toolResult("two"))) {
		t.Fatal("want the history asked for")
	}
	long := msgs(t, session)[:8]
	for i := 0; len(long) < longHistory+1; i++ {
		long = append(long,
			json.RawMessage(fmt.Sprintf(`{"role":"user","content":[{"type":"text","text":"task %d"}]}`, i)),
			json.RawMessage(fmt.Sprintf(`{"role":"assistant","content":[{"type":"text","text":"done %d"}]}`, i)))
	}
	long = append(long, json.RawMessage(`{"role":"user","content":[{"type":"text","text":"and now this"}]}`))
	send(t, s, "S", long)
	waitFor(t, func() bool { return a.summaries() == 1 })
	if got := rowsOf(s, "S"); len(got) != 1 || got[0].Thread {
		t.Fatalf("the thread known only by its response is this conversation now, got %+v", got)
	}
}

// On 6 Oct 2026 a request with the summarised history and a last message of
// its own came in the second the summary was swapped in. It was taken for
// the conversation rewound and the summary was dropped, so when the thread
// expired 39 minutes later its history went whole, 240k where 138k had been
// going.
func TestRequestBesideTheConversationDoesNotDropTheSummary(t *testing.T) {
	f := &fakeAnthropic{context: 450_000}
	s := compactServer(t, f, config.CompactionConfig{Enabled: true})
	all := msgs(t, session)
	send(t, s, "S", all[:5])
	send(t, s, "S", all[:7])
	waitFor(t, func() bool { return s.compactionReady("S") })
	send(t, s, "S", all[:9])
	st := stateFor(s, "S")
	if st.summary == "" || st.p0 < 3 || !strings.Contains(f.last(), "THE GIST") {
		t.Fatalf("the summary must be in force first: summary=%q p0=%d", st.summary, st.p0)
	}
	p0 := st.p0

	// The summarised history with another last message: sent as it came.
	own := msgs(t, `[{"role":"user","content":[{"type":"text","text":"a question beside the work"}]}]`)
	beside := append(append([]json.RawMessage(nil), all[:p0-1]...), own...)
	send(t, s, "S", beside)
	if st := stateFor(s, "S"); st.summary == "" || st.p0 != p0 {
		t.Fatal("a request beside the conversation dropped the summary")
	}
	if strings.Contains(f.last(), "THE GIST") || !strings.Contains(f.last(), "a question beside the work") {
		t.Fatal("it has nothing after the cut, so it goes as Claude Code sent it")
	}
	// The conversation goes on with its summary.
	send(t, s, "S", all[:9])
	if !strings.Contains(f.last(), "THE GIST") {
		t.Fatal("the conversation's next request must still carry the summary")
	}

	// Rewound to before the cut, the new prompt is where a summarised
	// message was by the request after it: dropped then.
	reply := msgs(t, `[{"role":"assistant","content":[{"type":"text","text":"answered"}]},{"role":"user","content":[{"type":"text","text":"and then"}]}]`)
	send(t, s, "S", append(append([]json.RawMessage(nil), beside...), reply...))
	if st := stateFor(s, "S"); st.summary != "" {
		t.Fatal("a history that went on from before the cut is not the summary's")
	}
	if strings.Contains(f.last(), "THE GIST") {
		t.Fatal("a dropped summary must not be sent")
	}
}
