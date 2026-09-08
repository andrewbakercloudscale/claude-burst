package router

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// ProbeResult describes one end-to-end test call to the secondary provider.
type ProbeResult struct {
	Provider       string `json:"provider"`
	Destination    string `json:"destination"`
	RequestedModel string `json:"requested_model"`
	ServeModel     string `json:"serve_model"`
	Status         int    `json:"status"`
	DurationMS     int64  `json:"duration_ms"`
	Reply          string `json:"reply,omitempty"`
	InputTokens    int64  `json:"input_tokens"`
	OutputTokens   int64  `json:"output_tokens"`
	// Detail carries the upstream's own error text when Status is not 2xx.
	// Truncated, because a provider that answers an auth failure with an
	// HTML login page would otherwise put the whole page on the dashboard.
	Detail string `json:"detail,omitempty"`
}

// probePrompt asks the model to identify itself rather than to echo a fixed
// string. Both prove the pipe works; only this one tells you anything about
// what is on the other end of it. "hello world" comes back identically from
// the right model, the wrong model, and a cheap model silently substituted
// by a provider -- which is the misconfiguration this button is most likely
// to be pressed to investigate.
//
// The answer is INFORMATIVE, NOT AUTHORITATIVE, and the UI says so: models
// are unreliable self-identifiers and routinely name the wrong family
// entirely. The authoritative answer to "what served this?" is the
// ServeModel below, which comes from config and the provider's own
// translation. Read together they catch the interesting case -- the served
// id and the model's own account of itself disagreeing.
const probePrompt = "In one short sentence: which AI model and version are you?"

// probeMaxTokens bounds the spend of a single press. A provider that ignores
// the instruction and starts an essay costs this much and no more.
//
// It was 64, which was wrong for the provider this feature was built
// against. GLM-5.3 is a reasoning model: it spends output tokens thinking
// before it emits any visible text, and the translated reply carries only
// the text. Three real probes billed 60, 45 and 39 output tokens for a
// two-token answer -- one of them within four tokens of the old cap. Past
// it, the model would have spent the entire budget reasoning, returned an
// empty message, and this probe would have reported a FAILURE against a
// provider that was working perfectly. A test button whose verdict depends
// on how long a model happened to think is worse than no test button.
//
// 512 still bounds a runaway response to a fraction of a cent, and leaves
// room for a reasoning model to think and then answer.
const probeMaxTokens = 512

// probeRequestedModel is the Claude model the probe asks for. It matters
// that this is a real Claude id rather than the upstream's own: the whole
// point is to exercise the same translation Claude Code's traffic gets,
// including the model_map lookup for bedrock and the fixed/mapped target
// for openai-compatible. Asking for the upstream id directly would skip the
// step most likely to be misconfigured.
const probeRequestedModel = "claude-sonnet-5"

// bufferWriter captures what a Translator writes, so the probe can read the
// translated Anthropic-format response instead of streaming it to a client.
// A local type rather than httptest.NewRecorder: httptest is a testing
// package and this is production code on the paid path.
type bufferWriter struct {
	hdr    http.Header
	status int
	buf    bytes.Buffer
}

func (b *bufferWriter) Header() http.Header {
	if b.hdr == nil {
		b.hdr = http.Header{}
	}
	return b.hdr
}
func (b *bufferWriter) WriteHeader(code int)        { b.status = code }
func (b *bufferWriter) Write(p []byte) (int, error) { return b.buf.Write(p) }

// anthropicMessage is the subset of the Messages response the probe reads.
type anthropicMessage struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	Usage struct {
		InputTokens  int64 `json:"input_tokens"`
		OutputTokens int64 `json:"output_tokens"`
	} `json:"usage"`
	Error *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

func (m anthropicMessage) text() string {
	var out bytes.Buffer
	for _, c := range m.Content {
		if c.Type == "text" {
			out.WriteString(c.Text)
		}
	}
	return out.String()
}

