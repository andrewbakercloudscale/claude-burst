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
	f.mu.Unlock()
	w.Header().Set("content-type", "text/event-stream")
	text := "ok"
	if isSummary {
		text = "<summary>THE GIST OF THE FIRST TASK</summary>"
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
		st := s.compaction.sessions["S|claude-opus-5-5"]
		return st != nil && !st.startedAt.IsZero() && !st.pending
	})
	if s.compactionReady("S") {
		t.Fatal("a summary that ended in a tool call must not be used")
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
