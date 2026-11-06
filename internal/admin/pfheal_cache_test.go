package admin

import (
	"os"
	"path/filepath"
	"testing"
)

// The held lines follow the file: a guard's new line is counted on the next
// look, and a caller appending to its copy never changes the held one.
func TestPFHealEventsFollowsTheFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "pf.log")
	if err := os.WriteFile(p, []byte("preamble\nBROKEN one\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := pfHealEvents(p, 1000); len(got) != 1 {
		t.Fatalf("first read: %q", got)
	}
	f, err := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString("  indented output\nHEALED one\n")
	f.Close()
	got := pfHealEvents(p, 1000)
	if len(got) != 2 || got[1] != "HEALED one" {
		t.Fatalf("after append: %q", got)
	}
	_ = append(got, "scribble")
	if again := pfHealEvents(p, 1000); len(again) != 2 {
		t.Fatalf("held copy changed: %q", again)
	}
	if last := pfHealEvents(p, 1); len(last) != 1 || last[0] != "HEALED one" {
		t.Fatalf("max 1: %q", last)
	}
	os.Remove(p)
	if gone := pfHealEvents(p, 1000); gone != nil {
		t.Fatalf("removed file: %q", gone)
	}
}
