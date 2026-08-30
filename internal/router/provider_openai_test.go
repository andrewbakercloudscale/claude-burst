package router

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func decodeOAIRequest(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var v map[string]any
	if err := json.Unmarshal(body, &v); err != nil {
		t.Fatalf("translated request is not valid JSON: %v\n%s", err, body)
	}
	return v
}

func TestTranslateRequestSimpleTextMessage(t *testing.T) {
	in := `{"model":"claude-sonnet-5","max_tokens":100,"messages":[{"role":"user","content":"hello"}]}`
	out, err := translateAnthropicRequest([]byte(in), "zai-org/GLM-5.3")
	if err != nil {
		t.Fatal(err)
	}
	v := decodeOAIRequest(t, out)
	if v["model"] != "zai-org/GLM-5.3" {
		t.Fatalf("model not remapped: %v", v["model"])
	}
	msgs, ok := v["messages"].([]any)
	if !ok || len(msgs) != 1 {
		t.Fatalf("expected 1 message, got %v", v["messages"])
	}
	m := msgs[0].(map[string]any)
	if m["role"] != "user" || m["content"] != "hello" {
		t.Fatalf("got %v", m)
	}
	if v["max_tokens"].(float64) != 100 {
		t.Fatalf("max_tokens not passed through: %v", v["max_tokens"])
	}
}

func TestTranslateRequestWithSystemPrompt(t *testing.T) {
	in := `{"model":"claude-sonnet-5","system":"you are helpful","messages":[{"role":"user","content":"hi"}]}`
	out, err := translateAnthropicRequest([]byte(in), "zai-org/GLM-5.3")
	if err != nil {
		t.Fatal(err)
	}
	v := decodeOAIRequest(t, out)
	msgs := v["messages"].([]any)
	first := msgs[0].(map[string]any)
	if first["role"] != "system" || first["content"] != "you are helpful" {
		t.Fatalf("system prompt not translated to leading system message: %v", first)
	}
}

func TestTranslateRequestWithToolsAndToolChoice(t *testing.T) {
	in := `{"model":"claude-sonnet-5","tool_choice":{"type":"tool","name":"Bash"},
	  "tools":[{"name":"Bash","description":"run a shell command","input_schema":{"type":"object","properties":{"command":{"type":"string"}}}}],
	  "messages":[{"role":"user","content":"list files"}]}`
	out, err := translateAnthropicRequest([]byte(in), "zai-org/GLM-5.3")
	if err != nil {
		t.Fatal(err)
	}
	v := decodeOAIRequest(t, out)
	tools, ok := v["tools"].([]any)
	if !ok || len(tools) != 1 {
		t.Fatalf("expected 1 tool, got %v", v["tools"])
	}
	tool := tools[0].(map[string]any)
	if tool["type"] != "function" {
		t.Fatalf("got %v", tool)
	}
	fn := tool["function"].(map[string]any)
	if fn["name"] != "Bash" || fn["parameters"] == nil {
		t.Fatalf("tool not translated correctly: %v", fn)
	}
	tc := v["tool_choice"].(map[string]any)
	if tc["type"] != "function" {
		t.Fatalf("tool_choice not translated: %v", v["tool_choice"])
	}
}

func TestTranslateRequestToolResultSplitsIntoToolMessages(t *testing.T) {
	in := `{"model":"claude-sonnet-5","messages":[
	  {"role":"assistant","content":[{"type":"tool_use","id":"call_1","name":"Bash","input":{"command":"ls"}}]},
	  {"role":"user","content":[{"type":"tool_result","tool_use_id":"call_1","content":"file1\nfile2"}]}
	]}`
	out, err := translateAnthropicRequest([]byte(in), "zai-org/GLM-5.3")
	if err != nil {
		t.Fatal(err)
	}
	v := decodeOAIRequest(t, out)
	msgs := v["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("expected 2 messages (assistant tool_call + tool result), got %d: %v", len(msgs), msgs)
	}
	assistant := msgs[0].(map[string]any)
	toolCalls, ok := assistant["tool_calls"].([]any)
	if !ok || len(toolCalls) != 1 {
		t.Fatalf("expected 1 tool_call on assistant message, got %v", assistant)
	}
	tc := toolCalls[0].(map[string]any)
	fn := tc["function"].(map[string]any)
	if fn["name"] != "Bash" {
		t.Fatalf("got %v", fn)
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(fn["arguments"].(string)), &args); err != nil {
		t.Fatalf("arguments not a JSON string: %v", fn["arguments"])
	}
	if args["command"] != "ls" {
		t.Fatalf("got %v", args)
	}

	toolMsg := msgs[1].(map[string]any)
	if toolMsg["role"] != "tool" || toolMsg["tool_call_id"] != "call_1" || toolMsg["content"] != "file1\nfile2" {
		t.Fatalf("tool_result not translated correctly: %v", toolMsg)
	}
}

