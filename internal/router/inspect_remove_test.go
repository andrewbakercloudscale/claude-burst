package router

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
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

// /burst-prune <word> removes every tool result and instruction file the
// word names, by the call's name or its input; a prompt that mentions it is
// counted and left; undo puts everything back.
func TestPruneRemovesWhatAWordNamesAndUndoPutsItBack(t *testing.T) {
	f := &fakeAnthropic{context: 1000}
	s := compactServer(t, f, config.CompactionConfig{})
	s.removals = ctxview.Open("")
	big := func(tag string) string { return tag + strings.Repeat(" x", 400) }
	msg := func(s string) json.RawMessage { return json.RawMessage(s) }
	history := []json.RawMessage{
		msg(`{"role":"user","content":"fix the bug in the other-repo panel"}`),
		msg(`{"role":"assistant","content":[
			{"type":"tool_use","id":"t1","name":"Read","input":{"file_path":"/work/other-repo/panel.js"}},
			{"type":"tool_use","id":"t2","name":"Bash","input":{"command":"cd /work && ls\ncat other-repo/notes.md"}},
			{"type":"tool_use","id":"t3","name":"Read","input":{"file_path":"/work/mine/main.go"}},
			{"type":"tool_use","id":"t4","name":"Read","input":{"file_path":"/work/other-repo/tiny.txt"}}]}`),
		msg(`{"role":"user","content":[
			{"type":"tool_result","tool_use_id":"t1","content":"` + big("PANEL-SOURCE") + `"},
			{"type":"tool_result","tool_use_id":"t2","content":"` + big("NOTES-TEXT") + `"},
			{"type":"tool_result","tool_use_id":"t3","content":"` + big("MY-MAIN") + `"},
			{"type":"tool_result","tool_use_id":"t4","content":"TINY"}]}`),
		msg(`{"role":"assistant","content":"done"}`),
		msg(`{"role":"user","content":"next"}`),
	}
	send(t, s, "P", history)
	if res, err := s.PruneContext("nobody", "other-repo"); res != nil || err != nil {
		t.Fatalf("unknown session: %+v %v", res, err)
	}
	res, err := s.PruneContext("P", "Other-Repo")
	if err != nil || res == nil {
		t.Fatalf("prune: %+v %v", res, err)
	}
	// The panel read by name, the notes by the command's second line; the
	// tiny file is not worth a note; the prompt and the three calls stay.
	if res.Removed != 2 || res.Kept != 4 || !strings.Contains(res.Detail, "Pruned 2 items") || !strings.Contains(res.Detail, "/burst-prune undo") {
		t.Fatalf("result: %+v", res)
	}
	send(t, s, "P", history)
	got := f.lastBody()
	if strings.Contains(got, "PANEL-SOURCE") || strings.Contains(got, "NOTES-TEXT") {
		t.Fatal("pruned content was sent")
	}
	if !strings.Contains(got, "MY-MAIN") || !strings.Contains(got, "TINY") || !strings.Contains(got, "fix the bug in the other-repo panel") {
		t.Fatalf("something that should stay was pruned: %s", got)
	}
	if res, _ := s.PruneContext("P", "other-repo"); res.Removed != 0 || !strings.Contains(res.Detail, "Nothing to prune") {
		t.Fatalf("a second prune found more: %+v", res)
	}
	// "results" is every tool result from before the latest prompt.
	if res, _ := s.PruneContext("P", PruneResults); res.Removed != 1 || res.Kept != 0 {
		t.Fatalf("results: %+v", res)
	}
	send(t, s, "P", history)
	if strings.Contains(f.lastBody(), "MY-MAIN") {
		t.Fatal("results left a tool result in")
	}
	if n, err := s.RestoreContext("P"); n != 3 || err != nil {
		t.Fatalf("restore: %d %v", n, err)
	}
	send(t, s, "P", history)
	if got := f.lastBody(); !strings.Contains(got, "PANEL-SOURCE") || !strings.Contains(got, "NOTES-TEXT") || !strings.Contains(got, "MY-MAIN") {
		t.Fatal("undo did not send the items again")
	}
}

// "stale" takes the copies of a file that was read again later, and leaves
// the newest.
func TestPruneStaleTakesOutOfDateCopies(t *testing.T) {
	old := ctxview.NewItem(grpResults, "Read /nowhere/a.go", 1, strings.Repeat("old ", 300))
	old.Flags = []string{"read again later: this copy is out of date"}
	fresh := ctxview.NewItem(grpResults, "Read /nowhere/a.go", 2, strings.Repeat("new ", 300))
	personal := ctxview.NewItem(grpResults, "Bash: whoami", 2, strings.Repeat("me ", 300))
	personal.Flags = []string{"personal data: 1 email"}
	hit, kept := pruneMatches([]ContextItem{old, fresh, personal}, " Stale ")
	if len(hit) != 1 || hit[0] != 0 || kept != 0 {
		t.Fatalf("hit %v kept %d", hit, kept)
	}
}

