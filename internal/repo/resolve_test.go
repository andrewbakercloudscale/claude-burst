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
