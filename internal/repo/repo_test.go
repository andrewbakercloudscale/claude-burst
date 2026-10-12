package repo

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
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

// Lookups under Desktop, Documents and Downloads record whether macOS let
// them through: a refusal (EPERM) is Denied, any answer is Allowed, and a
// folder no session ran under is NotSeen. Access itself never looks.
func TestAccessRecordsWhatLookupsSaw(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	ResetAccess()
	t.Cleanup(ResetAccess)
	projects := filepath.Join(home, ".claude", "projects")
	desk := filepath.Join(home, "Desktop", "proj")
	docs := filepath.Join(home, "Documents", "other")
	for _, d := range []string{filepath.Join(desk, ".git"), filepath.Join(docs, ".git"), filepath.Join(projects, "-p")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for sid, cwd := range map[string]string{"a": filepath.Join(desk, "sub"), "b": docs} {
		body := `{"type":"user","cwd":"` + cwd + `"}` + "\n"
		if err := os.WriteFile(filepath.Join(projects, "-p", sid+".jsonl"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// Documents answers as a folder macOS refused would.
	realStat := stat
	t.Cleanup(func() { stat = realStat })
	stat = func(p string) (os.FileInfo, error) {
		if strings.HasPrefix(p, filepath.Join(home, "Documents")+"/") {
			return nil, &fs.PathError{Op: "stat", Path: p, Err: syscall.EPERM}
		}
		return realStat(p)
	}

	for _, a := range Access() {
		if a.State != NotSeen {
			t.Fatalf("nothing looked yet, %s is %q", a.Folder, a.State)
		}
	}
	r := NewWith(projects, []string{"/private/tmp/"})
	if _, root := r.Resolve("a"); root != desk {
		t.Fatalf("Desktop repo root %q, want %q", root, desk)
	}
	r.Resolve("b")

	got := map[string]FolderAccess{}
	for _, a := range Access() {
		got[a.Folder] = a
	}
	if a := got["Desktop"]; a.State != Allowed || len(a.Repos) != 1 || a.Repos[0] != desk {
		t.Errorf("Desktop: %+v", a)
	}
	if a := got["Documents"]; a.State != Denied || len(a.Repos) != 0 {
		t.Errorf("Documents: %+v", a)
	}
	if a := got["Downloads"]; a.State != NotSeen {
		t.Errorf("Downloads: %+v", a)
	}
}

// A session id is a request header. One that is not a plain id never
// reaches the filesystem: no walking out of the projects directory, and no
// pattern that matches another session's transcript.
func TestResolveRefusesAnIDThatIsNotOne(t *testing.T) {
	home := t.TempDir()
	projects := filepath.Join(home, "projects")
	src := filepath.Join(home, "src", "proj")
	for _, d := range []string{filepath.Join(src, ".git"), filepath.Join(projects, "-p"), filepath.Join(home, "elsewhere", "x")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	body := `{"type":"user","cwd":"` + src + `"}` + "\n"
	for _, p := range []string{filepath.Join(projects, "-p", "0199c1d2-7a5b-7000-8000-000000000001.jsonl"), filepath.Join(home, "elsewhere", "x", "outside.jsonl")} {
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	r := NewWith(projects, nil)
	for _, id := range []string{"*", "0199c1d2*", "?199c1d2-7a5b-7000-8000-000000000001", "[0-9]199c1d2-7a5b-7000-8000-000000000001",
		"../../elsewhere/x/outside", "-p/0199c1d2-7a5b-7000-8000-000000000001", "", strings.Repeat("a", 129)} {
		if name, root := r.Resolve(id); name != Unknown || root != "" {
			t.Errorf("id %q resolved to %q %q", id, name, root)
		}
	}
	if len(r.miss) != 0 {
		t.Errorf("refused ids were remembered: %v", r.miss)
	}
	if name, root := r.Resolve("0199c1d2-7a5b-7000-8000-000000000001"); name != "proj" || root != src {
		t.Fatalf("a real id: %q %q", name, root)
	}
}
