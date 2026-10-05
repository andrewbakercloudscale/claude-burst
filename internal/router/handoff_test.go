package router

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/andrewbakercloudscale/claude-burst/internal/config"
)

func readHandoff(t *testing.T, s *Server, sid string) (*Handoff, bool) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(handoffDir(s.compaction.path), sid+".json"))
	if err != nil {
		return nil, false
	}
	var h Handoff
	if err := json.Unmarshal(b, &h); err != nil {
		t.Fatalf("handoff file is not JSON: %v\n%s", err, b)
	}
	return &h, true
}

// A summary in force is written where the mod can read it with the gateway
// down, naming the first message kept, and the file goes when the summary
// does: a dropped summary must never be handed to Claude Code.
func TestHandoffFileFollowsTheSummaryInForce(t *testing.T) {
	f := &fakeAnthropic{context: 450_000}
	s := compactServer(t, f, config.CompactionConfig{Enabled: true})
	all := msgs(t, session)
	send(t, s, "S", all[:5])
	send(t, s, "S", all[:7])
	if _, ok := readHandoff(t, s, "S"); ok {
		t.Fatal("no hand-off before a summary is in force")
	}
	waitFor(t, func() bool { return s.compactionReady("S") })
	send(t, s, "S", all[:9])
	h, ok := readHandoff(t, s, "S")
	if !ok {
		t.Fatal("want a hand-off file once the summary is in force")
	}
	if !strings.Contains(h.Lead, "THE GIST") || !strings.Contains(h.Lead, "compacted by claude-burst") {
		t.Fatalf("the lead must carry the summary: %q", h.Lead)
	}
	if h.Messages <= 0 || h.Messages >= 9 {
		t.Fatalf("messages = %d", h.Messages)
	}
	if want := anchorOf(all[h.Messages]); h.First != want {
		t.Fatalf("first kept = %+v, want %+v", h.First, want)
	}
	if want := anchorOf(all[h.Messages-1]); h.Last != want {
		t.Fatalf("last summarised = %+v, want %+v", h.Last, want)
	}
	// A reply is the first message kept, so the prompt it answers is carried.
	if h.First.Role == "assistant" && !strings.Contains(h.Lead, "second task") {
		t.Fatalf("the latest prompt must be carried word for word: %q", h.Lead)
	}
	// The answer to that request said how large the whole history is.
	if h.Raw <= 0 {
		t.Fatalf("the file must say how much Claude Code holds: %+v", h.Raw)
	}
	if fi, err := os.Stat(filepath.Join(handoffDir(s.compaction.path), "S.json")); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("the file holds the conversation's summary: mode %v, %v", fi.Mode().Perm(), err)
	}

	s.DropSummary("S")
	if _, ok := readHandoff(t, s, "S"); ok {
		t.Fatal("a dropped summary must not stay on offer")
	}
}

func TestAnchorsNameAMessageByToolCallOrText(t *testing.T) {
	all := msgs(t, session)
	if a := anchorOf(all[0]); a.Role != "user" || a.Tool != "" || a.Text != "firsttask" {
		t.Errorf("a prompt is named by its text, reminders left out: %+v", a)
	}
	if a := anchorOf(all[1]); a.Role != "assistant" || a.Tool != "t1" {
		t.Errorf("a reply is named by its tool call: %+v", a)
	}
	if a := anchorOf(all[2]); a.Role != "user" || a.Tool != "t1" {
		t.Errorf("tool results are named by the call they answer: %+v", a)
	}
	// Cutting at tool results would leave results whose calls are gone, and
	// "first task" is too short to name a message by itself.
	if h := buildHandoff("S", all, "x", 2, 0, time.Now()); h != nil {
		t.Errorf("no hand-off at tool results: %+v", h)
	}
	if h := buildHandoff("S", all, "x", 4, 0, time.Now()); h != nil {
		t.Errorf("no hand-off at a prompt too short to name: %+v", h)
	}
	// A short prompt is named by the reply before it, when that says enough.
	long := append([]json.RawMessage(nil), all...)
	long[3] = json.RawMessage(`{"role":"assistant","content":[{"type":"text","text":"done with the first task, all of it"}]}`)
	if h := buildHandoff("S", long, "x", 4, 0, time.Now()); h == nil || h.First.Text != "secondtask" || h.Last.Text != "donewiththefirsttask,allofit" {
		t.Errorf("want a hand-off at a short prompt after a reply that names itself: %+v", h)
	}
	if h := buildHandoff("S", all, "x", 5, 0, time.Now()); h == nil || h.First.Tool != "t2" {
		t.Errorf("want a hand-off at a reply with a tool call: %+v", h)
	}
}
