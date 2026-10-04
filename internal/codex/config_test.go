package codex

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const userConfig = `notify = [ "/x/notify", "turn-ended" ]

[desktop]
followUpQueueMode = "steer"

[model_providers.other]
name = "Other"
base_url = "https://example.com/v1"
`

// Enable puts the block first, leaves every other line as it was, and
// Disable gives back the original file byte for byte.
func TestEnableDisableRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	backups := filepath.Join(dir, "backups")
	os.WriteFile(path, []byte(userConfig), 0o600)

	if err := Enable(path, "127.0.0.1:7779", backups); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	if !strings.HasPrefix(string(b), beginMark) || !strings.HasSuffix(string(b), userConfig) {
		t.Fatalf("enabled file:\n%s", b)
	}
	st := ReadStatus(path)
	if !st.Enabled || st.BaseURL != "http://127.0.0.1:7779/backend-api/codex" || st.Conflict != "" {
		t.Errorf("status %+v", st)
	}

	// Enabling twice does not stack blocks.
	if err := Enable(path, "127.0.0.1:7779", backups); err != nil {
		t.Fatal(err)
	}
	b, _ = os.ReadFile(path)
	if strings.Count(string(b), beginMark) != 1 {
		t.Errorf("two blocks:\n%s", b)
	}

	if err := Disable(path, backups); err != nil {
		t.Fatal(err)
	}
	b, _ = os.ReadFile(path)
	if string(b) != userConfig {
		t.Errorf("disable did not restore the file:\n%q", b)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Errorf("mode changed to %v", fi.Mode().Perm())
	}
	if ents, _ := os.ReadDir(backups); len(ents) == 0 {
		t.Error("no backup written")
	}
}

// A model_provider the user chose is never overridden or duplicated.
func TestEnableRefusesUsersOwnProvider(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	orig := "model_provider = \"ollama\"\n\n[desktop]\nx = 1\n"
	os.WriteFile(path, []byte(orig), 0o600)
	if err := Enable(path, "127.0.0.1:7779", ""); !errors.Is(err, ErrConflict) {
		t.Fatalf("err = %v, want ErrConflict", err)
	}
	if b, _ := os.ReadFile(path); string(b) != orig {
		t.Errorf("file changed:\n%s", b)
	}
	if st := ReadStatus(path); st.Conflict != "ollama" {
		t.Errorf("conflict %q", st.Conflict)
	}
}

// model_provider inside a table is not the top-level key.
func TestProviderInsideTableIsNoConflict(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	os.WriteFile(path, []byte("[profiles.x]\nmodel_provider = \"ollama\"\n"), 0o600)
	if err := Enable(path, "127.0.0.1:7779", ""); err != nil {
		t.Fatal(err)
	}
}

// A begin marker with no end is left alone rather than guessed at.
func TestUnterminatedBlockIsNotDeleted(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	orig := beginMark + "\nmodel = \"x\"\n"
	os.WriteFile(path, []byte(orig), 0o600)
	if err := Disable(path, ""); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(path); string(b) != orig {
		t.Errorf("file changed:\n%s", b)
	}
}

// No config.toml yet: Enable creates one, Disable of a missing file is fine.
func TestEnableCreatesFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "codex", "config.toml")
	if err := Disable(path, ""); err != nil {
		t.Fatal(err)
	}
	if err := Enable(path, "127.0.0.1:7779", ""); err != nil {
		t.Fatal(err)
	}
	if !ReadStatus(path).Enabled {
		t.Error("not enabled")
	}
}
