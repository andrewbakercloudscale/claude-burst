package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/andrewbakercloudscale/claude-burst/internal/backup"
)

// Reproduces 2026-09-21: an OLD backup sits in latest.bak (from a deploy,
// shunt enabled), then Save writes a NEW config (shunt disabled) directly --
// the way `claude-burst shunt disable` does, with no call to
// scripts/backup-config.sh anywhere in that path. Before this package
// existed, nothing updated latest.bak in between, so
// `cp backups/config.json.latest.bak config.json` -- exactly what
// scripts/rollback.sh does -- would restore the stale, shunt-enabled config
// and silently undo the disable. This asserts latest.bak now holds what
// Save actually wrote, so a later unrelated rollback cannot resurrect the
// old value.
func TestSaveUpdatesTheBackupRollbackWouldRestore(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_BURST_BACKUP_DIR", "")

	stale := []byte(`{"shunt":{"read":true,"write":true}}`)
	if err := backup.SetLatest(filepath.Join(home, ".config", "claude-burst", "config.json"), stale); err != nil {
		t.Fatal(err)
	}

	cfg := Default()
	cfg.Shunt = ShuntConfig{Read: false, Write: false}
	if err := Save(cfg); err != nil {
		t.Fatal(err)
	}

	dir, _ := backup.Dir()
	b, err := os.ReadFile(filepath.Join(dir, "config.json.latest.bak"))
	if err != nil {
		t.Fatal(err)
	}
	if string(b) == string(stale) {
		t.Fatal("latest.bak still holds the pre-disable, shunt-enabled config: a rollback right now would silently re-enable shunting, same as 2026-09-21")
	}
	var got Config
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if got.Shunt.Enabled() {
		t.Fatalf("latest.bak must reflect the disabled state Save just wrote: %+v", got.Shunt)
	}
}

// The direct end-to-end check: simulate rollback.sh's own `cp latest.bak
// config.json` after a Save, and confirm the field Save set is what survives.
func TestBackupSurvivesARollbackCopy(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_BURST_BACKUP_DIR", "")

	cfg := Default()
	cfg.Shunt = ShuntConfig{Read: false, Write: false}
	if err := Save(cfg); err != nil {
		t.Fatal(err)
	}

	dir, _ := backup.Dir()
	latest, err := os.ReadFile(filepath.Join(dir, "config.json.latest.bak"))
	if err != nil {
		t.Fatal(err)
	}
	p, err := ConfigPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, latest, 0600); err != nil { // rollback.sh's own step
		t.Fatal(err)
	}
	restored, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if restored.Shunt.Enabled() {
		t.Fatalf("a rollback right after this Save must not undo it: %+v", restored.Shunt)
	}
}
