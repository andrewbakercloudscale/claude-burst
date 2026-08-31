package router

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func httptestReq(path, body string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	return r.WithContext(context.Background())
}

// The admin page and metrics must show the model that actually served a
// request, not the one Claude Code asked for. This was a real defect: GLM
// serving a forced-failover request was labelled "claude-opus-5" in the UI and
// priced at Opus's $5/$25 per MTok, overstating secondary spend.
func TestOpenAIServeModel(t *testing.T) {
	base, _ := url.Parse("https://api.together.xyz/v1")

	// Fixed failover: every Claude model maps to the single configured model.
	fixed := NewOpenAICompatibleProvider("together", base, "zai-org/GLM-5.3", nil, "svc", "TOGETHER_API_KEY")
	for _, asked := range []string{"claude-opus-5", "claude-sonnet-5", "claude-haiku-4-5-20251001", ""} {
		if got := fixed.ServeModel(asked); got != "zai-org/GLM-5.3" {
			t.Errorf("fixed failover: ServeModel(%q) = %q, want zai-org/GLM-5.3", asked, got)
		}
	}

	// Consistent failover: the modelMap entry wins for mapped models; the
	// fixed model remains the fallback for unmapped ones.
	mapped := NewOpenAICompatibleProvider("together", base, "zai-org/GLM-5.3", map[string]string{
		"claude-opus-5":   "zai-org/GLM-5.3",
		"claude-sonnet-5": "Qwen/Qwen3-Coder-480B",
	}, "svc", "TOGETHER_API_KEY")
	for asked, want := range map[string]string{
		"claude-opus-5":             "zai-org/GLM-5.3",
		"claude-sonnet-5":           "Qwen/Qwen3-Coder-480B",
		"claude-haiku-4-5-20251001": "zai-org/GLM-5.3", // fallback
	} {
		if got := mapped.ServeModel(asked); got != want {
			t.Errorf("consistent failover: ServeModel(%q) = %q, want %q", asked, got, want)
		}
	}
}

// Prepare must keep returning the REQUESTED model: forward() passes it into
// TranslateResponse, which echoes it back to Claude Code in the message
// envelope. Claude Code asked for opus and must see opus there -- while the
// upstream body carries the actual target model.
func TestOpenAIPrepareStillReturnsRequestedModel(t *testing.T) {
	t.Setenv("TOGETHER_API_KEY", "test-key")

	base, _ := url.Parse("https://api.together.xyz/v1")
	p := NewOpenAICompatibleProvider("together", base, "zai-org/GLM-5.3", nil, "claude-burst-serve-model-test", "TOGETHER_API_KEY")

	reqBody := []byte(`{"model":"claude-opus-5","max_tokens":1,"messages":[{"role":"user","content":"hi"}]}`)
	in := httptest.NewRequest(http.MethodPost, "http://local/v1/messages", nil)
	req, model, err := p.Prepare(in.Context(), in, reqBody)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if model != "claude-opus-5" {
		t.Errorf("Prepare model = %q, want the requested claude-opus-5 (it is echoed to Claude Code)", model)
	}
	var out map[string]any
	_ = json.NewDecoder(req.Body).Decode(&out)
	if got := out["model"]; got != "zai-org/GLM-5.3" {
		t.Errorf("upstream body model = %v, want zai-org/GLM-5.3", got)
	}
}
