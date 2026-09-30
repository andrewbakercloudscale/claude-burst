package admin

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/andrewbakercloudscale/claude-burst/internal/claudesettings"
	"github.com/andrewbakercloudscale/claude-burst/internal/config"
)

func TestPromptNoticeHookFollowsTheSetting(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	p, err := claudesettings.Path()
	if err != nil {
		t.Fatal(err)
	}
	// The user's own hook on the same event must survive both directions.
	if err := os.MkdirAll(strings.TrimSuffix(p, "/settings.json"), 0o755); err != nil {
		t.Fatal(err)
	}
	mine := `{"hooks":{"UserPromptSubmit":[{"matcher":"","hooks":[{"type":"command","command":"/usr/local/bin/mine.sh"}]}]}}`
	if err := os.WriteFile(p, []byte(mine), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.PrimaryCompaction = config.CompactionConfig{Enabled: true}

	if err := SyncPromptNoticeHook(cfg); err != nil {
		t.Fatal(err)
	}
	if promptNoticeState() != "installed" {
		t.Fatal("on by default: the hook must be installed")
	}
	dir, _ := config.ConfigDir()
	script, err := os.ReadFile(dir + "/" + promptNoticeScript)
	if err != nil || !strings.Contains(string(script), "http://127.0.0.1:7788/api/prompt-notice") {
		t.Fatalf("script missing or pointing elsewhere: %v\n%s", err, script)
	}
	if err := SyncPromptNoticeHook(cfg); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p)
	if n := strings.Count(string(b), promptNoticeScript); n != 1 {
		t.Fatalf("installed %d times", n)
	}

	cfg.PrimaryCompaction.NoPromptNotice = true
	if err := SyncPromptNoticeHook(cfg); err != nil {
		t.Fatal(err)
	}
	b, _ = os.ReadFile(p)
	if promptNoticeState() != "not installed" || !strings.Contains(string(b), "mine.sh") {
		t.Fatalf("off must remove only ours:\n%s", b)
	}
}

func TestPromptNoticeAnswersNothingWhenThereIsNothing(t *testing.T) {
	s := newTestServer(t)
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:7788/api/prompt-notice", bytes.NewReader([]byte(`{"session_id":"S"}`)))
	req.Header.Set(mutationHeader, "1")
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	// Any text here would reach the model: a UserPromptSubmit hook's plain
	// stdout is added to the context.
	if w.Code != http.StatusNoContent || w.Body.Len() != 0 {
		t.Fatalf("want an empty 204, got %d %q", w.Code, w.Body.String())
	}
}
