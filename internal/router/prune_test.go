package router

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"strings"
	"testing"

	"github.com/andrewbakercloudscale/claude-burst/internal/config"
)

// conversation builds an Anthropic request with n tool_use/tool_result
// turns, each result carrying size bytes of output.
func conversation(t *testing.T, n, size int) []byte {
	t.Helper()
	msgs := []any{map[string]any{"role": "user", "content": "start"}}
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("toolu_%d", i)
		msgs = append(msgs,
			map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "tool_use", "id": id, "name": "Bash", "input": map[string]any{"command": "ls"}}}},
			map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": id, "content": fmt.Sprintf("%03d", i) + strings.Repeat("x", size-3)}}},
		)
	}
	b, err := json.Marshal(map[string]any{"model": "claude-opus-5-5", "messages": msgs})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func resultContents(t *testing.T, body []byte) []string {
	t.Helper()
	var req struct {
		Messages []struct {
			Content any `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, m := range req.Messages {
		blocks, _ := m.Content.([]any)
		for _, b := range blocks {
			if bm, _ := b.(map[string]any); bm["type"] == "tool_result" {
				out = append(out, flattenTextContent(bm["content"]))
			}
		}
	}
	return out
}

func defaultPolicy(t *testing.T) prunePolicy {
	t.Helper()
	p, on := newPrunePolicy(config.PruneConfig{})
	if !on {
		t.Fatal("pruning must be on when not explicitly disabled")
	}
	return p
}

func TestPruneStubsOldResultsAndKeepsRecent(t *testing.T) {
	p := defaultPolicy(t)
	out, st := pruneAnthropicRequest(conversation(t, 25, 5000), p)
	got := resultContents(t, out)
	// 25 results, keep 10, step 10: boundary = (25-10)/10*10 = 10.
	for i, c := range got {
		stubbed := strings.HasPrefix(c, "[claude-burst: this earlier tool output")
		if stubbed != (i < 10) {
			t.Fatalf("result %d stubbed=%v, want %v", i, stubbed, i < 10)
		}
	}
	if st.stubbed != 10 || st.bytesRemoved != 10*5000 {
		t.Fatalf("stats = %+v, want 10 stubbed / 50000 bytes", st)
	}
}

// The boundary moves in steps so the pruned prefix stays byte-identical
// across turns. If it advanced every turn, the start of every overflow
// request would differ from the last and no provider-side prefix cache
// could ever hit.
func TestPruneBoundaryMovesInStepsSoThePrefixIsStable(t *testing.T) {
	p := defaultPolicy(t)
	prefix := func(n int) string {
		out, _ := pruneAnthropicRequest(conversation(t, n, 5000), p)
		return strings.Join(resultContents(t, out)[:20], "|")
	}
	base := prefix(30) // boundary 20
	for n := 31; n < 40; n++ {
		if prefix(n) != base {
			t.Fatalf("prefix changed between 30 and %d results; boundary must only move every %d", n, p.step)
		}
	}
	out, _ := pruneAnthropicRequest(conversation(t, 40, 5000), p) // boundary 30
	if c := resultContents(t, out)[25]; !strings.HasPrefix(c, "[claude-burst") {
		t.Fatalf("at 40 results the boundary must have stepped to 30; result 25 = %.40q", c)
	}
}

func TestPruneLeavesSmallOldResultsAlone(t *testing.T) {
	p := defaultPolicy(t)
	in := conversation(t, 25, 200)
	out, st := pruneAnthropicRequest(in, p)
	if st.stubbed != 0 || string(out) != string(in) {
		t.Fatalf("results under stub_min_bytes must be untouched and the body returned as-is: %+v", st)
	}
}

func TestPruneTruncatesOversizedRecentResultKeepingHeadAndTail(t *testing.T) {
	p := defaultPolicy(t)
	big := "HEAD" + strings.Repeat("m", 200_000) + "TAIL"
	body, _ := json.Marshal(map[string]any{"messages": []any{
		map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": "t", "content": big}}},
	}})
	out, st := pruneAnthropicRequest(body, p)
	c := resultContents(t, out)[0]
	if !strings.HasPrefix(c, "HEAD") || !strings.HasSuffix(c, "TAIL") || !strings.Contains(c, "cut from the middle") {
		t.Fatalf("truncation must keep head and tail and say so; got %d bytes", len(c))
	}
	if len(c) > p.maxBytes+200 || st.truncated != 1 || st.bytesRemoved != int64(len(big)-p.maxBytes) {
		t.Fatalf("len=%d stats=%+v", len(c), st)
	}
}

func TestPruneTruncationNeverSplitsARune(t *testing.T) {
	p := prunePolicy{keepRecent: 10, step: 10, stubMinBytes: 1024, maxBytes: 11}
	body, _ := json.Marshal(map[string]any{"messages": []any{
		map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": "t", "content": strings.Repeat("é", 50)}}},
	}})
	out, _ := pruneAnthropicRequest(body, p)
	if c := resultContents(t, out)[0]; !json.Valid(out) || strings.ContainsRune(c, '�') {
		t.Fatalf("truncation produced invalid UTF-8: %q", c)
	}
}

func TestPruneDisabledAndUnparseableBodiesPassThrough(t *testing.T) {
	if _, on := newPrunePolicy(config.PruneConfig{Disabled: true}); on {
		t.Fatal("Disabled must turn pruning off")
	}
	junk := []byte("not json")
	if out, _ := pruneAnthropicRequest(junk, defaultPolicy(t)); string(out) != "not json" {
		t.Fatal("an unparseable body must be returned unchanged, never failed")
	}
}

func TestPruneEachTechniqueSwitchesOffAlone(t *testing.T) {
	body := conversation(t, 25, 5000)
	big, _ := json.Marshal(map[string]any{"messages": []any{
		map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": "t", "content": strings.Repeat("m", 100_000)}}},
	}})

	noStub, on := newPrunePolicy(config.PruneConfig{NoStub: true})
	if !on {
		t.Fatal("capping alone is still pruning")
	}
	if _, st := pruneAnthropicRequest(body, noStub); st.stubbed != 0 {
		t.Fatalf("NoStub still stubbed: %+v", st)
	}
	if _, st := pruneAnthropicRequest(big, noStub); st.truncated != 1 {
		t.Fatalf("NoStub must leave capping on: %+v", st)
	}

	noCap, _ := newPrunePolicy(config.PruneConfig{NoCap: true})
	if _, st := pruneAnthropicRequest(big, noCap); st.truncated != 0 {
		t.Fatalf("NoCap still truncated: %+v", st)
	}
	if _, on := newPrunePolicy(config.PruneConfig{NoStub: true, NoCap: true}); on {
		t.Fatal("both techniques off must mean pruning off")
	}
}

// The admin page's toggle reaches the running secondary without a restart,
// and never reaches a primary.
func TestSetSecondaryPruningIsLive(t *testing.T) {
	t.Setenv("TOGETHER_API_KEY", "k")
	cfg := config.Default()
	cfg.Secondary = config.RouteConfig{Provider: "openai-compatible", BaseURL: "https://api.together.xyz/v1", Model: "zai-org/GLM-5.3", KeychainService: "claude-burst-prune-live-test"}
	cfg.ResolveRoutes()
	dir := t.TempDir()
	s, err := New(cfg, dir+"/state.json", dir+"/metrics.jsonl", log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	op := s.secondary.(*OpenAICompatibleProvider)
	if op.prune.Load() == nil {
		t.Fatal("pruning must be on at startup by default")
	}
	if !s.SetSecondaryPruning(config.PruneConfig{Disabled: true}) || op.prune.Load() != nil {
		t.Fatal("disabling did not reach the running provider")
	}
	if !s.SetSecondaryPruning(config.PruneConfig{KeepRecent: 3}) || op.prune.Load().keepRecent != 3 {
		t.Fatal("re-enabling with settings did not reach the running provider")
	}
	if pp, ok := s.primary.(*OpenAICompatibleProvider); ok && pp.prune.Load() != nil {
		t.Fatal("the primary must never be pruned")
	}
}

// withLastCall appends a final assistant turn calling Bash with command,
// plus its result, to a conversation built by conversation(). Every earlier
// call in conversation() is Bash {"command":"ls"}, so a final call with
// "ls" repeats all of them and anything else repeats none.
func withLastCall(t *testing.T, body []byte, command string) []byte {
	t.Helper()
	var req map[string]any
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatal(err)
	}
	msgs := req["messages"].([]any)
	msgs = append(msgs,
		map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "tool_use", "id": "toolu_last", "name": "Bash", "input": map[string]any{"command": command}}}},
		map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": "toolu_last", "content": "ok"}}},
	)
	req["messages"] = msgs
	out, _ := json.Marshal(req)
	return out
}

// The quality signal for stubbing: when the model's latest tool call asks
// again for exactly what a stub removed, pruning cost a round trip instead
// of saving one. Repeats of calls whose output is still visible are the
// base rate the model repeats itself anyway, counted separately so the two
// can be compared.
func TestPruneCountsRerunsOfStubbedCalls(t *testing.T) {
	p := defaultPolicy(t)

	// 25 earlier "ls" calls: the first 10 are stubbed, so repeating "ls"
	// asks again for stubbed output.
	_, st := pruneAnthropicRequest(withLastCall(t, conversation(t, 25, 5000), "ls"), p)
	if st.rerunsAfterStub != 1 || st.repeatedCalls != 1 {
		t.Fatalf("repeat of a stubbed call: %+v, want rerunsAfterStub=1 repeatedCalls=1", st)
	}

	_, st = pruneAnthropicRequest(withLastCall(t, conversation(t, 25, 5000), "git status"), p)
	if st.rerunsAfterStub != 0 || st.repeatedCalls != 0 {
		t.Fatalf("a new call is not a rerun: %+v", st)
	}

	// Only 5 earlier calls: nothing stubbed, so a repeat is a plain repeat.
	_, st = pruneAnthropicRequest(withLastCall(t, conversation(t, 5, 5000), "ls"), p)
	if st.rerunsAfterStub != 0 || st.repeatedCalls != 1 {
		t.Fatalf("repeat with nothing stubbed: %+v, want only repeatedCalls=1", st)
	}
}