// ProbeSecondary sends one trivial completion through the secondary provider
// and reports what came back.
//
// It goes through the provider's real Prepare and TranslateResponse rather
// than composing its own request, because everything worth testing lives in
// exactly those two steps: the auth header, the base URL, the model
// translation and the response shape. A hand-rolled probe that talked to the
// endpoint directly would pass while the configured path was broken, which
// is the failure mode this button exists to rule out.
//
// It deliberately does NOT touch failover state. A probe that failed must
// not arm an overflow window, and a probe that succeeded must not clear one:
// this is a question about the secondary, not a signal about the primary.
func (s *Server) ProbeSecondary(ctx context.Context) (ProbeResult, error) {
	if s.secondary == nil {
		return ProbeResult{}, fmt.Errorf("no secondary provider is configured on the running gateway, so there is nothing to test " +
			"(if you just added one, restart the gateway first -- it only reads config at startup)")
	}
	p := s.secondary
	start := time.Now()

	body, err := json.Marshal(map[string]any{
		"model":      probeRequestedModel,
		"max_tokens": probeMaxTokens,
		"messages": []map[string]string{
			{"role": "user", "content": probePrompt},
		},
	})
	if err != nil {
		return ProbeResult{}, err
	}

	in, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://local/v1/messages", bytes.NewReader(body))
	if err != nil {
		return ProbeResult{}, err
	}
	in.Header.Set("Content-Type", "application/json")

	req, model, err := p.Prepare(ctx, in, body)
	if err != nil {
		return ProbeResult{Provider: p.Name(), RequestedModel: probeRequestedModel},
			fmt.Errorf("could not build the request for %s: %w", p.Name(), err)
	}

	res := ProbeResult{
		Provider:       p.Name(),
		Destination:    req.URL.Scheme + "://" + req.URL.Host + req.URL.Path,
		RequestedModel: model,
		ServeModel:     model,
	}
	if sm, ok := p.(ServeModeler); ok {
		res.ServeModel = sm.ServeModel(model)
	}

	resp, err := s.client.Do(req)
	if err != nil {
		res.DurationMS = time.Since(start).Milliseconds()
		return res, fmt.Errorf("could not reach %s: %w", res.Destination, err)
	}

	// Recorded on the same ring the real traffic uses, so a probe shows up in
	// "Recent responses" with its headers alongside everything else -- the
	// headers are frequently the actual answer when a provider rejects a key.
	s.recent.add(RecentResponse{
		Time: start, RequestID: "probe", Method: in.Method, Path: in.URL.Path,
		Slot: "secondary", Route: p.Name(), Model: res.ServeModel, Status: resp.StatusCode,
		DurationMS: time.Since(start).Milliseconds(), Headers: filterHeaders(resp.Header),
		Destination: res.Destination,
	})
	res.Status = resp.StatusCode

	if resp.StatusCode >= 400 {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 8*1024))
		_ = resp.Body.Close()
		res.DurationMS = time.Since(start).Milliseconds()
		res.Detail = string(bytes.TrimSpace(errBody))
		if len(res.Detail) > 1000 {
			res.Detail = res.Detail[:1000] + " ... (truncated)"
		}
		return res, fmt.Errorf("%s answered HTTP %d", p.Name(), resp.StatusCode)
	}

	// Translate exactly as a real response would be, so a provider whose
	// wire format has drifted fails here rather than silently in production.
	var raw []byte
	var tok tokenUsage
	if t, ok := p.(Translator); ok {
		bw := &bufferWriter{}
		tok, err = t.TranslateResponse(bw, resp, model)
		if err != nil {
			res.DurationMS = time.Since(start).Milliseconds()
			return res, fmt.Errorf("%s answered, but its response could not be translated: %w", p.Name(), err)
		}
		raw = bw.buf.Bytes()
	} else {
		raw, err = io.ReadAll(io.LimitReader(resp.Body, 1024*1024))
		_ = resp.Body.Close()
		if err != nil {
			res.DurationMS = time.Since(start).Milliseconds()
			return res, fmt.Errorf("could not read %s's response: %w", p.Name(), err)
		}
	}

	var msg anthropicMessage
	if err := json.Unmarshal(raw, &msg); err != nil {
		res.DurationMS = time.Since(start).Milliseconds()
		snippet := string(raw)
		if len(snippet) > 500 {
			snippet = snippet[:500] + " ... (truncated)"
		}
		res.Detail = snippet
		return res, fmt.Errorf("%s answered HTTP %d but not in the Messages format Claude Code expects", p.Name(), resp.StatusCode)
	}
	if msg.Error != nil {
		res.DurationMS = time.Since(start).Milliseconds()
		res.Detail = msg.Error.Message
		return res, fmt.Errorf("%s returned an error: %s", p.Name(), msg.Error.Type)
	}

	res.Reply = msg.text()
	res.InputTokens, res.OutputTokens = msg.Usage.InputTokens, msg.Usage.OutputTokens
	if tok.input > 0 || tok.output > 0 {
		res.InputTokens, res.OutputTokens = tok.input, tok.output
	}
	res.DurationMS = time.Since(start).Milliseconds()

	// A 200 with no text is not a pass. A provider that returns a
	// well-formed empty message would otherwise render as a tick over an
	// empty reply box, which reads as success.
	//
	// The two ways it happens need different answers, because only one of
	// them is the provider's fault: a reasoning model that spent its whole
	// output budget thinking is HEALTHY and just needs more room, while an
	// empty message well under the cap is a genuinely broken response. Told
	// apart here rather than left to whoever reads the dashboard.
	if res.Reply == "" {
		if res.OutputTokens >= probeMaxTokens {
			return res, fmt.Errorf("%s answered HTTP 200 but spent all %d output tokens before emitting any text -- "+
				"this is a reasoning model that ran out of budget mid-thought, not a broken provider", p.Name(), probeMaxTokens)
		}
		return res, fmt.Errorf("%s answered HTTP 200 but with no text content", p.Name())
	}

	// Real spend on a metered provider, so it belongs in the totals. Leaving
	// it out would make the dashboard's cost figure quietly understate what
	// this Mac actually spent.
	s.writeMetric(in, "secondary", p.Name(), res.ServeModel, model, resp.StatusCode, start,
		tokenUsage{input: res.InputTokens, output: res.OutputTokens}, "", 0, "connection test from the admin UI", res.Destination)

	return res, nil
}
