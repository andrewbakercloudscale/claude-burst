package admin

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/andrewbakercloudscale/claude-burst/internal/claudesettings"
	"github.com/andrewbakercloudscale/claude-burst/internal/config"
	"github.com/andrewbakercloudscale/claude-burst/internal/router"
)

// The prompt notice: a UserPromptSubmit hook that shows, under the prompt in
// Claude Code's own window, what Pauseless Compaction did since the last one.
// The hook's JSON carries it as systemMessage, which Claude Code shows to the
// user and does not add to the model's context. The router keeps the lines
// (Server.PromptNotices); this file serves them and keeps the hook installed
// while compaction and the notice are on.

const promptNoticeScript = "prompt-notice.sh"

// promptNoticeTimeout is the hook's limit in seconds. The script gives up
// after one, so a stopped gateway never holds a prompt back.
const promptNoticeTimeout = 3

// modNoticeFor is how long after the burst-band mod last asked for a
// session's lines the hook is answered with nothing. The mod asks every 5
// seconds and shows each line as a toast in the session; the hook printing
// the same news under the prompt was the two racing for one queue, and the
// hook won often enough to keep the red "PostToolUse:Bash says" lines. A mod
// that has gone stops asking, and the hook has the lines again.
const modNoticeFor = 30 * time.Second

// modNotices is when each session's mod last asked, by session id.
type modNotices struct {
	mu   sync.Mutex
	seen map[string]time.Time
}

// asked records the mod asking now.
func (m *modNotices) asked(sid string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.seen == nil {
		m.seen = map[string]time.Time{}
	}
	for k, t := range m.seen {
		if time.Since(t) > modNoticeFor {
			delete(m.seen, k)
		}
	}
	m.seen[sid] = time.Now()
}

// has reports whether the session's mod is taking its lines.
func (m *modNotices) has(sid string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.seen[sid]
	return ok && time.Since(t) <= modNoticeFor
}

func isPromptNotice(cmd string) bool {
	return strings.HasSuffix(cmd, "/claude-burst/"+promptNoticeScript)
}

// promptNoticeScriptText is the hook. It prints the gateway's JSON answer or
// nothing: plain text from a UserPromptSubmit hook would reach the model.
func promptNoticeScriptText(url string) string {
	return `#!/bin/sh
# UserPromptSubmit, PostToolUse and SessionEnd hook, installed by claude-burst (internal/admin/promptnotice.go).
# Shows Pauseless Compaction's news under the prompt, and tells the gateway
# when a session is cleared. Generated: edits are
# overwritten; turn it off on the dashboard instead.
curl -sf -m 1 -X POST -H 'X-Claude-Burst-Admin: 1' -H 'Content-Type: application/json' \
  --data-binary @- '` + url + `' 2>/dev/null
exit 0
`
}

// noticeURL is the running admin listener's notice endpoint, on loopback.
func noticeURL(adminListen string) string {
	host, port, err := net.SplitHostPort(adminListen)
	if err != nil {
		host, port = "127.0.0.1", "7788"
	}
	switch host {
	case "", "0.0.0.0", "::", "[::]":
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port) + "/api/prompt-notice"
}

// SyncPromptNoticeHook installs the hook when compaction and its notice are
// on and removes it otherwise. It touches only its own entry in
// ~/.claude/settings.json.
func SyncPromptNoticeHook(cfg config.Config) error {
	if err := SyncCompactCommand(cfg); err != nil {
		return err
	}
	c := cfg.PrimaryCompaction
	want := cfg.AdminListen != "" && c.Enabled && !c.NoPromptNotice
	dir, err := config.ConfigDir()
	if err != nil {
		return err
	}
	script := filepath.Join(dir, promptNoticeScript)
	// SessionEnd goes with compaction itself, not with the notice: it stops
	// a summary being written for a session that ran /clear.
	wantEnd := cfg.AdminListen != "" && c.Enabled
	if wantEnd {
		if err := config.EnsureDir(); err != nil {
			return err
		}
		body := []byte(promptNoticeScriptText(noticeURL(cfg.AdminListen)))
		if old, err := os.ReadFile(script); err != nil || string(old) != string(body) {
			if err := os.WriteFile(script, body, 0o755); err != nil {
				return err
			}
		}
		if err := os.Chmod(script, 0o755); err != nil {
			return err
		}
	}
	p, err := claudesettings.Path()
	if err != nil {
		return err
	}
	root, err := claudesettings.Read(p)
	if err != nil {
		return err
	}
	var changed bool
	if want {
		// UserPromptSubmit shows news under each prompt; PostToolUse shows
		// it inside a long turn, where a waiting summary otherwise looked
		// like compaction had not fired.
		changed = claudesettings.AddCommandHook(root, "UserPromptSubmit", "", script, promptNoticeTimeout, isPromptNotice)
		changed = claudesettings.AddCommandHook(root, "PostToolUse", "", script, promptNoticeTimeout, isPromptNotice) || changed
	} else {
		changed = claudesettings.RemoveCommandHooks(root, "UserPromptSubmit", isPromptNotice) > 0
		changed = claudesettings.RemoveCommandHooks(root, "PostToolUse", isPromptNotice) > 0 || changed
	}
	if wantEnd {
		changed = claudesettings.AddCommandHook(root, "SessionEnd", "", script, promptNoticeTimeout, isPromptNotice) || changed
	} else {
		changed = claudesettings.RemoveCommandHooks(root, "SessionEnd", isPromptNotice) > 0 || changed
	}
	if !changed {
		return nil
	}
	return claudesettings.Write(p, root)
}

