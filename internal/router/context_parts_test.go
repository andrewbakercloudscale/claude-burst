package router

import (
	"encoding/json"
	"testing"
)

func TestContextPartsAddUpToTheReportedContext(t *testing.T) {
	body := []byte(`{
		"system": [{"type":"text","text":"` + pad(400) + `"}],
		"tools": [{"name":"Bash","input_schema":{}}, {"name":"mcp__chrome__navigate","description":"` + pad(200) + `"}],
		"messages": [
			{"role":"user","content":[{"type":"text","text":"<system-reminder>Contents of /x/CLAUDE.md ` + pad(300) + `</system-reminder>"},{"type":"text","text":"hi"}]},
			{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"Bash","input":{}}]},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"` + pad(1000) + `"}]},
			{"role":"user","content":"plain string message"}
		]}`)
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil {
		t.Fatal(err)
	}
	var msgs []json.RawMessage
	_ = json.Unmarshal(top["messages"], &msgs)

	parts := scaleParts(contextBytes(top, msgs), 50000)
	var sum int64
	got := map[string]int64{}
	for _, p := range parts {
		sum += p.Tokens
		got[p.Name] = p.Tokens
	}
	if sum != 50000 {
		t.Fatalf("parts add up to %d, want 50000: %+v", sum, parts)
	}
	for _, name := range contextPartNames {
		if got[name] <= 0 {
			t.Errorf("%s missing or empty: %+v", name, parts)
		}
	}
	if got["Tool results"] <= got["Memory files"] || got["System prompt"] <= got["System tools"] {
		t.Errorf("proportions wrong: %+v", parts)
	}
}

func TestScalePartsWithNothingMeasured(t *testing.T) {
	if p := scaleParts(make([]int64, len(contextPartNames)), 1000); p != nil {
		t.Fatalf("want nil, got %+v", p)
	}
}

func pad(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = 'a'
	}
	return string(b)
}
