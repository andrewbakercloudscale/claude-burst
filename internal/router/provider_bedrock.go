package router

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/andrewbaker/claude-burst/internal/keychain"
)

// BedrockProvider forwards a request to Amazon Bedrock's Anthropic-compatible
// Messages endpoint, injecting a Bedrock API key loaded from macOS Keychain,
// remapping the model ID via modelMap, and stripping the OAuth-specific beta
// header (Bedrock doesn't understand it and doesn't need it).
type BedrockProvider struct {
	base            *url.URL
	modelMap        map[string]string
	keychainService string
}

func NewBedrockProvider(base *url.URL, modelMap map[string]string, keychainService string) *BedrockProvider {
	return &BedrockProvider{base: base, modelMap: modelMap, keychainService: keychainService}
}

func (p *BedrockProvider) Name() string { return "bedrock" }

func (p *BedrockProvider) Prepare(ctx context.Context, in *http.Request, body []byte) (*http.Request, string, error) {
	requestedModel := requestModel(body)

	key, err := keychain.Load(p.keychainService)
	if err != nil {
		return nil, "", &ProviderError{
			Status: http.StatusServiceUnavailable, Stage: "keychain_load", Model: requestedModel,
			Err: fmt.Errorf("Bedrock overflow unavailable: %w. Run: claude-burst keychain-set", err),
		}
	}

	rewritten, mappedModel, err := rewriteModel(body, p.modelMap)
	if err != nil {
		return nil, "", &ProviderError{
			Status: http.StatusBadGateway, Stage: "model_mapping", Model: requestedModel,
			Err: fmt.Errorf("Bedrock model mapping: %w", err),
		}
	}

	req, err := buildForwardRequest(ctx, p.base, in, rewritten, true)
	if err != nil {
		return nil, "", &ProviderError{Status: http.StatusBadGateway, Stage: "build_request", Model: requestedModel, Err: err}
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Del("x-api-key")
	stripOAuthBeta(req.Header)
	return req, mappedModel, nil
}

func rewriteModel(body []byte, modelMap map[string]string) ([]byte, string, error) {
	var v map[string]any
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		// Deliberately not %w-wrapping err: Go's JSON decode errors quote
		// the offending byte from the input, which would otherwise put a
		// fragment of the request body into the log and metrics.jsonl,
		// breaking the metadata-only logging guarantee.
		return nil, "", fmt.Errorf("request body is not valid JSON")
	}
	model, _ := v["model"].(string)
	if model == "" {
		return nil, "", fmt.Errorf("request has no model")
	}
	mapped := modelMap[model]
	if mapped == "" {
		if strings.HasPrefix(model, "global.anthropic.") || strings.HasPrefix(model, "anthropic.") {
			mapped = model
		} else {
			return nil, "", fmt.Errorf("no Bedrock model_map entry for %q", model)
		}
	}
	v["model"] = mapped
	b, err := json.Marshal(v)
	return b, mapped, err
}

func stripOAuthBeta(h http.Header) {
	vals := h.Values("anthropic-beta")
	if len(vals) == 0 {
		return
	}
	var kept []string
	for _, header := range vals {
		for _, piece := range strings.Split(header, ",") {
			p := strings.TrimSpace(piece)
			if p == "" || strings.HasPrefix(p, "oauth-") {
				continue
			}
			kept = append(kept, p)
		}
	}
	h.Del("anthropic-beta")
	if len(kept) > 0 {
		h.Set("anthropic-beta", strings.Join(kept, ","))
	}
}
