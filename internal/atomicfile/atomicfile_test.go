package atomicfile

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWriteCreatesWithPerm(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.json")
	if err := Write(p, []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(p)
	if err != nil || string(b) != "hello" {
		t.Fatalf("got %q, %v", b, err)
	}
	fi, _ := os.Stat(p)
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v, want 0600", fi.Mode().Perm())
	}
}

func TestWriteReplacesAndKeepsExistingMode(t *testing.T) {
	p := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(p, []byte("a much longer old body"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Write(p, []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p)
	if string(b) != "new" {
		t.Fatalf("got %q", b)
	}
	fi, _ := os.Stat(p)
	if fi.Mode().Perm() != 0o644 {
		t.Fatalf("mode %v, want the existing 0644 kept", fi.Mode().Perm())
	}
}

func TestWriteLeavesNoTempFiles(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "state.json")
	for i := 0; i < 3; i++ {
		if err := Write(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		names := []string{}
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("directory holds %v, want only state.json", names)
	}
}

// A failed write must leave the old contents exactly as they were: that is
// the whole point of the package.
func TestWriteFailureLeavesOldContents(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.json")
	if err := os.WriteFile(p, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A read-only directory refuses the temp file.
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(dir, 0o700)
	if err := Write(p, []byte("new"), 0o600); err == nil {
		t.Skip("directory permissions not enforced (running as root?)")
	}
	b, _ := os.ReadFile(p)
	if string(b) != "old" {
		t.Fatalf("got %q after a failed write, want the old contents", b)
	}
}

func TestWriteFollowsSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "dotfiles-settings.json")
	link := filepath.Join(dir, "settings.json")
	if err := os.WriteFile(target, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if err := Write(link, []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Lstat(link)
	if err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("the symlink was replaced by a plain file")
	}
	b, _ := os.ReadFile(target)
	if string(b) != "new" {
		t.Fatalf("target holds %q, want new", b)
	}
}

func TestWriteMissingDirFails(t *testing.T) {
	p := filepath.Join(t.TempDir(), "no", "such", "dir", "f")
	if err := Write(p, []byte("x"), 0o600); err == nil {
		t.Fatal("want an error for a missing directory")
	}
}
