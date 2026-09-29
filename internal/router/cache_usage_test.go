package router

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/andrewbakercloudscale/claude-burst/internal/config"
	"github.com/andrewbakercloudscale/claude-burst/internal/metrics"
)

// A long primary session is nearly all cache: Anthropic reports the cached
// context separately, and recording input_tokens alone turned a 150k-token
// turn into "input_tokens": 2 in metrics.jsonl.
func TestSSEUsageRecordsCacheTokens(t *testing.T) {
	var tok tokenUsage
	parseSSEUsage(`data: {"type":"message_start","message":{"usage":{"input_tokens":2,"cache_read_input_tokens":150000,"cache_creation_input_tokens":800,"output_tokens":1}}}`, &tok)
	// message_delta restates usage without the cache fields; that must not zero them.
	parseSSEUsage(`data: {"type":"message_delta","usage":{"output_tokens":244}}`, &tok)
	if tok != (tokenUsage{input: 2, output: 244, cacheRead: 150000, cacheWrite: 800}) {
		t.Fatalf("got %+v", tok)
	}
	if got := usageFromMessageJSON([]byte(`{"usage":{"input_tokens":5,"output_tokens":6,"cache_read_input_tokens":7,"cache_creation_input_tokens":8}}`)); got != (tokenUsage{input: 5, output: 6, cacheRead: 7, cacheWrite: 8}) {
		t.Fatalf("non-streaming: got %+v", got)
	}
}

// OpenAI's prompt_tokens INCLUDES the cached share; Anthropic's input_tokens
// excludes it. Both shapes servers use for the cached count are read.
func TestOpenAIUsageSplitsCachedTokensOutOfPrompt(t *testing.T) {
	for name, raw := range map[string]string{
		"prompt_tokens_details": `{"prompt_tokens":1000,"completion_tokens":50,"prompt_tokens_details":{"cached_tokens":900}}`,
		"top-level":             `{"prompt_tokens":1000,"completion_tokens":50,"cached_tokens":900}`,
	} {
		var u openaiUsage
		if err := json.Unmarshal([]byte(raw), &u); err != nil {
			t.Fatal(err)
		}
		if got := u.tokens(); got != (tokenUsage{input: 100, output: 50, cacheRead: 900}) {
			t.Fatalf("%s: got %+v", name, got)
		}
	}
	var none openaiUsage
	_ = json.Unmarshal([]byte(`{"prompt_tokens":1000,"completion_tokens":50}`), &none)
	if got := none.tokens(); got != (tokenUsage{input: 1000, output: 50}) {
		t.Fatalf("no cache info: got %+v", got)
	}
}

func TestOpenAIStreamReportsCacheReadToClient(t *testing.T) {
	up := "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
		"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":1000,\"completion_tokens\":5,\"prompt_tokens_details\":{\"cached_tokens\":600}}}\n\n" +
		"data: [DONE]\n\n"
	rr := httptest.NewRecorder()
	tok, err := translateOpenAIStream(rr, strings.NewReader(up), "claude-opus-5-5")
	if err != nil {
		t.Fatal(err)
	}
	if tok != (tokenUsage{input: 400, output: 5, cacheRead: 600}) {
		t.Fatalf("got %+v", tok)
	}
	if !strings.Contains(rr.Body.String(), `"cache_read_input_tokens":600`) {
		t.Fatalf("client's message_delta must carry the cached share:\n%s", rr.Body.String())
	}
}

func TestCacheRates(t *testing.T) {
	claude := config.ModelPrice{InputPerMTok: 10, OutputPerMTok: 50}
	if r, w := claude.CacheRates("claude-opus-5"); r != 1 || w != 12.5 {
		t.Fatalf("claude defaults: read=%v write=%v, want 1 / 12.5", r, w)
	}
	// An unknown discount is priced as none: overstated, never hidden.
	other := config.ModelPrice{InputPerMTok: 1.4, OutputPerMTok: 4.4}
	if r, w := other.CacheRates("zai-org/GLM-5.3"); r != 1.4 || w != 1.4 {
		t.Fatalf("non-claude defaults: read=%v write=%v, want full input rate", r, w)
	}
	explicit := config.ModelPrice{InputPerMTok: 1.4, CacheReadPerMTok: 0.3, CacheWritePerMTok: 2}
	if r, w := explicit.CacheRates("zai-org/GLM-5.3"); r != 0.3 || w != 2 {
		t.Fatalf("explicit: read=%v write=%v", r, w)
	}
}

