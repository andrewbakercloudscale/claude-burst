package router

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/andrewbakercloudscale/claude-burst/internal/keychain"
)

// OpenAICompatibleProvider forwards requests to an OpenAI-compatible
// chat-completions endpoint (e.g. Together AI), translating Anthropic's
// Messages wire format to/from OpenAI's chat-completions format in both
// directions -- including streaming SSE and tool calls. Unlike the other
// providers, it always targets ONE fixed model regardless of which Claude
// model Claude Code requested: there is no equivalence between Claude
// models and a third-party model, so per-request remapping (like Bedrock's
// model_map) doesn't apply here.
type OpenAICompatibleProvider struct {
	name            string
	base            *url.URL
	model           string
	keychainService string
	apiKeyEnvVar    string
}

func NewOpenAICompatibleProvider(name string, base *url.URL, model, keychainService, apiKeyEnvVar string) *OpenAICompatibleProvider {
	return &OpenAICompatibleProvider{name: name, base: base, model: model, keychainService: keychainService, apiKeyEnvVar: apiKeyEnvVar}
}

func (p *OpenAICompatibleProvider) Name() string { return p.name }

func (p *OpenAICompatibleProvider) Prepare(ctx context.Context, in *http.Request, body []byte) (*http.Request, string, error) {
	requestedModel := requestModel(body)

	key, err := keychain.Load(p.keychainService, p.apiKeyEnvVar)
	if err != nil {
		return nil, "", &ProviderError{
			Status: http.StatusServiceUnavailable, Stage: "keychain_load", Model: requestedModel,
			Err: fmt.Errorf("%s overflow unavailable: %w. Run: claude-burst keychain-set --provider %s", p.name, err, p.name),
		}
	}

	openaiBody, err := translateAnthropicRequest(body, p.model)
	if err != nil {
		return nil, "", &ProviderError{Status: http.StatusBadGateway, Stage: "request_translation", Model: requestedModel, Err: err}
	}

	u := *p.base
	u.Path = strings.TrimRight(u.Path, "/") + "/chat/completions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(openaiBody))
	if err != nil {
		return nil, "", &ProviderError{Status: http.StatusBadGateway, Stage: "build_request", Model: requestedModel, Err: err}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)
	// Deliberately not copying any inbound headers: Claude Code's
	// Authorization/x-api-key/anthropic-* headers belong to a completely
	// different, incompatible auth scheme and must never reach a
	// third-party endpoint.
	return req, requestedModel, nil
}

func (p *OpenAICompatibleProvider) TranslateResponse(w http.ResponseWriter, resp *http.Response, model string) (tokenUsage, error) {
	defer resp.Body.Close()
	ct := strings.ToLower(resp.Header.Get("content-type"))
	if strings.Contains(ct, "text/event-stream") {
		return translateOpenAIStream(w, resp.Body, model)
	}
	return translateOpenAINonStream(w, resp.Body, model)
}

// --- request translation: Anthropic Messages -> OpenAI chat-completions ---