func TestTranslateRequestAssistantTextAndToolUseCombined(t *testing.T) {
	in := `{"model":"claude-sonnet-5","messages":[
	  {"role":"assistant","content":[{"type":"text","text":"Let me check"},{"type":"tool_use","id":"call_1","name":"Bash","input":{"command":"ls"}}]}
	]}`
	out, err := translateAnthropicRequest([]byte(in), "zai-org/GLM-5.3")
	if err != nil {
		t.Fatal(err)
	}
	v := decodeOAIRequest(t, out)
	msgs := v["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("expected exactly 1 combined assistant message, got %d: %v", len(msgs), msgs)
	}
	m := msgs[0].(map[string]any)
	if m["content"] != "Let me check" {
		t.Fatalf("got content=%v", m["content"])
	}
	if _, ok := m["tool_calls"].([]any); !ok {
		t.Fatalf("expected tool_calls alongside text: %v", m)
	}
}

func TestMapFinishReason(t *testing.T) {
	cases := map[string]string{
		"stop": "end_turn", "length": "max_tokens", "tool_calls": "tool_use",
		"content_filter": "end_turn", "something_unrecognized": "end_turn",
	}
	for in, want := range cases {
		if got := mapFinishReason(in); got != want {
			t.Fatalf("mapFinishReason(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestTranslateOpenAINonStreamText(t *testing.T) {
	body := `{"choices":[{"message":{"content":"hello there"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`
	rr := httptest.NewRecorder()
	tok, err := translateOpenAINonStream(rr, strings.NewReader(body), "claude-sonnet-5")
	if err != nil {
		t.Fatal(err)
	}
	if tok.input != 10 || tok.output != 5 {
		t.Fatalf("got tok=%+v", tok)
	}
	var resp map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp["role"] != "assistant" || resp["model"] != "claude-sonnet-5" || resp["stop_reason"] != "end_turn" {
		t.Fatalf("got %v", resp)
	}
	content := resp["content"].([]any)
	if len(content) != 1 || content[0].(map[string]any)["type"] != "text" || content[0].(map[string]any)["text"] != "hello there" {
		t.Fatalf("got content=%v", content)
	}
}

func TestTranslateOpenAINonStreamToolCalls(t *testing.T) {
	body := `{"choices":[{"message":{"content":"","tool_calls":[
	  {"id":"call_1","function":{"name":"Bash","arguments":"{\"command\":\"ls\"}"}},
	  {"id":"call_2","function":{"name":"Read","arguments":"{\"path\":\"/tmp\"}"}}
	]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":20,"completion_tokens":8}}`
	rr := httptest.NewRecorder()
	_, err := translateOpenAINonStream(rr, strings.NewReader(body), "claude-sonnet-5")
	if err != nil {
		t.Fatal(err)
	}
	var resp map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp["stop_reason"] != "tool_use" {
		t.Fatalf("got stop_reason=%v", resp["stop_reason"])
	}
	content := resp["content"].([]any)
	if len(content) != 2 {
		t.Fatalf("expected 2 tool_use blocks (parallel calls), got %d: %v", len(content), content)
	}
	first := content[0].(map[string]any)
	if first["type"] != "tool_use" || first["name"] != "Bash" || first["id"] != "call_1" {
		t.Fatalf("got %v", first)
	}
	input := first["input"].(map[string]any)
	if input["command"] != "ls" {
		t.Fatalf("arguments not parsed back into input object: %v", input)
	}
}

// synthOAISSE builds a synthetic OpenAI-style SSE stream from raw JSON chunk
// strings, terminated with [DONE].
func synthOAISSE(chunks ...string) string {
	var sb strings.Builder
	for _, c := range chunks {
		sb.WriteString("data: ")
		sb.WriteString(c)
		sb.WriteString("\n\n")
	}
	sb.WriteString("data: [DONE]\n\n")
	return sb.String()
}

// parseAnthropicSSE extracts the sequence of "event:" names and matching
// "data:" JSON payloads written by the translator, for assertions.
func parseAnthropicSSE(t *testing.T, raw string) []map[string]any {
	t.Helper()
	var events []map[string]any
	lines := strings.Split(raw, "\n")
	var currentEvent string
	for _, line := range lines {
		switch {
		case strings.HasPrefix(line, "event: "):
			currentEvent = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			var v map[string]any
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &v); err != nil {
				t.Fatalf("invalid JSON in SSE data line: %v\n%s", err, line)
			}
			v["__event"] = currentEvent
			events = append(events, v)
		}
	}
	return events
}

func TestTranslateStreamingTextOnly(t *testing.T) {
	stream := synthOAISSE(
		`{"choices":[{"delta":{"role":"assistant"},"finish_reason":null}]}`,
		`{"choices":[{"delta":{"content":"Hello"},"finish_reason":null}]}`,
		`{"choices":[{"delta":{"content":" world"},"finish_reason":null}]}`,
		`{"choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2}}`,
	)
	rr := httptest.NewRecorder()
	tok, err := translateOpenAIStream(rr, strings.NewReader(stream), "claude-sonnet-5")
	if err != nil {
		t.Fatal(err)
	}
	if tok.input != 3 || tok.output != 2 {
		t.Fatalf("got tok=%+v", tok)
	}
	events := parseAnthropicSSE(t, rr.Body.String())
	seq := make([]string, len(events))
	for i, e := range events {
		seq[i] = e["__event"].(string)
	}
	want := []string{"message_start", "content_block_start", "content_block_delta", "content_block_delta", "content_block_stop", "message_delta", "message_stop"}
	if len(seq) != len(want) {
		t.Fatalf("got event sequence %v, want %v", seq, want)
	}
	for i := range want {
		if seq[i] != want[i] {
			t.Fatalf("event %d: got %q want %q (full: %v)", i, seq[i], want[i], seq)
		}
	}
	var text strings.Builder
	for _, e := range events {
		if e["__event"] == "content_block_delta" {
			delta := e["delta"].(map[string]any)
			text.WriteString(delta["text"].(string))
		}
	}
	if text.String() != "Hello world" {
		t.Fatalf("got text=%q", text.String())
	}
	last := events[len(events)-1]
	if last["__event"] != "message_stop" {
		t.Fatalf("stream must end with message_stop, got %v", last["__event"])
	}
}

func TestTranslateStreamingSingleToolCall(t *testing.T) {
	stream := synthOAISSE(
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"Bash","arguments":""}}]},"finish_reason":null}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"command\":"}}]},"finish_reason":null}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"ls\"}"}}]},"finish_reason":null}]}`,
		`{"choices":[{"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":5,"completion_tokens":4}}`,
	)
	rr := httptest.NewRecorder()
	_, err := translateOpenAIStream(rr, strings.NewReader(stream), "claude-sonnet-5")
	if err != nil {
		t.Fatal(err)
	}
	events := parseAnthropicSSE(t, rr.Body.String())

	var start map[string]any
	var argJSON strings.Builder
	for _, e := range events {
		switch e["__event"] {
		case "content_block_start":
			if start == nil {
				start = e
			}
		case "content_block_delta":
			d := e["delta"].(map[string]any)
			if d["type"] == "input_json_delta" {
				argJSON.WriteString(d["partial_json"].(string))
			}
		}
	}
	if start == nil {
		t.Fatal("no content_block_start emitted")
	}
	cb := start["content_block"].(map[string]any)
	if cb["type"] != "tool_use" || cb["id"] != "call_1" || cb["name"] != "Bash" {
		t.Fatalf("got %v", cb)
	}
	var input map[string]any
	if err := json.Unmarshal([]byte(argJSON.String()), &input); err != nil {
		t.Fatalf("accumulated partial_json is not valid JSON: %v\n%s", err, argJSON.String())
	}
	if input["command"] != "ls" {
		t.Fatalf("got %v", input)
	}

	// stop_reason must reflect tool_calls -> tool_use.
	for _, e := range events {
		if e["__event"] == "message_delta" {
			d := e["delta"].(map[string]any)
			if d["stop_reason"] != "tool_use" {
				t.Fatalf("stop_reason=%v", d["stop_reason"])
			}
		}
	}
}

func TestTranslateStreamingParallelToolCalls(t *testing.T) {
	stream := synthOAISSE(
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"Bash","arguments":""}}]},"finish_reason":null}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":1,"id":"call_2","function":{"name":"Read","arguments":""}}]},"finish_reason":null}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{}"}}]},"finish_reason":null}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":1,"function":{"arguments":"{\"path\":\"/tmp\"}"}}]},"finish_reason":null}]}`,
		`{"choices":[{"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`,
	)
	rr := httptest.NewRecorder()
	_, err := translateOpenAIStream(rr, strings.NewReader(stream), "claude-sonnet-5")
	if err != nil {
		t.Fatal(err)
	}
	events := parseAnthropicSSE(t, rr.Body.String())

	var starts, stops []map[string]any
	for _, e := range events {
		switch e["__event"] {
		case "content_block_start":
			starts = append(starts, e)
		case "content_block_stop":
			stops = append(stops, e)
		}
	}
	if len(starts) != 2 {
		t.Fatalf("expected 2 content_block_start (parallel tool calls), got %d", len(starts))
	}
	if len(stops) != 2 {
		t.Fatalf("expected 2 content_block_stop, got %d", len(stops))
	}
	// Both blocks must have distinct indices and close in the order opened.
	i0 := starts[0]["index"].(float64)
	i1 := starts[1]["index"].(float64)
	if i0 == i1 {
		t.Fatalf("parallel tool call blocks must have distinct indices, both got %v", i0)
	}
	if stops[0]["index"] != i0 || stops[1]["index"] != i1 {
		t.Fatalf("blocks did not close in the order they opened: starts=%v stops=%v", starts, stops)
	}
}

