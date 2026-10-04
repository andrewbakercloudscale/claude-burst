package router

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/andrewbakercloudscale/claude-burst/internal/config"
)

func TestInspectContextItemsAndFlags(t *testing.T) {
	f := &fakeAnthropic{context: 1000}
	s := compactServer(t, f, config.CompactionConfig{})
	reminder := "<system-reminder>\\nContents of /nowhere/CLAUDE.md (project instructions):\\n\\nUse tabs.\\n\\nContents of /nowhere/other/CLAUDE.md (project instructions):\\n\\nNo dashes.\\n</system-reminder>"
	msg := func(s string) json.RawMessage { return json.RawMessage(s) }
	history := []json.RawMessage{
		msg(`{"role":"user","content":[{"type":"text","text":"` + reminder + `"},{"type":"text","text":"fix the bug"}]}`),
		msg(`{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"Read","input":{"file_path":"/nowhere/a.go"}}]}`),
		msg(`{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"` + strings.Repeat("x", 30000) + ` card 4111 1111 1111 1111"}]}`),
	}
	for i := 0; i < 11; i++ {
		history = append(history, msg(fmt.Sprintf(`{"role":"assistant","content":"done %d"}`, i)), msg(fmt.Sprintf(`{"role":"user","content":"next %d"}`, i)))
	}
	history = append(history,
		msg(`{"role":"assistant","content":[{"type":"tool_use","id":"t2","name":"Read","input":{"file_path":"/nowhere/a.go"}}]}`),
		msg(`{"role":"user","content":[{"type":"tool_result","tool_use_id":"t2","content":"short"}]}`))
	send(t, s, "S9", history)

	if got := s.InspectSessions(); len(got) != 1 || got[0].Session != "S9" {
		t.Fatalf("sessions = %+v", got)
	}
	rep := s.InspectContext("S9")
	if rep == nil {
		t.Fatal("no report")
	}
	if rep.Prompts != 12 {
		t.Errorf("prompts = %d, want 12", rep.Prompts)
	}
	by := map[string]ContextItem{}
	for _, it := range rep.Items {
		if _, seen := by[it.Group+"|"+it.Name]; !seen {
			by[it.Group+"|"+it.Name] = it
		}
	}
	if it, ok := by["Instruction files|/nowhere/CLAUDE.md"]; !ok || it.Preview != "Use tabs." {
		t.Errorf("instruction file = %+v", it)
	}
	if _, ok := by["Instruction files|/nowhere/other/CLAUDE.md"]; !ok {
		t.Error("second instruction file missing")
	}
	old := by["Tool results|Read /nowhere/a.go"]
	flags := strings.Join(old.Flags, "; ")
	for _, want := range []string{"large and 11 prompts old", "read again later", "file no longer exists", "personal data: 1 credit card number"} {
		if !strings.Contains(flags, want) {
			t.Errorf("old read flags %q lack %q", flags, want)
		}
	}
	if rep.Flagged < 3 {
		t.Errorf("flagged = %d", rep.Flagged)
	}
	full, ok := s.InspectItem("S9", 0)
	if !ok || full == "" {
		t.Error("no full text for item 0")
	}
}
