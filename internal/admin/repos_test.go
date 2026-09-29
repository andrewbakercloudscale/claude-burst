package admin

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/andrewbakercloudscale/claude-burst/internal/metrics"
)

func TestRepoSpendGroupsSessionsByRepository(t *testing.T) {
	home := t.TempDir()
	repo := filepath.Join(home, "work", "proj")
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(repo, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	projects := filepath.Join(home, "claude", "projects", "-x")
	if err := os.MkdirAll(projects, 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(sid, cwd string) {
		body := `{"type":"custom-title","sessionId":"` + sid + `"}` + "\n" +
			`{"type":"user","cwd":"` + cwd + `","sessionId":"` + sid + `"}` + "\n"
		if err := os.WriteFile(filepath.Join(projects, sid+".jsonl"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("a", repo)
	write("b", filepath.Join(repo, "sub")) // a subdirectory counts as the repo
	write("c", "/private/tmp/scratch")
	r := &repoResolver{cache: map[string][2]string{}, dir: filepath.Join(home, "claude", "projects"),
		temp: []string{"/private/tmp/"}}
	got := r.repoSpend(map[string]metrics.SessionUse{
		"a": {Requests: 2, USD: 1}, "b": {Requests: 3, USD: 4},
		"c": {Requests: 1, USD: 0.5}, "gone": {Requests: 1, USD: 0.25},
	})
	want := []struct {
		repo     string
		sessions int
		usd      float64
	}{{"proj", 2, 5}, {tempRepo, 1, 0.5}, {unknownRepo, 1, 0.25}}
	if len(got) != len(want) {
		t.Fatalf("want %d repos, got %+v", len(want), got)
	}
	for i, w := range want {
		if got[i].Repo != w.repo || got[i].Sessions != w.sessions || got[i].USD != w.usd {
			t.Fatalf("row %d: want %+v, got %+v", i, w, got[i])
		}
	}
	if got[0].Path != repo {
		t.Fatalf("repo path: %q", got[0].Path)
	}
}