func translateAnthropicRequest(body []byte, targetModel string) ([]byte, error) {
	var req map[string]any
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, fmt.Errorf("request body is not valid JSON")
	}

	var oaiMessages []map[string]any

	if sys, ok := req["system"]; ok {
		if text := flattenTextContent(sys); text != "" {
			oaiMessages = append(oaiMessages, map[string]any{"role": "system", "content": text})
		}
	}

	if msgs, ok := req["messages"].([]any); ok {
		for _, m := range msgs {
			mm, ok := m.(map[string]any)
			if !ok {
				continue
			}
			role, _ := mm["role"].(string)
			translated, err := translateAnthropicMessage(role, mm["content"])
			if err != nil {
				return nil, err
			}
			oaiMessages = append(oaiMessages, translated...)
		}
	}

	out := map[string]any{
		"model":    targetModel,
		"messages": oaiMessages,
	}
	for _, k := range []string{"max_tokens", "temperature", "top_p"} {
		if v, ok := req[k]; ok {
			out[k] = v
		}
	}
	if v, ok := req["stop_sequences"]; ok {
		out["stop"] = v
	}
	stream, _ := req["stream"].(bool)
	out["stream"] = stream
	if stream {
		// Without this, many OpenAI-compatible servers omit token usage
		// from streaming responses entirely, breaking the cost-metrics
		// feature (writeMetric in router.go).
		out["stream_options"] = map[string]any{"include_usage": true}
	}

	if tools, ok := req["tools"].([]any); ok && len(tools) > 0 {
		var oaiTools []map[string]any
		for _, t := range tools {
			tm, ok := t.(map[string]any)
			if !ok {
				continue
			}
			fn := map[string]any{"name": tm["name"]}
			if d, ok := tm["description"]; ok {
				fn["description"] = d
			}
			if s, ok := tm["input_schema"]; ok {
				fn["parameters"] = s
			}
			oaiTools = append(oaiTools, map[string]any{"type": "function", "function": fn})
		}
		out["tools"] = oaiTools
	}
	if tc, ok := req["tool_choice"]; ok {
		out["tool_choice"] = translateToolChoice(tc)
	}

	return json.Marshal(out)
}

func translateToolChoice(tc any) any {
	m, ok := tc.(map[string]any)
	if !ok {
		return "auto"
	}
	switch t, _ := m["type"].(string); t {
	case "any":
		return "required"
	case "none":
		return "none"
	case "tool":
		return map[string]any{"type": "function", "function": map[string]any{"name": m["name"]}}
	default: // "auto" or unrecognized
		return "auto"
	}
}

// flattenTextContent handles Anthropic's "content can be a string OR an
// array of content blocks" pattern by joining any text blocks together.
// Non-text blocks (images, etc.) are dropped -- see README "Roadmap" for
// why that's an accepted scope boundary.
func flattenTextContent(content any) string {
	switch v := content.(type) {
	case string:
		return v
	case []any:
		var sb strings.Builder
		for _, item := range v {
			block, ok := item.(map[string]any)
			if !ok {
				continue
			}
			if t, _ := block["type"].(string); t == "text" {
				if txt, ok := block["text"].(string); ok {
					if sb.Len() > 0 {
						sb.WriteString("\n")
					}
					sb.WriteString(txt)
				}
			}
		}
		return sb.String()
	}
	return ""
}

// translateAnthropicMessage converts one Anthropic message into zero or
// more OpenAI messages. This isn't 1:1: a single Anthropic user-role
// message containing tool_result blocks must split into one OpenAI "tool"
// message PER block, since OpenAI represents tool results as sibling
// messages rather than nested content blocks.
func translateAnthropicMessage(role string, content any) ([]map[string]any, error) {
	if text, ok := content.(string); ok {
		return []map[string]any{{"role": role, "content": text}}, nil
	}

	blocks, ok := content.([]any)
	if !ok {
		return nil, fmt.Errorf("unsupported message content shape for role %q", role)
	}

	var out []map[string]any
	var textParts []string
	var toolCalls []map[string]any

	flushAssistant := func() {
		if len(textParts) == 0 && len(toolCalls) == 0 {
			return
		}
		msg := map[string]any{"role": "assistant"}
		if len(textParts) > 0 {
			msg["content"] = strings.Join(textParts, "\n")
		} else {
			msg["content"] = nil
		}
		if len(toolCalls) > 0 {
			msg["tool_calls"] = toolCalls
		}
		out = append(out, msg)
		textParts, toolCalls = nil, nil
	}

	for _, item := range blocks {
		block, ok := item.(map[string]any)
		if !ok {
			continue
		}
		switch t, _ := block["type"].(string); t {
		case "text":
			txt, _ := block["text"].(string)
			if role == "assistant" {
				textParts = append(textParts, txt)
			} else {
				out = append(out, map[string]any{"role": role, "content": txt})
			}
		case "tool_use":
			args, err := json.Marshal(block["input"])
			if err != nil {
				return nil, fmt.Errorf("tool_use input is not serializable: %w", err)
			}
			id, _ := block["id"].(string)
			name, _ := block["name"].(string)
			toolCalls = append(toolCalls, map[string]any{
				"id":   id,
				"type": "function",
				"function": map[string]any{
					"name":      name,
					"arguments": string(args),
				},
			})
		case "tool_result":
			toolUseID, _ := block["tool_use_id"].(string)
			out = append(out, map[string]any{
				"role":         "tool",
				"tool_call_id": toolUseID,
				"content":      flattenTextContent(block["content"]),
			})
		case "thinking", "redacted_thinking", "image":
			// Dropped: no meaningful equivalent on a generic
			// OpenAI-compatible endpoint (README "Roadmap" scope note).
		}
	}
	if role == "assistant" {
		flushAssistant()
	}
	return out, nil
}

