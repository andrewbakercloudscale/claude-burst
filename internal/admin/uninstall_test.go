package admin

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/andrewbakercloudscale/claude-burst/internal/claudesettings"
	"github.com/andrewbakercloudscale/claude-burst/internal/config"
	"github.com/andrewbakercloudscale/claude-burst/internal/coord"
	"github.com/andrewbakercloudscale/claude-burst/internal/handover"
	"github.com/andrewbakercloudscale/claude-burst/internal/shunt"
)

// Every kind of hook Burst installs, in a settings.json that already belongs
// to someone, and then RemoveAllHooks. Uninstall deletes the binary next, so
// anything this misses runs a missing file on every session or tool call.
func TestRemoveAllHooksLeavesOnlyTheUsersOwn(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	p, err := claudesettings.Path()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}

	// The user's own hooks sit on the very events Burst uses, beside keys it
	// never touches. Written the way claudesettings.Write would write them,
	// so a clean round trip can be held to the same bytes.
	var mine map[string]any
	if err := json.Unmarshal([]byte(`{
  "model": "sonnet",
  "permissions": {"allow": ["Bash(go test:*)"]},
  "statusLine": {"type": "command", "command": "~/statusline.sh"},
  "env": {"FOO": "bar"},
  "hooks": {
    "PreToolUse": [{"matcher": "Bash", "hooks": [{"type": "command", "command": "~/theirs.sh"}]}],
    "SessionStart": [{"matcher": "", "hooks": [{"type": "command", "command": "~/hello.sh", "timeout": 5}]}],
    "UserPromptSubmit": [{"matcher": "", "hooks": [{"type": "command", "command": "/usr/local/bin/mine.sh"}]}]
  }
}`), &mine); err != nil {
		t.Fatal(err)
	}
	before, err := json.MarshalIndent(mine, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	before = append(before, '\n')
	if err := os.WriteFile(p, before, 0o600); err != nil {
		t.Fatal(err)
	}
	// A command of the user's own beside ours must survive too.
	cmdDir := filepath.Join(home, ".claude", "commands")
	if err := os.MkdirAll(cmdDir, 0o755); err != nil {
		t.Fatal(err)
	}
	theirCmd := filepath.Join(cmdDir, "theirs.md")
	if err := os.WriteFile(theirCmd, []byte("my command\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	bin := filepath.Join(home, ".local", "bin", "claude-burst")
	on := config.Default()
	on.Shunt = config.ShuntConfig{Read: true, Write: true}
	on.PrimaryCompaction = config.CompactionConfig{Enabled: true}
	if err := shunt.Apply(on, bin); err != nil {
		t.Fatal(err)
	}
	if err := coord.SyncHooks(true, bin); err != nil {
		t.Fatal(err)
	}
	if err := handover.Install(); err != nil {
		t.Fatal(err)
	}
	if err := SyncPromptNoticeHook(on); err != nil {
		t.Fatal(err)
	}

	// Precondition: every kind is really there, or the test proves nothing.
	installed, _ := os.ReadFile(p)
	for _, want := range []string{" shunt guard", " coord session-start", " coord pre-tool", "/handover/start.sh", "/handover/end.sh", "/" + promptNoticeScript} {
		if !strings.Contains(string(installed), want) {
			t.Fatalf("precondition: %q not installed:\n%s", want, installed)
		}
	}
	skill, _ := shunt.SkillPath()
	compact, _ := compactCommandPath()
	for _, f := range []string{skill, compact} {
		if _, err := os.Stat(f); err != nil {
			t.Fatalf("precondition: %s not installed: %v", f, err)
		}
	}
	if left, _ := HookLeftovers(); len(left) == 0 {
		t.Fatal("HookLeftovers must see the installed hooks")
	}

	if err := RemoveAllHooks(); err != nil {
		t.Fatal(err)
	}

	after, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(after), "claude-burst") {
		t.Fatalf("settings.json still mentions claude-burst:\n%s", after)
	}
	if string(after) != string(before) {
		t.Fatalf("the user's settings were not restored byte for byte:\nbefore:\n%s\nafter:\n%s", before, after)
	}
	for _, f := range []string{skill, compact} {
		if _, err := os.Stat(f); err == nil {
			t.Errorf("%s is still there", f)
		}
	}
	if b, err := os.ReadFile(theirCmd); err != nil || string(b) != "my command\n" {
		t.Errorf("the user's own command was disturbed: %v %q", err, b)
	}
	if left, err := HookLeftovers(); err != nil || len(left) != 0 {
		t.Fatalf("leftovers after removal: %v %v", err, left)
	}

	// Twice in a row is a no-op, not an error: install.sh may be rerun.
	if err := RemoveAllHooks(); err != nil {
		t.Fatal(err)
	}
	if again, _ := os.ReadFile(p); string(again) != string(before) {
		t.Fatalf("a second run changed settings.json:\n%s", again)
	}
}

// A hook no remover knows about (here, a hand-added one naming the binary)
// must be reported, not silently passed as clean.
func TestHookLeftoversReportsUnknownEntries(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	p, _ := claudesettings.Path()
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("{\n  \"statusLine\": {\"command\": \"~/.local/bin/claude-burst stats\"}\n}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := RemoveAllHooks(); err != nil {
		t.Fatal(err)
	}
	left, err := HookLeftovers()
	if err != nil || len(left) != 1 || !strings.Contains(left[0], "settings.json:2:") {
		t.Fatalf("want the one line reported with its number, got %v %v", err, left)
	}
}

// No settings.json at all is clean, and removing creates nothing.
func TestRemoveAllHooksWithNoSettingsFile(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if err := RemoveAllHooks(); err != nil {
		t.Fatal(err)
	}
	p, _ := claudesettings.Path()
	if _, err := os.Stat(p); err == nil {
		t.Fatal("removing hooks created a settings.json")
	}
	if left, err := HookLeftovers(); err != nil || len(left) != 0 {
		t.Fatalf("%v %v", err, left)
	}
}
