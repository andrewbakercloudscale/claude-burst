package handover

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/andrewbakercloudscale/claude-burst/internal/claudesettings"
)

func home(t *testing.T) string {
	t.Helper()
	h := t.TempDir()
	t.Setenv("HOME", h)
	return h
}

func settings(t *testing.T) map[string]any {
	t.Helper()
	p, _ := claudesettings.Path()
	root, err := claudesettings.Read(p)
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func readEffective(t *testing.T) effective {
	t.Helper()
	d, _ := Dir()
	b, err := os.ReadFile(filepath.Join(d, "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	var e effective
	if err := json.Unmarshal(b, &e); err != nil {
		t.Fatal(err)
	}
	return e
}

func TestInstallWritesScriptsAndBothHooksAndKeepsTheUsersHooks(t *testing.T) {
	h := home(t)
	p, _ := claudesettings.Path()
	os.MkdirAll(filepath.Dir(p), 0700)
	// the user's own hook, and the hand-installed legacy pair this replaces
	os.WriteFile(p, []byte(`{"hooks":{"SessionStart":[
	  {"hooks":[{"type":"command","command":"~/.local/bin/panel.sh","timeout":5}]},
	  {"hooks":[{"type":"command","command":"~/.claude/hooks/handover-start.sh","timeout":10}]}],
	  "SessionEnd":[{"hooks":[{"type":"command","command":"~/.claude/hooks/handover-end.sh","timeout":10}]}]}}`), 0600)

	if err := Install(); err != nil {
		t.Fatal(err)
	}
	st := GetStatus()
	if !st.Installed || st.Error != "" {
		t.Fatalf("status %+v", st)
	}
	dir := filepath.Join(h, ".config", "claude-burst", "handover")
	for _, n := range scriptNames {
		fi, err := os.Stat(filepath.Join(dir, n))
		if err != nil || fi.Mode()&0100 == 0 {
			t.Fatalf("%s missing or not executable: %v", n, err)
		}
	}
	b, _ := os.ReadFile(p)
	if strings.Contains(string(b), ".claude/hooks/handover-") {
		t.Errorf("legacy hooks must be replaced, not kept alongside:\n%s", b)
	}
	if !strings.Contains(string(b), "panel.sh") {
		t.Errorf("the user's own hook was removed:\n%s", b)
	}
	if strings.Count(string(b), "/handover/start.sh") != 1 || strings.Count(string(b), "/handover/end.sh") != 1 {
		t.Errorf("want exactly one of each hook:\n%s", b)
	}
	if err := Install(); err != nil {
		t.Fatal(err)
	}
	if b2, _ := os.ReadFile(p); string(b2) != string(b) {
		t.Errorf("a second install must change nothing")
	}

	if err := Uninstall(); err != nil {
		t.Fatal(err)
	}
	root := settings(t)
	if claudesettings.HasCommandHook(root, "SessionStart", isStart) || claudesettings.HasCommandHook(root, "SessionEnd", isEnd) {
		t.Errorf("uninstall left a hook")
	}
	if !claudesettings.HasCommandHook(root, "SessionStart", func(c string) bool { return strings.Contains(c, "panel.sh") }) {
		t.Errorf("uninstall removed the user's hook")
	}
	if GetStatus().Installed {
		t.Errorf("status still says installed")
	}
}

func TestDefaultsAreStoredAsEmptySoTheyKeepFollowingTheDefault(t *testing.T) {
	home(t)
	if err := Save(Config{Model: " opus ", MinPrompts: DefaultMinPrompts, Briefing: DefaultBriefing + "\n", Instructions: DefaultInstructions}); err != nil {
		t.Fatal(err)
	}
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c != (Config{}) {
		t.Errorf("defaults should be stored as zero values, got %+v", c)
	}
	e := readEffective(t)
	if !e.Brief || !e.Write || !e.Commit || e.MinPrompts != 2 || e.Model != "opus" || e.Instructions != DefaultInstructions {
		t.Errorf("effective settings %+v", e)
	}
}

func TestSaveAppliesToTheScriptsSettings(t *testing.T) {
	home(t)
	if err := Save(Config{NoCommit: true, MinPrompts: 5, Model: "sonnet", Instructions: "write {{file}}"}); err != nil {
		t.Fatal(err)
	}
	e := readEffective(t)
	if e.Commit || e.MinPrompts != 5 || e.Model != "sonnet" || e.Instructions != "write {{file}}" || e.Briefing != DefaultBriefing {
		t.Errorf("effective settings %+v", e)
	}
}

func TestNormalizeRejectsBadValues(t *testing.T) {
	for _, c := range []Config{
		{MinPrompts: -1}, {MinPrompts: 51},
		{Model: "opus; rm -rf ~"}, {Model: "two words"},
		{Instructions: strings.Repeat("x", maxTextBytes+1)},
	} {
		if _, err := c.Normalize(); err == nil {
			t.Errorf("%+v: want an error", c)
		}
	}
}

func TestAConfigThatDoesNotParseIsReportedAndNotOverwritten(t *testing.T) {
	h := home(t)
	if err := Install(); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(h, ".config", "claude-burst", "handover.json")
	os.WriteFile(p, []byte("{not json"), 0600)
	st := GetStatus()
	if st.Error == "" {
		t.Errorf("a broken handover.json must be reported")
	}
	if b, _ := os.ReadFile(p); string(b) != "{not json" {
		t.Errorf("the broken file was overwritten")
	}
}

func TestEveryScriptIsEmbedded(t *testing.T) {
	for _, n := range scriptNames {
		b, err := scripts.ReadFile("scripts/" + n)
		if err != nil || !strings.HasPrefix(string(b), "#!/usr/bin/env bash") {
			t.Errorf("%s: %v", n, err)
		}
	}
}

func TestTheLegacyPairIsNotReportedAsInstalled(t *testing.T) {
	home(t)
	p, _ := claudesettings.Path()
	os.MkdirAll(filepath.Dir(p), 0700)
	os.WriteFile(p, []byte(`{"hooks":{
	  "SessionStart":[{"hooks":[{"type":"command","command":"~/.claude/hooks/handover-start.sh"}]}],
	  "SessionEnd":[{"hooks":[{"type":"command","command":"~/.claude/hooks/handover-end.sh"}]}]}}`), 0600)
	st := GetStatus()
	if st.Installed || st.StartHook || !st.Legacy {
		t.Fatalf("legacy hooks ignore these settings and must not read as installed: %+v", st)
	}
	if err := Install(); err != nil {
		t.Fatal(err)
	}
	if st := GetStatus(); !st.Installed || st.Legacy {
		t.Fatalf("after install: %+v", st)
	}
}
