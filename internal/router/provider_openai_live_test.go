package router

import (
	"bytes"
	"io"
	"net/http"
	"os"
	"testing"
	"time"
)

// Every unit test in provider_openai_test.go asserts on JSON that this
// package itself produced -- they encode our belief about what an
// OpenAI-compatible endpoint accepts, and never check it. That is exactly
// how the schema-less server-tool bug shipped: the translation was asserted
// "correct" against our own assumption for months while Together answered
// 400 Invalid JSON data: missing field `parameters` to the real thing.
//
// This one asks the endpoint, over the internet, so it never runs unasked:
// it needs CLAUDE_BURST_LIVE=1 as well as a key. A key alone used to be
// enough, and one exported in a shell profile put a network call in every
// `go test ./...`, deploy.sh's included: a slow Together failed a deploy of
// unrelated code (2026-10-04).
//
//	CLAUDE_BURST_LIVE=1 TOGETHER_API_KEY=$(security find-generic-password -s claude-burst-together -w) \
//	  go test ./internal/router/ -run TestLiveSecondary -v
//
// Run it after any change to translateAnthropicRequest.
func TestLiveSecondaryAcceptsTranslatedRequest(t *testing.T) {
	if os.Getenv("CLAUDE_BURST_LIVE") != "1" {
		t.Skip("live test: set CLAUDE_BURST_LIVE=1 (and TOGETHER_API_KEY) to run it")
	}
	key := os.Getenv("TOGETHER_API_KEY")
	if key == "" {
		t.Skip("TOGETHER_API_KEY not set -- see the comment above for how to run this")
	}

	// The shapes Claude Code actually sends in one turn: a server tool with
	// no input_schema, a no-argument tool, and an ordinary one.
	in := `{"model":"claude-sonnet-5","max_tokens":16,
	  "messages":[{"role":"user","content":"say hi"}],
	  "tools":[
	    {"type":"web_search_20250305","name":"web_search","max_uses":5},
	    {"name":"NoArgs","input_schema":{}},
	    {"name":"Bash","description":"run a shell command","input_schema":{"type":"object","properties":{"command":{"type":"string"}}}}
	  ]}`

	body, dropped, err := translateAnthropicRequest([]byte(in), "zai-org/GLM-5.3")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("dropped server-only tools: %v", dropped)

	req, err := http.NewRequest(http.MethodPost, "https://api.together.xyz/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 60 * time.Second}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	got, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("secondary rejected our translated request: %d %s", resp.StatusCode, got)
	}
}
