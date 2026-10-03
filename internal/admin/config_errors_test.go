package admin

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/andrewbakercloudscale/claude-burst/internal/config"
)

// breakConfig leaves a config.json that does not parse.
func breakConfig(t *testing.T) {
	t.Helper()
	dir := filepath.Join(os.Getenv("HOME"), ".config", "claude-burst")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// /api/config: every refusal says why, changes nothing, and an unreadable
// config.json is reported rather than overwritten.
func TestConfigHandlerRefusals(t *testing.T) {
	s := newTestServer(t)
	writeConfig(t, os.Getenv("HOME"))
	before, err := os.ReadFile(filepath.Join(os.Getenv("HOME"), ".config", "claude-burst", "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name, body string
		code       int
		want       string
	}{
		{"not JSON", `{`, http.StatusBadRequest, "bad request body"},
		{"unknown strategy", `{"failover_strategy":"yolo"}`, http.StatusBadRequest, "unknown failover strategy"},
		{"unknown intercept mode", `{"intercept_mode":"sideways"}`, http.StatusBadRequest, "not recognised"},
		{"model for a secondary that takes none", `{"secondary_model":"glm-5"}`, http.StatusBadRequest, "openai-compatible"},
		{"nothing asked", `{}`, http.StatusOK, "nothing to change"},
	} {
		rr := mutate(t, s, "/api/config", c.body)
		if rr.Code != c.code || !strings.Contains(rr.Body.String(), c.want) {
			t.Errorf("%s: status=%d body=%s", c.name, rr.Code, rr.Body.String())
		}
	}
	after, _ := os.ReadFile(filepath.Join(os.Getenv("HOME"), ".config", "claude-burst", "config.json"))
	if string(after) != string(before) {
		t.Fatal("a refused change rewrote config.json")
	}

	rr := mutate(t, s, "/api/config", `{"failover_strategy":"none"}`)
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "failover strategy") {
		t.Fatalf("a valid change: status=%d body=%s", rr.Code, rr.Body.String())
	}
	if cfg, _ := config.Load(); cfg.Primary.FailoverStrategy != "none" {
		t.Fatalf("not saved: %q", cfg.Primary.FailoverStrategy)
	}

	breakConfig(t)
	rr = mutate(t, s, "/api/config", `{"failover_strategy":"subscription-limit"}`)
	if rr.Code != http.StatusInternalServerError || !strings.Contains(rr.Body.String(), "does not parse") {
		t.Fatalf("unreadable config: status=%d body=%s", rr.Code, rr.Body.String())
	}
	if b, _ := os.ReadFile(filepath.Join(os.Getenv("HOME"), ".config", "claude-burst", "config.json")); string(b) != "{not json" {
		t.Fatal("an unreadable config.json was overwritten")
	}
}

// /api/keep-awake refusals, and the fallback when sudo has no cached
// credentials: a generated script opened in Terminal, which is stubbed here.
// Ghostty's App Nap setting is stubbed too: `defaults` ignores HOME.
func TestKeepAwakeRefusalsAndPasswordFallback(t *testing.T) {
	s := newTestServer(t)
	writeConfig(t, os.Getenv("HOME"))
	oldNap, oldSudo, oldTerm := setGhosttyAppNap, sudoNonInteractive, launchTerminal
	t.Cleanup(func() { setGhosttyAppNap, sudoNonInteractive, launchTerminal = oldNap, oldSudo, oldTerm })
	setGhosttyAppNap = func(bool) error { return nil }
	var sudoRan int
	sudoNonInteractive = func(args ...string) ([]byte, error) {
		sudoRan++
		return []byte("sudo: a password is required"), os.ErrPermission
	}
	var opened []string
	launchTerminal = func(path string) error { opened = append(opened, path); return nil }

	s.rootHelper = "" // no scripts directory found
	for _, c := range []struct {
		name, body string
		want       string
	}{
		{"not JSON", `{`, "mode must be"},
		{"unknown mode", `{"mode":"sometimes"}`, "mode must be"},
		{"negative idle", `{"mode":"ac","idle_minutes":-1}`, "idle_minutes must be"},
		{"no scripts directory", `{"mode":"ac"}`, "scripts/ directory"},
	} {
		rr := mutate(t, s, "/api/keep-awake", c.body)
		if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), c.want) {
			t.Errorf("%s: status=%d body=%s", c.name, rr.Code, rr.Body.String())
		}
	}
	if sudoRan != 0 || len(opened) != 0 {
		t.Fatalf("a refusal ran something: sudo %d, terminal %v", sudoRan, opened)
	}

	scripts := filepath.Join(t.TempDir(), "scripts")
	if err := os.MkdirAll(scripts, 0o755); err != nil {
		t.Fatal(err)
	}
	s.rootHelper = filepath.Join(scripts, "transparent-root.sh")
	rr := mutate(t, s, "/api/keep-awake", `{"mode":"always"}`)
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "asking for your password") || len(opened) != 1 {
		t.Fatalf("password fallback: status=%d body=%s opened=%v", rr.Code, rr.Body.String(), opened)
	}
	script, err := os.ReadFile(opened[0])
	if err != nil || !strings.Contains(string(script), "lid-awake-root.sh' 'apply' 'always'") {
		t.Fatalf("generated script: %v\n%s", err, script)
	}

	breakConfig(t)
	rr = mutate(t, s, "/api/keep-awake", `{"mode":"off"}`)
	if rr.Code != http.StatusInternalServerError || !strings.Contains(rr.Body.String(), "does not parse") {
		t.Fatalf("unreadable config: status=%d body=%s", rr.Code, rr.Body.String())
	}
}
