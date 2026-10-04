package integration

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// codex-unroute.sh is what rollback, burst-off, uninstall and repair use to
// send Codex straight to ChatGPT again. It may remove Burst's block and
// nothing else, and must never guess at a damaged one.
func runUnroute(t *testing.T, content *string) (string, int, string) {
	t.Helper()
	home := t.TempDir()
	toml := filepath.Join(home, "config.toml")
	if content != nil {
		if err := os.WriteFile(toml, []byte(*content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command(filepath.Join(repoRoot(t), "scripts", "codex-unroute.sh"))
	cmd.Env = append(os.Environ(), "CODEX_HOME="+home)
	out, err := cmd.CombinedOutput()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(toml)
	return string(b), code, string(out)
}

func TestCodexUnrouteRemovesOnlyBurstsBlock(t *testing.T) {
	user := "notify = 1\n\n[desktop]\nx = 1\n"
	in := "# BEGIN claude-burst (Codex)\nmodel_provider = \"claude-burst\"\nmodel_providers.claude-burst = { name = \"Claude Burst\" }\n# END claude-burst\n" + user
	got, code, out := runUnroute(t, &in)
	if code != 0 || got != user || !strings.Contains(out, "removed Burst's provider") {
		t.Fatalf("code %d, out %q, file:\n%s", code, out, got)
	}
}

func TestCodexUnrouteLeavesOtherFilesAlone(t *testing.T) {
	in := "model_provider = \"ollama\"\n"
	if got, code, _ := runUnroute(t, &in); code != 0 || got != in {
		t.Fatalf("code %d, file:\n%s", code, got)
	}
	if _, code, _ := runUnroute(t, nil); code != 0 {
		t.Fatalf("no config.toml: code %d", code)
	}
}

func TestCodexUnrouteRefusesAnUnterminatedBlock(t *testing.T) {
	in := "# BEGIN claude-burst\nmodel = \"x\"\n[desktop]\n"
	got, code, out := runUnroute(t, &in)
	if code == 0 || got != in || !strings.Contains(out, "no END") {
		t.Fatalf("code %d, out %q, file:\n%s", code, out, got)
	}
}
