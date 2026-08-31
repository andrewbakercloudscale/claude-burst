package rotate

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestRotateIfOversizedNoopWhenMissing(t *testing.T) {
	p := filepath.Join(t.TempDir(), "does-not-exist.log")
	if err := RotateIfOversized(p, 100, 5); err != nil {
		t.Fatalf("expected no error for a missing file, got %v", err)
	}
}

func TestRotateIfOversizedNoopUnderThreshold(t *testing.T) {
	p := filepath.Join(t.TempDir(), "small.log")
	if err := os.WriteFile(p, []byte("hello"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := RotateIfOversized(p, 1000, 5); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "hello" {
		t.Fatalf("file was rotated when under threshold: content = %q", b)
	}
	if _, err := os.Stat(p + ".1"); err == nil {
		t.Fatal("a .1 backup should not exist when nothing was rotated")
	}
}

func TestRotateIfOversizedShiftsBackups(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "app.log")

	// Fill path, path.1, path.2 with distinguishable content, each over the
	// threshold, and rotate three times -- verifying the FULL shift chain,
	// not just the first rotation.
	write := func(path, content string) {
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}

	write(p, "generation-0")
	if err := RotateIfOversized(p, 5, 2); err != nil { // maxBytes=5 < len("generation-0")
		t.Fatal(err)
	}
	assertContent(t, p+".1", "generation-0")
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Fatal("path itself should be gone after rotation (renamed to .1)")
	}

	write(p, "generation-1")
	if err := RotateIfOversized(p, 5, 2); err != nil {
		t.Fatal(err)
	}
	assertContent(t, p+".1", "generation-1")
	assertContent(t, p+".2", "generation-0")

	// A third rotation with maxBackups=2 must discard generation-0 (currently
	// at .2) entirely -- not error, not silently keep growing the chain.
	write(p, "generation-2")
	if err := RotateIfOversized(p, 5, 2); err != nil {
		t.Fatal(err)
	}
	assertContent(t, p+".1", "generation-2")
	assertContent(t, p+".2", "generation-1")
	if _, err := os.Stat(p + ".3"); err == nil {
		t.Fatal("maxBackups=2 must not keep a .3 file")
	}
}

func TestRotateIfOversizedZeroBackupsRemoves(t *testing.T) {
	p := filepath.Join(t.TempDir(), "app.log")
	if err := os.WriteFile(p, []byte("gone soon"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := RotateIfOversized(p, 1, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Fatal("expected the oversized file to be removed with maxBackups=0")
	}
}

func TestWriterRotatesAtThreshold(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "app.log")
	// Each line is 11 bytes ("0123456789\n"); maxBytes=22 fits exactly two
	// lines (0+11=11, 11+11=22, neither exceeds 22), so the third write
	// (22+11=33 > 22) is the one that must trigger rotation before it lands.
	w := NewWriter(p, 22, 3)
	defer w.Close()

	line := []byte("0123456789\n")
	for i := 0; i < 3; i++ {
		if _, err := w.Write(line); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}

	if _, err := os.Stat(p + ".1"); err != nil {
		t.Fatalf("expected a .1 backup after crossing the threshold: %v", err)
	}
	b, err := os.ReadFile(p + ".1")
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != fmt.Sprintf("%s%s", line, line) {
		t.Fatalf(".1 backup content = %q, want the first two lines", b)
	}
	cur, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if string(cur) != string(line) {
		t.Fatalf("current file content = %q, want just the third line", cur)
	}
}

func TestWriterSurvivesAcrossReopens(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "app.log")

	w1 := NewWriter(p, 1000, 3)
	if _, err := w1.Write([]byte("first\n")); err != nil {
		t.Fatal(err)
	}
	if err := w1.Close(); err != nil {
		t.Fatal(err)
	}

	// A fresh Writer over the same path (e.g. after a process restart) must
	// pick up the existing file's size, not reset its rotation threshold to
	// zero and rotate prematurely on the next write.
	w2 := NewWriter(p, 1000, 3)
	defer w2.Close()
	if _, err := w2.Write([]byte("second\n")); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "first\nsecond\n" {
		t.Fatalf("content = %q, want both writes appended in one file", b)
	}
	if _, err := os.Stat(p + ".1"); err == nil {
		t.Fatal("should not have rotated -- total content is well under maxBytes")
	}
}

func assertContent(t *testing.T, path, want string) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	if string(b) != want {
		t.Fatalf("%s content = %q, want %q", path, b, want)
	}
}
