package admin

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

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
	if n := strings.Count(string(b), promptNoticeScript); n != 3 {
		t.Fatalf("want it once each under UserPromptSubmit, PostToolUse and SessionEnd, installed %d times:\n%s", n, b)
	}
	root, _ := claudesettings.Read(p)
	if !claudesettings.HasCommandHook(root, "PostToolUse", isPromptNotice) {
		t.Fatalf("the mid-turn hook is missing:\n%s", b)
	}

	cfg.PrimaryCompaction.NoPromptNotice = true
	if err := SyncPromptNoticeHook(cfg); err != nil {
		t.Fatal(err)
	}
	b, _ = os.ReadFile(p)
	// SessionEnd stays: it goes with compaction, which is still on.
	if promptNoticeState() != "not installed" || !strings.Contains(string(b), "mine.sh") || strings.Count(string(b), promptNoticeScript) != 1 {
		t.Fatalf("off must remove only ours, and leave SessionEnd:\n%s", b)
	}
	cfg.PrimaryCompaction.Enabled = false
	if err := SyncPromptNoticeHook(cfg); err != nil {
		t.Fatal(err)
	}
	b, _ = os.ReadFile(p)
	if !strings.Contains(string(b), "mine.sh") || strings.Contains(string(b), promptNoticeScript) {
		t.Fatalf("compaction off must remove every one of ours and nothing else:\n%s", b)
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

func TestPromptNoticeTestNeedsASession(t *testing.T) {
	s := newTestServer(t)
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:7788/api/prompt-notice-test", bytes.NewReader([]byte(`{}`)))
	req.Header.Set(mutationHeader, "1")
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("no sessions tracked: want 404, got %d %q", w.Code, w.Body.String())
	}
}

// /compact-async is installed while compaction is on and removed when off;
// a command of the same name that is not ours is never touched.
func TestCompactCommandFollowsTheSetting(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	p, err := compactCommandPath()
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.PrimaryCompaction = config.CompactionConfig{Enabled: true}
	if err := SyncCompactCommand(cfg); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(p)
	if err != nil || !strings.Contains(string(b), "claude-burst:compact-async") || !strings.Contains(string(b), "description:") {
		t.Fatalf("not installed: %v\n%s", err, b)
	}
	cfg.PrimaryCompaction.Enabled = false
	if err := SyncCompactCommand(cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Fatal("compaction off: the command must be removed")
	}
	os.WriteFile(p, []byte("my own command"), 0o644)
	cfg.PrimaryCompaction.Enabled = true
	SyncCompactCommand(cfg)
	cfg.PrimaryCompaction.Enabled = false
	SyncCompactCommand(cfg)
	if b, _ := os.ReadFile(p); string(b) != "my own command" {
		t.Fatalf("someone else's compact-async.md was changed: %q", b)
	}
}

// The mod shows a session's lines as toasts, so while it is asking the hook
// is answered with nothing and the lines are left for the mod.
func TestPromptNoticeHookStandsAsideForTheMod(t *testing.T) {
	s := newTestServer(t)
	ask := func(body string) int {
		req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:7788/api/prompt-notice", bytes.NewReader([]byte(body)))
		req.Header.Set(mutationHeader, "1")
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, req)
		return w.Code
	}
	if s.modNotices.has("S") {
		t.Fatal("no mod has asked yet")
	}
	ask(`{"session_id":"S","hook_event_name":"PostToolUse","mod":true}`)
	if !s.modNotices.has("S") || s.modNotices.has("T") {
		t.Fatal("the mod asking for S must mark S, and only S")
	}
	// A mod that stopped asking half a minute ago has gone.
	s.modNotices.mu.Lock()
	s.modNotices.seen["S"] = time.Now().Add(-modNoticeFor - time.Second)
	s.modNotices.mu.Unlock()
	if s.modNotices.has("S") {
		t.Fatal("a mod that has gone must give the lines back to the hook")
	}
	// A session ending is never answered with text.
	if code := ask(`{"session_id":"S","hook_event_name":"SessionEnd","reason":"clear"}`); code != http.StatusNoContent {
		t.Fatalf("SessionEnd answered %d", code)
	}
}

// SessionEnd goes with compaction, not with the notice under the prompt.
func TestSessionEndHookFollowsCompaction(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	p, err := claudesettings.Path()
	if err != nil {
		t.Fatal(err)
	}
	has := func() bool {
		root, err := claudesettings.Read(p)
		return err == nil && claudesettings.HasCommandHook(root, "SessionEnd", isPromptNotice)
	}
	cfg := config.Default()
	cfg.PrimaryCompaction = config.CompactionConfig{Enabled: true, NoPromptNotice: true}
	if err := SyncPromptNoticeHook(cfg); err != nil {
		t.Fatal(err)
	}
	if !has() || promptNoticeState() != "not installed" {
		t.Fatal("notice off, compaction on: SessionEnd installed, the prompt hooks not")
	}
	cfg.PrimaryCompaction.Enabled = false
	if err := SyncPromptNoticeHook(cfg); err != nil {
		t.Fatal(err)
	}
	if has() {
		t.Fatal("compaction off must remove the SessionEnd hook")
	}
}