// A session on a message thread sends only what is new, so what a prune
// removes is still in the history the API holds. The next request is asked
// for the conversation whole, which goes out without the pruned items; the
// inspector says how far behind it is until then.
func TestAPruneOnAThreadAsksForTheHistory(t *testing.T) {
	a := &threadAPI{ctx: 90_000}
	s := threadServer(t, a, config.CompactionConfig{Enabled: true, CompactAtTokens: 200_000, WarnAtPercent: 75, WindowMinutes: 60})
	s.removals = ctxview.Open("")
	msg := func(s string) json.RawMessage { return json.RawMessage(s) }
	history := []json.RawMessage{
		msg(`{"role":"user","content":"look at other-repo"}`),
		msg(`{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"Read","input":{"file_path":"/work/other-repo/panel.js"}}]}`),
		msg(`{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"PANEL-SOURCE` + strings.Repeat(" x", 400) + `"}]}`),
	}
	send(t, s, "T", history)                                                            // msg_01
	if rec := continueThread(t, s, "T", "msg_01", toolResult("one")); rec.Code != 200 { // msg_02
		t.Fatalf("a thread nobody asked about goes to the API, got %d", rec.Code)
	}
	rep := s.InspectContext("T")
	if rep == nil || rep.Since != 1 || len(rep.Items) < 3 {
		t.Fatalf("the inspector shows the last whole request and counts the one since: %+v", rep)
	}
	res, err := s.PruneContext("T", "other-repo")
	if err != nil || res.Removed != 1 || !strings.Contains(res.Detail, "1 requests since") {
		t.Fatalf("prune: %+v %v", res, err)
	}
	before, _ := a.turns()
	if rec := continueThread(t, s, "T", "msg_02", toolResult("two")); !askedForHistory(rec) {
		t.Fatalf("want the history asked for, got %d %s", rec.Code, rec.Body)
	}
	if after, _ := a.turns(); after != before {
		t.Fatal("a request Burst refuses must not reach the API")
	}
	send(t, s, "T", append(history[:3:3], msg(`{"role":"assistant","content":"ok"}`), msg(`{"role":"user","content":"go on"}`))) // msg_03
	_, last := a.turns()
	if strings.Contains(last, "PANEL-SOURCE") || !strings.Contains(last, "burst-removed:") {
		t.Fatalf("the history sent whole must go without what was pruned:\n%s", last)
	}
	if rep := s.InspectContext("T"); rep.Since != 0 {
		t.Fatalf("the inspector is up to date again: since %d", rep.Since)
	}
	// Asked once: the thread goes on as any other.
	if rec := continueThread(t, s, "T", "msg_03", toolResult("three")); rec.Code != 200 {
		t.Fatalf("asked for the history twice: %d %s", rec.Code, rec.Body)
	}
}

// After a restart a session on a message thread has sent nothing whole, and
// a helper of its own (a small model, one message) has. The helper must not
// stand for the session: the inspector shows nothing listed and how many
// requests it is behind, until the conversation is next sent whole.
func TestAHelperRequestDoesNotStandForAThreadAfterARestart(t *testing.T) {
	a := &threadAPI{ctx: 90_000}
	s := threadServer(t, a, config.CompactionConfig{Enabled: true, CompactAtTokens: 200_000, WarnAtPercent: 75, WindowMinutes: 60})
	helper, _ := json.Marshal(map[string]any{"model": "claude-haiku-4-5", "max_tokens": 100, "system": strings.Repeat("big ", 50),
		"messages": []any{map[string]any{"role": "user", "content": "name this session"}}})
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/v1/messages", bytes.NewReader(helper))
	req.Header.Set("x-claude-code-session-id", "T")
	req.Header.Set("authorization", "Bearer oauth")
	s.ServeHTTP(httptest.NewRecorder(), req)
	if rec := continueThread(t, s, "T", "msg_from_before_the_restart", toolResult("one")); rec.Code != 200 {
		t.Fatalf("the thread goes on: %d %s", rec.Code, rec.Body)
	}
	rep := s.InspectContext("T")
	if rep == nil || len(rep.Items) != 0 || rep.Since != 1 || rep.Model != "claude-opus-5-5" {
		t.Fatalf("want the thread, nothing listed and one request behind: %v", rep != nil)
	}
	if l := s.InspectSessions(); len(l) != 1 || l[0].Model != "claude-opus-5-5" {
		t.Fatalf("the session is listed by its thread: %+v", l)
	}
	history := []json.RawMessage{json.RawMessage(`{"role":"user","content":"hello` + strings.Repeat(" there", 200) + `"}`), json.RawMessage(`{"role":"assistant","content":"hi"}`), json.RawMessage(`{"role":"user","content":"go on"}`)}
	send(t, s, "T", history)
	if rep := s.InspectContext("T"); rep == nil || rep.Since != 0 || rep.Prompts != 2 {
		t.Fatalf("sent whole, the conversation is what the inspector shows: %v", rep != nil)
	}
}
