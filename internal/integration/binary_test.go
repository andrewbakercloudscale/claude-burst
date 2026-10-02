package integration

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

var (
	buildOnce sync.Once
	binPath   string
	buildErr  error
)

// burstBinary builds cmd/claude-burst once per test run.
func burstBinary(t *testing.T) string {
	t.Helper()
	buildOnce.Do(func() {
		dir, err := os.MkdirTemp("", "claude-burst-e2e-bin")
		if err != nil {
			buildErr = err
			return
		}
		binPath = filepath.Join(dir, "claude-burst")
		cmd := exec.Command("go", "build", "-o", binPath, "./cmd/claude-burst")
		cmd.Dir = filepath.Join("..", "..")
		if out, err := cmd.CombinedOutput(); err != nil {
			buildErr = fmt.Errorf("go build: %v\n%s", err, out)
		}
	})
	if buildErr != nil {
		t.Fatal(buildErr)
	}
	return binPath
}

// homeRig is a throwaway HOME with a settings.json that already belongs to
// someone, so every test can check their hook and settings survive.
type homeRig struct {
	t    *testing.T
	bin  string
	home string
}

func newHomeRig(t *testing.T) *homeRig {
	t.Helper()
	r := &homeRig{t: t, bin: burstBinary(t), home: t.TempDir()}
	must(t, os.MkdirAll(filepath.Join(r.home, ".config", "claude-burst"), 0o700))
	must(t, os.MkdirAll(filepath.Join(r.home, ".claude"), 0o700))
	must(t, os.WriteFile(r.settingsPath(), []byte(`{"model":"sonnet","hooks":{"PreToolUse":[{"matcher":"Bash","hooks":[{"type":"command","command":"~/theirs.sh"}]}]}}`), 0o600))
	return r
}

func (r *homeRig) settingsPath() string { return filepath.Join(r.home, ".claude", "settings.json") }

func (r *homeRig) settings() map[string]any {
	r.t.Helper()
	b, err := os.ReadFile(r.settingsPath())
	must(r.t, err)
	var m map[string]any
	must(r.t, json.Unmarshal(b, &m))
	return m
}

// run executes the binary with a minimal, explicit environment so nothing from
// the developer's shell, and nothing of the real HOME, leaks in.
func (r *homeRig) run(stdin string, args ...string) (stdout, stderr string, code int) {
	r.t.Helper()
	cmd := exec.Command(r.bin, args...)
	cmd.Dir = r.home
	cmd.Env = []string{"HOME=" + r.home, "PATH=" + os.Getenv("PATH"), "CLAUDE_BURST_BACKUP_DIR=" + filepath.Join(r.home, "backups")}
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var so, se bytes.Buffer
	cmd.Stdout, cmd.Stderr = &so, &se
	err := cmd.Run()
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		r.t.Fatalf("running %v: %v", args, err)
	}
	return so.String(), se.String(), code
}

func preToolUseCommands(m map[string]any) []string {
	var out []string
	hooks, _ := m["hooks"].(map[string]any)
	groups, _ := hooks["PreToolUse"].([]any)
	for _, g := range groups {
		gm, _ := g.(map[string]any)
		inner, _ := gm["hooks"].([]any)
		for _, h := range inner {
			hm, _ := h.(map[string]any)
			out = append(out, fmt.Sprint(hm["command"]))
		}
	}
	return out
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func contains(t *testing.T, what, got string, wants ...string) {
	t.Helper()
	for _, w := range wants {
		if !strings.Contains(got, w) {
			t.Errorf("%s: missing %q in:\n%s", what, w, got)
		}
	}
}