// --- response translation: OpenAI chat-completions -> Anthropic Messages ---

func mapFinishReason(r string) string {
	switch r {
	case "length":
		return "max_tokens"
	case "tool_calls":
		return "tool_use"
	default: // "stop", "content_filter", or anything unrecognized
		return "end_turn"
	}
}

type openaiToolCall struct {
	ID       string `json:"id"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

func translateOpenAINonStream(w http.ResponseWriter, body io.Reader, model string) (tokenUsage, error) {
	raw, err := io.ReadAll(body)
	if err != nil {
		return tokenUsage{}, err
	}
	var resp struct {
		Choices []struct {
			Message struct {
				Content   string            `json:"content"`
				ToolCalls []openaiToolCall  `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int64 `json:"prompt_tokens"`
			CompletionTokens int64 `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return tokenUsage{}, fmt.Errorf("upstream response is not valid JSON")
	}
	if len(resp.Choices) == 0 {
		return tokenUsage{}, fmt.Errorf("upstream response has no choices")
	}
	choice := resp.Choices[0]

	var content []map[string]any
	if choice.Message.Content != "" {
		content = append(content, map[string]any{"type": "text", "text": choice.Message.Content})
	}
	for _, tc := range choice.Message.ToolCalls {
		var input any
		if err := json.Unmarshal([]byte(tc.Function.Arguments), &input); err != nil {
			input = map[string]any{}
		}
		content = append(content, map[string]any{
			"type": "tool_use", "id": tc.ID, "name": tc.Function.Name, "input": input,
		})
	}
	if content == nil {
		content = []map[string]any{}
	}

	out := map[string]any{
		"id": "msg_" + newRequestID(), "type": "message", "role": "assistant", "model": model,
		"content": content, "stop_reason": mapFinishReason(choice.FinishReason), "stop_sequence": nil,
		"usage": map[string]any{"input_tokens": resp.Usage.PromptTokens, "output_tokens": resp.Usage.CompletionTokens},
	}
	b, err := json.Marshal(out)
	if err != nil {
		return tokenUsage{}, err
	}
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(b)
	return tokenUsage{input: resp.Usage.PromptTokens, output: resp.Usage.CompletionTokens}, nil
}

type openaiStreamChunk struct {
	Choices []struct {
		Delta struct {
			Content   *string `json:"content"`
			ToolCalls []struct {
				Index    int    `json:"index"`
				ID       string `json:"id"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     int64 `json:"prompt_tokens"`
		CompletionTokens int64 `json:"completion_tokens"`
	} `json:"usage"`
}

