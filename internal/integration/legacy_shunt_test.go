package integration

// Token shunting was removed on 2026-10-02, but a machine that enabled it
// before then still has its guard hook in ~/.claude/settings.json and its
// skill in ~/.claude/skills. These tests pin the way out, using the real
// binary in a throwaway HOME.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// installLegacyShunt writes what `claude-burst shunt enable` used to install:
// the PreToolUse guard hook, beside the user's own hook, and the skill.
func (r *homeRig) installLegacyShunt() (skill string) {
	r.t.Helper()
	must(r.t, os.WriteFile(r.settingsPath(), []byte(`{"model":"sonnet","hooks":{"PreToolUse":[
		{"matcher":"Bash","hooks":[{"type":"command","command":"~/theirs.sh"}]},
		{"matcher":"Read|Bash","hooks":[{"type":"command","command":"`+r.bin+` shunt guard","timeout":5}]}
	]}}`), 0o600))
	skill = filepath.Join(r.home, ".claude", "skills", "claude-burst-shunt", "SKILL.md")
	must(r.t, os.MkdirAll(filepath.Dir(skill), 0o755))
	must(r.t, os.WriteFile(skill, []byte("---\nname: claude-burst-shunt\n---\n"), 0o644))
	return skill
}

func (r *homeRig) assertLegacyShuntGone(skill string) {
	r.t.Helper()
	cmds := preToolUseCommands(r.settings())
	if len(cmds) != 1 || cmds[0] != "~/theirs.sh" {
		r.t.Errorf("the shunt hook must go and only that, leaving the user's own hook: %v", cmds)
	}
	if _, err := os.Stat(skill); err == nil {
		r.t.Errorf("the skill tells Claude to run a command that no longer exists; it must be removed")
	}
	if _, err := os.Stat(filepath.Dir(skill)); err == nil {
		r.t.Errorf("the empty skill directory should be removed too")
	}
	if r.settings()["model"] != "sonnet" {
		r.t.Errorf("an unrelated setting was disturbed")
	}
}

// A session that loaded the hook before it was removed still calls
// `shunt guard` on every Read and Bash. Exit 2 from a PreToolUse hook blocks
// the call, so the guard must keep answering "allow" (exit 0, nothing on
// stderr) for the most suspicious payload the old guard used to refuse.
func TestLegacyShuntGuardAllowsEverything(t *testing.T) {
	r := newHomeRig(t)
	big := filepath.Join(r.home, "big.go")
	must(t, os.WriteFile(big, []byte(strings.Repeat("func f() {}\n", 5000)), 0o644))
	for _, payload := range []string{
		`{"session_id":"s","cwd":"` + r.home + `","tool_name":"Read","tool_input":{"file_path":"` + big + `"}}`,
		`{"session_id":"s","tool_name":"Bash","tool_input":{"command":"cat ` + big + `"}}`,
		`not json at all`,
		``,
	} {
		_, se, code := r.run(payload, "shunt", "guard")
		if code != 0 || se != "" {
			t.Errorf("guard must allow (exit 0, silent stderr), got exit %d stderr %q for payload %q", code, se, payload)
		}
	}
}

func TestLegacyShuntDisableRemovesHookAndSkill(t *testing.T) {
	r := newHomeRig(t)
	skill := r.installLegacyShunt()
	so, se, code := r.run("", "shunt", "disable")
	if code != 0 {
		t.Fatalf("shunt disable: exit %d: %s", code, se)
	}
	contains(t, "shunt disable output", so, "removed")
	r.assertLegacyShuntGone(skill)

	// Run again: nothing left, and it says so rather than failing.
	so, se, code = r.run("", "shunt", "disable")
	if code != 0 {
		t.Fatalf("a second shunt disable must succeed: exit %d: %s", code, se)
	}
	contains(t, "second shunt disable output", so, "not installed")
}

func TestDisableAlsoRemovesALegacyShuntHookAndSkill(t *testing.T) {
	r := newHomeRig(t)
	skill := r.installLegacyShunt()
	so, se, code := r.run("", "disable")
	if code != 0 {
		t.Fatalf("disable: exit %d: %s", code, se)
	}
	contains(t, "disable output", so, "token-shunting")
	r.assertLegacyShuntGone(skill)
}

// The other subcommands are gone; they must fail loudly and point at the
// removal rather than pretend to work.
func TestRemovedShuntCommandsFailWithAPointer(t *testing.T) {
	r := newHomeRig(t)
	for _, sub := range []string{"enable", "read", "status"} {
		_, se, code := r.run("", "shunt", sub)
		if code == 0 {
			t.Errorf("shunt %s should fail now that the feature is removed", sub)
		}
		contains(t, "shunt "+sub+" stderr", se, "removed", "shunt disable")
	}
}
