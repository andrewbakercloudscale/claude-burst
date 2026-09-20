package shunt

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/andrewbakercloudscale/claude-burst/internal/config"
	"github.com/andrewbakercloudscale/claude-burst/internal/keychain"
	"github.com/andrewbakercloudscale/claude-burst/internal/router"
)

// Worker is a non-streaming client for the configured secondary. It talks to
// the provider directly rather than through the gateway: a worker call is not
// Claude Code traffic, must not open or feed a failover window, and must work
// whether or not the gateway is running.
type Worker struct {
	BaseURL string
	Model   string
	Label   string // vendor label, for messages ("together", "openrouter", ...)
	key     string
	HTTP    *http.Client
	Pricing map[string]config.ModelPrice
}

// Result is one completion.
type Result struct {
	Text         string
	InputTokens  int64
	OutputTokens int64
	FinishReason string
}

// NewWorker builds a worker from the secondary slot. Only an openai-compatible
// secondary can serve: Bedrock's Anthropic wire format is a different protocol
// and the worker deliberately has one code path rather than two.
func NewWorker(cfg config.Config) (*Worker, error) {
	sec := cfg.Secondary
	if sec.Provider != "openai-compatible" {
		return nil, fmt.Errorf("the shunt worker needs an openai-compatible secondary (yours is %q). Run: claude-burst configure --secondary openai-compatible --secondary-base-url <url> --secondary-model <model>",
			sec.Provider)
	}
	if sec.BaseURL == "" {
		return nil, fmt.Errorf("secondary.base_url is empty")
	}
	u, err := url.Parse(sec.BaseURL)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return nil, fmt.Errorf("secondary.base_url %q is not a usable http(s) URL", sec.BaseURL)
	}
	model := cfg.Shunt.Model
	if model == "" {
		model = sec.Model
	}
	if model == "" {
		return nil, fmt.Errorf("no worker model: set secondary.model or shunt.model")
	}
	label, envVar := router.OpenAICompatibleIdentity(sec.KeychainService)
	key, err := keychain.Load(sec.KeychainService, envVar)
	if err != nil {
		return nil, fmt.Errorf("no API key for the %s secondary: %w. Run: claude-burst keychain-set --provider %s", label, err, label)
	}
	return &Worker{
		BaseURL: strings.TrimRight(sec.BaseURL, "/"),
		Model:   model,
		Label:   label,
		key:     key,
		HTTP:    &http.Client{Timeout: time.Duration(cfg.Shunt.TimeoutOrDefault()) * time.Second},
		Pricing: cfg.Pricing,
	}, nil
}

// NewTestWorker is for tests: it skips the keychain and points at a fake server.
func NewTestWorker(baseURL, model string) *Worker {
	return &Worker{BaseURL: baseURL, Model: model, Label: "test", key: "test-key", HTTP: http.DefaultClient}
}

// Cost prices a call at the configured rate. known is false when the model has
// no pricing entry, so a zero is never mistaken for "free".
func (w *Worker) Cost(in, out int64) (usd float64, known bool) {
	p, ok := w.Pricing[w.Model]
	if !ok {
		return 0, false
	}
	return float64(in)/1e6*p.InputPerMTok + float64(out)/1e6*p.OutputPerMTok, true
}

// Complete runs one chat completion at temperature 0.
func (w *Worker) Complete(ctx context.Context, system, user string, maxTokens int) (Result, error) {
	body, err := json.Marshal(map[string]any{
		"model":       w.Model,
		"temperature": 0,
		"max_tokens":  maxTokens,
		"stream":      false,
		"messages": []map[string]string{
			{"role": "system", "content": system},
			{"role": "user", "content": user},
		},
	})
	if err != nil {
		return Result{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.BaseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return Result{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+w.key)

	resp, err := w.HTTP.Do(req)
	if err != nil {
		return Result{}, fmt.Errorf("worker call to %s failed: %w", w.Label, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return Result{}, fmt.Errorf("reading worker response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return Result{}, fmt.Errorf("worker (%s, model %s) returned HTTP %d: %s", w.Label, w.Model, resp.StatusCode, snippet(raw, 300))
	}

	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int64 `json:"prompt_tokens"`
			CompletionTokens int64 `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return Result{}, fmt.Errorf("worker response is not JSON: %s", snippet(raw, 200))
	}
	if len(out.Choices) == 0 {
		return Result{}, fmt.Errorf("worker response had no choices: %s", snippet(raw, 200))
	}
	return Result{
		Text:         out.Choices[0].Message.Content,
		InputTokens:  out.Usage.PromptTokens,
		OutputTokens: out.Usage.CompletionTokens,
		FinishReason: out.Choices[0].FinishReason,
	}, nil
}

func snippet(b []byte, n int) string {
	s := strings.TrimSpace(string(b))
	if len(s) > n {
		s = s[:n] + "..."
	}
	return s
}

// EstimateTokens is the usual ~4 bytes per token. Only used to report what was
// kept out of context; never for billing, which comes from the provider.
func EstimateTokens(bytes int64) int64 { return (bytes + 3) / 4 }