// translateOpenAIStream reads OpenAI SSE chunks and writes an equivalent
// Anthropic SSE event sequence, flushing after every write so the client
// sees real streaming rather than a buffered response.
//
// Content-block lifecycle: a text block and each parallel tool-call block
// each get exactly one content_block_start, opened on first appearance and
// kept open (never closed mid-stream) until the very end, where every open
// block is closed in the order it was opened, right before message_delta.
// This is deliberate, not a simplification: OpenAI's protocol never signals
// "this specific tool call is now finished" mid-stream -- only the overall
// finish_reason signals the end -- so there is no earlier point at which
// closing a block would be more correct, and closing eagerly on an
// index transition risks emitting a delta for an already-closed block if
// two tool calls' argument chunks interleave.
func translateOpenAIStream(w http.ResponseWriter, body io.Reader, model string) (tokenUsage, error) {
	w.Header().Set("content-type", "text/event-stream")
	w.Header().Set("cache-control", "no-cache")
	w.WriteHeader(http.StatusOK)
	fl, _ := w.(http.Flusher)

	msgID := "msg_" + newRequestID()
	writeEvent := func(event string, data map[string]any) {
		b, _ := json.Marshal(data)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b)
		if fl != nil {
			fl.Flush()
		}
	}

	started := false
	ensureStarted := func() {
		if started {
			return
		}
		started = true
		writeEvent("message_start", map[string]any{
			"type": "message_start",
			"message": map[string]any{
				"id": msgID, "type": "message", "role": "assistant", "model": model,
				"content": []any{}, "stop_reason": nil, "stop_sequence": nil,
				"usage": map[string]any{"input_tokens": 0, "output_tokens": 0},
			},
		})
	}

	nextIndex := 0
	textBlockIndex := -1
	toolBlockIndex := map[int]int{} // OpenAI tool-call index -> Anthropic content-block index
	var openIndices []int

	var tok tokenUsage
	stopReason := "end_turn"

	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		raw := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if raw == "" {
			continue
		}
		if raw == "[DONE]" {
			break
		}
		var chunk openaiStreamChunk
		if json.Unmarshal([]byte(raw), &chunk) != nil {
			continue // tolerate a malformed/unrecognized line rather than aborting the whole stream
		}
		if chunk.Usage != nil {
			tok.input = chunk.Usage.PromptTokens
			tok.output = chunk.Usage.CompletionTokens
		}
		if len(chunk.Choices) == 0 {
			continue
		}
		choice := chunk.Choices[0]
		ensureStarted()

		if choice.Delta.Content != nil && *choice.Delta.Content != "" {
			if textBlockIndex == -1 {
				textBlockIndex = nextIndex
				nextIndex++
				openIndices = append(openIndices, textBlockIndex)
				writeEvent("content_block_start", map[string]any{
					"type": "content_block_start", "index": textBlockIndex,
					"content_block": map[string]any{"type": "text", "text": ""},
				})
			}
			writeEvent("content_block_delta", map[string]any{
				"type": "content_block_delta", "index": textBlockIndex,
				"delta": map[string]any{"type": "text_delta", "text": *choice.Delta.Content},
			})
		}

		for _, tc := range choice.Delta.ToolCalls {
			idx, seen := toolBlockIndex[tc.Index]
			if !seen {
				idx = nextIndex
				nextIndex++
				toolBlockIndex[tc.Index] = idx
				openIndices = append(openIndices, idx)
				writeEvent("content_block_start", map[string]any{
					"type": "content_block_start", "index": idx,
					"content_block": map[string]any{"type": "tool_use", "id": tc.ID, "name": tc.Function.Name, "input": map[string]any{}},
				})
			}
			if tc.Function.Arguments != "" {
				writeEvent("content_block_delta", map[string]any{
					"type": "content_block_delta", "index": idx,
					"delta": map[string]any{"type": "input_json_delta", "partial_json": tc.Function.Arguments},
				})
			}
		}

		if choice.FinishReason != nil && *choice.FinishReason != "" {
			stopReason = mapFinishReason(*choice.FinishReason)
		}
	}
	if err := sc.Err(); err != nil {
		return tok, err
	}

	ensureStarted() // guarantee a valid envelope even if the upstream produced no content chunks at all
	for _, idx := range openIndices {
		writeEvent("content_block_stop", map[string]any{"type": "content_block_stop", "index": idx})
	}
	writeEvent("message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": stopReason, "stop_sequence": nil},
		"usage": map[string]any{"output_tokens": tok.output},
	})
	writeEvent("message_stop", map[string]any{"type": "message_stop"})
	return tok, nil
}
