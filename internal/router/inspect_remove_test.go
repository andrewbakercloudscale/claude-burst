package router

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/andrewbakercloudscale/claude-burst/internal/config"
	"github.com/andrewbakercloudscale/claude-burst/internal/ctxview"
)

func (f *fakeAnthropic) lastBody() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.bodies[len(f.bodies)-1]
}

// Removing an instruction file and a tool result leaves them out of every
// later request, a note in their place; the inspector shows them removed
// under the same id; Restore sends them again. Prompts and replies are
// never removable.
func TestRemoveAndRestoreContextItems(t *testing.T) {
	f := &fakeAnthropic{context: 1000}
	s := compactServer(t, f, config.CompactionConfig{})
	s.removals = ctxview.Open("")
	reminder := "<system-reminder>\\nContents of /nowhere/CLAUDE.md (project instructions):\\n\\nUSE-TABS-RULE\\n</system-reminder>"
	msg := func(s string) json.RawMessage { return json.RawMessage(s) }
	history := []json.RawMessage{
		msg(`{"role":"user","content":[{"type":"text","text":"` + reminder + `"},{"type":"text","text":"fix the bug"}]}`),
		msg(`{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"Read","input":{"file_path":"/nowhere/a.go"}}]}`),
		msg(`{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"BIG-FILE-CONTENT"}]}`),
		msg(`{"role":"assistant","content":"done"}`),
		msg(`{"role":"user","content":"next"}`),
	}
	send(t, s, "R", history)
	rep := s.InspectContext("R")
	byName := map[string]ContextItem{}
	for _, it := range rep.Items {
		byName[it.Name] = it
	}
	claude, result := byName["/nowhere/CLAUDE.md"], byName["Read /nowhere/a.go"]
	if !claude.Removable || !result.Removable || byName["Prompt"].Removable {
		t.Fatalf("removable: claude=%v result=%v prompt=%v", claude.Removable, result.Removable, byName["Prompt"].Removable)
	}
	for _, it := range []ContextItem{claude, result} {
		if err := s.Removals().Add(RemovalKey("R"), ctxview.Removal{ID: it.ID, Group: it.Group, Name: it.Name, Bytes: it.Bytes}); err != nil {
			t.Fatal(err)
		}
	}

	send(t, s, "R", history)
	got := f.lastBody()
	if strings.Contains(got, "USE-TABS-RULE") || strings.Contains(got, "BIG-FILE-CONTENT") {
		t.Fatal("removed content was sent")
	}
	if !strings.Contains(got, "burst-removed:"+claude.ID) || !strings.Contains(got, "burst-removed:"+result.ID) ||
		!strings.Contains(got, "fix the bug") || !strings.Contains(got, `"tool_use_id":"t1"`) {
		t.Fatalf("notes, prompt or the result's pairing missing: %s", got)
	}
	rep = s.InspectContext("R")
	removed := 0
	for _, it := range rep.Items {
		if it.Removed {
			removed++
			if it.ID != claude.ID && it.ID != result.ID {
				t.Errorf("removed item with a new id: %+v", it)
			}
			if it.Name != claude.Name && it.Name != result.Name {
				t.Errorf("removed item lost its name: %q", it.Name)
			}
		}
	}
	if removed != 2 {
		t.Fatalf("inspector shows %d removed, want 2", removed)
	}

	_ = s.Removals().Restore(RemovalKey("R"), claude.ID)
	_ = s.Removals().Restore(RemovalKey("R"), result.ID)
	send(t, s, "R", history)
	if got := f.lastBody(); !strings.Contains(got, "USE-TABS-RULE") || !strings.Contains(got, "BIG-FILE-CONTENT") {
		t.Fatal("restore did not send the items again")
	}
	// Another session is never touched.
	_ = s.Removals().Add(RemovalKey("R"), ctxview.Removal{ID: result.ID, Name: result.Name})
	send(t, s, "other", history)
	if !strings.Contains(f.lastBody(), "BIG-FILE-CONTENT") {
		t.Fatal("a removal leaked into another session")
	}
}

// A mid-turn swap the API refuses is resent without the swap. What the user
// removed stays removed on that resend: it used to go out whole, because
// the resend skipped the step that takes removed items out.
func TestARejectedMidTurnSwapResendsWithoutWhatWasRemoved(t *testing.T) {
	f := &fakeAnthropic{context: 450_000, rejectMidTurn: true}
	s, all := readyMidTurn(t, f, true)
	s.removals = ctxview.Open("")
	var read ContextItem
	for _, it := range s.InspectContext("S").Items {
		if it.Removable && strings.HasPrefix(it.Name, "Read") {
			read = it
		}
	}
	if read.ID == "" {
		t.Fatalf("no removable Read result in %+v", s.InspectContext("S").Items)
	}
	if err := s.Removals().Add(RemovalKey("S"), ctxview.Removal{ID: read.ID, Group: read.Group, Name: read.Name, Bytes: read.Bytes}); err != nil {
		t.Fatal(err)
	}
	if code := sendCode(t, s, "S", all[:8]); code != 200 {
		t.Fatalf("status %d", code)
	}
	got := f.lastBody()
	if strings.Contains(got, "THE GIST") {
		t.Fatalf("the resend must be without the swap:\n%s", got)
	}
	if strings.Contains(got, "old file") || !strings.Contains(got, "burst-removed:"+read.ID) {
		t.Fatalf("the removed tool result was sent on the resend:\n%s", got)
	}
}

// A 400 on a request with no unproven swap of its own is the request's own
// fault: it must not turn mid-turn swaps off for every session.
func TestA400WithNoUnprovenSwapLeavesMidTurnOn(t *testing.T) {
	f := &fakeAnthropic{context: 1000}
	s := compactServer(t, f, config.CompactionConfig{Enabled: true, MidTurn: true})
	r := httptest.NewRequest("POST", "/v1/messages", nil)
	r = r.WithContext(context.WithValue(r.Context(), compactInfoKey{}, compactInfo{key: "nobody|m", midTurn: true}))
	if s.rejectMidTurn(r, "image too large") {
		t.Fatal("nothing to undo, so nothing was rejected")
	}
	if s.MidTurnOff() {
		t.Fatal("an unrelated 400 turned mid-turn swaps off for every session")
	}
}
