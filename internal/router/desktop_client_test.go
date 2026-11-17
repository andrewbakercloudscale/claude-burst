package router

import (
	"bufio"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/andrewbakercloudscale/claude-burst/internal/config"
	"github.com/andrewbakercloudscale/claude-burst/internal/metrics"
)

// syncBuf is a log buffer the gateway's goroutines may write while the test
// reads it.
type syncBuf struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// A request from Claude Desktop's Code tab, with the headers its bundled
// Claude Code 2.1.325 sends (read from the binary on 17 Nov 2026), goes to
// Anthropic as it came and is recorded as Desktop's, in metrics.jsonl and
// on its log line; the terminal's lines stay as they were. Before this the
// gateway kept nothing that told the two apart, so a Desktop user could
// not see whether their sessions went through Burst at all.
func TestDesktopRequestsPassThroughAndAreRecordedAsDesktops(t *testing.T) {
	// Nothing the gateway reads on the side (transcripts, for the
	// repository) may come from the real Mac.
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, ".claude"))

	type seen struct{ ua, sid, auth, app string }
	var mu sync.Mutex
	var got []seen
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		mu.Lock()
		got = append(got, seen{r.Header.Get("User-Agent"), r.Header.Get("x-claude-code-session-id"), r.Header.Get("Authorization"), r.Header.Get("x-app")})
		mu.Unlock()
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","model":"claude-haiku-5-5","content":[{"type":"text","text":"hi"}],"usage":{"input_tokens":12,"output_tokens":3}}`))
	}))
	defer upstream.Close()

	cfg := config.Default()
	cfg.AnthropicBaseURL = upstream.URL
	dir := t.TempDir()
	metricsPath := filepath.Join(dir, "metrics.jsonl")
	logs := &syncBuf{}
	s, err := New(cfg, filepath.Join(dir, "state.json"), metricsPath, log.New(logs, "", 0))
	if err != nil {
		t.Fatal(err)
	}

	clients := []struct{ ua, sid, want string }{
		{"claude-cli/2.1.325 (external, claude-desktop, agent-sdk/0.2.1, client-app/claude-desktop)", "d05afcac-6360-450b-bb56-31797989cdc1", "claude-desktop"},
		{"claude-cli/2.1.325 (external, local-agent, agent-sdk/0.2.1)", "c0c0c0c0-0000-4000-8000-000000000001", "cowork"},
		{"claude-cli/2.1.325 (external, cli)", "e43a9760-c000-490c-85fc-4ccc46fefba3", "cli"},
	}
	for _, c := range clients {
		req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/v1/messages?beta=true",
			strings.NewReader(`{"model":"claude-haiku-5-5","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`))
		req.Header.Set("User-Agent", c.ua)
		req.Header.Set("x-app", "cli")
		req.Header.Set("x-claude-code-session-id", c.sid)
		req.Header.Set("Authorization", "Bearer sk-ant-oat01-test")
		req.Header.Set("anthropic-beta", "oauth-2025-04-20,interleaved-thinking-2025-05-14")
		req.Header.Set("content-type", "application/json")
		rr := httptest.NewRecorder()
		s.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"msg_1"`) {
			t.Fatalf("%s: status %d body %s", c.want, rr.Code, rr.Body.String())
		}
	}

	mu.Lock()
	if len(got) != len(clients) {
		t.Fatalf("upstream saw %d requests, want %d", len(got), len(clients))
	}
	for i, c := range clients {
		if got[i].ua != c.ua || got[i].sid != c.sid || got[i].auth != "Bearer sk-ant-oat01-test" || got[i].app != "cli" {
			t.Errorf("%s reached Anthropic changed: %+v", c.want, got[i])
		}
	}
	mu.Unlock()

	f, err := os.Open(metricsPath)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	bySession := map[string]metrics.Event{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var e metrics.Event
		if json.Unmarshal(sc.Bytes(), &e) == nil {
			bySession[e.SessionID] = e
		}
	}
	for _, c := range clients {
		if e, ok := bySession[c.sid]; !ok || e.Client != c.want || e.HTTPStatus != 200 {
			t.Errorf("metrics for %s: %+v (present %v)", c.want, e, ok)
		}
	}

	text := logs.String()
	var okLines []string
	for _, l := range strings.Split(text, "\n") {
		if strings.Contains(l, " ok route=") {
			okLines = append(okLines, l)
		}
	}
	if len(okLines) != 3 {
		t.Fatalf("ok lines:\n%s", text)
	}
	if !strings.HasSuffix(okLines[0], ` client=claude-desktop`) || !strings.HasSuffix(okLines[1], ` client=cowork`) {
		t.Errorf("Desktop's lines do not say so:\n%s", strings.Join(okLines, "\n"))
	}
	// diagnose.sh and peer-log.sh read these lines: the terminal's must not
	// change shape.
	if strings.Contains(okLines[2], "client=") || !strings.HasSuffix(okLines[2], `note=""`) {
		t.Errorf("the terminal's line changed: %s", okLines[2])
	}
}
