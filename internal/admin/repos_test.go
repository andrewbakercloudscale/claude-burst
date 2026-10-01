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
	}, map[string]float64{"a": 3, "b": -0.5})
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
	// Compaction's saving adds up per repository, net (b's is negative),
	// and a repository nothing was compacted in says so.
	if !got[0].Compacted || got[0].SavedUSD != 2.5 {
		t.Fatalf("proj saved: %+v", got[0])
	}
	if got[1].Compacted || got[1].SavedUSD != 0 {
		t.Fatalf("nothing compacted in %s: %+v", got[1].Repo, got[1])
	}

	// The savings section's split: same sessions, grouped the same way,
	// largest net first, every column summed.
	sv := r.savingsByRepo([]metrics.CompactedSession{
		{Session: "c", Compactions: 1, Requests: 2, SavedUSD: 1, SummaryUSD: 0.25, NetUSD: 0.75},
		{Session: "a", Compactions: 2, Requests: 5, SavedTokens: 100, SavedUSD: 4, SummaryUSD: 0.5, RewriteUSD: 0.5, NetUSD: 3},
		{Session: "b", Compactions: 1, Requests: 1, SavedTokens: 50, SavedUSD: 0.25, RewriteUSD: 0.75, NetUSD: -0.5},
	})
	if len(sv) != 2 || sv[0].Repo != "proj" || sv[1].Repo != tempRepo {
		t.Fatalf("savings by repo: %+v", sv)
	}
	if p := sv[0]; p.Sessions != 2 || p.Compactions != 3 || p.Requests != 6 || p.SavedTokens != 150 ||
		p.SavedUSD != 4.25 || p.SummaryUSD != 0.5 || p.RewriteUSD != 1.25 || p.NetUSD != 2.5 || p.Path != repo {
		t.Fatalf("proj savings: %+v", p)
	}
}
