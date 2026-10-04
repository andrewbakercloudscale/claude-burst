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

// Every TOML spelling of a top-level model_provider counts as the user's
// own; text inside a multi-line string and keys inside a table do not.
func TestProviderSpellings(t *testing.T) {
	for in, want := range map[string]string{
		"model_provider = 'openai'\n":                                       "openai",
		"model_provider=\"openai\"\n":                                       "openai",
		"\"model_provider\" = \"x\"\n":                                      "x",
		"'model_provider' = 'y'\n":                                          "y",
		"  model_provider   =   \"z\"   # mine\n":                           "z",
		"notes = \"\"\"\n[not a table]\n\"\"\"\nmodel_provider = 'after'\n": "after",
		"notes = '''\nmodel_provider = \"inside\"\n'''\n":                   "",
		"[desktop]\nmodel_provider = 'in-table'\n":                          "",
		"model_providers.x = { name = \"x\" }\n":                            "",
	} {
		doc, err := parse(in)
		if err != nil {
			t.Fatalf("%q: %v", in, err)
		}
		if got := topLevelProvider(doc); got != want {
			t.Errorf("%q: got %q, want %q", in, got, want)
		}
	}
}

// The case the review found: a single-quoted provider is refused, not
// duplicated.
func TestEnableRefusesSingleQuotedProvider(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	orig := "model_provider = 'openai'\n"
	os.WriteFile(path, []byte(orig), 0o600)
	if err := Enable(path, "127.0.0.1:7779", ""); !errors.Is(err, ErrConflict) {
		t.Fatalf("err = %v", err)
	}
	if b, _ := os.ReadFile(path); string(b) != orig {
		t.Errorf("file changed: %q", b)
	}
}

// A claude-burst provider the user defined is never defined twice.
func TestEnableRefusesExistingBurstTable(t *testing.T) {
	for _, orig := range []string{"[model_providers.claude-burst]\nname = \"mine\"\n", "[desktop]\n\n[model_providers.\"claude-burst\"]\n"} {
		path := filepath.Join(t.TempDir(), "config.toml")
		os.WriteFile(path, []byte(orig), 0o600)
		if err := Enable(path, "127.0.0.1:7779", ""); !errors.Is(err, ErrConflict) {
			t.Errorf("%q: err = %v", orig, err)
		}
	}
}

// A config.toml that does not parse is never rewritten, and says why.
func TestEnableRefusesInvalidTOML(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	orig := "notify = [\n"
	os.WriteFile(path, []byte(orig), 0o600)
	err := Enable(path, "127.0.0.1:7779", "")
	if err == nil || !strings.Contains(err.Error(), "not valid TOML") {
		t.Fatalf("err = %v", err)
	}
	if b, _ := os.ReadFile(path); string(b) != orig {
		t.Errorf("file changed: %q", b)
	}
	if st := ReadStatus(path); st.Invalid == "" {
		t.Error("status does not say the file is invalid")
	}
}

// A config.toml that is a symlink stays one; its target gets the block.
func TestEnableWritesThroughSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "dotfiles-config.toml")
	os.WriteFile(target, []byte("notify = 1\n"), 0o600)
	link := filepath.Join(dir, "config.toml")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if err := Enable(link, "127.0.0.1:7779", ""); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Lstat(link); fi.Mode()&os.ModeSymlink == 0 {
		t.Error("the symlink was replaced by a file")
	}
	if b, _ := os.ReadFile(target); !strings.HasPrefix(string(b), beginMark) {
		t.Errorf("target not updated: %q", b)
	}
	if err := Disable(link, ""); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(target); string(b) != "notify = 1\n" {
		t.Errorf("after disable: %q", b)
	}
}
