package router

import (
	"encoding/json"
	"strings"
	"testing"
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