func TestTranslateStreamingHandlesMalformedChunkGracefully(t *testing.T) {
	stream := "data: {not valid json\n\ndata: " +
		`{"choices":[{"delta":{"content":"ok"},"finish_reason":"stop"}]}` + "\n\ndata: [DONE]\n\n"
	rr := httptest.NewRecorder()
	_, err := translateOpenAIStream(rr, strings.NewReader(stream), "claude-sonnet-5")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(rr.Body.String(), `"text":"ok"`) {
		t.Fatalf("a malformed line must not abort the rest of the stream: %s", rr.Body.String())
	}
}

// TestOpenAICompatibleProviderPreparePropertiesNoInboundAuthLeak verifies
// that Claude Code's own Authorization/x-api-key headers are never forwarded
// to the third-party endpoint -- a genuinely different, incompatible auth
// scheme, unlike Bedrock which forwards most headers and only swaps auth.
func TestOpenAICompatibleProviderPreparePropertiesNoInboundAuthLeak(t *testing.T) {
	t.Setenv("TOGETHER_API_KEY", "test-together-key")

	base, _ := url.Parse("https://api.together.xyz/v1")
	p := NewOpenAICompatibleProvider("together", base, "zai-org/GLM-5.3", nil, "claude-burst-together-test-noleak", "TOGETHER_API_KEY")

	body := []byte(`{"model":"claude-sonnet-5","messages":[{"role":"user","content":"hi"}]}`)
	in := httptest.NewRequest(http.MethodPost, "http://local/v1/messages", nil)
	in.Header.Set("Authorization", "Bearer oauth-should-not-leak")
	in.Header.Set("x-api-key", "sk-ant-should-not-leak")
	in.Header.Set("anthropic-beta", "claude-code-20250219")

	req, model, err := p.Prepare(in.Context(), in, body)
	if err != nil {
		t.Fatal(err)
	}
	if model != "claude-sonnet-5" {
		t.Fatalf("got requested model=%q", model)
	}
	if req.Header.Get("Authorization") != "Bearer test-together-key" {
		t.Fatalf("Authorization not set to the Together key: %q", req.Header.Get("Authorization"))
	}
	if req.Header.Get("x-api-key") != "" {
		t.Fatal("inbound x-api-key leaked into the outbound request")
	}
	if req.Header.Get("anthropic-beta") != "" {
		t.Fatal("inbound anthropic-beta header leaked into the outbound request")
	}
	if !strings.HasSuffix(req.URL.Path, "/chat/completions") {
		t.Fatalf("unexpected outbound path: %s", req.URL.Path)
	}
	sentBody, _ := io.ReadAll(req.Body)
	v := decodeOAIRequest(t, sentBody)
	if v["model"] != "zai-org/GLM-5.3" {
		t.Fatalf("outbound body model = %v", v["model"])
	}
}

