package claudesettings

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func ours(cmd string) bool { return strings.HasSuffix(cmd, " shunt guard") }

func parse(t *testing.T, s string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

const existing = `{
  "model": "sonnet",
  "hooks": {
    "PreToolUse": [
      {"matcher": "Bash", "hooks": [{"type": "command", "command": "~/.local/bin/their-hook.sh", "timeout": 3}]}
    ],
    "Notification": [{"matcher": "", "hooks": [{"type": "command", "command": "afplay x.aiff"}]}]
  }
}`

func TestAddPreservesTheUsersHooks(t *testing.T) {
	root := parse(t, existing)
	if !AddCommandHook(root, "PreToolUse", "Read|Bash", "/bin/claude-burst shunt guard", 5, ours) {
		t.Fatal("expected a change")
	}
	groups := root["hooks"].(map[string]any)["PreToolUse"].([]any)
	if len(groups) != 2 {
		t.Fatalf("want their group plus ours, got %d", len(groups))
	}
	first := groups[0].(map[string]any)["hooks"].([]any)[0].(map[string]any)
	if first["command"] != "~/.local/bin/their-hook.sh" {
		t.Errorf("the user's hook was disturbed: %v", first)
	}
	if root["model"] != "sonnet" || root["hooks"].(map[string]any)["Notification"] == nil {
		t.Errorf("unrelated keys must survive")
	}
}

func TestAddIsIdempotentAndRepairsAMovedBinary(t *testing.T) {
	root := parse(t, existing)
	AddCommandHook(root, "PreToolUse", "Read|Bash", "/old/claude-burst shunt guard", 5, ours)
	if AddCommandHook(root, "PreToolUse", "Read|Bash", "/old/claude-burst shunt guard", 5, ours) {
		t.Errorf("a second identical add must change nothing")
	}
	if !AddCommandHook(root, "PreToolUse", "Read|Bash", "/new/claude-burst shunt guard", 5, ours) {
		t.Errorf("a moved binary must be repaired")
	}
	groups := root["hooks"].(map[string]any)["PreToolUse"].([]any)
	if len(groups) != 2 {
		t.Errorf("repair must update in place, not append: %d groups", len(groups))
	}
	if !HasCommandHook(root, "PreToolUse", ours) {
		t.Errorf("hook should be present")
	}
}

func TestRemoveRestoresTheOriginal(t *testing.T) {
	root := parse(t, existing)
	AddCommandHook(root, "PreToolUse", "Read|Bash", "/bin/claude-burst shunt guard", 5, ours)
	if n := RemoveCommandHooks(root, "PreToolUse", ours); n != 1 {
		t.Fatalf("removed %d", n)
	}
	if !reflect.DeepEqual(root, parse(t, existing)) {
		t.Errorf("add then remove must round-trip to the original")
	}
}

func TestRemovePrunesEmptyHooksKey(t *testing.T) {
	root := map[string]any{}
	AddCommandHook(root, "PreToolUse", "Read|Bash", "/bin/claude-burst shunt guard", 5, ours)
	RemoveCommandHooks(root, "PreToolUse", ours)
	if _, ok := root["hooks"]; ok {
		t.Errorf("an emptied hooks key should be removed, got %v", root)
	}
}

func TestRemoveWithNothingToRemove(t *testing.T) {
	root := parse(t, existing)
	if RemoveCommandHooks(root, "PreToolUse", ours) != 0 || !reflect.DeepEqual(root, parse(t, existing)) {
		t.Errorf("removing an absent hook must be a no-op")
	}
}
