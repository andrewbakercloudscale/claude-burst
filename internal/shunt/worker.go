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

// target is the validated destination of worker calls, resolved from config
// without touching the credential.
type target struct {
	baseURL, model, label, envVar, service string
}

// resolveTarget validates the secondary as a worker endpoint. Only an
// openai-compatible secondary can serve: Bedrock's Anthropic wire format is a
// different protocol and the worker deliberately has one code path, not two.
func resolveTarget(cfg config.Config) (target, error) {
	sec := cfg.Secondary
	if sec.Provider != "openai-compatible" {
		return target{}, fmt.Errorf("the shunt worker needs an openai-compatible secondary (yours is %q). Run: claude-burst configure --secondary openai-compatible --secondary-base-url <url> --secondary-model <model>",
			sec.Provider)
	}
	if sec.BaseURL == "" {
		return target{}, fmt.Errorf("secondary.base_url is empty")
	}
	u, err := url.Parse(sec.BaseURL)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return target{}, fmt.Errorf("secondary.base_url %q is not a usable http(s) URL", sec.BaseURL)
	}
	model := cfg.Shunt.Model
	if model == "" {
		model = sec.Model
	}
	if model == "" {
		return target{}, fmt.Errorf("no worker model: set secondary.model or shunt.model")
	}
	label, envVar := router.OpenAICompatibleIdentity(sec.KeychainService)
	return target{baseURL: strings.TrimRight(sec.BaseURL, "/"), model: model, label: label, envVar: envVar, service: sec.KeychainService}, nil
}

func (t target) noKeyError(cause error) error {
	return fmt.Errorf("no API key for the %s secondary: %w. Run: claude-burst keychain-set --provider %s", t.label, cause, t.label)
}

// Readiness reports whether a worker could be built, WITHOUT reading the
// secret: describe only says whether a key exists. The dashboard asks this on
// every poll, and a value that is never fetched cannot leak into a response.
// A nil return is a promise NewWorker will get past validation and key lookup.
func Readiness(cfg config.Config, describe func(service, envVar string) keychain.Info) error {
	t, err := resolveTarget(cfg)
	if err != nil {
		return err
	}
	if !describe(t.service, t.envVar).Present {
		return t.noKeyError(fmt.Errorf("key not found in %s or macOS Keychain (service %q)", t.envVar, t.service))
	}
	return nil
}

// ModelOf is the model worker calls will use, for display.
func ModelOf(cfg config.Config) string {
	if cfg.Shunt.Model != "" {
		return cfg.Shunt.Model
	}
	return cfg.Secondary.Model
}

// NewWorker builds a worker from the secondary slot.
func NewWorker(cfg config.Config) (*Worker, error) { return NewWorkerWith(cfg, keychain.Load) }

// NewWorkerWith is NewWorker with the key loader substituted, so callers that
// must not touch the real Keychain (tests) can drive it.
func NewWorkerWith(cfg config.Config, load func(service, envVar string) (string, error)) (*Worker, error) {
	t, err := resolveTarget(cfg)
	if err != nil {
		return nil, err
	}
	key, err := load(t.service, t.envVar)
	if err != nil {
		return nil, t.noKeyError(err)
	}
	return &Worker{
		BaseURL: t.baseURL,
		Model:   t.model,
		Label:   t.label,
		key:     key,
		HTTP:    &http.Client{Timeout: time.Duration(cfg.Shunt.TimeoutOrDefault()) * time.Second},
		Pricing: cfg.Pricing,
	}, nil
}

// NewTestWorker is for tests: it skips the keychain and points at a fake server.
func NewTestWorker(baseURL, model string) *Worker {
	return &Worker{BaseURL: baseURL, Model: model, Label: "test", key: "test-key", HTTP: http.DefaultClient}
}

// Endpoint is the URL worker calls go to, for the log.
func (w *Worker) Endpoint() string { return w.BaseURL + "/chat/completions" }

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
