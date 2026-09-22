package claudesettings

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/andrewbakercloudscale/claude-burst/internal/backup"
)

func TestClearBaseURL(t *testing.T) {
	cases := []struct {
		name    string
		env     map[string]any
		listen  string
		want    bool
		leftKey bool // ANTHROPIC_BASE_URL still present afterwards
	}{
		{"loopback default", map[string]any{"ANTHROPIC_BASE_URL": "http://127.0.0.1:7777"}, "127.0.0.1:7777", true, false},
		{"non-loopback listen", map[string]any{"ANTHROPIC_BASE_URL": "http://192.168.1.9:7777"}, "192.168.1.9:7777", true, false},
		{"https listen", map[string]any{"ANTHROPIC_BASE_URL": "https://127.0.0.1:443"}, "127.0.0.1:443", true, false},
		{"someone else's proxy is left alone", map[string]any{"ANTHROPIC_BASE_URL": "https://corp-gateway.example.com"}, "127.0.0.1:7777", false, true},
		{"no key", map[string]any{"OTHER": "x"}, "127.0.0.1:7777", false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := map[string]any{"env": tc.env}
			if got := ClearBaseURL(root, tc.listen); got != tc.want {
				t.Fatalf("ClearBaseURL = %v, want %v", got, tc.want)
			}
			env, _ := root["env"].(map[string]any)
			_, present := env["ANTHROPIC_BASE_URL"]
			if present != tc.leftKey {
				t.Errorf("key present = %v, want %v", present, tc.leftKey)
			}
		})
	}
}

func TestClearBaseURLPreservesEverythingElse(t *testing.T) {
	root := map[string]any{
		"model": "opus[1m]",
		"hooks": map[string]any{"Stop": "something"},
		"env":   map[string]any{"ANTHROPIC_BASE_URL": "http://127.0.0.1:7777", "NODE_EXTRA_CA_CERTS": "/path/to.pem"},
	}
	if !ClearBaseURL(root, "127.0.0.1:7777") {
		t.Fatal("expected removal")
	}
	if root["model"] != "opus[1m]" || root["hooks"] == nil {
		t.Error("unrelated top-level settings were lost")
	}
	env := root["env"].(map[string]any)
	if env["NODE_EXTRA_CA_CERTS"] != "/path/to.pem" {
		t.Error("unrelated env var was lost")
	}
}

func TestClearBaseURLDropsEmptyEnvBlock(t *testing.T) {
	root := map[string]any{"env": map[string]any{"ANTHROPIC_BASE_URL": "http://127.0.0.1:7777"}}
	ClearBaseURL(root, "127.0.0.1:7777")
	if _, present := root["env"]; present {
		t.Error("empty env block should have been removed")
	}
}

// TestReadWriteRoundTrip covers the two functions that were never exercised
// at all before this file existed: Read/Write is the actual disk I/O path
// enable/disable/the admin server all depend on, not just the in-memory map
// logic ClearBaseURL tests above.
func TestReadWriteRoundTrip(t *testing.T) {
	p := filepath.Join(t.TempDir(), "settings.json")

	// A path that doesn't exist yet reads as an empty, writable map rather
	// than an error -- enable() on a machine with no prior settings.json
	// depends on this.
	root, err := Read(p)
	if err != nil {
		t.Fatalf("Read of missing file: %v", err)
	}
	if len(root) != 0 {
		t.Fatalf("expected empty map for missing file, got %v", root)
	}

	root["model"] = "opus[1m]"
	SetBaseURL(root, "http://127.0.0.1:7777")
	if err := Write(p, root); err != nil {
		t.Fatalf("Write: %v", err)
	}

	reread, err := Read(p)
	if err != nil {
		t.Fatalf("Read after Write: %v", err)
	}
	if reread["model"] != "opus[1m]" {
		t.Errorf("model not preserved across round trip: %v", reread["model"])
	}
	if got := BaseURL(reread); got != "http://127.0.0.1:7777" {
		t.Errorf("BaseURL after round trip = %q, want http://127.0.0.1:7777", got)
	}
}

// TestReadRejectsInvalidJSON is a regression guard for a real failure mode:
// silently treating a corrupted settings.json as empty would make the next
// Write() truncate the user's hooks/statusLine/model/permissions -- all the
// settings this package's own doc comment says it must never touch. A parse
// error must stop the edit, not paper over it.
func TestReadRejectsInvalidJSON(t *testing.T) {
	p := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(p, []byte("{not valid json"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Read(p); err == nil {
		t.Fatal("expected an error reading invalid JSON, got nil")
	}
}

func TestSetBaseURLPreservesExistingEnv(t *testing.T) {
	root := map[string]any{"env": map[string]any{"NODE_EXTRA_CA_CERTS": "/path/to.pem"}}
	SetBaseURL(root, "http://127.0.0.1:7777")
	env := root["env"].(map[string]any)
	if env["NODE_EXTRA_CA_CERTS"] != "/path/to.pem" {
		t.Error("existing env var was lost")
	}
	if env["ANTHROPIC_BASE_URL"] != "http://127.0.0.1:7777" {
		t.Error("base URL not set")
	}
}

// Same regression as internal/config's TestSaveUpdatesTheBackupRollbackWouldRestore:
// this file carries the shunt guard hook, and a Write that skips updating the
// restore point is exactly how scripts/rollback.sh silently reinstalled it on
// 2026-09-21 (settings.json.latest.bak held an old, hook-present snapshot).
func TestWriteUpdatesTheBackupRollbackWouldRestore(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_BURST_BACKUP_DIR", "")
	p := filepath.Join(home, ".claude", "settings.json")

	stale := []byte(`{"hooks":{"PreToolUse":[{"hooks":[{"command":"claude-burst shunt guard"}]}]}}`)
	if err := backup.SetLatest(p, stale); err != nil {
		t.Fatal(err)
	}

	if err := Write(p, map[string]any{"hooks": map[string]any{}}); err != nil {
		t.Fatal(err)
	}

	dir, _ := backup.Dir()
	b, err := os.ReadFile(filepath.Join(dir, "settings.json.latest.bak"))
	if err != nil {
		t.Fatal(err)
	}
	if string(b) == string(stale) {
		t.Fatal("latest.bak still holds the stale, hook-present settings: a rollback right now would silently reinstall the shunt guard hook, same as 2026-09-21")
	}
	if strings.Contains(string(b), "shunt guard") {
		t.Fatalf("the restore point must reflect what Write just wrote, not an old hook-bearing version: %s", b)
	}
}
