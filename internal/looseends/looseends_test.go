package looseends

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func run(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.email=t@example.com", "-c", "user.name=t"}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

func write(t *testing.T, p, body string) {
	t.Helper()
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// newRepo is a clone of a bare origin with one pushed commit.
func newRepo(t *testing.T) string {
	t.Helper()
	base, _ := filepath.EvalSymlinks(t.TempDir())
	origin, repo := filepath.Join(base, "origin.git"), filepath.Join(base, "work")
	run(t, base, "init", "-q", "--bare", "-b", "main", origin)
	run(t, base, "clone", "-q", origin, repo)
	run(t, repo, "checkout", "-q", "-b", "main")
	write(t, filepath.Join(repo, "a.txt"), "a\n")
	write(t, filepath.Join(repo, "b.txt"), "b\n")
	run(t, repo, "add", "a.txt", "b.txt")
	run(t, repo, "commit", "-qm", "init")
	run(t, repo, "push", "-q", "-u", "origin", "main")
	return repo
}

func TestACleanPushedRepositoryIsNotLoose(t *testing.T) {
	repo := newRepo(t)
	r := ScanRepo(repo, nil)
	if r.Loose() || r.Error != "" || r.Branch != "main" || r.Upstream != "origin/main" {
		t.Fatalf("clean repo: %+v", r)
	}
	if rp := Scan([]string{repo}, nil, time.Now()); len(rp.Repos) != 0 || rp.Scanned != 1 {
		t.Fatalf("clean repo listed: %+v", rp)
	}
}

func TestItFindsEveryKindOfLooseEnd(t *testing.T) {
	repo := newRepo(t)
	// Unpushed: two commits.
	for _, m := range []string{"one", "two"} {
		write(t, filepath.Join(repo, "c.txt"), m)
		run(t, repo, "add", "c.txt")
		run(t, repo, "commit", "-qm", "commit "+m)
	}
	// A stash.
	write(t, filepath.Join(repo, "a.txt"), "stashed\n")
	run(t, repo, "stash", "-q")
	// Uncommitted: a change, a rename, an untracked file in a new folder.
	write(t, filepath.Join(repo, "a.txt"), "changed\n")
	run(t, repo, "mv", "b.txt", "renamed.txt")
	os.MkdirAll(filepath.Join(repo, "new"), 0o755)
	write(t, filepath.Join(repo, "new", "u.txt"), "u")

	owners := map[string]Owner{filepath.Join(repo, "a.txt"): {Session: "s1", Label: "work [s1]", Since: 100}}
	r := ScanRepo(repo, owners)
	if r.Ahead != 2 || len(r.Unpushed) != 2 || r.Unpushed[0].Subject != "commit two" {
		t.Fatalf("unpushed: ahead %d, %+v", r.Ahead, r.Unpushed)
	}
	if len(r.Stashes) != 1 || r.Stashes[0].Ref != "stash@{0}" {
		t.Fatalf("stashes: %+v", r.Stashes)
	}
	got := map[string]string{}
	for _, f := range r.Uncommitted {
		got[strings.TrimPrefix(f.Path, repo+"/")] = f.Status
	}
	want := map[string]string{"a.txt": " M", "renamed.txt": "R ", "new/u.txt": "??"}
	if len(got) != len(want) {
		t.Fatalf("uncommitted = %v, want %v (a rename's source must not show as a file)", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("uncommitted = %v, want %v", got, want)
		}
	}
	for _, f := range r.Uncommitted {
		if strings.HasSuffix(f.Path, "/a.txt") && (f.Owner == nil || f.Owner.Session != "s1") {
			t.Fatalf("a.txt owner = %+v", f.Owner)
		}
		if f.MTime == 0 {
			t.Fatalf("%s has no mtime", f.Path)
		}
	}
	if r.Oldest() != 100 {
		t.Fatalf("oldest = %v, want the owner's 100", r.Oldest())
	}
	files, commits, stashes := Report{Repos: []Repo{r}}.Counts()
	if files != 3 || commits != 2 || stashes != 1 {
		t.Fatalf("counts %d %d %d", files, commits, stashes)
	}
}

// A branch that tracks nothing counts what no remote branch has; a
// repository with no remote has nowhere to push, so nothing is unpushed.
func TestBranchesWithoutAnUpstream(t *testing.T) {
	repo := newRepo(t)
	run(t, repo, "checkout", "-q", "-b", "side")
	write(t, filepath.Join(repo, "s.txt"), "s")
	run(t, repo, "add", "s.txt")
	run(t, repo, "commit", "-qm", "side work")
	if r := ScanRepo(repo, nil); r.Upstream != "" || r.Ahead != 1 {
		t.Fatalf("untracked branch: %+v", r)
	}

	local, _ := filepath.EvalSymlinks(t.TempDir())
	run(t, local, "init", "-q", "-b", "main")
	write(t, filepath.Join(local, "x"), "x")
	run(t, local, "add", "x")
	run(t, local, "commit", "-qm", "x")
	if r := ScanRepo(local, nil); !r.NoRemote || r.Ahead != 0 || r.Loose() {
		t.Fatalf("no-remote repo: %+v", r)
	}
}

func TestRootsComeFromRecentTranscripts(t *testing.T) {
	if !scratch(t.TempDir()) {
		t.Fatal("a temporary directory is not taken for scratch")
	}
	was := scratch
	scratch = func(string) bool { return false }
	defer func() { scratch = was }()
	repo := newRepo(t)
	os.MkdirAll(filepath.Join(repo, "sub"), 0o755)
	projects := t.TempDir()
	old := filepath.Join(projects, "p2", "old.jsonl")
	for p, cwd := range map[string]string{
		filepath.Join(projects, "p1", "s1.jsonl"): filepath.Join(repo, "sub"),
		filepath.Join(projects, "p1", "s2.jsonl"): repo,
		filepath.Join(projects, "p3", "s3.jsonl"): t.TempDir(), // not a repository
		old: repo + "-gone",
	} {
		os.MkdirAll(filepath.Dir(p), 0o755)
		write(t, p, `{"type":"summary"}`+"\n"+`{"type":"user","cwd":"`+cwd+`"}`+"\n")
	}
	past := time.Now().Add(-60 * 24 * time.Hour)
	os.Chtimes(old, past, past)
	got := Roots(projects, 30*24*time.Hour, nil, time.Now())
	if len(got) != 1 || got[0] != repo {
		t.Fatalf("roots = %v, want [%s]", got, repo)
	}
}
