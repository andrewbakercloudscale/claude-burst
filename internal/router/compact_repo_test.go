package router

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/andrewbakercloudscale/claude-burst/internal/config"
	"github.com/andrewbakercloudscale/claude-burst/internal/repo"
)

// A repository's own Compact at decides, "off" never compacts on its own,
// and a session whose repository is unknown gets the default.
func TestCompactionPerRepository(t *testing.T) {
	home := t.TempDir()
	projects := filepath.Join(home, "projects", "-x")
	mk := func(name string) string {
		d := filepath.Join(home, "src", name)
		if err := os.MkdirAll(filepath.Join(d, ".git"), 0o755); err != nil {
			t.Fatal(err)
		}
		return d
	}
	big, small, never := mk("big"), mk("small"), mk("never")
	if err := os.MkdirAll(projects, 0o755); err != nil {
		t.Fatal(err)
	}
	transcript := func(sid, cwd string) {
		body := `{"type":"user","cwd":"` + cwd + `","sessionId":"` + sid + `"}` + "\n"
		if err := os.WriteFile(filepath.Join(projects, sid+".jsonl"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	transcript("in-big", big)
	transcript("in-small", small)
	transcript("in-never", never)

	// 450k of context: over the 400k default, under big's 600k.
	f := &fakeAnthropic{context: 450_000}
	s := compactServer(t, f, config.CompactionConfig{Enabled: true, CompactAtTokens: 400_000, WindowMinutes: 60,
		RepoOverrides: []config.RepoCompaction{
			{Repo: big + "/", CompactAtTokens: 600_000},
			{Repo: never, Off: true},
		}})
	s.repos = repo.NewWith(filepath.Join(home, "projects"), nil)
	all := msgs(t, session)

	run := func(sid string) int {
		before := f.summaryCount()
		send(t, s, sid, all[:5])
		send(t, s, sid, all[:7])
		s.compaction.running.Wait()
		return f.summaryCount() - before
	}
	if n := run("in-big"); n != 0 {
		t.Fatalf("big compacts at 600k: 450k must not start a summary, got %d", n)
	}
	if n := run("in-never"); n != 0 {
		t.Fatalf("compaction off for never: got %d summaries", n)
	}
	if n := run("in-small"); n != 1 {
		t.Fatalf("small has no override, so the 400k default: want 1 summary, got %d", n)
	}
	if n := run("no-transcript"); n != 1 {
		t.Fatalf("an unknown repository falls back to the default: want 1 summary, got %d", n)
	}

	got := map[string]CompactionSession{}
	for _, c := range s.CompactionSessions() {
		got[c.Session] = c
	}
	for sid, want := range map[string]struct {
		at       int64
		override bool
		repo     string
	}{
		"in-big":        {600_000, true, "big"},
		"in-never":      {0, true, "never"},
		"in-small":      {400_000, false, "small"},
		"no-transcript": {400_000, false, repo.Unknown},
	} {
		c := got[sid]
		if c.CompactAt != want.at || c.Override != want.override || c.Repo != want.repo {
			t.Errorf("%s: compact_at=%d override=%v repo=%q, want %d %v %q", sid, c.CompactAt, c.Override, c.Repo, want.at, want.override, want.repo)
		}
	}
}
