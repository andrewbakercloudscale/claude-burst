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
	f.mu.Unlock()
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
	req := httptest.NewRequest(http.MethodPost, "http://local/v1/messages", bytes.NewReader(b))
	req.Header.Set("x-claude-code-session-id", sid)
	req.Header.Set("authorization", "Bearer oauth")
	s.ServeHTTP(httptest.NewRecorder(), req)
}

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
	s := compactServer(t, f, config.CompactionConfig{Enabled: true, WarnAtTokens: 300_000, CompactAtTokens: 400_000, WindowMinutes: 60})
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
	time.Sleep(100 * time.Millisecond)
	if n := f.summaryCount(); n != 1 {
		t.Fatalf("one compaction per window, got %d summaries", n)
	}
	// The user ran /clear (history no longer matches): pass through untouched.
	send(t, s, "S", msgs(t, `[{"role":"user","content":"a fresh start"}]`))
	if strings.Contains(f.last(), "THE GIST") {
		t.Fatal("a changed history must drop the swap")
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
	time.Sleep(100 * time.Millisecond)
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
	if e.CompactedMessages != 4 || e.CompactedBytes <= 0 {
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
	time.Sleep(50 * time.Millisecond)
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
	s := compactServer(t, f, config.CompactionConfig{Enabled: true, WarnAtTokens: 300_000, CompactAtTokens: 400_000, WindowMinutes: 60})
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
	if !strings.Contains(last, "Summarize the transcript inside") || !strings.Contains(last, "BEFORE the user message that begins") {
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
	cfg.PrimaryCompaction = config.CompactionConfig{Enabled: true, WarnAtTokens: 300_000, CompactAtTokens: 400_000, WindowMinutes: 60}
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
	time.Sleep(100 * time.Millisecond)
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
	s := compactServer(t, f, config.CompactionConfig{Enabled: true, WarnAtTokens: 300_000, CompactAtTokens: 400_000, WindowMinutes: 60})
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
	s := compactServer(t, f, config.CompactionConfig{Enabled: true, WarnAtTokens: 300_000, CompactAtTokens: 400_000, WindowMinutes: 60})
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
	if !strings.Contains(logBuf.String(), "message 2 of 4 changed") {
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
	if !strings.Contains(got, "450k, so 4 earlier messages are being summarised") || !strings.Contains(got, "ready and swaps in with this message") {
		t.Fatalf("want the start and the ready line, got:\n%s", got)
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
	if !strings.Contains(got, "Claude Burst, pauseless compaction: done. Context down 87%, 450k → 60k: 4 earlier messages now go as a summary") {
		t.Fatalf("want the result of the swap, got:\n%s", got)
	}

	// Turned off: nothing, and nothing queued is shown later either.
	s.SetCompaction(config.CompactionConfig{Enabled: true, NoPromptNotice: true})
	send(t, s, "S", msgs(t, `[{"role":"user","content":"a fresh start"}]`))
	if got := s.PromptNotices("S", false); got != nil {
		t.Fatalf("notices off, got %q", got)
	}
}

// Inside a long turn the summary cannot swap in. The hook after a tool call
// says so once, and the next prompt still gets its own ready line.
func TestPromptNoticesMidTurnSayItWaitsForTheNextPrompt(t *testing.T) {
	f := &fakeAnthropic{context: 450_000}
	s := compactServer(t, f, config.CompactionConfig{Enabled: true})
	all := msgs(t, session)
	send(t, s, "S", all[:5])
	send(t, s, "S", all[:7])
	waitFor(t, func() bool { return s.compactionReady("S") })

	got := strings.Join(s.PromptNotices("S", true), "\n")
	if !strings.Contains(got, "being summarised") || !strings.Contains(got, "swaps in when this turn finishes and you send your next prompt") {
		t.Fatalf("mid-turn: want the start and the waiting line, got:\n%s", got)
	}
	if strings.Contains(got, "swaps in with this message") {
		t.Fatalf("mid-turn must not claim this message swaps it in:\n%s", got)
	}
	if again := s.PromptNotices("S", true); len(again) != 0 {
		t.Fatalf("the waiting line is shown once per summary, got %q", again)
	}
	if got := strings.Join(s.PromptNotices("S", false), "\n"); !strings.Contains(got, "ready and swaps in with this message") {
		t.Fatalf("the next prompt still gets its ready line, got:\n%s", got)
	}

	// After the swap nothing is waiting, so mid-turn says only what happened.
	f.mu.Lock()
	f.context = 60_000
	f.mu.Unlock()
	send(t, s, "S", all[:9])
	got = strings.Join(s.PromptNotices("S", true), "\n")
	if !strings.Contains(got, "done. Context down") || strings.Contains(got, "swaps in") {
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