// TestOpenAICompatibleProviderConsistentFailoverUsesModelMap verifies that,
// when modelMap is populated ("consistent failover"), a Claude model with an
// explicit entry is sent to that entry's target rather than the fixed
// fallback model.
func TestOpenAICompatibleProviderConsistentFailoverUsesModelMap(t *testing.T) {
	t.Setenv("TOGETHER_API_KEY", "test-together-key")

	base, _ := url.Parse("https://api.together.xyz/v1")
	modelMap := map[string]string{"claude-opus-5": "zai-org/GLM-5.3-Big"}
	p := NewOpenAICompatibleProvider("together", base, "zai-org/GLM-5.3", modelMap, "claude-burst-together-test-map", "TOGETHER_API_KEY")

	body := []byte(`{"model":"claude-opus-5","messages":[{"role":"user","content":"hi"}]}`)
	in := httptest.NewRequest(http.MethodPost, "http://local/v1/messages", nil)

	req, _, err := p.Prepare(in.Context(), in, body)
	if err != nil {
		t.Fatal(err)
	}
	sentBody, _ := io.ReadAll(req.Body)
	v := decodeOAIRequest(t, sentBody)
	if v["model"] != "zai-org/GLM-5.3-Big" {
		t.Fatalf("mapped Claude model must use its model_map entry, got %v", v["model"])
	}
}

