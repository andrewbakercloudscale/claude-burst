package repo

import (
	"os"
	"path/filepath"
	"testing"
)

// Resolve takes the transcript in the first project directory by name, as
// filepath.Glob did, and passes over a plain file among the directories.
func TestResolveFindsTheFirstProjectHoldingTheSession(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		holders  []string
		wantName string
	}{
		"only one project holds it":        {holders: []string{"-b"}, wantName: "repo-b"},
		"first by name wins when two do":   {holders: []string{"-b", "-a"}, wantName: "repo-a"},
		"no project holds it":              {holders: nil, wantName: Unknown},
		"a plain file in the projects dir": {holders: []string{"-c"}, wantName: "repo-c"},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			projects := filepath.Join(root, "projects")
			if err := os.MkdirAll(projects, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(projects, "-0-stray-file"), nil, 0o600); err != nil {
				t.Fatal(err)
			}
			for _, h := range tc.holders {
				src := filepath.Join(root, "src", "repo"+h)
				if err := os.MkdirAll(filepath.Join(src, ".git"), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.MkdirAll(filepath.Join(projects, h), 0o755); err != nil {
					t.Fatal(err)
				}
				body := []byte(`{"type":"user","cwd":"` + src + `"}` + "\n")
				if err := os.WriteFile(filepath.Join(projects, h, "s1.jsonl"), body, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			got, _ := NewWith(projects, []string{"/private/tmp/"}).Resolve("s1")
			if got != tc.wantName {
				t.Errorf("Resolve = %q, want %q", got, tc.wantName)
			}
		})
	}
}

// Claude Desktop's scratch sessions group under one name rather than one
// repository each, and a Claude Code worktree is named after the repository
// it belongs to (17 Nov 2026: Desktop runs most Code sessions in one).
func TestResolveNamesDesktopScratchAndWorktrees(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	projects := filepath.Join(root, "projects")
	scratch := filepath.Join(root, "Library", "Application Support", "Claude", "scratch-workspaces")
	repoDir := filepath.Join(root, "src", "shop")
	wt := filepath.Join(repoDir, ".claude", "worktrees", "eager-turing-a1b2")
	for _, d := range []string{filepath.Join(repoDir, ".git"), wt, filepath.Join(scratch, "u1", "u2", "scratch-2026-11-17-cae9f8")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// A worktree's .git is a file naming the main repository's.
	if err := os.WriteFile(filepath.Join(wt, ".git"), []byte("gitdir: "+repoDir+"/.git/worktrees/eager-turing-a1b2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sessions := map[string]string{
		"scr": filepath.Join(scratch, "u1", "u2", "scratch-2026-11-17-cae9f8"),
		"wt":  filepath.Join(wt, "pkg"),
		"top": repoDir,
	}
	for sid, cwd := range sessions {
		d := filepath.Join(projects, "-"+sid)
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		if cwd == sessions["wt"] {
			if err := os.MkdirAll(cwd, 0o755); err != nil {
				t.Fatal(err)
			}
		}
		body := `{"type":"user","entrypoint":"claude-desktop","cwd":"` + cwd + `"}` + "\n"
		if err := os.WriteFile(filepath.Join(d, sid+".jsonl"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	r := NewWith(projects, nil).WithDesktopScratch(scratch)
	if name, root := r.Resolve("scr"); name != DesktopScratch || root != "" {
		t.Errorf("scratch = %q %q", name, root)
	}
	if name, root := r.Resolve("wt"); name != "shop" || root != repoDir {
		t.Errorf("worktree = %q %q, want shop %q", name, root, repoDir)
	}
	if name, root := r.Resolve("top"); name != "shop" || root != repoDir {
		t.Errorf("repo = %q %q", name, root)
	}
	// A folder merely named "worktrees" elsewhere is not one of Claude Code's.
	if _, ok := worktreeOwner("/x/worktrees/y"); ok {
		t.Error("worktreeOwner matched a plain worktrees folder")
	}
	if _, ok := worktreeOwner("/x/.claude/worktrees/y/z"); ok {
		t.Error("worktreeOwner matched below a worktree's root")
	}
}
