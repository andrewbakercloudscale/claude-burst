package router

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/andrewbakercloudscale/claude-burst/internal/config"
)

func TestAutomaskMasksWhatIsSentAndSaysSo(t *testing.T) {
	f := &fakeAnthropic{context: 1000}
	s := compactServer(t, f, config.CompactionConfig{})
	history := []json.RawMessage{
		json.RawMessage(`{"role":"user","content":"my card is 4111 1111 1111 1111"}`),
		json.RawMessage(`{"role":"assistant","content":[{"type":"thinking","thinking":"4111111111111111","signature":"s1"},{"type":"tool_use","id":"t1","name":"Read","input":{"p":"4111111111111111"}}]}`),
		json.RawMessage(`{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":[{"type":"text","text":"ID 8001015009087"}]},{"type":"text","text":"thanks","cache_control":{"type":"ephemeral"}}]}`),
	}

	// Off by default: sent as is.
	send(t, s, "S1", history)
	if !strings.Contains(f.last(), "4111 1111 1111 1111") {
		t.Fatal("masked while automask is off")
	}

	s.SetAutomask(config.AutomaskConfig{Enabled: true})
	send(t, s, "S1", history)
	got := f.last()
	for _, want := range []string{"my card is [CARD-1 ...1111]", "ID [SAID-1]", `"cache_control":{"type":"ephemeral"}`,
		`"thinking":"4111111111111111"`, `"input":{"p":"4111111111111111"}`} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %s in %s", want, got)
		}
	}
	if strings.Contains(got, "8001015009087") || strings.Contains(got, "4111 1111") {
		t.Errorf("sent unmasked: %s", got)
	}
	lines := s.PromptNotices("S1", false)
	if len(lines) != 1 || !strings.Contains(lines[0], "masked 1 credit card number and 1 SA ID number") {
		t.Fatalf("notices = %q", lines)
	}

	// The same history again: the same masks, from the cache, and no new notice.
	send(t, s, "S1", history)
	if f.last() != got {
		t.Errorf("second send differs:\n%s\n%s", f.last(), got)
	}
	if lines := s.PromptNotices("S1", false); len(lines) != 0 {
		t.Errorf("repeated notice %q", lines)
	}
	if n := s.AutomaskTotals()["card"]; n != 1 {
		t.Errorf("card total = %d", n)
	}

	// A rule switched off is left alone from the next request.
	s.SetAutomask(config.AutomaskConfig{Enabled: true, Rules: map[string]bool{"card": false}})
	send(t, s, "S1", history)
	if !strings.Contains(f.last(), "4111 1111 1111 1111") || strings.Contains(f.last(), "8001015009087") {
		t.Errorf("rule switch not applied: %s", f.last())
	}
}
