package repo

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

// 32 project directories of 118 transcripts, the shape of the projects
// directory the lookup was measured against on 1 Nov 2026 (3,774
// transcripts): a lookup's cost is in how many names it has to read.
const (
	benchProjectDirs = 32
	benchPerDir      = 118
)

func benchProjects(b *testing.B) (projects, foundSession string) {
	b.Helper()
	root := b.TempDir()
	projects = filepath.Join(root, "projects")
	src := filepath.Join(root, "src", "proj")
	if err := os.MkdirAll(filepath.Join(src, ".git"), 0o755); err != nil {
		b.Fatal(err)
	}
	body := []byte(`{"type":"user","cwd":"` + src + `"}` + "\n")
	for d := range benchProjectDirs {
		dir := filepath.Join(projects, fmt.Sprintf("-proj-%02d", d))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			b.Fatal(err)
		}
		for f := range benchPerDir {
			id := fmt.Sprintf("%08x-0000-4000-8000-%012x", d, f)
			if err := os.WriteFile(filepath.Join(dir, id+".jsonl"), body, 0o600); err != nil {
				b.Fatal(err)
			}
			if d == benchProjectDirs-2 && f == benchPerDir/2 {
				foundSession = id
			}
		}
	}
	return projects, foundSession
}

func BenchmarkResolveMiss(b *testing.B) {
	projects, _ := benchProjects(b)
	r := NewWith(projects, []string{"/private/tmp/"})
	b.ReportAllocs()
	i := 0
	for b.Loop() {
		i++
		if name, _ := r.Resolve("gone-" + strconv.Itoa(i)); name != Unknown {
			b.Fatalf("resolved %q", name)
		}
	}
}

func BenchmarkResolveFound(b *testing.B) {
	projects, found := benchProjects(b)
	b.ReportAllocs()
	for b.Loop() {
		r := NewWith(projects, []string{"/private/tmp/"})
		if name, _ := r.Resolve(found); name != "proj" {
			b.Fatalf("resolved %q", name)
		}
	}
}
