package backup

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSnapshotOfMissingFileIsANoOp(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_BURST_BACKUP_DIR", "")

	if err := Snapshot(filepath.Join(home, "nope.json")); err != nil {
		t.Fatalf("a file that has never existed is not an error: %v", err)
	}
	dir, _ := Dir()
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("nothing should have been created: %v", err)
	}
}

func TestSnapshotArchivesCurrentContentAsATimestampedFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_BURST_BACKUP_DIR", "")
	p := filepath.Join(home, "config.json")
	if err := os.WriteFile(p, []byte(`{"v":1}`), 0600); err != nil {
		t.Fatal(err)
	}

	if err := Snapshot(p); err != nil {
		t.Fatal(err)
	}

	dir, _ := Dir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() == "config.json.latest.bak" {
		t.Fatalf("Snapshot must write only a timestamped history file, not latest.bak: %v", entries)
	}
	b, err := os.ReadFile(filepath.Join(dir, entries[0].Name()))
	if err != nil || string(b) != `{"v":1}` {
		t.Fatalf("archived content = %q, err=%v", b, err)
	}
}

func TestSetLatestWritesExactlyLatestBak(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_BURST_BACKUP_DIR", "")
	p := filepath.Join(home, "config.json")

	if err := SetLatest(p, []byte(`{"v":2}`)); err != nil {
		t.Fatal(err)
	}
	dir, _ := Dir()
	b, err := os.ReadFile(filepath.Join(dir, "config.json.latest.bak"))
	if err != nil || string(b) != `{"v":2}` {
		t.Fatalf("latest.bak = %q, err=%v", b, err)
	}
}

// The property that actually matters, proven the way it was found: a stale
// snapshot from long before must not be what a later, unrelated restore
// point points to. Each call must overwrite latest.bak with what was just
// written, however old the previous latest.bak was.
func TestSetLatestOverwritesAnOlderRestorePoint(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_BURST_BACKUP_DIR", "")
	p := filepath.Join(home, "config.json")

	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(SetLatest(p, []byte(`{"shunt":true}`)))  // an old deploy's snapshot
	must(SetLatest(p, []byte(`{"shunt":false}`))) // a later, deliberate disable

	dir, _ := Dir()
	b, err := os.ReadFile(filepath.Join(dir, "config.json.latest.bak"))
	must(err)
	if string(b) != `{"shunt":false}` {
		t.Fatalf("latest.bak = %q, want the most recent write -- a rollback right now must not resurrect the old one", b)
	}
}

func TestBackupDirOverrideAppliesToBoth(t *testing.T) {
	home := t.TempDir()
	custom := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_BURST_BACKUP_DIR", custom)
	p := filepath.Join(home, "settings.json")
	if err := os.WriteFile(p, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}

	if err := Snapshot(p); err != nil {
		t.Fatal(err)
	}
	if err := SetLatest(p, []byte("y")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(home, ".config", "claude-burst", "backups")); !os.IsNotExist(err) {
		t.Fatalf("must not ALSO write to the default location: %v", err)
	}
	if _, err := os.Stat(filepath.Join(custom, "settings.json.latest.bak")); err != nil {
		t.Fatalf("CLAUDE_BURST_BACKUP_DIR was not honoured: %v", err)
	}
}

func TestDirDefaultsUnderHomeDotConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_BURST_BACKUP_DIR", "")
	dir, err := Dir()
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(home, ".config", "claude-burst", "backups")
	if dir != want {
		t.Fatalf("Dir() = %q, want %q", dir, want)
	}
}