func TestWriteMetricRecordsCacheAndPruning(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "metrics.jsonl")
	s, err := New(config.Default(), filepath.Join(dir, "state.json"), path, log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "http://local/v1/messages", nil)
	s.writeMetric(req, "primary", "anthropic", "claude-opus-5", "claude-opus-5", 200, time.Now(),
		tokenUsage{input: 1_000_000, cacheRead: 1_000_000, cacheWrite: 1_000_000, prunedBytes: 9, prunedResults: 2, truncatedResults: 1,
			repeatedCalls: 3, rerunsAfterStub: 1}, "", 0, "", "")
	b, _ := os.ReadFile(path)
	var e metrics.Event
	if err := json.Unmarshal(b, &e); err != nil {
		t.Fatal(err)
	}
	// opus-5 at $5/MTok: 5 input + 0.5 cache read + 6.25 cache write.
	if e.CacheReadTokens != 1_000_000 || e.CacheWriteTokens != 1_000_000 || e.APIEquivalentUSD != 11.75 {
		t.Fatalf("cache: %+v", e)
	}
	if e.PrunedBytes != 9 || e.PrunedToolResults != 2 || e.TruncatedToolResults != 1 || e.RepeatedCalls != 3 || e.RerunsAfterStub != 1 {
		t.Fatalf("pruning: %+v", e)
	}
}

// Prepare prunes before translating and hands the stats to the router on
// the outbound request's context; a primary built by buildProvider never
// has pruning switched on.
func TestPreparePrunesSecondaryOnly(t *testing.T) {
	t.Setenv("TOGETHER_API_KEY", "k")
	base, _ := url.Parse("https://api.together.xyz/v1")
	body := conversation(t, 25, 5000)
	in := httptest.NewRequest(http.MethodPost, "http://local/v1/messages", nil)

	off := NewOpenAICompatibleProvider("together", base, "zai-org/GLM-5.3", nil, "claude-burst-prune-test", "TOGETHER_API_KEY")
	req, _, err := off.Prepare(in.Context(), in, body)
	if err != nil {
		t.Fatal(err)
	}
	if st := pruneStatsFrom(req.Context()); st.bytesRemoved != 0 {
		t.Fatalf("a provider without pruning switched on must not prune: %+v", st)
	}

	on := NewOpenAICompatibleProvider("together", base, "zai-org/GLM-5.3", nil, "claude-burst-prune-test", "TOGETHER_API_KEY")
	on.setPruning(config.PruneConfig{})
	req, _, err = on.Prepare(in.Context(), in, body)
	if err != nil {
		t.Fatal(err)
	}
	if st := pruneStatsFrom(req.Context()); st.stubbed != 10 {
		t.Fatalf("stats did not reach the outbound request: %+v", st)
	}
	sent, _ := io.ReadAll(req.Body)
	if !strings.Contains(string(sent), "was removed to save overflow cost") {
		t.Fatal("the translated body sent upstream must be the pruned one")
	}
}

// Opus 5.5 is what Claude Code runs by default here; before it was priced,
// every one of its turns recorded pricing_unknown and $0.
func TestDefaultPricingCoversCurrentModels(t *testing.T) {
	p := config.Default().Pricing
	o, ok := p["claude-opus-5-5"]
	if !ok || o.InputPerMTok != 4 || o.OutputPerMTok != 20 {
		t.Fatalf("claude-opus-5-5 = %+v, %v", o, ok)
	}
	// Anthropic lists $0.20 for Opus 5.5 cache reads; the 0.1x default would
	// say $0.40 and double the dominant cost of every long session.
	if r, w := o.CacheRates("claude-opus-5-5"); r != 0.20 || w != 5 {
		t.Fatalf("opus 5.5 cache read/write = %v / %v, want 0.20 / 5", r, w)
	}
	if r, _ := p["claude-fable-5-1"].CacheRates("claude-fable-5-1"); r != 0.25 {
		t.Fatalf("fable 5.1 cache read = %v, want 0.25", r)
	}
}
