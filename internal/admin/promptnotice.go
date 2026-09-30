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

	"github.com/andrewbakercloudscale/claude-burst/internal/claudesettings"
	"github.com/andrewbakercloudscale/claude-burst/internal/config"
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

func isPromptNotice(cmd string) bool {
	return strings.HasSuffix(cmd, "/claude-burst/"+promptNoticeScript)
}

// promptNoticeScriptText is the hook. It prints the gateway's JSON answer or
// nothing: plain text from a UserPromptSubmit hook would reach the model.
func promptNoticeScriptText(url string) string {
	return `#!/bin/sh
# UserPromptSubmit hook, installed by claude-burst (internal/admin/promptnotice.go).
# Shows Pauseless Compaction's news under the prompt. Generated: edits are
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
	c := cfg.PrimaryCompaction
	want := cfg.AdminListen != "" && c.Enabled && !c.NoPromptNotice
	dir, err := config.ConfigDir()
	if err != nil {
		return err
	}
	script := filepath.Join(dir, promptNoticeScript)
	if want {
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
		changed = claudesettings.AddCommandHook(root, "UserPromptSubmit", "", script, promptNoticeTimeout, isPromptNotice)
	} else {
		changed = claudesettings.RemoveCommandHooks(root, "UserPromptSubmit", isPromptNotice) > 0
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
	}
	b, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	_ = json.Unmarshal(b, &in)
	lines := s.gateway.PromptNotices(in.SessionID)
	if len(lines) == 0 {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	writeJSON(w, map[string]string{"systemMessage": strings.Join(lines, "\n")})
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
	if claudesettings.HasCommandHook(root, "UserPromptSubmit", isPromptNotice) {
		return "installed"
	}
	return "not installed"
}