// TestOpenAICompatibleProviderConsistentFailoverFallsBackForUnmappedModel
// verifies that a Claude model with NO entry in modelMap still gets a
// target: it falls back to the fixed model, rather than erroring the way
// Bedrock's model_map does for an unmapped model.
func TestOpenAICompatibleProviderConsistentFailoverFallsBackForUnmappedModel(t *testing.T) {
	t.Setenv("TOGETHER_API_KEY", "test-together-key")

	base, _ := url.Parse("https://api.together.xyz/v1")
	modelMap := map[string]string{"claude-opus-5": "zai-org/GLM-5.3-Big"}
	p := NewOpenAICompatibleProvider("together", base, "zai-org/GLM-5.3", modelMap, "claude-burst-together-test-map-fallback", "TOGETHER_API_KEY")

	body := []byte(`{"model":"claude-haiku-4-5-20251001","messages":[{"role":"user","content":"hi"}]}`)
	in := httptest.NewRequest(http.MethodPost, "http://local/v1/messages", nil)

	req, _, err := p.Prepare(in.Context(), in, body)
	if err != nil {
		t.Fatal(err)
	}
	sentBody, _ := io.ReadAll(req.Body)
	v := decodeOAIRequest(t, sentBody)
	if v["model"] != "zai-org/GLM-5.3" {
		t.Fatalf("unmapped Claude model must fall back to the fixed model, got %v", v["model"])
	}
}
