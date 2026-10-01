package coord

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSessionTaskIsTheLatestTypedRequest(t *testing.T) {
	p := filepath.Join(t.TempDir(), "t.jsonl")
	os.WriteFile(p, []byte(`{"type":"user","message":{"role":"user","content":"fix the comment flood limits"}}
{"type":"assistant","message":{"content":[{"type":"text","text":"ok"}]}}
{"type":"user","message":{"role":"user","content":[{"type":"text","text":"[Image: source: /tmp/1.png]"},{"type":"text","text":"[Image #1] now   add the\nCloudflare rule"},{"type":"image"}]}}
{"type":"user","message":{"role":"user","content":[{"type":"tool_result","content":"x"}]}}
{"type":"user","isMeta":true,"message":{"role":"user","content":"Caveat: meta"}}
{"type":"user","message":{"role":"user","content":"<command-name>/clear</command-name>"}}
{"type":"user","message":{"role":"user","content":"now?"}}
`), 0o644)
	if got := sessionTask(p); got != "now add the Cloudflare rule" {
		t.Fatalf("got %q", got)
	}
	// Only short requests: the latest of them.
	q := filepath.Join(t.TempDir(), "q.jsonl")
	os.WriteFile(q, []byte(`{"type":"user","message":{"content":"retry"}}
{"type":"user","message":{"content":"now?"}}
`), 0o644)
	if got := sessionTask(q); got != "now?" {
		t.Fatalf("got %q", got)
	}
	if sessionTask(filepath.Join(t.TempDir(), "none")) != "" {
		t.Fatal("no transcript, no task")
	}
}