// handlePromptNotice answers the hook. Its body is the hook's input; the
// answer is the hook's output: a systemMessage, or nothing at all.
func (s *Server) handlePromptNotice(w http.ResponseWriter, r *http.Request) {
	var in struct {
		SessionID string `json:"session_id"`
		Event     string `json:"hook_event_name"`
		Reason    string `json:"reason"`
		Mod       bool   `json:"mod"` // asked by the burst-band mod, not the hook
	}
	b, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	_ = json.Unmarshal(b, &in)
	if in.Event == "SessionEnd" {
		// Nothing to show a session that has ended. One that was cleared
		// has no use for a summary still being written.
		if in.Reason == "clear" {
			s.gateway.ClearSession(in.SessionID)
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if in.Mod {
		s.modNotices.asked(in.SessionID)
	} else if s.modNotices.has(in.SessionID) {
		// The lines stay queued for the mod, which shows them as toasts.
		w.WriteHeader(http.StatusNoContent)
		return
	}
	lines := s.gateway.PromptNotices(in.SessionID, in.Event == "PostToolUse")
	if len(lines) == 0 {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	writeJSON(w, map[string]string{"systemMessage": strings.Join(lines, "\n")})
}

// handlePromptNoticeTest queues a test line for one session, or all.
func (s *Server) handlePromptNoticeTest(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Session string `json:"session"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	n := s.gateway.QueueTestNotice(req.Session)
	if n == 0 {
		http.Error(w, "no tracked session to send it to; send a prompt in Claude Code first", http.StatusNotFound)
		return
	}
	writeJSON(w, map[string]string{"ok": fmt.Sprintf("a test line is waiting for %d session(s); it appears under the next prompt you send in each", n)})
}

// promptNoticeState is the hook's line for the dashboard.
func promptNoticeState() string {
	p, err := claudesettings.Path()
	if err != nil {
		return ""
	}
	root, err := claudesettings.Read(p)
	if err != nil {
		return fmt.Sprintf("settings.json: %v", err)
	}
	if claudesettings.HasCommandHook(root, "UserPromptSubmit", isPromptNotice) &&
		claudesettings.HasCommandHook(root, "PostToolUse", isPromptNotice) {
		return "installed"
	}
	return "not installed"
}

// compactCommandText is ~/.claude/commands/compact-async.md: the prompt it
// sends carries router.CompactAsyncMarker, which makes the gateway start a
// summary now. The model only has to acknowledge it.
const compactCommandText = `---
description: Pauseless compaction (Claude Burst) - summarise this session in the background now, with no pause
---
(` + router.CompactAsyncMarker + `) The Claude Burst gateway has started Pauseless Compaction for this conversation: a background summary that swaps in with the next prompt. Reply with exactly this one line and nothing else, using no tools: "Pauseless compaction started: it swaps in with your next prompt, keep working."
`

// compactCommandPath is where Claude Code looks for the user's commands.
func compactCommandPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".claude", "commands", "compact-async.md"), nil
}

// SyncCompactCommand installs /compact-async while Pauseless Compaction is
// on and removes it when off. A file there that is not ours is left alone.
func SyncCompactCommand(cfg config.Config) error {
	p, err := compactCommandPath()
	if err != nil {
		return err
	}
	old, rerr := os.ReadFile(p)
	ours := rerr == nil && strings.Contains(string(old), router.CompactAsyncMarker)
	if rerr == nil && !ours {
		return nil
	}
	if !cfg.PrimaryCompaction.Enabled {
		if ours {
			return os.Remove(p)
		}
		return nil
	}
	if ours && string(old) == compactCommandText {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	return os.WriteFile(p, []byte(compactCommandText), 0o644)
}
