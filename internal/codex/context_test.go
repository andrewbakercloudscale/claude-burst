package codex

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/andrewbakercloudscale/claude-burst/internal/ctxview"
)

const codexBody = `{"model":"gpt-6.1-sol","input":[
 {"type":"additional_tools","role":"developer","tools":[{"type":"namespace","name":"functions","tools":[{"type":"custom","name":"exec"}]}]},
 {"type":"message","role":"developer","content":[{"type":"input_text","text":"You are Codex, an agent."},{"type":"input_text","text":"<skills_instructions>SKILLS-LIST</skills_instructions>"}]},
 {"type":"message","role":"user","content":[{"type":"input_text","text":"# AGENTS.md instructions for /repo\n\n<INSTRUCTIONS>AGENTS-RULES</INSTRUCTIONS>"},{"type":"input_text","text":"<environment_context>\n  <cwd>/repo</cwd>\n</environment_context>"}]},
 {"type":"message","role":"user","content":[{"type":"input_text","text":"fix the bug"}]},
 {"type":"reasoning","summary":[],"encrypted_content":"gAAAA"},
 {"type":"function_call","name":"shell","arguments":"{\"command\":[\"cat\",\"big.txt\"]}","call_id":"c1"},
 {"type":"function_call_output","call_id":"c1","output":"BIG-OUTPUT"},
 {"type":"message","role":"assistant","content":[{"type":"output_text","text":"done"}]},
 {"type":"message","role":"user","content":[{"type":"input_text","text":"thanks"}]}
]}`

func TestCodexContextItems(t *testing.T) {
	items, prompts := ContextItems([]byte(codexBody))
	if prompts != 2 {
		t.Errorf("prompts = %d, want 2", prompts)
	}
	got := map[string]ctxview.Item{}
	for _, it := range items {
		got[it.Group+"|"+it.Name] = it
	}
	for _, k := range []string{"Built-in tools|functions: 1 tools", "System prompt|Codex's instructions", "Skills|skills_instructions",
		"Instruction files|/repo/AGENTS.md", "Other reminders|environment_context", "Your prompts|Prompt",
		"Reasoning (encrypted)|Reasoning", "Codex's replies|Call: shell: cat big.txt", "Tool results|shell: cat big.txt", "Codex's replies|Reply"} {
		if _, ok := got[k]; !ok {
			t.Errorf("missing %s; have %v", k, keys(got))
		}
	}
	if !got["Tool results|shell: cat big.txt"].Removable || got["Your prompts|Prompt"].Removable || got["System prompt|Codex's instructions"].Removable {
		t.Error("removable: only tool outputs, instructions, skills and reminders")
	}
	if cwdName([]byte(codexBody)) != "repo" {
		t.Errorf("repo = %q", cwdName([]byte(codexBody)))
	}
}

func keys(m map[string]ctxview.Item) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

// Removed items leave the request as notes, nothing else changes, the
// inspector shows them removed, and Restore sends them again.
func TestCodexRemoveAndRestore(t *testing.T) {
	var mu sync.Mutex
	var last string
	g, _, gw := newTestGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		last = string(b)
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, completedSSE)
	}))
	turn := func() string {
		req, _ := http.NewRequest("POST", gw.URL+"/backend-api/codex/responses", strings.NewReader(codexBody))
		req.Header.Set("Session-Id", "S")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		mu.Lock()
		defer mu.Unlock()
		return last
	}
	if got := turn(); got != codexBody {
		t.Fatal("with nothing removed the body must pass byte for byte")
	}
	rep := g.InspectContext("S", 0)
	if rep == nil || rep.Repo != "repo" {
		t.Fatalf("report = %+v", rep)
	}
	for _, it := range rep.Items {
		if it.Name == "/repo/AGENTS.md" || it.Name == "shell: cat big.txt" {
			g.Removals().Add(RemovalKey("S"), ctxview.Removal{ID: it.ID, Group: it.Group, Name: it.Name, Bytes: it.Bytes})
		}
	}
	got := turn()
	if strings.Contains(got, "AGENTS-RULES") || strings.Contains(got, "BIG-OUTPUT") {
		t.Fatal("removed content was sent")
	}
	var top struct {
		Input []json.RawMessage `json:"input"`
	}
	if json.Unmarshal([]byte(got), &top) != nil || len(top.Input) != 9 || !strings.Contains(got, `"call_id":"c1"`) ||
		!strings.Contains(got, "gAAAA") || !strings.Contains(got, "fix the bug") || strings.Count(got, "burst-removed:") != 2 {
		t.Fatalf("edited body wrong: %s", got)
	}
	removed := 0
	for _, it := range g.InspectContext("S", 0).Items {
		if it.Removed {
			removed++
		}
	}
	if removed != 2 {
		t.Fatalf("inspector shows %d removed", removed)
	}
	for _, r := range g.Removals().For(RemovalKey("S")) {
		g.Removals().Restore(RemovalKey("S"), r.ID)
	}
	if turn() != codexBody {
		t.Fatal("restore did not send the original again")
	}
}
