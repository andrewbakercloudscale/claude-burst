package repo

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A session with no transcript yet is retried after missTTL, not on every
// request and not never: its transcript appears after its first request.
func TestResolveRetriesAMissAfterTTL(t *testing.T) {
	home := t.TempDir()
	projects := filepath.Join(home, "projects")
	src := filepath.Join(home, "src", "proj")
	if err := os.MkdirAll(filepath.Join(src, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(projects, "-p"), 0o755); err != nil {
		t.Fatal(err)
	}
	r := NewWith(projects, []string{"/private/tmp/"})
	now := time.Unix(1_000_000, 0)
	r.now = func() time.Time { return now }

	if name, root := r.Resolve("s"); name != Unknown || root != "" {
		t.Fatalf("no transcript: %q %q", name, root)
	}
	body := `{"type":"user","cwd":"` + filepath.Join(src, "sub") + `"}` + "\n"
	if err := os.WriteFile(filepath.Join(projects, "-p", "s.jsonl"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if name, _ := r.Resolve("s"); name != Unknown {
		t.Fatalf("a miss is remembered for a minute: %q", name)
	}
	now = now.Add(missTTL)
	if name, root := r.Resolve("s"); name != "proj" || root != src {
		t.Fatalf("after the TTL the transcript is read: %q %q", name, root)
	}
}
