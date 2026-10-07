package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A config.json that does not load is replaced by the newest backup that
// does, the broken file is kept, and a config that loads is left alone.
func TestRestoreLastGoodReplacesAConfigThatWillNotLoad(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_BURST_BACKUP_DIR", filepath.Join(home, "backups"))
	p, err := ConfigPath()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(p, home) {
		t.Skipf("config path %s is not under the test home", p)
	}
	good := Default()
	good.AdminListen = "127.0.0.1:7999"
	if err := Save(good); err != nil {
		t.Fatal(err)
	}
	if _, err := RestoreLastGood(); !errors.Is(err, ErrConfigLoads) {
		t.Fatalf("a config that loads: %v", err)
	}
	dir := filepath.Join(home, "backups")
	// The restore point itself is damaged too, and so is the newest of the
	// history: the next one back is what must come back.
	older := filepath.Join(dir, "config.json.20260101-000000.bak")
	b, _ := os.ReadFile(p)
	if err := os.WriteFile(older, b, 0o600); err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-time.Hour)
	os.Chtimes(older, past, past)
	for _, f := range []string{p, filepath.Join(dir, "config.json.latest.bak"), filepath.Join(dir, "config.json.20260102-000000.bak")} {
		if err := os.WriteFile(f, []byte(`{"admin_listen": "127.0.0.1:7999",`), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := Load(); err == nil {
		t.Fatal("the damaged config must not load")
	}
	r, err := RestoreLastGood()
	if err != nil {
		t.Fatal(err)
	}
	if r.From != older {
		t.Fatalf("restored from %s, want %s", r.From, older)
	}
	cfg, err := Load()
	if err != nil || cfg.AdminListen != "127.0.0.1:7999" {
		t.Fatalf("after the restore: %v, admin_listen %q", err, cfg.AdminListen)
	}
	if kept, err := os.ReadFile(r.Kept); err != nil || !strings.HasSuffix(string(kept), `7999",`) {
		t.Fatalf("the broken file must be kept: %v %q", err, kept)
	}
	// Nothing that loads: said, and config.json left as it is.
	os.WriteFile(p, []byte("{"), 0o600)
	os.Remove(older)
	if _, err := RestoreLastGood(); err == nil || !strings.Contains(err.Error(), "loads either") {
		t.Fatalf("no good backup: %v", err)
	}
	if b, _ := os.ReadFile(p); string(b) != "{" {
		t.Fatalf("config.json was changed with nothing to restore: %q", b)
	}
}
